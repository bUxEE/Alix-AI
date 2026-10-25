<script setup lang="ts">
import { computed } from "vue";
import { formatCanonicalValue } from "../agui/parse";
import { useAlix, type MessageKey } from "../i18n";
import type { ActionReceipt } from "../sdk-types";

// The durable record of what happened to a prepared action: the application's own deterministic facts. The model's wording never
// decides the outcome shown here; the status, the facts and the errors come from the receipt.

const props = defineProps<{
  receipt: ActionReceipt;
}>();

const { t, formatDateTime } = useAlix();

const tone = computed(() => {
  switch (props.receipt.status) {
    case "committed":
      return "alix-tone-success";
    case "queued":
      return "alix-tone-accent";
    case "rejected":
      return "alix-tone-danger";
    default:
      return "alix-tone-warning";
  }
});
</script>

<template>
  <section class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
    <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
      <h3 class="alix-heading">
        {{ t("receipt.title") }}
      </h3>
      <span
        :class="tone"
        class="alix-badge"
      >
        {{ t(`receipt.status.${receipt.status}` as MessageKey) }}
      </span>
    </header>
    <dl
      v-if="receipt.facts.length > 0"
      class="alix-ma-0 alix-d-flex alix-flex-column alix-ga-2"
    >
      <div
        v-for="(fact, index) in receipt.facts"
        :key="`${fact.field}-${index}`"
        class="alix-d-flex alix-flex-wrap alix-justify-between alix-ga-2"
      >
        <dt class="alix-text-muted alix-text-sm">
          {{ fact.field }}
        </dt>
        <dd class="alix-ma-0 alix-font-medium alix-break-word">
          {{ fact.display ?? formatCanonicalValue(fact.value) }}
        </dd>
      </div>
    </dl>
    <ul
      v-if="receipt.errors.length > 0"
      :aria-label="t('receipt.errors')"
      class="alix-banner alix-tone-danger alix-reset-list alix-d-flex alix-flex-column alix-ga-1"
    >
      <li
        v-for="(issue, index) in receipt.errors"
        :key="index"
      >
        {{ issue.message }}
      </li>
    </ul>
    <p
      v-if="receipt.follow_up"
      class="alix-text-muted alix-text-sm alix-ma-0"
    >
      {{ t("receipt.followUp", { state: t(`receipt.followUp.${receipt.follow_up.state}` as MessageKey) }) }}
    </p>
    <p class="alix-text-muted alix-text-xs alix-ma-0">
      {{ t("receipt.issued", { time: formatDateTime(receipt.issued_at) }) }}
    </p>
  </section>
</template>
