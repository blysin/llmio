"use client"

import { useTranslation } from "react-i18next"
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from "recharts"

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  type ChartConfig,
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart"
import type { ProviderModelUsage } from "@/lib/api"

// 两组柱子并排：总 Tokens 与缓存 Tokens。
// 注意缓存 Tokens 是总 Tokens 的子集（cached ⊆ prompt ⊆ total），只能并排对比，堆叠会重复计数。
const SERIES = [
  { key: "tokens", color: "var(--color-tokens)" },
  { key: "cached_tokens", color: "var(--color-cached_tokens)" },
] as const

// 每行一个「供应商 / 模型」，行高固定，整图高度随条数增长（横向柱状图不能用固定高度）
const ROW_HEIGHT = 44
const MIN_HEIGHT = 320
const CHART_PADDING = 56

// Y 轴标签过长会挤爆轴线，截断显示；完整值在 tooltip 里
const MAX_LABEL_CHARS = 30

const truncateLabel = (text: string) =>
  text.length > MAX_LABEL_CHARS ? `${text.slice(0, MAX_LABEL_CHARS)}…` : text

interface ProviderModelBarChartProps {
  data: ProviderModelUsage[]
}

export function ProviderModelBarChart({ data }: ProviderModelBarChartProps) {
  const { t } = useTranslation('home')

  // color 不能省：ChartStyle 只为带 color 的 config 项生成 --color-* 变量
  const chartConfig: ChartConfig = {
    tokens: { label: t('provider_model.series_total'), color: "var(--chart-1)" },
    cached_tokens: { label: t('provider_model.series_cached'), color: "var(--chart-2)" },
  }

  const chartData = data.map((item) => ({
    label: item.others
      ? t('provider_model.others')
      : item.provider_model
        ? `${item.provider} / ${item.provider_model}`
        : item.provider,
    tokens: item.tokens,
    cached_tokens: item.cached_tokens,
  }))

  const height = Math.max(MIN_HEIGHT, chartData.length * ROW_HEIGHT + CHART_PADDING)

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('provider_model.title')}</CardTitle>
      </CardHeader>
      <CardContent>
        {chartData.length === 0 ? (
          <div className="flex h-[320px] items-center justify-center text-sm text-muted-foreground">
            {t('provider_model.empty')}
          </div>
        ) : (
          <ChartContainer config={chartConfig} className="aspect-auto w-full" style={{ height }}>
            <BarChart
              accessibilityLayer
              data={chartData}
              layout="vertical"
              margin={{ left: 8, right: 24, top: 8, bottom: 8 }}
            >
              <CartesianGrid horizontal={false} />
              <XAxis
                type="number"
                tickLine={false}
                axisLine={false}
                tickFormatter={(value) => Number(value).toLocaleString()}
              />
              <YAxis
                type="category"
                dataKey="label"
                tickLine={false}
                axisLine={false}
                width={200}
                interval={0}
                tickFormatter={(value: string) => truncateLabel(value)}
              />
              <ChartTooltip cursor={false} content={<ChartTooltipContent indicator="line" />} />
              <ChartLegend content={<ChartLegendContent />} />
              {SERIES.map(({ key, color }) => (
                <Bar key={key} dataKey={key} fill={color} radius={[0, 4, 4, 0]} />
              ))}
            </BarChart>
          </ChartContainer>
        )}
      </CardContent>
    </Card>
  )
}
