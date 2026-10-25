<script setup lang="ts">
import { citationLink } from "@alixai/sdk";
import { computed } from "vue";
import { useAlix } from "../i18n";
import type { Citation, CitationLink } from "../sdk-types";

// The sources behind an answer. They are never hidden: a grounded answer shows where it came from, with the claims each source
// supports and a plain statement when a source is stale, unavailable or removed. A citation's `open_ref` is opaque, never an address:
// the kit reports `open` and the host resolves it (`rag.resolveCitation` for a file, its own router for a record), so routing and
// authorisation stay with the host.
//
// `display` is the user's setting (`sources.citation_display`): `panel` cards, `footnote` list or `inline` chips.

const props = withDefaults(
  defineProps<{
    citations: readonly Citation[];
    display?: "inline" | "footnote" | "panel";
  }>(),
  { display: "panel" },
);

defineEmits<{
  "open-citation": [citation: Citation];
}>();

const { t, plural } = useAlix();

const entries = computed(() => props.citations.map((citation) => ({ citation, link: citationLink(citation) })));

const placeOf = (link: CitationLink): string | undefined => {
  const { locator } = link;
  if (locator === undefined) return undefined;
  if (locator.page !== undefined) return t("citations.page", { n: locator.page });
  if (locator.section !== undefined) return t("citations.section", { name: locator.section });
  if (locator.record !== undefined) return t("citations.record", { id: locator.record });
  if (locator.span !== undefined) return t("citations.span", { from: locator.span.start, to: locator.span.end });
  return undefined;
};

const openable = (link: CitationLink): boolean => link.available && link.open !== undefined;
</script>

<template>
  <section
    v-if="citations.length > 0"
    :aria-label="t('citations.title')"
    class="alix-root alix-d-flex alix-flex-column alix-ga-2"
  >
    <slot name="title">
      <p class="alix-eyebrow">
        {{ t("citations.title") }}
        <span class="alix-font-mono">
          &middot; {{ plural("citations.count", citations.length) }}
        </span>
      </p>
    </slot>
    <ol
      :class="display === 'inline' ? 'alix-d-flex alix-flex-wrap alix-ga-2' : 'alix-d-flex alix-flex-column alix-ga-2'"
      class="alix-reset-list"
    >
      <li
        v-for="(entry, index) in entries"
        :key="`${entry.citation.source_id}-${index}`"
        :class="display === 'panel' ? 'alix-card alix-pa-3' : display === 'footnote' ? 'alix-py-1' : ''"
      >
        <slot
          :citation="entry.citation"
          :index="index"
          :link="entry.link"
          name="item"
        >
          <div class="alix-d-flex alix-flex-wrap alix-align-center alix-ga-2">
            <span
              class="alix-badge alix-font-mono"
              aria-hidden="true"
            >
              {{ index + 1 }}
            </span>
            <button
              v-if="openable(entry.link)"
              :class="display === 'inline' ? 'alix-chip' : 'alix-btn alix-btn-quiet'"
              :aria-label="t('citations.open', { name: entry.link.label })"
              type="button"
              @click="$emit('open-citation', entry.citation)"
            >
              {{ entry.link.label }}
            </button>
            <span
              v-else
              class="alix-font-medium alix-break-word"
            >
              {{ entry.link.label }}
            </span>
            <span
              v-if="placeOf(entry.link)"
              class="alix-text-muted alix-text-sm"
            >
              {{ placeOf(entry.link) }}
            </span>
            <span
              v-if="entry.link.stale"
              class="alix-badge alix-tone-warning"
            >
              {{ t("citations.stale") }}
            </span>
            <span
              v-if="!entry.link.available"
              class="alix-badge alix-tone-danger"
            >
              {{ entry.citation.availability === "removed" ? t("citations.removed") : t("citations.unavailable") }}
            </span>
          </div>
          <ul
            v-if="display !== 'inline' && entry.link.claims.length > 0"
            :aria-label="t('citations.claims')"
            class="alix-reset-list alix-text-sm alix-text-muted alix-mt-2 alix-d-flex alix-flex-column alix-ga-1"
          >
            <li
              v-for="(claim, at) in entry.link.claims"
              :key="at"
              class="alix-break-word"
            >
              {{ claim }}
            </li>
          </ul>
        </slot>
      </li>
    </ol>
  </section>
</template>
