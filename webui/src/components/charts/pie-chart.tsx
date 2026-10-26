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
import type { ModelCount, StatMetric } from "@/lib/api"
import { compactNumber, shortLabel } from "@/lib/format"
import { seriesVar } from "@/lib/palette"

// 扇区外的标签是 recharts 的默认 label：直接把该行数值原样印出来。
// 按 tokens 统计时就是十一位数字，绕着环排一圈既挤又读不出量级，
// 因此换成 compactNumber（K/M/B）。tooltip 不缩，那里才看精确值。
//
// 占比不足 3% 的扇区不印标签：recharts 的环外标签不会自动避让，
// 一大一小两个相邻扇区的标签必然叠字（截图里 350.9K 压着 2,331 就是它）。
// 小扇区的数值由 tooltip 兜底，图例里也有它的名字。
const renderSectorLabel = ({ value, percent }: PieLabelRenderProps) =>
  (percent ?? 0) >= 0.03 ? compactNumber(Number(value)) : ""

// 颜色走项目统一色板（palette.ts 的 seriesVar，对应 index.css 的 --series-1..8）。
// 原先引用的是 shadcn 默认的 --chart-*，本主题从未定义，导致扇区 fill 回落到黑色。
// 配置项按数值降序排列，使图例/扇区颜色（series-1..N）与「最大→最小」顺序一致。
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

// 根据模型数据生成图表数据：按数值降序，最大扇区从 12 点方向起，
// 相邻色相区分度最高（palette 的 CVD 校验按相邻槽位测），且整体观感更稳重。
const generateChartData = (data: ModelCount[]) => {
  return [...data]
    .sort((a, b) => b.value - a.value)
    .map((item, index) => ({
      model: item.model,
      value: item.value,
      fill: seriesVar(index),
    }))
}

interface ChartPieDonutTextProps {
  data: ModelCount[]
  metric: StatMetric
}

export function ChartPieDonutText({ data, metric }: ChartPieDonutTextProps) {
  const { t } = useTranslation('analytics')
  const chartData = generateChartData(data)
  const chartConfig = generateChartConfig(data, t(metric === 'tokens' ? 'usage.metric_tokens' : 'usage.metric_count'))
  
  return (
    <Card className="flex flex-col">
      <CardHeader className="items-center pb-0">
        <CardTitle>{t(metric === 'tokens' ? 'usage.model_pie_tokens' : 'usage.model_pie_count')}</CardTitle>
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
              nameKey="model"
              label={renderSectorLabel}
              labelLine={false}
              innerRadius={70}
              startAngle={90}
              endAngle={-270}
              stroke="var(--card)"
              strokeWidth={2}
            />
            <ChartLegend
              content={
                <ChartLegendContent
                  nameKey="model"
                  payload={undefined}
                  // 模型名是完整路径，图例里截短显示，hover 的 title 给全名；
                  // 完整名仍走 tooltip，config.label 不动
                  formatter={(label) => (
                    <span
                      className="truncate"
                      title={typeof label === "string" ? label : undefined}
                    >
                      {typeof label === "string" ? shortLabel(label) : label}
                    </span>
                  )}
                />
              }
              className="-translate-y-2 flex-wrap gap-2 min-h-12 *:basis-1/4 *:justify-center"
            />
          </PieChart>
        </ChartContainer>
      </CardContent>
    </Card>
  )
}
