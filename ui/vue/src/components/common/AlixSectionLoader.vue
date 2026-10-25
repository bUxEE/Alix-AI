<script setup lang="ts">
import { useAlix } from "../../i18n";

// A scrim and a spinner OVER content that is already on screen: the previous answer stays readable while a refetch runs.
// It is absolutely positioned, so its container must be positioned (`alix-position-relative`); AlixLoadable provides that.

const { t } = useAlix();
</script>

<template>
  <div
    class="alix-section-loader"
    role="status"
    aria-live="polite"
  >
    <span
      class="alix-spinner"
      aria-hidden="true"
    />
    <span class="alix-sr-only">
      {{ t("common.loading") }}
    </span>
  </div>
</template>
