<script setup lang="ts">
import { ref } from "vue";
import { useAlix } from "../../i18n";
import type { ProvenanceBlock } from "../../sdk-types";

defineProps<{
  block: ProvenanceBlock;
}>();

const { t } = useAlix();
const open = ref(false);
</script>

<template>
  <slot :block="block">
    <details
      class="alix-root alix-card alix-pa-3"
      data-alixai="provenance"
      @toggle="open = ($event.target as HTMLDetailsElement).open"
    >
      <summary class="alix-heading alix-text-sm alix-cursor-pointer">
        {{ block.title || t("provenance.title") }}
      </summary>
      <ol class="alix-ma-3 alix-pa-0 alix-pl-4 alix-d-flex alix-flex-column alix-ga-2">
        <li
          v-for="(step, index) in block.steps"
          :key="index"
          class="alix-text-sm"
        >
          <span class="alix-text-muted">{{ step.kind }}</span>
          — {{ step.summary }}
          <span
            v-if="step.op"
            class="alix-text-muted"
          > ({{ step.op }})</span>
        </li>
      </ol>
    </details>
  </slot>
</template>
