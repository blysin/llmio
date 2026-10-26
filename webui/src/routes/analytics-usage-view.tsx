import { useEffect, useMemo, useState } from "react"
import { useTranslation } from "react-i18next"

import { ModelRankingChart } from "@/components/charts/bar-chart"
import { ChartPieDonutText } from "@/components/charts/pie-chart"
import { ProjectRankingChart } from "@/components/charts/project-bar-chart"
import { ProjectChartPieDonutText } from "@/components/charts/project-pie-chart"
import { ProviderModelBarChart } from "@/components/charts/provider-model-bar-chart"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  getProviderModelUsage,
  type GroupStat,
  type ModelCount,
  type ProjectCount,
  type ProviderModelUsage,
  type StatMetric,
  type StatsQuery,
  type StatsResult,
} from "@/lib/api"

/**
 * 用量视图：模型 / 项目 / 供应商三个维度的占比与排行。
 *
 * ## 数据来源为什么分成两路
 *
 * 四张「模型 / 项目」图直接读 `/metrics/stats` 已有的分组结果（`byModel` /
 * `byKey`），因此**与页面上其余视图共用同一个切片**：筛选一改，它们跟着改，
 * 不需要额外请求，也不会出现"趋势图说 100 次、排行图说 98 次"这种由两次
 * 查询之间的时间漂移造成的假矛盾。
 *
 * 供应商 × 上游模型那张图必须另取一次 `/metrics/provider-models`：stats 的
 * 联合维度按「请求名 × 提供商」分组，而这张图要看的是**真正被路由到的上游
 * 模型**（备用接管时会体现差异）。该端点接受与 stats 完全相同的查询串，
 * 所以两路数据的筛选口径仍然一致。
 *
 * ## 为什么取前 N 名 + 「其他」
 *
 * 一个窗口里可能有几十个模型，环形图切成几十片就只剩颜色噪声了。因此统一取
 * 前 5 名、其余合并成一项「其他」——环与柱状图用的是同一份数据，顺序不会打架。
 */
const TOP_N = 5

/** 分组行 → 图表用的 {name, value}，按口径取值、丢弃零值、降序、截前 N 名并合并其余。 */
function topRows(rows: GroupStat[], metric: StatMetric, othersLabel: string): { name: string; value: number }[] {
  const sorted = rows
    .map((g) => ({ name: g.name, value: metric === "tokens" ? g.totalTokens : g.total }))
    // 零值行没有扇区也没有柱子，留在图里只会多一个图例项
    .filter((r) => r.value > 0)
    .sort((a, b) => b.value - a.value)
  if (sorted.length <= TOP_N) return sorted
  const rest = sorted.slice(TOP_N).reduce((sum, r) => sum + r.value, 0)
  return [...sorted.slice(0, TOP_N), { name: othersLabel, value: rest }]
}

export function UsageView({ stats, query }: { stats: StatsResult; query: StatsQuery | null }) {
  const { t } = useTranslation("analytics")
  // 口径默认按 Tokens：这一页回答的是"用量"，而调用次数已经是趋势视图的事
  const [metric, setMetric] = useState<StatMetric>("tokens")
  const [providers, setProviders] = useState<ProviderModelUsage[]>([])

  const others = t("usage.others")

  const modelRows: ModelCount[] = useMemo(
    () => topRows(stats.byModel, metric, others).map((r) => ({ model: r.name, value: r.value })),
    [stats.byModel, metric, others]
  )

  const projectRows: ProjectCount[] = useMemo(
    () => topRows(stats.byKey, metric, others).map((r) => ({ project: r.name, value: r.value })),
    [stats.byKey, metric, others]
  )

  useEffect(() => {
    // query 为 null 表示第一次取数还没落地（或自定义范围非法），此时不发请求
    if (!query) return
    let active = true
    getProviderModelUsage(query)
      .then((data) => {
        if (active) setProviders(data ?? [])
      })
      .catch(() => {
        // 静默：这一张图取不到就继续显示上一轮结果，不把整页打成失败态。
        // stats 那条链路失败时页面已经有明确的错误态了。
      })
    return () => {
      active = false
    }
  }, [query])

  return (
    <>
      {/* 口径开关只作用于本视图：它换的是"数什么"，不是"看哪一段" */}
      <div className="flex flex-wrap items-center justify-end gap-2">
        <span className="text-xs text-muted-foreground">{t("usage.metric_label")}</span>
        <ToggleGroup
          type="single"
          variant="outline"
          value={metric}
          onValueChange={(v) => {
            if (v) setMetric(v as StatMetric)
          }}
          aria-label={t("usage.metric_label")}
        >
          <ToggleGroupItem value="count" size="sm" className="px-2">
            {t("usage.by_count")}
          </ToggleGroupItem>
          <ToggleGroupItem value="tokens" size="sm" className="px-2">
            {t("usage.by_tokens")}
          </ToggleGroupItem>
        </ToggleGroup>
      </div>

      <div className="grid grid-cols-1 gap-3 xl:grid-cols-2">
        <ChartPieDonutText data={modelRows} metric={metric} />
        <ProjectChartPieDonutText data={projectRows} metric={metric} />
      </div>

      <div className="grid grid-cols-1 gap-3 xl:grid-cols-2">
        <ModelRankingChart data={modelRows} metric={metric} />
        <ProjectRankingChart data={projectRows} metric={metric} />
      </div>

      <ProviderModelBarChart data={providers} />
    </>
  )
}
