import { useTranslation } from "react-i18next"
import { Pie, PieChart, type PieLabelRenderProps } from "recharts"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import {
  type ChartConfig,
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart"
import type { ProjectCount, StatMetric } from "@/lib/api"
import { compactNumber } from "@/lib/format"
import { seriesVar } from "@/lib/palette"

// 扇区外的标签是 recharts 的默认 label：直接把该行数值原样印出来。
// 按 tokens 统计时就是十一位数字，绕着环排一圈既挤又读不出量级，
// 因此换成 compactNumber（K/M/B）。tooltip 不缩，那里才看精确值。
const renderSectorLabel = ({ value }: PieLabelRenderProps) =>
  compactNumber(Number(value))

// 颜色走项目统一色板（palette.ts 的 seriesVar，对应 index.css 的 --series-1..8）。
// 原先引用的是 shadcn 默认的 --chart-*，本主题从未定义，导致扇区 fill 回落到黑色。
// 配置项按数值降序排列，使图例/扇区颜色（series-1..N）与「最大→最小」顺序一致。
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

// 按数值降序，最大扇区从 12 点方向起，相邻色相区分度最高（palette 的 CVD 校验按相邻槽位测）。
const generateChartData = (data: ProjectCount[]) => {
  return [...data]
    .sort((a, b) => b.value - a.value)
    .map((item, index) => ({
      project: item.project,
      value: item.value,
      fill: seriesVar(index),
    }))
}

interface ProjectChartPieDonutTextProps {
  data: ProjectCount[]
  metric: StatMetric
}

export function ProjectChartPieDonutText({ data, metric }: ProjectChartPieDonutTextProps) {
  const { t } = useTranslation('home')
  const chartData = generateChartData(data)
  const chartConfig = generateChartConfig(data, t(metric === 'tokens' ? 'charts.metric_tokens' : 'charts.metric_count'))

  return (
    <Card className="flex flex-col">
      <CardHeader className="items-center pb-0">
        <CardTitle>{t(metric === 'tokens' ? 'charts.project_pie_tokens' : 'charts.project_pie_count')}</CardTitle>
      </CardHeader>
      <CardContent className="flex-1 pb-0">
        <ChartContainer
          config={chartConfig}
          className="mx-auto aspect-square max-h-[500px] sm:max-h-[390px] pb-0"
        >
          <PieChart>
            <ChartTooltip
              cursor={false}
              content={<ChartTooltipContent hideLabel />}
            />
            <Pie
              data={chartData}
              dataKey="value"
              nameKey="project"
              label={renderSectorLabel}
              labelLine={false}
              innerRadius={70}
              startAngle={90}
              endAngle={-270}
              stroke="var(--card)"
              strokeWidth={2}
            />
            <ChartLegend
              content={<ChartLegendContent nameKey="project" payload={undefined} />}
              className="-translate-y-2 flex-wrap gap-2 min-h-12 *:basis-1/4 *:justify-center"
            />
          </PieChart>
        </ChartContainer>
      </CardContent>
    </Card>
  )
}
