/**
 * 模型可用时间窗的纯逻辑层（前端）。
 *
 * 权威在后端：`models/availability.go` 的 Compile/AvailableAt 是语义定义，
 * `handler/availability.go` 的 validateAvailability 是最终裁决。这一层做三件事：
 *
 *   1. 维护 7×48 的槽位网格，并把网格 ↔ 后端区间列表来回翻译；
 *   2. 在提交前把明显的问题（一格没画）指出来，省一次往返；
 *   3. 给列表行的状态徽标算「此刻是否可用 / 下次几点变」，让用户配完立刻能验证。
 *
 * ## 两态而不是三态
 *
 * 后端字段有 `enabled`，前端**不暴露**它：`enabled=false` 与"没配"在运行期
 * 完全等价（都恒可用），把它做成界面上一个开关，用户只会问"不限时间和暂停
 * 有什么区别"。因此：
 *
 *   - `null`          → 不限时间（24×7），后端落 NULL
 *   - 有值            → 已配置且生效，提交时 `enabled: true`
 *
 * 代价：通过 API 设成 `enabled=false` 的存量配置，在界面打开后再保存会变成
 * `null`（表格丢了，行为不变）。这是刻意的取舍——留着那个中间态会让弹窗多
 * 一个没人理解的选项，而它带来的能力（"保留表格但先不用"）用预设按钮三秒
 * 就能重画。
 *
 * ## 半小时粒度与不跨零点
 *
 * 网格一行就是 48 个槽，22:00-02:00 天然落在两行上。这里因此**没有**跨零点
 * 概念——后端也拒绝 `start > end`（见 validateAvailability）。别为了"表达更
 * 自然"给它加跨零点支持：那段"算周几"的歧义会被每一处判定重复一遍。
 *
 * ## 时区
 *
 * 所有判定用**浏览器本地时区**。真正的判定在服务端配置时区（peak_calendar），
 * 两者不同时徽标可能显示错，因此界面上必须写明"按本地时区估算"。要让前端
 * 精确对齐，就得把时区随配置一起下发并自己做时区换算——为了一行状态徽标
 * 引入一整套时区逻辑，不划算。
 */

import { WEEKDAY_DISPLAY_ORDER, formatClock, parseClock, weekdayKey } from "./peak"

export { WEEKDAY_DISPLAY_ORDER, weekdayKey }

export const SLOTS_PER_DAY = 48
export const SLOT_MINUTES = 30

/** 工作日的时间窗预设：09:00-18:00。 */
const WORK_START_MINUTES = 9 * 60
const WORK_END_MINUTES = 18 * 60
const WORKDAYS = [1, 2, 3, 4, 5]

export type AvailabilityMode = "allow" | "deny"

/** 后端契约形状，与 models/availability.go 的 json tag 一一对应。 */
export interface AvailabilitySlot {
  /** 0=周日 … 6=周六 */
  day: number
  /** "HH:MM" */
  start: string
  /** "HH:MM" 或 "24:00" */
  end: string
}

export interface AvailabilityTerms {
  enabled: boolean
  mode: AvailabilityMode
  slots: AvailabilitySlot[]
}

/** 表单态：7 行 × 48 列的槽位网格。grid[day][slot] 为 true 表示该槽在列表内。 */
export interface AvailabilityForm {
  mode: AvailabilityMode
  grid: boolean[][]
}

export type AvailabilityPreset = "workHours" | "weekdays" | "all" | "none"

// ---------------------------------------------------------------------------
// 网格构造
// ---------------------------------------------------------------------------

export function emptyGrid(): boolean[][] {
  return Array.from({ length: 7 }, () => new Array<boolean>(SLOTS_PER_DAY).fill(false))
}

export function fullGrid(): boolean[][] {
  return Array.from({ length: 7 }, () => new Array<boolean>(SLOTS_PER_DAY).fill(true))
}

/** 默认新建时的网格：工作日 09:00-18:00。这是绝大多数人的真实需求。 */
export function defaultForm(): AvailabilityForm {
  return { mode: "allow", grid: applyPreset("workHours") }
}

export function applyPreset(preset: AvailabilityPreset): boolean[][] {
  switch (preset) {
    case "all":
      return fullGrid()
    case "none":
      return emptyGrid()
    case "weekdays": {
      const grid = emptyGrid()
      for (const day of WORKDAYS) grid[day].fill(true)
      return grid
    }
    case "workHours":
    default: {
      const grid = emptyGrid()
      const from = WORK_START_MINUTES / SLOT_MINUTES
      const to = WORK_END_MINUTES / SLOT_MINUTES
      for (const day of WORKDAYS) {
        for (let slot = from; slot < to; slot++) grid[day][slot] = true
      }
      return grid
    }
  }
}

// ---------------------------------------------------------------------------
// 网格 ↔ 后端区间列表
// ---------------------------------------------------------------------------

/** 网格 → 区间列表。每行的连续 true 段合并成一条，全行开就是 "00:00"-"24:00"。 */
export function slotsFromGrid(grid: boolean[][]): AvailabilitySlot[] {
  const slots: AvailabilitySlot[] = []
  for (let day = 0; day < 7; day++) {
    const row = grid[day] ?? []
    let start = -1
    for (let i = 0; i <= SLOTS_PER_DAY; i++) {
      const on = i < SLOTS_PER_DAY && row[i] === true
      if (on && start < 0) start = i
      if (!on && start >= 0) {
        slots.push({
          day,
          start: formatClock(start * SLOT_MINUTES),
          end: formatClock(i * SLOT_MINUTES),
        })
        start = -1
      }
    }
  }
  return slots
}

/** 区间列表 → 网格。非法条目直接忽略，与后端 Compile 的"脏数据跳过"一致。 */
export function gridFromSlots(slots: AvailabilitySlot[] | null | undefined): boolean[][] {
  const grid = emptyGrid()
  for (const slot of slots ?? []) {
    if (!Number.isInteger(slot.day) || slot.day < 0 || slot.day > 6) continue
    const start = parseClock(slot.start)
    const end = parseClock(slot.end)
    if (start === null || end === null) continue
    if (start % SLOT_MINUTES !== 0 || end % SLOT_MINUTES !== 0) continue
    if (start >= end) continue
    for (let i = start / SLOT_MINUTES; i < end / SLOT_MINUTES; i++) grid[slot.day][i] = true
  }
  return grid
}

export function termsToForm(terms: AvailabilityTerms | null | undefined): AvailabilityForm | null {
  if (!terms) return null
  // enabled=false 在运行期与"没配"完全等价，前端不保留这个中间态（见文件头）。
  if (!terms.enabled) return null
  return {
    mode: terms.mode === "deny" ? "deny" : "allow",
    grid: gridFromSlots(terms.slots),
  }
}

export function formToTerms(form: AvailabilityForm | null): AvailabilityTerms | null {
  if (!form) return null
  return { enabled: true, mode: form.mode, slots: slotsFromGrid(form.grid) }
}

// ---------------------------------------------------------------------------
// 网格编辑（全部返回新对象，方便 React 与撤销）
// ---------------------------------------------------------------------------

function clone(grid: boolean[][]): boolean[][] {
  return grid.map((row) => [...row])
}

export function isOn(grid: boolean[][], day: number, slot: number): boolean {
  return grid[day]?.[slot] === true
}

export function setSlot(grid: boolean[][], day: number, slot: number, on: boolean): boolean[][] {
  const next = clone(grid)
  next[day][slot] = on
  return next
}

/**
 * 把 [from, to]（含两端）刷成同一个值。拖动选择与快捷操作都走这里。
 *
 * 调用方负责保证 from <= to；颠倒的区间在这里返回原网格而不是静默交换——
 * 静默交换会把"拖回去取消"变成"再刷一遍"，手感直接坏掉。
 */
export function paintRange(grid: boolean[][], day: number, from: number, to: number, on: boolean): boolean[][] {
  if (from > to) return grid
  const next = clone(grid)
  for (let i = from; i <= to; i++) next[day][i] = on
  return next
}

/** 点星期标签：整行取反（不是"全开"，避免用户想全关时得点两次）。 */
export function toggleRow(grid: boolean[][], day: number): boolean[][] {
  const next = clone(grid)
  const row = next[day]
  const nextValue = !row.every(Boolean)
  row.fill(nextValue)
  return next
}

/** 点小时刻度：整列取反。列是"每个半小时槽"，索引 0..47。 */
export function toggleColumn(grid: boolean[][], slot: number): boolean[][] {
  const next = clone(grid)
  const nextValue = !next.every((row) => row[slot])
  for (const row of next) row[slot] = nextValue
  return next
}

/** 逐格取反。配合「工作时间」一键得到"非工作时间禁用"。 */
export function invertGrid(grid: boolean[][]): boolean[][] {
  return grid.map((row) => row.map((on) => !on))
}

export function countOn(grid: boolean[][]): number {
  return grid.reduce((sum, row) => sum + row.filter(Boolean).length, 0)
}

// ---------------------------------------------------------------------------
// 判定（仅用于界面回显）
// ---------------------------------------------------------------------------

/** 某时刻对应的槽位序号（0..47）。 */
export function slotOf(at: Date): number {
  return Math.floor((at.getHours() * 60 + at.getMinutes()) / SLOT_MINUTES)
}

/** 此刻是否可用。与后端 service.availabilityGate.available 同一套取反逻辑。 */
export function isAvailableAt(form: AvailabilityForm, at: Date): boolean {
  const on = isOn(form.grid, at.getDay(), slotOf(at))
  return on !== (form.mode === "deny")
}

/**
 * 下一次状态翻转的时刻；一周内不变则返回 null。
 *
 * 用毫秒加法而不是 setMinutes：跨夏令时会偏一小时，但本项目在中国无夏令时，
 * 而 setMinutes 在 DST 边界上的行为更绕。真需要精确到秒的场景不该在前端算。
 */
export function nextChangeAt(form: AvailabilityForm, at: Date): Date | null {
  const current = isAvailableAt(form, at)
  const base = new Date(at.getTime())
  base.setSeconds(0, 0)
  base.setMinutes(Math.floor(base.getMinutes() / SLOT_MINUTES) * SLOT_MINUTES)

  for (let i = 1; i <= 7 * SLOTS_PER_DAY; i++) {
    const t = new Date(base.getTime() + i * SLOT_MINUTES * 60_000)
    if (isAvailableAt(form, t) !== current) return t
  }
  return null
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

export type AvailabilityIssueKey = "no_slots"

export interface AvailabilityIssue {
  key: AvailabilityIssueKey
}

/**
 * 提交前校验。只拦后端一定会拒的那一条：allow 模式下启用却没画任何槽。
 *
 * deny + 空表是合法的（= 不禁用任何时段），后端已放宽，这里也必须放行——
 * 前端比后端严就是在发明规则。
 */
export function validateForm(form: AvailabilityForm | null): AvailabilityIssue[] {
  if (!form) return []
  if (form.mode === "allow" && countOn(form.grid) === 0) {
    return [{ key: "no_slots" }]
  }
  return []
}

// ---------------------------------------------------------------------------
// 展示
// ---------------------------------------------------------------------------

/** 把区间列表压成一句摘要，用于列表徽标的 tooltip。 */
export function summarizeSlots(slots: AvailabilitySlot[]): string {
  if (slots.length === 0) return ""
  return slots
    .map((s) => `${weekdayShort(s.day)} ${s.start}-${s.end}`)
    .join("  ")
}

/** 星期缩写（0=周日）。不走向 i18n：它只出现在紧凑的 tooltip 里。 */
export function weekdayShort(day: number): string {
  return ["日", "一", "二", "三", "四", "五", "六"][day] ?? "?"
}

/** 徽标用的形态。 */
export type AvailabilityBadge =
  | { kind: "unlimited" }
  | { kind: "restricted"; availableNow: boolean; nextChange: Date | null }

export function describeAvailability(
  terms: AvailabilityTerms | null | undefined,
  form: AvailabilityForm | null,
  now: Date,
): AvailabilityBadge {
  // 编辑中的草稿优先：用户刚改完还没保存，徽标也该跟着动。
  if (form) {
    return {
      kind: "restricted",
      availableNow: isAvailableAt(form, now),
      nextChange: nextChangeAt(form, now),
    }
  }
  if (!terms || !terms.enabled) return { kind: "unlimited" }
  const loaded = termsToForm(terms)
  if (!loaded) return { kind: "unlimited" }
  return {
    kind: "restricted",
    availableNow: isAvailableAt(loaded, now),
    nextChange: nextChangeAt(loaded, now),
  }
}
