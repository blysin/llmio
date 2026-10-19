"use client"

import { useTranslation } from "react-i18next"
import { Bar, BarChart, CartesianGrid, Cell, LabelList, XAxis, YAxis } from "recharts"

import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import {
  type ChartConfig,
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart"
import type { ModelCount, StatMetric } from "@/lib/api"
import { compactNumber } from "@/lib/format"
import { seriesVar } from "@/lib/palette"

// 数值一律走 compactNumber（K/M/B）。按 tokens 统计时原始值动辄 1e8，
// 轴刻度与柱顶标签直接印十一位数字，柱子再长也读不出量级。
// tooltip 不缩——那是唯一能读到精确值的地方，全页都缩就没处可查了。
//
// 颜色按数值降序取 series-1..N，与同页环形图（pie-chart）**同一套规则**：
// 两图喂的是同一份 ModelCount[]，排序与取色都一致，于是同一个模型在环形图
// 和柱状图里是同一个颜色。上一轮为了消灭「彩虹柱」把这里压成单一品牌色，
// 代价正是与紧挨着的环形图对不上色——单指标排名确实靠柱长比大小，
// 但两图并排时同色反而比彩虹更费解。
//
// config 里为每个模型建一项不是装饰：tooltip 靠它把行名从「Tokens」
// （该条目没有 name，回落到 config.value 的标签）换成模型名。
const generateChartConfig = (data: ModelCount[], metricLabel: string): ChartConfig => {
  const sorted = [...data].sort((a, b) => b.value - a.value)
  const config: ChartConfig = {
    value: {
      label: metricLabel,
      color: seriesVar(0),
    },
  }

  sorted.forEach((item, index) => {
    config[item.model] = {
      label: item.model,
      color: seriesVar(index),
    }
  })

  return config
}

// 排序口径与环形图严格一致（同为 b.value - a.value；Array.prototype.sort 稳定，
// 同值时两侧保持同一相对顺序），否则同一模型会在两图里落到不同槽位。
const generateChartData = (data: ModelCount[]) => {
  return [...data]
    .sort((a, b) => b.value - a.value)
    .map((item, index) => ({
      model: item.model,
      value: item.value,
      fill: seriesVar(index),
    }))
}

interface ModelRankingChartProps {
  data: ModelCount[]
  metric: StatMetric
}

export function ModelRankingChart({ data, metric }: ModelRankingChartProps) {
  const { t } = useTranslation('home')
  const chartData = generateChartData(data)
  const chartConfig = generateChartConfig(data, t(metric === 'tokens' ? 'charts.metric_tokens' : 'charts.metric_count'))

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t(metric === 'tokens' ? 'charts.model_bar_tokens' : 'charts.model_bar_count')}</CardTitle>
      </CardHeader>
      <CardContent>
        <ChartContainer config={chartConfig} className="aspect-auto h-[320px] w-full">
          <BarChart
            accessibilityLayer
            data={chartData}
            barSize={32}
          >
            <CartesianGrid vertical={false} />
            <XAxis
              dataKey="model"
              tickLine={false}
              axisLine={false}
              tickMargin={16}
              interval={0}
              tickFormatter={(value) => String(value)}
            />
            <YAxis
              dataKey="value"
              tickLine={false}
              axisLine={false}
              tickFormatter={(value) => compactNumber(Number(value))}
              width={60}
            />
            <ChartTooltip
              cursor={false}
              content={<ChartTooltipContent indicator="line" hideLabel nameKey="model" />}
            />
            <Bar
              dataKey="value"
              fill="var(--primary)"
              radius={[8, 8, 0, 0]}
            >
              {chartData.map((entry, index) => (
                // 槽位由数据项的 fill 决定（与环形图同一套），Cell 只是把它交给 SVG
                <Cell key={`cell-${index}`} fill={entry.fill} />
              ))}
              <LabelList
                dataKey="value"
                position="top"
                offset={12}
                className="fill-foreground font-medium"
                fontSize={12}
                formatter={(value: unknown) => compactNumber(Number(value))}
              />
            </Bar>
          </BarChart>
        </ChartContainer>
      </CardContent>
    </Card>
  )
}
