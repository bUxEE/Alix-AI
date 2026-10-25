<script setup lang="ts">
import { computed } from "vue";
import { buildChart } from "../../chart";
import { useAlix } from "../../i18n";
import type { ChartBlock } from "../../sdk-types";

// A declarative chart drawn as plain SVG: no chart library, no expression, no URL. The text alternative is always present (the
// block requires one) and the data is always reachable as a real table, so the picture is never the only way to read it.
// A host that prefers its own renderer (ECharts, say) fills the `chart` slot and receives the block.

const props = defineProps<{
  block: ChartBlock;
}>();

const { t, formatNumber } = useAlix();

const model = computed(() => buildChart(props.block));
const fields = computed(() => {
  const { encoding } = props.block;
  return [encoding.x, encoding.y, encoding.color, encoding.theta].filter((channel) => channel !== undefined);
});
const records = computed(() => props.block.data.slice(0, 200));

const label = computed(() => (props.block.title ? `${props.block.title}. ${props.block.description}` : props.block.description));
const colorOf = (index: number): string => `var(--alix-chart-${index + 1})`;
const percent = (share: number): string => `${formatNumber(Math.round(share * 1000) / 10)}%`;
</script>

<template>
  <figure class="alix-root alix-card alix-pa-4 alix-ma-0 alix-d-flex alix-flex-column alix-ga-3">
    <figcaption
      v-if="block.title"
      class="alix-heading"
    >
      {{ block.title }}
    </figcaption>
    <slot :block="block">
      <svg
        v-if="model.valid"
        :viewBox="`0 0 ${model.width} ${model.height}`"
        :aria-label="label"
        class="alix-w-100"
        role="img"
        preserveAspectRatio="xMidYMid meet"
      >
        <template v-if="model.mark !== 'pie'">
          <g
            fill="none"
            stroke="var(--alix-border)"
          >
            <line
              v-for="tick in model.yTicks"
              :key="`y${tick.pos}`"
              :x1="model.plot.left"
              :x2="model.plot.right"
              :y1="tick.pos"
              :y2="tick.pos"
            />
          </g>
          <g
            fill="var(--alix-text-muted)"
            font-size="11"
            text-anchor="end"
          >
            <text
              v-for="tick in model.yTicks"
              :key="`yl${tick.pos}`"
              :x="model.plot.left - 8"
              :y="tick.pos + 4"
            >
              {{ tick.label }}
            </text>
          </g>
          <g
            fill="var(--alix-text-muted)"
            font-size="11"
            text-anchor="middle"
          >
            <text
              v-for="tick in model.xTicks"
              :key="`x${tick.pos}`"
              :x="tick.pos"
              :y="model.plot.bottom + 18"
            >
              {{ tick.label.length > 12 ? `${tick.label.slice(0, 11)}…` : tick.label }}
            </text>
          </g>
          <g
            v-for="shape in model.series"
            :key="shape.name"
            :fill="colorOf(shape.color)"
            :stroke="colorOf(shape.color)"
          >
            <path
              v-if="shape.area"
              :d="shape.area"
              fill-opacity="0.18"
              stroke="none"
            />
            <path
              v-if="shape.line"
              :d="shape.line"
              fill="none"
              stroke-width="2"
              stroke-linejoin="round"
            />
            <rect
              v-for="(bar, index) in shape.bars"
              :key="`b${index}`"
              :x="bar.x"
              :y="bar.y"
              :width="bar.width"
              :height="bar.height"
              rx="2"
              stroke="none"
            />
            <circle
              v-for="(point, index) in model.mark === 'bar' ? [] : shape.points"
              :key="`p${index}`"
              :cx="point.x"
              :cy="point.y"
              :r="model.mark === 'point' ? 4 : 2.5"
              stroke="none"
            />
          </g>
        </template>
        <template v-else>
          <path
            v-for="slice in model.slices"
            :key="slice.label"
            :d="slice.path"
            :fill="colorOf(slice.color)"
            stroke="var(--alix-surface)"
            stroke-width="2"
          />
        </template>
      </svg>
      <p
        v-else
        class="alix-banner alix-ma-0"
      >
        {{ block.description }}
      </p>
    </slot>
    <ul
      v-if="model.valid && (model.slices.length > 0 || model.series.length > 1)"
      :aria-label="t('chart.legend')"
      class="alix-reset-list alix-d-flex alix-flex-wrap alix-ga-3 alix-text-sm"
    >
      <li
        v-for="entry in model.slices.length > 0 ? model.slices : model.series"
        :key="'label' in entry ? entry.label : entry.name"
        class="alix-d-flex alix-align-center alix-ga-2"
      >
        <span
          :style="{ background: colorOf(entry.color) }"
          class="alix-swatch"
          aria-hidden="true"
        />
        <span>
          {{ 'label' in entry ? entry.label : entry.name }}
        </span>
        <span
          v-if="'share' in entry"
          class="alix-text-muted alix-font-mono"
        >
          {{ percent(entry.share) }}
        </span>
      </li>
    </ul>
    <p
      v-if="model.truncated"
      class="alix-text-muted alix-text-sm alix-ma-0"
    >
      {{ t("chart.truncated") }}
    </p>
    <details class="alix-text-sm">
      <summary>
        {{ t("chart.data") }}
      </summary>
      <div class="alix-table-wrap alix-mt-2">
        <table>
          <thead>
            <tr>
              <th
                v-for="channel in fields"
                :key="channel.field"
                scope="col"
              >
                {{ channel.title ?? channel.field }}
              </th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(record, index) in records"
              :key="index"
            >
              <td
                v-for="channel in fields"
                :key="channel.field"
              >
                {{ record[channel.field] ?? "" }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </details>
  </figure>
</template>
