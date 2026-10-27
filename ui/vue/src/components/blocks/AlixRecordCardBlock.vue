<script setup lang="ts">
import { useAlix } from "../../i18n";
import type { RecordCardBlock } from "../../sdk-types";
import AlixSourceLabel from "./AlixSourceLabel.vue";

defineProps<{
  block: RecordCardBlock;
}>();

const { formatNumber, t } = useAlix();

const show = (value: string | number | boolean | null): string => {
  if (value === null) return t("common.none");
  if (typeof value === "boolean") return value ? t("common.yes") : t("common.no");
  if (typeof value === "number") return formatNumber(value);
  return String(value);
};
</script>

<template>
  <slot :block="block">
    <div
      class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3"
      data-alixai="record_card"
    >
      <div class="alix-d-flex alix-justify-space-between alix-align-center alix-ga-2">
        <h3 class="alix-heading alix-ma-0 alix-text-md">
          {{ block.title }}
        </h3>
        <AlixSourceLabel
          v-if="block.source"
          :source="block.source"
        />
      </div>
      <dl class="alix-ma-0 alix-d-flex alix-flex-column alix-ga-2">
        <div
          v-for="field in block.fields"
          :key="field.key"
          class="alix-d-flex alix-justify-space-between alix-ga-3 alix-flex-wrap"
        >
          <dt class="alix-text-muted alix-text-sm alix-ma-0">
            {{ field.label }}
          </dt>
          <dd class="alix-ma-0 alix-text-sm alix-text-end">
            {{ show(field.value) }}
          </dd>
        </div>
      </dl>
    </div>
  </slot>
</template>
