"use client"

import { useTranslation } from "react-i18next"
import { Bar, BarChart, CartesianGrid, LabelList, XAxis, YAxis } from "recharts"

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

// 排名柱状图是单指标、靠柱长比大小：所有柱用同一品牌色（铜金 --primary），
// 用「长度」而非「色相」传达量级，避免「彩虹柱」噪声；也规避了按排名取色会导致
// 同一模型每次排名变动就换色的问题（见 palette.ts 的铁律：绝不按排名取色）。
const generateChartConfig = (metricLabel: string): ChartConfig => {
  return {
    value: {
      label: metricLabel,
      color: "var(--primary)",
    },
  }
}

// 根据模型数据生成图表数据
const generateChartData = (data: ModelCount[]) => {
  return data.map((item) => ({
    model: item.model,
    value: item.value,
  }))
}

interface ModelRankingChartProps {
  data: ModelCount[]
  metric: StatMetric
}

export function ModelRankingChart({ data, metric }: ModelRankingChartProps) {
  const { t } = useTranslation('home')
  const chartData = generateChartData(data)
  const chartConfig = generateChartConfig(t(metric === 'tokens' ? 'charts.metric_tokens' : 'charts.metric_count'))

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
              tickFormatter={(value) => Number(value).toLocaleString()}
              width={60}
            />
            <ChartTooltip
              cursor={false}
              content={<ChartTooltipContent indicator="line" hideLabel />}
            />
            <Bar
              dataKey="value"
              fill="var(--primary)"
              radius={[8, 8, 0, 0]}
            >
              <LabelList
                dataKey="value"
                position="top"
                offset={12}
                className="fill-foreground font-medium"
                fontSize={12}
              />
            </Bar>
          </BarChart>
        </ChartContainer>
      </CardContent>
    </Card>
  )
}
