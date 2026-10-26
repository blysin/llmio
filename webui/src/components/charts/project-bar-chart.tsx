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
import type { ProjectCount, StatMetric } from "@/lib/api"
import { compactNumber, shortLabel } from "@/lib/format"
import { seriesVar } from "@/lib/palette"

// 数值一律走 compactNumber（K/M/B）。按 tokens 统计时原始值动辄 1e8，
// 轴刻度与柱顶标签直接印十一位数字，柱子再长也读不出量级。
// tooltip 不缩——那是唯一能读到精确值的地方，全页都缩就没处可查了。
//
// 颜色按数值降序取 series-1..N，与同页环形图（project-pie-chart）**同一套规则**：
// 两图喂的是同一份 ProjectCount[]，排序与取色都一致，于是同一个项目在环形图
// 和柱状图里是同一个颜色。上一轮为了消灭「彩虹柱」把这里压成单一品牌色，
// 代价正是与紧挨着的环形图对不上色——单指标排名确实靠柱长比大小，
// 但两图并排时同色反而比彩虹更费解。
//
// config 里为每个项目建一项不是装饰：tooltip 靠它把行名从「Tokens」
// （该条目没有 name，回落到 config.value 的标签）换成项目名。
const generateChartConfig = (data: ProjectCount[], metricLabel: string): ChartConfig => {
  const sorted = [...data].sort((a, b) => b.value - a.value)
  const config: ChartConfig = {
    value: {
      label: metricLabel,
      color: seriesVar(0),
    },
  }

  sorted.forEach((item, index) => {
    config[item.project] = {
      label: item.project,
      color: seriesVar(index),
    }
  })

  return config
}

// 排序口径与环形图严格一致（同为 b.value - a.value；Array.prototype.sort 稳定，
// 同值时两侧保持同一相对顺序），否则同一项目会在两图里落到不同槽位。
const generateChartData = (data: ProjectCount[]) => {
  return [...data]
    .sort((a, b) => b.value - a.value)
    .map((item, index) => ({
      project: item.project,
      value: item.value,
      fill: seriesVar(index),
    }))
}

interface ProjectRankingChartProps {
  data: ProjectCount[]
  metric: StatMetric
}

export function ProjectRankingChart({ data, metric }: ProjectRankingChartProps) {
  const { t } = useTranslation('analytics')
  const chartData = generateChartData(data)
  const chartConfig = generateChartConfig(data, t(metric === 'tokens' ? 'usage.metric_tokens' : 'usage.metric_count'))

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t(metric === 'tokens' ? 'usage.project_bar_tokens' : 'usage.project_bar_count')}</CardTitle>
      </CardHeader>
      <CardContent>
        <ChartContainer config={chartConfig} className="aspect-auto h-[320px] w-full">
          <BarChart
            accessibilityLayer
            data={chartData}
            barSize={32}
            // 与 bar-chart 同一口径：顶部留出柱顶标签的高度，防止最高柱的数值被裁
            margin={{ top: 28 }}
          >
            <CartesianGrid vertical={false} />
            {/* 与 bar-chart 同一套刻度规则：shortLabel 短名 + 放不下自动跳过。
                项目名目前都短，这里是防御性对齐。 */}
            <XAxis
              dataKey="project"
              tickLine={false}
              axisLine={false}
              tickMargin={16}
              tickFormatter={(value) => shortLabel(String(value))}
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
              content={<ChartTooltipContent indicator="line" hideLabel nameKey="project" />}
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
