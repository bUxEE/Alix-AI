<script setup lang="ts">
import type { DiagramBlock } from "@alixai/vue";
import { computed, useId } from "vue";
import { layoutFlowchart } from "../flowchart";
import { useDemoText } from "../state";

// The demo host's diagram renderer, plugged into the kit's `diagram` slot. It draws the flowchart subset of Mermaid as SVG from
// the template (text stays text, nothing is injected). A diagram outside that subset shows its text alternative and its source,
// exactly as the kit does without a renderer.

const props = defineProps<{
  block: DiagramBlock;
}>();

const text = useDemoText();
const uid = useId();
const layout = computed(() => layoutFlowchart(props.block.source));
const diamond = (x: number, y: number, w: number, h: number): string => `${x + w / 2},${y} ${x + w},${y + h / 2} ${x + w / 2},${y + h} ${x},${y + h / 2}`;
</script>

<template>
  <div class="alix-d-flex alix-flex-column alix-ga-3">
    <div
      v-if="layout"
      class="alix-overflow-auto"
    >
      <svg
        :viewBox="`0 0 ${layout.width} ${layout.height}`"
        :width="layout.width"
        :aria-label="block.description"
        class="alix-demo-flow"
        role="img"
      >
        <defs>
          <marker
            :id="`${uid}-arrow`"
            viewBox="0 0 10 10"
            refX="9"
            refY="5"
            markerWidth="7"
            markerHeight="7"
            orient="auto-start-reverse"
          >
            <path
              class="alix-demo-flow-head"
              d="M 0 1 L 9 5 L 0 9 z"
            />
          </marker>
        </defs>
        <g class="alix-demo-flow-edges">
          <path
            v-for="edge in layout.edges"
            :key="edge.key"
            :d="edge.path"
            :class="{ 'alix-demo-flow-back': edge.back }"
            :marker-end="`url(#${uid}-arrow)`"
            fill="none"
          />
        </g>
        <g
          v-for="node in layout.nodes"
          :key="node.id"
          class="alix-demo-flow-node"
        >
          <polygon
            v-if="node.shape === 'decision'"
            :points="diamond(node.x, node.y, node.width, node.height)"
          />
          <rect
            v-else
            :x="node.x"
            :y="node.y"
            :width="node.width"
            :height="node.height"
            :rx="node.shape === 'round' ? node.height / 2 : 9"
          />
          <text
            :x="node.x + node.width / 2"
            :y="node.y + node.height / 2"
            text-anchor="middle"
            dominant-baseline="central"
          >
            {{ node.label }}
          </text>
        </g>
        <g
          v-for="edge in layout.edges.filter((candidate) => candidate.label)"
          :key="`${edge.key}-label`"
          class="alix-demo-flow-label"
        >
          <rect
            :x="edge.labelX - ((edge.label?.length ?? 0) * 6.4 + 12) / 2"
            :y="edge.labelY - 10"
            :width="(edge.label?.length ?? 0) * 6.4 + 12"
            height="20"
            rx="10"
          />
          <text
            :x="edge.labelX"
            :y="edge.labelY"
            text-anchor="middle"
            dominant-baseline="central"
          >
            {{ edge.label }}
          </text>
        </g>
      </svg>
    </div>
    <p
      v-else
      class="alix-ma-0"
    >
      {{ block.description }}
    </p>
    <details class="alix-text-sm">
      <summary>
        {{ layout ? text("flow.source") : text("flow.unsupported") }}
      </summary>
      <pre class="alix-font-mono alix-overflow-auto alix-pa-3 alix-mt-2 alix-card"><code>{{ block.source }}</code></pre>
    </details>
  </div>
</template>
