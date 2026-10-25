<script setup lang="ts">
import { computed, useId } from "vue";
import { useLoadFromProps } from "../composables/useLoadState";
import { useAlix, type MessageKey } from "../i18n";
import type { SourceStatus } from "../sdk-types";
import AlixIcon from "./common/AlixIcon.vue";
import AlixLoadable from "./common/AlixLoadable.vue";
import AlixSkeleton from "./common/AlixSkeleton.vue";

// The health of the sources behind the answers: each source's lifecycle state, whether it is available and fresh, how much of it is
// indexed, when it last succeeded, and the typed error when it did not. The statuses are the core's (`SourceStatus`), the same
// input as the React kit's panel; the panel decides nothing and changes nothing. `refresh` asks the host to fetch them again, and
// the previous list stays on screen under an overlay while it does.

const props = withDefaults(
  defineProps<{
    sources?: readonly SourceStatus[];
    title?: string;
    loading?: boolean;
    failed?: boolean;
  }>(),
  { sources: undefined, title: undefined, loading: false, failed: false },
);

const emit = defineEmits<{
  refresh: [];
}>();

const { t, formatDateTime, formatNumber, plural } = useAlix();
const uid = useId();
const state = useLoadFromProps({ loading: () => props.loading, failed: () => props.failed, hasData: () => props.sources !== undefined });

const ready = computed(() => (props.sources ?? []).filter((source) => source.state === "ready" && source.error === undefined).length);

const tone = (source: SourceStatus): string => {
  if (source.error !== undefined || source.counts.failed > 0 || source.availability !== "available") return "alix-tone-danger";
  if (source.staleness === "stale" || source.state === "paused" || source.state === "disabled") return "alix-tone-warning";
  return source.state === "ready" ? "alix-tone-success" : "";
};
</script>

<template>
  <section
    :aria-labelledby="`${uid}-title`"
    class="alix-root alix-d-flex alix-flex-column alix-ga-3"
  >
    <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
      <div>
        <h2
          :id="`${uid}-title`"
          class="alix-heading"
        >
          {{ title ?? t("sources.title") }}
        </h2>
        <p
          v-if="sources && sources.length > 0"
          class="alix-text-muted alix-text-sm alix-ma-0"
        >
          {{ t("sources.summary", { ready, total: plural("sources.count", sources.length) }) }}
        </p>
      </div>
      <button
        :disabled="loading"
        :aria-label="t('common.refresh')"
        class="alix-btn alix-btn-quiet alix-btn-icon"
        type="button"
        @click="emit('refresh')"
      >
        <AlixIcon name="refresh" />
      </button>
    </header>
    <AlixLoadable
      :loading="state.isLoading.value"
      :loaded="state.hasLoadedOnce.value"
    >
      <template #skeleton>
        <ul class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2">
          <li
            v-for="n in 3"
            :key="n"
            class="alix-card alix-pa-3 alix-d-flex alix-flex-column alix-ga-2"
          >
            <AlixSkeleton width="45%" />
            <AlixSkeleton
              height="12px"
              width="70%"
            />
          </li>
        </ul>
      </template>
      <div
        v-if="failed && !state.hasLoadedOnce.value"
        class="alix-banner alix-tone-danger alix-d-flex alix-align-center alix-justify-between alix-ga-2"
        role="alert"
      >
        {{ t("sources.failed") }}
        <button
          class="alix-btn"
          type="button"
          @click="emit('refresh')"
        >
          {{ t("common.retry") }}
        </button>
      </div>
      <ul
        v-else-if="sources && sources.length > 0"
        class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2"
      >
        <li
          v-for="source in sources"
          :key="source.source_id"
          class="alix-card alix-pa-3 alix-d-flex alix-flex-column alix-ga-2"
        >
          <slot
            :source="source"
            :tone="tone(source)"
            name="row"
          >
            <div class="alix-d-flex alix-flex-wrap alix-align-center alix-justify-between alix-ga-2">
              <span class="alix-font-medium alix-break-word">
                {{ source.display_name }}
              </span>
              <span class="alix-d-flex alix-flex-wrap alix-ga-1">
                <span
                  :class="tone(source)"
                  class="alix-badge"
                >
                  {{ t(`sources.state.${source.state}` as MessageKey) }}
                </span>
                <span
                  v-if="source.availability !== 'available'"
                  class="alix-badge alix-tone-danger"
                >
                  {{ t(`sources.availability.${source.availability}` as MessageKey) }}
                </span>
                <span
                  v-if="source.staleness === 'stale'"
                  class="alix-badge alix-tone-warning"
                >
                  {{ t("citations.stale") }}
                </span>
              </span>
            </div>
            <p class="alix-text-muted alix-text-sm alix-ma-0">
              {{
                t("sources.counts", {
                  indexed: formatNumber(source.counts.indexed),
                  failed: formatNumber(source.counts.failed),
                  skipped: formatNumber(source.counts.skipped),
                })
              }}
              &middot;
              {{ source.last_success_at ? t("sources.lastSuccess", { time: formatDateTime(source.last_success_at) }) : t("sources.never") }}
            </p>
            <p
              v-if="source.error"
              class="alix-banner alix-tone-danger alix-ma-0"
            >
              {{ source.error.message }}
            </p>
          </slot>
        </li>
      </ul>
      <slot
        v-else
        name="empty"
      >
        <p class="alix-text-muted alix-text-center alix-pa-4 alix-ma-0">
          {{ t("sources.empty") }}
        </p>
      </slot>
    </AlixLoadable>
  </section>
</template>
