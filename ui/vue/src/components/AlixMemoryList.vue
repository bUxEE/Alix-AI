<script setup lang="ts">
import { ref } from "vue";
import { useLoadFromProps } from "../composables/useLoadState";
import { useAlix } from "../i18n";
import type { MemoryListItem } from "../agui/types";
import AlixIcon from "./common/AlixIcon.vue";
import AlixLoadable from "./common/AlixLoadable.vue";
import AlixSkeleton from "./common/AlixSkeleton.vue";

// The user's memories for the current view. The host owns the data and the API calls; this list only
// reports edit / delete / retry. Skeleton on first load, overlay on refetch.

const props = withDefaults(
  defineProps<{
    items?: readonly MemoryListItem[];
    title?: string;
    loading?: boolean;
    failed?: boolean;
    editable?: boolean;
  }>(),
  { items: undefined, title: undefined, loading: false, failed: false, editable: true },
);

const emit = defineEmits<{
  edit: [item: MemoryListItem];
  delete: [id: string];
  retry: [];
  save: [item: MemoryListItem, text: string];
}>();

const { t } = useAlix();
const state = useLoadFromProps({
  loading: () => props.loading,
  failed: () => props.failed,
  hasData: () => props.items !== undefined,
});

const editingId = ref<string | undefined>(undefined);
const draft = ref("");

const startEdit = (item: MemoryListItem): void => {
  editingId.value = item.id;
  draft.value = item.text;
  emit("edit", item);
};

const cancelEdit = (): void => {
  editingId.value = undefined;
  draft.value = "";
};

const saveEdit = (item: MemoryListItem): void => {
  const text = draft.value.trim();
  if (text === "") return;
  emit("save", item, text);
  cancelEdit();
};
</script>

<template>
  <section
    :aria-label="t('memory.list.title')"
    class="alix-root alix-d-flex alix-flex-column alix-ga-3"
  >
    <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
      <slot name="header">
        <h2 class="alix-heading alix-ma-0">
          {{ title ?? t("memory.list.title") }}
        </h2>
      </slot>
    </header>

    <AlixLoadable
      :loaded="state.hasLoadedOnce.value"
      :loading="state.isLoading.value"
    >
      <template #skeleton>
        <ul class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2">
          <slot name="skeleton">
            <li
              v-for="n in 3"
              :key="n"
              class="alix-card alix-pa-3 alix-d-flex alix-flex-column alix-ga-2"
            >
              <AlixSkeleton width="70%" />
              <AlixSkeleton
                height="12px"
                width="40%"
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
        <span>{{ t("memory.list.failed") }}</span>
        <button
          class="alix-btn"
          type="button"
          @click="emit('retry')"
        >
          {{ t("common.retry") }}
        </button>
      </div>

      <p
        v-else-if="items !== undefined && items.length === 0"
        class="alix-text-muted alix-ma-0"
      >
        {{ t("memory.list.empty") }}
      </p>

      <ul
        v-else-if="items !== undefined"
        class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2"
      >
        <li
          v-for="item in items"
          :key="item.id"
          class="alix-card alix-pa-3 alix-d-flex alix-flex-column alix-ga-2"
        >
          <slot
            :editing="editingId === item.id"
            :item="item"
            name="row"
          >
            <template v-if="editingId === item.id">
              <textarea
                v-model="draft"
                class="alix-input no-validation"
                rows="3"
              />
              <div class="alix-d-flex alix-ga-2">
                <button
                  class="alix-btn alix-btn-primary"
                  type="button"
                  @click="saveEdit(item)"
                >
                  {{ t("common.save") }}
                </button>
                <button
                  class="alix-btn"
                  type="button"
                  @click="cancelEdit"
                >
                  {{ t("common.discard") }}
                </button>
              </div>
            </template>
            <template v-else>
              <p class="alix-ma-0 alix-break-word">
                {{ item.text }}
              </p>
              <div class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
                <span class="alix-text-muted alix-text-sm">
                  {{ t(`memory.status.${item.status}`) }}
                </span>
                <div
                  v-if="editable"
                  class="alix-d-flex alix-ga-1"
                >
                  <button
                    :aria-label="t('memory.list.edit')"
                    class="alix-btn alix-btn-ghost"
                    type="button"
                    @click="startEdit(item)"
                  >
                    <AlixIcon
                      :size="16"
                      name="edit"
                    />
                  </button>
                  <button
                    :aria-label="t('memory.list.delete')"
                    class="alix-btn alix-btn-ghost"
                    type="button"
                    @click="emit('delete', item.id)"
                  >
                    <AlixIcon
                      :size="16"
                      name="trash"
                    />
                  </button>
                </div>
              </div>
            </template>
          </slot>
        </li>
      </ul>
    </AlixLoadable>
  </section>
</template>
