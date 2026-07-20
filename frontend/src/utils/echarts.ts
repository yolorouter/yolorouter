// frontend/src/utils/echarts.ts
//
// Centralized ECharts registration. Importing this side-effect module once
// (from any component that renders a chart) registers the renderer, chart
// types and components we use project-wide. Keeping the `use([...])` list in
// one place avoids each chart component having to repeat it, and avoids
// accidentally tree-shaking away a component a sibling chart relies on.
//
// To add a new chart (e.g. PieChart) or component (e.g. TitleComponent):
//   1. add it to the import list below
//   2. add it to the `use([...])` array
// Nothing else in the codebase needs to change — every `<VChart>` already
// imports `./echarts` for registration.

import { use } from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
import { BarChart, LineChart } from 'echarts/charts'
import {
  DataZoomComponent,
  GridComponent,
  LegendComponent,
  TooltipComponent,
} from 'echarts/components'

use([
  CanvasRenderer,
  BarChart,
  LineChart,
  GridComponent,
  TooltipComponent,
  LegendComponent,
  DataZoomComponent,
])

// Re-export VChart so callers can `import { VChart } from '@/utils/echarts'`
// and get both the component and the registration in one import.
export { default as VChart } from 'vue-echarts'
