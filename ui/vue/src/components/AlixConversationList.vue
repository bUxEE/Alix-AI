<script setup lang="ts">
import { ref } from "vue";
import type { ConversationSummary } from "../agui/types";
import { useLoadFromProps } from "../composables/useLoadState";
import { useAlix } from "../i18n";
import AlixIcon from "./common/AlixIcon.vue";
import AlixLoadable from "./common/AlixLoadable.vue";
import AlixSkeleton from "./common/AlixSkeleton.vue";

// The conversations a user can switch between. The list is the host's data (the public API has no "list my conversations"
// operation: a conversation is a session the host minted), and switching is the host's routing: the list only reports `select`.
// Arrow keys move between rows, Home and End jump to the ends, Enter or Space selects. First load shows rows of skeletons inside
// the real row chrome; a refresh keeps the rows and lays the overlay over them.

const props = withDefaults(
  defineProps<{
    items?: readonly ConversationSummary[];
    activeId?: string;
    title?: string;
    loading?: boolean;
    failed?: boolean;
    /** Shows a delete button on each row; the host decides what deleting means. */
    removable?: boolean;
  }>(),
  { items: undefined, activeId: undefined, title: undefined, loading: false, failed: false, removable: false },
);

const emit = defineEmits<{
  select: [id: string];
  new: [];
  remove: [id: string];
  retry: [];
}>();

const { t, formatDateTime } = useAlix();
const state = useLoadFromProps({
  loading: () => props.loading,
  failed: () => props.failed,
  hasData: () => props.items !== undefined,
});
const list = ref<HTMLElement | null>(null);

const move = (event: KeyboardEvent): void => {
  const keys = ["ArrowDown", "ArrowUp", "Home", "End"];
  if (!keys.includes(event.key) || list.value === null) return;
  const rows = Array.from(list.value.querySelectorAll<HTMLElement>("[data-row]"));
  if (rows.length === 0) return;
  const at = rows.findIndex((row) => row === document.activeElement);
  const next = event.key === "Home" ? 0 : event.key === "End" ? rows.length - 1 : event.key === "ArrowDown" ? Math.min(rows.length - 1, at + 1) : Math.max(0, at - 1);
  event.preventDefault();
  rows[next]?.focus();
};
</script>

<template>
  <nav
    :aria-label="t('conv.title')"
    class="alix-root alix-d-flex alix-flex-column alix-ga-3"
  >
    <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
      <slot name="header">
        <h2 class="alix-heading">
          {{ title ?? t("conv.title") }}
        </h2>
      </slot>
      <button
        class="alix-btn"
        type="button"
        @click="emit('new')"
      >
        <AlixIcon
          :size="16"
          name="plus"
        />
        {{ t("conv.new") }}
      </button>
    </header>
    <AlixLoadable
      :loading="state.isLoading.value"
      :loaded="state.hasLoadedOnce.value"
    >
      <template #skeleton>
        <ul class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2">
          <slot name="skeleton">
            <li
              v-for="n in 4"
              :key="n"
              class="alix-card alix-pa-3 alix-d-flex alix-flex-column alix-ga-2"
            >
              <AlixSkeleton width="55%" />
              <AlixSkeleton
                height="12px"
                width="85%"
              />
            </li>
          </slot>
        </ul>
      </template>
      <div
        v-if="failed && !state.hasLoadedOnce.value"
        class="alix-banner alix-tone-danger alix-d-flex alix-align-center alix-justify-between alix-ga-2"
        role="alert"
      >
        {{ t("conv.failed") }}
        <button
          class="alix-btn"
          type="button"
          @click="emit('retry')"
        >
          {{ t("common.retry") }}
        </button>
      </div>
      <ul
        v-else-if="items && items.length > 0"
        ref="list"
        class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2"
        @keydown="move"
      >
        <li
          v-for="conversation in items"
          :key="conversation.id"
          class="alix-d-flex alix-align-center alix-ga-1"
        >
          <slot
            :active="conversation.id === activeId"
            :conversation="conversation"
            name="item"
          >
            <button
              :aria-current="conversation.id === activeId ? 'true' : undefined"
              :class="conversation.id === activeId ? 'alix-row-active' : ''"
              class="alix-row-button alix-flex-grow-1 alix-min-w-0 alix-pa-3 alix-d-flex alix-flex-column alix-ga-1"
              type="button"
              data-row
              @click="emit('select', conversation.id)"
            >
              <span class="alix-font-medium alix-text-truncate">
                {{ conversation.title || t("conv.untitled") }}
              </span>
              <span
                v-if="conversation.preview"
                class="alix-text-muted alix-text-sm alix-text-truncate"
              >
                {{ conversation.preview }}
              </span>
              <span
                v-if="conversation.updatedAt"
                class="alix-text-muted alix-text-xs"
              >
                {{ formatDateTime(conversation.updatedAt) }}
              </span>
            </button>
            <button
              v-if="removable"
              :aria-label="t('conv.remove', { title: conversation.title || t('conv.untitled') })"
              class="alix-btn alix-btn-quiet alix-btn-icon alix-flex-shrink-0"
              type="button"
              @click="emit('remove', conversation.id)"
            >
              <AlixIcon name="trash" />
            </button>
          </slot>
        </li>
      </ul>
      <slot
        v-else
        name="empty"
      >
        <p class="alix-text-muted alix-text-center alix-pa-4 alix-ma-0">
          {{ t("conv.empty") }}
        </p>
      </slot>
    </AlixLoadable>
  </nav>
</template>

<style scoped lang="scss">
@use "../styles/mixins";

// A conversation row: a full-width button, text left, with the active row marked by a tint and a border (never a side stripe).
.alix-row-button {
  @include mixins.pressable;
  @include mixins.focus-ring;

  border: 1px solid var(--alix-border);
  border-radius: var(--alix-radius-md);
  background: var(--alix-surface);
  color: inherit;
  font: inherit;
  text-align: start;
  cursor: pointer;

  @include mixins.hover {
    background: var(--alix-surface-2);
  }

  &.alix-row-active {
    border-color: var(--alix-accent);
    background: var(--alix-accent-soft);
  }
}
</style>
