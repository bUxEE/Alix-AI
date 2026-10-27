<script setup lang="ts">
import { computed } from "vue";
import { useAlix } from "../../i18n";
import type { KpiBlock } from "../../sdk-types";
import AlixSourceLabel from "./AlixSourceLabel.vue";
import { formatKpiDelta } from "./kpi-delta";

const props = defineProps<{
  block: KpiBlock;
}>();

const { formatNumber } = useAlix();

const valueText = computed(() => {
  const v = props.block.value;
  return typeof v === "number" ? formatNumber(v) : String(v);
});
const delta = computed(() => formatKpiDelta(props.block.delta, formatNumber));
</script>

<template>
  <slot :block="block">
    <div
      class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-1"
      data-alixai="kpi"
    >
      <div class="alix-d-flex alix-justify-space-between alix-align-center alix-ga-2">
        <p class="alix-text-muted alix-ma-0 alix-text-sm">
          {{ block.label }}
        </p>
        <AlixSourceLabel
          v-if="block.source"
          :source="block.source"
        />
      </div>
      <p class="alix-heading alix-ma-0 alix-text-xl">
        {{ valueText }}
        <span
          v-if="block.unit"
          class="alix-text-muted alix-text-sm"
        >{{ block.unit }}</span>
      </p>
      <p
        v-if="delta || block.period"
        class="alix-ma-0 alix-text-sm alix-text-muted"
      >
        <span
          v-if="delta"
          :aria-label="delta.aria"
        >
          <span
            aria-hidden="true"
          >{{ delta.arrow }}</span>
          {{ delta.text }}
        </span>
        <span v-if="delta && block.period"> · </span>
        <span v-if="block.period">{{ block.period }}</span>
      </p>
    </div>
  </slot>
</template>
