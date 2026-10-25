<script setup lang="ts">
import { AlixConversationList, useAlix, type ConversationSummary } from "@alixai/vue";
import { computed, onMounted, ref } from "vue";
import { DEMO_PATHS, fetchDemoJson } from "../../../demo-mock/src/index";
import DemoIcon from "../components/DemoIcon.vue";
import DemoSection from "../components/DemoSection.vue";
import { useDemoResource } from "../composables/useDemoResource";
import { requestedThread, useDemoText } from "../state";

// The conversation list on its own, fed by the mock's `/demo/conversations` (the public API has no "list my conversations":
// the host supplies them). New, remove, refresh (overlay over the rows already shown) and a failed load (the error state with
// its retry) all go through the same resource; the first load is a skeleton shaped like the rows.

const text = useDemoText();
const { formatDateTime } = useAlix();
const broken = ref(false);
const selected = ref<string>();

const resource = useDemoResource(async (signal) => {
  const path = broken.value ? "/demo/conversations-unavailable" : DEMO_PATHS.conversations;
  return (await fetchDemoJson<{ conversations: ConversationSummary[] }>(path, "", { signal })).conversations;
});
onMounted(() => void resource.refresh());

const items = computed(() => (resource.error.value === undefined ? resource.data.value : undefined));
const active = computed(() => resource.data.value?.find((item) => item.id === selected.value));

const create = async (): Promise<void> => {
  const created = await fetchDemoJson<{ id: string }>(DEMO_PATHS.conversations, "", { method: "POST" });
  selected.value = created.id;
  await resource.refresh();
};

const remove = async (id: string): Promise<void> => {
  await fetch(`${DEMO_PATHS.conversations}/${encodeURIComponent(id)}`, { method: "DELETE" });
  if (selected.value === id) selected.value = undefined;
  await resource.refresh();
};

const fail = (): void => {
  broken.value = true;
  void resource.refresh();
};

const recover = (): void => {
  broken.value = false;
  void resource.refresh();
};

const open = (id: string): void => {
  requestedThread.value = id;
  globalThis.location.hash = "#/chat";
};
</script>

<template>
  <DemoSection>
    <template #eyebrow>
      AlixConversationList
    </template>
    <template #title>
      {{ text("nav.conversations.title") }}
    </template>
    <template #lede>
      {{ text("conv.lede") }}
    </template>
    <template #actions>
      <button
        :disabled="resource.state.isLoading.value"
        class="alix-btn"
        type="button"
        @click="resource.refresh()"
      >
        <DemoIcon
          :size="16"
          name="refresh"
        />
        {{ text("common.refresh") }}
      </button>
      <button
        v-if="!broken"
        class="alix-btn alix-btn-quiet"
        type="button"
        @click="fail"
      >
        {{ text("conv.fail") }}
      </button>
    </template>
    <div class="alix-row">
      <div class="alix-col-12 alix-col-md-5 alix-card alix-pa-4 alix-demo-list-card alix-sticky-md-top">
        <AlixConversationList
          :items="items"
          :active-id="selected"
          :loading="resource.state.isLoading.value"
          :failed="resource.error.value !== undefined"
          removable
          @new="create"
          @remove="remove"
          @retry="recover"
          @select="selected = $event"
        />
      </div>
      <div class="alix-col-12 alix-col-md-7 alix-flex-1 alix-card alix-pa-5 alix-d-flex alix-flex-column alix-ga-4 alix-demo-detail alix-sticky-md-top">
        <template v-if="active">
          <p class="alix-ma-0 alix-font-mono alix-text-xs alix-text-muted">
            {{ active.id }}
          </p>
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ active.title ?? text("conv.untitled") }}
          </h2>
          <p
            v-if="active.preview"
            class="alix-ma-0 alix-demo-quote"
          >
            {{ active.preview }}
          </p>
          <p
            v-if="active.updatedAt"
            class="alix-ma-0 alix-text-sm alix-text-muted"
          >
            {{ text("conv.updated", { time: formatDateTime(active.updatedAt) }) }}
          </p>
          <div class="alix-d-flex alix-flex-wrap alix-ga-2">
            <button
              class="alix-btn alix-btn-primary"
              type="button"
              @click="open(active.id)"
            >
              {{ text("conv.open") }}
              <DemoIcon
                :size="16"
                name="arrow"
              />
            </button>
          </div>
        </template>
        <div
          v-else
          class="alix-my-auto alix-d-flex alix-flex-column alix-align-start alix-ga-2 alix-demo-empty"
        >
          <DemoIcon
            :size="28"
            class="alix-text-muted"
            name="conversations"
          />
          <p class="alix-ma-0 alix-font-medium">
            {{ text("conv.pick") }}
          </p>
          <p class="alix-ma-0 alix-text-sm alix-text-muted">
            {{ text("conv.pick.body") }}
          </p>
        </div>
      </div>
    </div>
  </DemoSection>
</template>
