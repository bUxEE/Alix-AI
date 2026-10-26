<script setup lang="ts">
import { AlixMemoryList, AlixMemoryProposal, type MemoryListItem } from "@alixai/vue";
import { computed, onMounted, ref } from "vue";
import { DEMO_PATHS, fetchDemoJson } from "../../../demo-mock/src/index";
import DemoHostLog from "../components/DemoHostLog.vue";
import DemoSection from "../components/DemoSection.vue";
import { useDemoResource } from "../composables/useDemoResource";
import { useHostLog } from "../composables/useHostLog";
import { useDemoText } from "../state";

// AlixMemoryList and a standalone AlixMemoryProposal over the mock's /v1/memory. The host owns every API call.

const text = useDemoText();
const proposalStatus = ref<"pending" | "submitting" | "remembered" | "forgotten" | "failed">("pending");
const proposal = {
  id: "mem_01JDEMO000000000000000000A",
  text: "Prefer dark mode",
  expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
};

type HostEvent =
  | { kind: "confirm"; memoryId: string }
  | { kind: "reject"; memoryId: string }
  | { kind: "delete"; memoryId: string }
  | { kind: "save"; memoryId: string };

const log = useHostLog<HostEvent>();

const resource = useDemoResource(async (signal) => {
  const body = await fetchDemoJson<{ items: Array<{ id: string; text: string; status: MemoryListItem["status"] }> }>(
    DEMO_PATHS.memory,
    "",
    { signal },
  );
  return body.items.map((item) => ({ id: item.id, text: item.text, status: item.status }));
});
onMounted(() => void resource.refresh());

const items = computed(() => (resource.error.value === undefined ? resource.data.value : undefined));

const describe = (entry: HostEvent & { id: number; at: string }): string => {
  if (entry.kind === "confirm") return text("memory.log.confirm", { id: entry.memoryId });
  if (entry.kind === "reject") return text("memory.log.reject", { id: entry.memoryId });
  if (entry.kind === "delete") return text("memory.log.delete", { id: entry.memoryId });
  return text("memory.log.save", { id: entry.memoryId });
};

const onConfirm = (): void => {
  proposalStatus.value = "remembered";
  log.push({ kind: "confirm", memoryId: proposal.id });
};

const onReject = (): void => {
  proposalStatus.value = "forgotten";
  log.push({ kind: "reject", memoryId: proposal.id });
};

const onDelete = async (id: string): Promise<void> => {
  await fetch(`${DEMO_PATHS.memory}/${encodeURIComponent(id)}`, { method: "DELETE" });
  log.push({ kind: "delete", memoryId: id });
  await resource.refresh();
};

const onSave = async (item: MemoryListItem, next: string): Promise<void> => {
  await fetch(`${DEMO_PATHS.memory}/${encodeURIComponent(item.id)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text: next }),
  });
  log.push({ kind: "save", memoryId: item.id });
  await resource.refresh();
};
</script>

<template>
  <DemoSection>
    <template #eyebrow>
      AlixMemoryProposal · AlixMemoryList
    </template>
    <template #title>
      {{ text("nav.memory.title") }}
    </template>
    <template #lede>
      {{ text("memory.lede") }}
    </template>
    <template #actions>
      <button
        :disabled="resource.state.isLoading.value"
        class="alix-btn"
        type="button"
        @click="resource.refresh()"
      >
        {{ text("common.refresh") }}
      </button>
    </template>

    <div class="alix-row">
      <div class="alix-col-12 alix-col-md-5 alix-d-flex alix-flex-column alix-ga-4">
        <h2 class="alix-ma-0 alix-demo-subtitle">
          {{ text("memory.proposal") }}
        </h2>
        <AlixMemoryProposal
          :proposal="proposal"
          :status="proposalStatus"
          @confirm="onConfirm"
          @reject="onReject"
        />
      </div>
      <div class="alix-col-12 alix-col-md-7 alix-card alix-pa-4">
        <AlixMemoryList
          :items="items"
          :loading="resource.state.isLoading.value"
          :failed="resource.error.value !== undefined"
          @delete="onDelete"
          @retry="resource.refresh()"
          @save="onSave"
        />
      </div>
    </div>

    <div class="alix-d-flex alix-flex-column alix-ga-3 alix-mt-4">
      <div class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
        <h2 class="alix-ma-0 alix-demo-subtitle">
          {{ text("chat.host") }}
        </h2>
        <button
          v-if="log.entries.value.length > 0"
          class="alix-btn alix-btn-quiet alix-demo-btn-xs"
          type="button"
          @click="log.clear()"
        >
          {{ text("common.clear") }}
        </button>
      </div>
      <DemoHostLog :entries="log.entries.value">
        <template #entry="{ entry }">
          {{ describe(entry) }}
        </template>
        <template #empty>
          {{ text("chat.host.empty") }}
        </template>
      </DemoHostLog>
    </div>
  </DemoSection>
</template>
