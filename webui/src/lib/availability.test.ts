import { describe, expect, it } from "vitest"

import {
  SLOTS_PER_DAY,
  applyPreset,
  countOn,
  emptyGrid,
  formToTerms,
  gridFromSlots,
  invertGrid,
  isAvailableAt,
  nextChangeAt,
  paintRange,
  setSlot,
  slotsFromGrid,
  termsToForm,
  toggleRow,
  validateForm,
  type AvailabilityForm,
} from "./availability"

/** 2026-11-02 是周一。用本地时间构造，避免与浏览器时区打架。 */
const monday = (hour: number, minute = 0) => new Date(2026, 10, 2, hour, minute, 0, 0)

const slotIndex = (hour: number, minute = 0) => (hour * 60 + minute) / 30

function formWith(slots: Parameters<typeof gridFromSlots>[0], mode: "allow" | "deny" = "allow"): AvailabilityForm {
  return { mode, grid: gridFromSlots(slots) }
}

describe("网格 ↔ 区间列表", () => {
  it("整行全开合并成 00:00-24:00", () => {
    const grid = emptyGrid()
    grid[1].fill(true)
    expect(slotsFromGrid(grid)).toEqual([{ day: 1, start: "00:00", end: "24:00" }])
  })

  it("同一天的多个连续段各成一条", () => {
    const grid = emptyGrid()
    for (let i = slotIndex(9); i < slotIndex(12); i++) grid[1][i] = true
    for (let i = slotIndex(14); i < slotIndex(18); i++) grid[1][i] = true
    expect(slotsFromGrid(grid)).toEqual([
      { day: 1, start: "09:00", end: "12:00" },
      { day: 1, start: "14:00", end: "18:00" },
    ])
  })

  it("往返一致：slots → grid → slots", () => {
    const slots = [
      { day: 0, start: "00:00", end: "24:00" },
      { day: 1, start: "09:30", end: "18:00" },
      { day: 3, start: "22:00", end: "23:30" },
      { day: 6, start: "06:00", end: "07:30" },
    ]
    expect(slotsFromGrid(gridFromSlots(slots))).toEqual(slots)
  })

  it("脏数据整条跳过，不做向下取整", () => {
    const grid = gridFromSlots([
      { day: 1, start: "09:15", end: "18:00" }, // 未对齐
      { day: 1, start: "22:00", end: "02:00" }, // 跨零点
      { day: 7, start: "09:00", end: "10:00" }, // day 越界
      { day: 1, start: "9am", end: "10:00" }, // 写坏
    ])
    expect(countOn(grid)).toBe(0)
  })

  it("单槽就是半小时", () => {
    const grid = setSlot(emptyGrid(), 2, slotIndex(9, 30), true)
    expect(slotsFromGrid(grid)).toEqual([{ day: 2, start: "09:30", end: "10:00" }])
  })
})

describe("编辑操作", () => {
  it("行取反：全关变全开，全开变全关", () => {
    const on = toggleRow(emptyGrid(), 1)
    expect(countOn(on)).toBe(SLOTS_PER_DAY)
    expect(countOn(toggleRow(on, 1))).toBe(0)
  })

  it("行取反不是一律置满：部分选中会被补满", () => {
    const partial = setSlot(emptyGrid(), 1, 0, true)
    expect(countOn(toggleRow(partial, 1))).toBe(SLOTS_PER_DAY)
  })

  it("刷区间从快照重算，拖回去能恢复", () => {
    const base = emptyGrid()
    const painted = paintRange(base, 1, 10, 20, true)
    expect(countOn(painted)).toBe(11)
    // 拖回起点：从原始快照只刷一格
    expect(countOn(paintRange(base, 1, 10, 10, true))).toBe(1)
  })

  it("倒序区间原样返回（不静默交换）", () => {
    const base = emptyGrid()
    expect(paintRange(base, 1, 20, 10, true)).toBe(base)
  })

  it("反转后开与关互换", () => {
    const grid = applyPreset("workHours")
    expect(countOn(invertGrid(grid))).toBe(7 * SLOTS_PER_DAY - countOn(grid))
  })
})

describe("判定", () => {
  it("allow 模式只在选中的槽内可用", () => {
    const form = formWith([{ day: 1, start: "09:00", end: "18:00" }])
    expect(isAvailableAt(form, monday(9, 0))).toBe(true)
    expect(isAvailableAt(form, monday(17, 30))).toBe(true)
    expect(isAvailableAt(form, monday(18, 0))).toBe(false)
    expect(isAvailableAt(form, monday(8, 30))).toBe(false)
  })

  it("deny 模式在选中的槽内不可用", () => {
    const form = formWith([{ day: 1, start: "22:00", end: "24:00" }], "deny")
    expect(isAvailableAt(form, monday(23, 0))).toBe(false)
    expect(isAvailableAt(form, monday(15, 0))).toBe(true)
  })

  it("下次变更落在槽边界上", () => {
    const form = formWith([{ day: 1, start: "09:00", end: "18:00" }])
    // 周一 17:40 → 下次变更 18:00
    const next = nextChangeAt(form, monday(17, 40))
    expect(next).not.toBeNull()
    expect(next?.getHours()).toBe(18)
    expect(next?.getMinutes()).toBe(0)
  })

  it("一周都不变时返回 null", () => {
    const all = { mode: "allow" as const, grid: applyPreset("all") }
    expect(nextChangeAt(all, monday(10, 0))).toBeNull()
  })
})

describe("校验", () => {
  it("allow 模式一格没画会被拦（后端也会拒）", () => {
    expect(validateForm({ mode: "allow", grid: emptyGrid() })).toEqual([{ key: "no_slots" }])
  })

  it("deny 模式空表合法：等于不禁用任何时段", () => {
    expect(validateForm({ mode: "deny", grid: emptyGrid() })).toEqual([])
  })

  it("null（不限时间）不校验", () => {
    expect(validateForm(null)).toEqual([])
  })
})

describe("契约映射", () => {
  it("formToTerms 总是写 enabled:true，null 原样返回", () => {
    expect(formToTerms(null)).toBeNull()
    expect(formToTerms({ mode: "allow", grid: emptyGrid() })).toEqual({
      enabled: true,
      mode: "allow",
      slots: [],
    })
  })

  it("termsToForm 把 enabled:false 当成不限时间", () => {
    expect(
      termsToForm({
        enabled: false,
        mode: "allow",
        slots: [{ day: 1, start: "09:00", end: "18:00" }],
      }),
    ).toBeNull()
  })

  it("termsToForm 对未知 mode 回落到 allow", () => {
    const form = termsToForm({
      enabled: true,
      // 后端理论上不会给这个值，但手工改库能造出来
      mode: "weird" as "allow",
      slots: [{ day: 1, start: "09:00", end: "18:00" }],
    })
    expect(form?.mode).toBe("allow")
    expect(countOn(form!.grid)).toBe(18)
  })
})

describe("预设", () => {
  it("工作时间是周一至周五 09:00-18:00", () => {
    const grid = applyPreset("workHours")
    expect(countOn(grid)).toBe(5 * 18)
    for (const day of [0, 6]) expect(countOn([grid[day]])).toBe(0)
  })

  it("仅工作日是周一至周五全天", () => {
    const grid = applyPreset("weekdays")
    expect(countOn(grid)).toBe(5 * SLOTS_PER_DAY)
  })
})
