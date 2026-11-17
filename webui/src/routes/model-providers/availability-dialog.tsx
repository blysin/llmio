import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { Ban, Clock, Infinity as InfinityIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  SLOTS_PER_DAY,
  WEEKDAY_DISPLAY_ORDER,
  applyPreset,
  countOn,
  defaultForm,
  invertGrid,
  isAvailableAt,
  isOn,
  nextChangeAt,
  paintRange,
  setSlot,
  toggleRow,
  validateForm,
  weekdayKey,
  type AvailabilityForm,
  type AvailabilityMode,
  type AvailabilityPreset,
} from "@/lib/availability"

/**
 * 可用时间配置弹窗：7×48 的槽位网格。
 *
 * ## 为什么是独立弹窗而不是塞进关联表单
 *
 * `model-provider-form-dialog.tsx` 已经近 600 行、`PeakTermsEditor` 又 500 行。
 * 再往里塞一张 7 行 × 48 列的网格，主表单会变成一千多行且一屏看不完——
 * 用户在配权重、价格、extra_body 的时候被一张大表格打断，两边都做不好。
 *
 * 弹窗化还有一个实际收益：网格的交互（点、拖、行/列操作、反转）能独立测，
 * 不需要拉起整个表单与它的 RHF 上下文。
 *
 * ## 草稿语义
 *
 * 弹窗内的改动只写本地 draft，点「确定」才回写父表单；父表单的「保存」才是
 * 真正落库的那一刻。弹窗自己不带保存按钮——否则会出现"弹窗存了、主表单还
 * 没存，用户点取消但时间窗已经进库"这种撕裂状态，排查起来极其难受。
 *
 * ## 拖动选择
 *
 * 每次都从**按下时的网格快照**重画区间，而不是在上一次结果上叠加。叠加会在
 * "拖远了再拖回来"时留下中间那一段已经改掉的格子，用户看到的是"我明明退回来
 * 了它却没恢复"。从快照重画让整个过程可逆。
 */

type Props = {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 当前值：null = 不限时间。 */
  value: AvailabilityForm | null
  /** 点「确定」。null 表示改成"不限时间"。 */
  onConfirm: (next: AvailabilityForm | null) => void
}

type ModeChoice = AvailabilityMode | "unlimited"

/** 拖动状态：记住起点的网格快照，enter 时从快照重画。 */
type DragState = {
  day: number
  from: number
  value: boolean
  base: boolean[][]
}

export function AvailabilityDialog({ open, onOpenChange, value, onConfirm }: Props) {
  const { t } = useTranslation(["availability", "common"])

  const [draft, setDraft] = useState<AvailabilityForm | null>(value)
  /** 最近一份网格。在"不限时间 ↔ 按时段"之间来回切换时不至于每次重画。 */
  const lastGridRef = useRef<boolean[][]>(value?.grid ?? defaultForm().grid)
  const dragRef = useRef<DragState | null>(null)

  // 每次打开都从外部值重新开始：上一次取消掉的草稿不能带到这一次。
  useEffect(() => {
    if (!open) return
    setDraft(value)
    if (value) lastGridRef.current = value.grid
    dragRef.current = null
  }, [open, value])

  // 拖动时指针移出网格再松开，也必须结束拖动状态，否则之后每次 hover 都在刷。
  useEffect(() => {
    if (!open) return
    const stop = () => {
      dragRef.current = null
    }
    window.addEventListener("pointerup", stop)
    window.addEventListener("pointercancel", stop)
    return () => {
      window.removeEventListener("pointerup", stop)
      window.removeEventListener("pointercancel", stop)
    }
  }, [open])

  const mode: ModeChoice = draft === null ? "unlimited" : draft.mode
  const issues = useMemo(() => validateForm(draft), [draft])
  // 每次渲染重新取当前时刻：用户改一格就重算一次「此刻可用」，
  // 不至于因为时间推进而显示一份过期的结论。
  const now = new Date()

  const setMode = (next: ModeChoice) => {
    if (next === "unlimited") {
      if (draft) lastGridRef.current = draft.grid
      setDraft(null)
      return
    }
    setDraft({ mode: next, grid: draft?.grid ?? lastGridRef.current })
  }

  const updateGrid = useCallback((grid: boolean[][]) => {
    setDraft((prev) => {
      if (!prev) return prev
      lastGridRef.current = grid
      return { ...prev, grid }
    })
  }, [])

  const handlePointerDown = (day: number, slot: number) => {
    if (!draft) return
    const nextValue = !isOn(draft.grid, day, slot)
    dragRef.current = { day, from: slot, value: nextValue, base: draft.grid }
    updateGrid(setSlot(draft.grid, day, slot, nextValue))
  }

  const handlePointerEnter = (day: number, slot: number) => {
    const drag = dragRef.current
    if (!drag || !draft || day !== drag.day) return
    // 只在起始行内画：跨行拖动在 12px 的格子上几乎必然是手滑，
    // 而"从周一拖到周三"这种需求用两三次点击更快也更准。
    updateGrid(paintRange(drag.base, day, Math.min(drag.from, slot), Math.max(drag.from, slot), drag.value))
  }

  const availableNow = draft ? isAvailableAt(draft, now) : true
  const nextChange = draft ? nextChangeAt(draft, now) : null
  const canConfirm = issues.length === 0

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("availability:title")}</DialogTitle>
          <DialogDescription>{t("availability:description")}</DialogDescription>
        </DialogHeader>

        {/* 三种模式：不限时间 / 白名单 / 黑名单。 */}
        <div className="flex flex-wrap gap-2" role="group" aria-label={t("availability:mode.label")}>
          <ModeButton
            active={mode === "unlimited"}
            icon={<InfinityIcon className="size-4" aria-hidden="true" />}
            label={t("availability:mode.unlimited")}
            hint={t("availability:mode.unlimited_hint")}
            onClick={() => setMode("unlimited")}
          />
          <ModeButton
            active={mode === "allow"}
            icon={<Clock className="size-4" aria-hidden="true" />}
            label={t("availability:mode.allow")}
            hint={t("availability:mode.allow_hint")}
            onClick={() => setMode("allow")}
          />
          <ModeButton
            active={mode === "deny"}
            icon={<Ban className="size-4" aria-hidden="true" />}
            label={t("availability:mode.deny")}
            hint={t("availability:mode.deny_hint")}
            onClick={() => setMode("deny")}
          />
        </div>

        {draft && (
          <>
            <div className="flex flex-wrap gap-2">
              <PresetButton preset="workHours" label={t("availability:preset.work_hours")} onApply={updateGrid} />
              <PresetButton preset="weekdays" label={t("availability:preset.weekdays")} onApply={updateGrid} />
              <PresetButton preset="all" label={t("availability:preset.all")} onApply={updateGrid} />
              <PresetButton preset="none" label={t("availability:preset.none")} onApply={updateGrid} />
              <Button type="button" variant="ghost" size="sm" onClick={() => updateGrid(invertGrid(draft.grid))}>
                {t("availability:preset.invert")}
              </Button>
              <span className="ml-auto self-center text-xs text-muted-foreground tabular-nums">
                {t("availability:selected_count", { n: countOn(draft.grid) })}
              </span>
            </div>

            <AvailabilityGrid
              grid={draft.grid}
              onPointerDown={handlePointerDown}
              onPointerEnter={handlePointerEnter}
              onToggleRow={(day) => updateGrid(toggleRow(draft.grid, day))}
            />

            <div
              className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground"
              aria-live="polite"
            >
              <span className="font-medium text-foreground">{t("availability:now.label")}</span>
              <span className={availableNow ? "text-status-good-ink" : "text-status-critical-ink"}>
                {availableNow ? t("availability:now.available") : t("availability:now.unavailable")}
              </span>
              <span>
                {nextChange
                  ? t("availability:now.next_change", { time: formatMoment(nextChange) })
                  : t("availability:now.never_changes")}
              </span>
              <span className="text-[10px] opacity-80">{t("availability:now.timezone_hint")}</span>
            </div>

            {issues.length > 0 && (
              <p className="text-xs text-status-critical-ink" role="alert">
                {t(`availability:issue.${issues[0].key}`)}
              </p>
            )}
          </>
        )}

        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            {t("common:actions.cancel")}
          </Button>
          <Button
            type="button"
            disabled={!canConfirm}
            onClick={() => {
              onConfirm(draft)
              onOpenChange(false)
            }}
          >
            {t("availability:action.confirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ModeButton({
  active,
  icon,
  label,
  hint,
  onClick,
}: {
  active: boolean
  icon: ReactNode
  label: string
  hint: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={[
        "flex flex-1 min-w-[140px] flex-col gap-0.5 rounded-md border p-2.5 text-left transition-colors",
        active
          ? "border-primary bg-primary/10 text-foreground"
          : "border-border bg-background text-muted-foreground hover:bg-muted/60",
      ].join(" ")}
    >
      <span className="flex items-center gap-1.5 text-sm font-medium">
        {icon}
        {label}
      </span>
      <span className="text-[11px] leading-tight opacity-80">{hint}</span>
    </button>
  )
}

function PresetButton({
  preset,
  label,
  onApply,
}: {
  preset: AvailabilityPreset
  label: string
  onApply: (grid: boolean[][]) => void
}) {
  return (
    <Button type="button" variant="outline" size="sm" onClick={() => onApply(applyPreset(preset))}>
      {label}
    </Button>
  )
}

const CELL_WIDTH = 12

function AvailabilityGrid({
  grid,
  onPointerDown,
  onPointerEnter,
  onToggleRow,
}: {
  grid: boolean[][]
  onPointerDown: (day: number, slot: number) => void
  onPointerEnter: (day: number, slot: number) => void
  onToggleRow: (day: number) => void
}) {
  const { t } = useTranslation(["availability"])

  return (
    <div className="overflow-x-auto">
      <div className="min-w-[624px] select-none">
        {WEEKDAY_DISPLAY_ORDER.map((day) => (
          <div key={day} className="flex items-stretch">
            <button
              type="button"
              onClick={() => onToggleRow(day)}
              className="w-12 shrink-0 text-left text-xs text-muted-foreground hover:text-foreground"
              aria-label={t("availability:grid.toggle_row", { day: t(`availability:${weekdayKey(day)}`) })}
            >
              {t(`availability:${weekdayKey(day)}`)}
            </button>
            <div className="flex" style={{ gap: 1 }}>
              {Array.from({ length: SLOTS_PER_DAY }, (_, slot) => {
                const on = grid[day]?.[slot] === true
                return (
                  <div
                    key={slot}
                    role="checkbox"
                    aria-checked={on}
                    aria-label={`${t(`availability:${weekdayKey(day)}`)} ${slotLabel(slot)}`}
                    tabIndex={-1}
                    onPointerDown={(e) => {
                      e.preventDefault()
                      onPointerDown(day, slot)
                    }}
                    onPointerEnter={() => onPointerEnter(day, slot)}
                    className={[
                      "h-5 rounded-[2px] transition-colors",
                      on ? "bg-primary" : "bg-muted hover:bg-muted-foreground/30",
                    ].join(" ")}
                    style={{ width: CELL_WIDTH }}
                  />
                )
              })}
            </div>
          </div>
        ))}

        {/* 小时刻度：0…24，均匀分布与 48 个槽对齐 */}
        <div className="mt-1 flex justify-between pl-12 text-[10px] text-muted-foreground tabular-nums">
          {[0, 6, 12, 18, 24].map((hour) => (
            <span key={hour}>{String(hour).padStart(2, "0")}</span>
          ))}
        </div>
      </div>
    </div>
  )
}

function slotLabel(slot: number): string {
  const h = Math.floor((slot * 30) / 60)
  const m = (slot * 30) % 60
  return `${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}`
}

function formatMoment(at: Date): string {
  const day = ["日", "一", "二", "三", "四", "五", "六"][at.getDay()]
  return `周${day} ${String(at.getHours()).padStart(2, "0")}:${String(at.getMinutes()).padStart(2, "0")}`
}
