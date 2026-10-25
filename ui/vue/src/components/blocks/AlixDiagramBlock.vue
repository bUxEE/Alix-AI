<script setup lang="ts">
import { useAlix } from "../../i18n";
import type { DiagramBlock } from "../../sdk-types";

// AlixAI never renders a diagram and the kit ships no Mermaid: the block is flagged `requires_host_renderer`. A host that has a
// renderer (run in its strict security mode) fills the `renderer` slot and receives the block; without one the reader gets the
// text alternative and the source as plain text, so nothing is lost and nothing executes.

defineProps<{
  block: DiagramBlock;
}>();

const { t } = useAlix();
</script>

<template>
  <figure class="alix-root alix-card alix-pa-4 alix-ma-0 alix-d-flex alix-flex-column alix-ga-3">
    <figcaption
      v-if="block.title"
      class="alix-heading"
    >
      {{ block.title }}
    </figcaption>
    <slot
      :block="block"
      name="renderer"
    >
      <p class="alix-ma-0">
        {{ block.description }}
      </p>
      <details class="alix-text-sm">
        <summary>
          {{ t("diagram.source") }}
        </summary>
        <pre class="alix-font-mono alix-overflow-auto alix-pa-3 alix-mt-2 alix-card"><code>{{ block.source }}</code></pre>
      </details>
    </slot>
  </figure>
</template>
