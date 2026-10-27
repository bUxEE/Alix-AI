<script setup lang="ts">
import { computed } from "vue";
import { useAlix } from "../../i18n";

// Subtle source chrome (ADR 0047): app · calculated · web (with optional citation) · general knowledge.
// Knowledge is visually marked as general information, not tenant fact.

const props = defineProps<{
  source?: "app" | "computed" | "web" | "knowledge";
  href?: string;
}>();

const { t } = useAlix();

const label = computed(() => {
  switch (props.source) {
    case "app":
      return t("source.app");
    case "computed":
      return t("source.computed");
    case "web":
      return t("source.web");
    case "knowledge":
      return t("source.knowledge");
    default:
      return "";
  }
});
</script>

<template>
  <span
    v-if="source"
    :data-source="source"
    class="alix-root alix-source-label alix-text-xs alix-text-muted"
  >
    <a
      v-if="source === 'web' && href"
      :href="href"
      class="alix-source-link"
      rel="noopener noreferrer"
      target="_blank"
    >
      {{ label }}
    </a>
    <span
      v-else
      :class="{ 'alix-source-knowledge': source === 'knowledge' }"
    >
      {{ label }}
    </span>
  </span>
</template>

<style lang="scss">
.alix-source-label {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  letter-spacing: 0.02em;
}

.alix-source-knowledge {
  border-bottom: 1px dotted var(--alix-border, currentColor);
}

.alix-source-link {
  color: inherit;
  text-decoration: underline;
  text-underline-offset: 2px;
}
</style>
