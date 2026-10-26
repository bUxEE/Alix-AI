<script setup lang="ts">
import {
  AlixChatPanel,
  createAguiClient,
  useAlixChat,
  type AlixAttachment,
  type Citation,
  type ConversationSummary,
  type DiagramBlock,
  type PermittedAction,
  type VoiceModeOption,
} from "@alixai/vue";
import { computed, onActivated, onMounted, ref } from "vue";
import {
  DEMO_PATHS,
  DEMO_THREAD_ID,
  demoDictationClient,
  demoPrompts,
  demoUploader,
  fetchDemoJson,
  type DemoPrompt,
} from "../../../demo-mock/src/index";
import DemoAttachmentImages from "../components/DemoAttachmentImages.vue";
import DemoFlowchart from "../components/DemoFlowchart.vue";
import DemoHostLog from "../components/DemoHostLog.vue";
import DemoIcon from "../components/DemoIcon.vue";
import DemoSection from "../components/DemoSection.vue";
import { useDemoResource } from "../composables/useDemoResource";
import { useHostLog } from "../composables/useHostLog";
import { downloadDemoFile } from "../files";
import { locale, requestedThread, useDemoText } from "../state";

// The whole chat panel over the mock's AG-UI route, with the "try these" prompts beside it. The host owns the controller
// (useAlixChat) so the prompt rail can send through it and the conversation switcher can move between threads. Two of the
// panel's slots are filled the way a host would: image attachments drawn through the files API, and diagrams drawn by the
// demo's own flowchart renderer.

const text = useDemoText();
const client = createAguiClient({ endpoint: DEMO_PATHS.agui });
const threadId = ref(DEMO_THREAD_ID);
const voiceMode = ref<VoiceModeOption>("off");
const dictationClient = demoDictationClient();
const uploader = demoUploader();
const chat = useAlixChat({ client, threadId: () => threadId.value, language: () => locale.value });
const voiceOptions = computed(() => [
  { value: "off" as const, label: text("chat.voice.off") },
  { value: "dictation" as const, label: text("chat.voice.dictation") },
  { value: "conversation" as const, label: text("chat.voice.conversation") },
]);

const conversations = useDemoResource(async (signal) => (await fetchDemoJson<{ conversations: ConversationSummary[] }>(DEMO_PATHS.conversations, "", { signal })).conversations);
onMounted(() => void conversations.refresh());

const prompts = demoPrompts.filter((prompt) => prompt.kits === undefined || prompt.kits.includes("vue"));
const busy = computed(() => chat.running.value);

const run = (prompt: DemoPrompt): void => {
  void chat.send(prompt.prompt[locale.value]);
};

const select = (id: string): void => {
  if (id === threadId.value) return;
  threadId.value = id;
  chat.reset();
  void chat.connect();
};

// Another section (the conversation list) asked for a thread: open it when this section shows.
onActivated(() => {
  if (requestedThread.value === undefined) return;
  select(requestedThread.value);
  requestedThread.value = undefined;
  void conversations.refresh();
});

const create = async (): Promise<void> => {
  const created = await fetchDemoJson<{ id: string }>(DEMO_PATHS.conversations, "", { method: "POST" });
  await conversations.refresh();
  select(created.id);
};

type HostEvent =
  | { kind: "citation"; name: string }
  | { kind: "continue"; id: string }
  | { kind: "action"; action: string }
  | { kind: "open"; name: string }
  | { kind: "download"; name: string }
  | { kind: "voice"; phase: "start" | "stop" | "cancel" };

const log = useHostLog<HostEvent>();

const onCitation = (citation: Citation): void => log.push({ kind: "citation", name: citation.display_name });
const onContinue = (id: string): void => log.push({ kind: "continue", id });
const onAction = (action: PermittedAction): void => log.push({ kind: "action", action: action.label ?? action.action });
const onOpen = (file: AlixAttachment): void => log.push({ kind: "open", name: file.name });
const onDownload = (file: AlixAttachment): void => {
  log.push({ kind: "download", name: file.name });
  void downloadDemoFile(file);
};
const onVoiceStart = (): void => log.push({ kind: "voice", phase: "start" });
const onVoiceStop = (): void => log.push({ kind: "voice", phase: "stop" });
const onVoiceCancel = (): void => log.push({ kind: "voice", phase: "cancel" });

const describe = (entry: HostEvent): string => {
  switch (entry.kind) {
    case "citation":
      return text("chat.log.citation", { name: entry.name });
    case "continue":
      return text("chat.log.continue", { id: entry.id });
    case "action":
      return text("chat.log.action", { action: entry.action });
    case "open":
      return text("chat.log.open", { name: entry.name });
    case "voice":
      return entry.phase === "start"
        ? text("chat.log.voiceStart")
        : entry.phase === "stop"
          ? text("chat.log.voiceStop")
          : text("chat.log.voiceCancel");
    default:
      return text("chat.log.download", { name: entry.name });
  }
};
</script>

<template>
  <DemoSection>
    <template #eyebrow>
      AlixChatPanel · AlixMessageStream · useAlixChat
    </template>
    <template #title>
      {{ text("nav.chat.title") }}
    </template>
    <template #lede>
      {{ text("chat.lede") }}
    </template>
    <div class="alix-row alix-demo-chat-row">
      <div class="alix-col-12 alix-col-lg-8 alix-d-flex alix-flex-column alix-ga-3 alix-demo-chat-panel alix-sticky-lg-top">
        <div
          :aria-label="text('chat.voice')"
          class="alix-d-flex alix-align-center alix-ga-3 alix-flex-wrap"
        >
          <span class="alix-text-sm alix-text-muted">
            {{ text("chat.voice") }}
          </span>
          <div
            class="alix-d-flex alix-demo-segmented"
            role="radiogroup"
          >
            <button
              v-for="option in voiceOptions"
              :key="option.value"
              :aria-checked="voiceMode === option.value"
              class="alix-demo-segment alix-text-xs"
              role="radio"
              type="button"
              @click="voiceMode = option.value"
            >
              {{ option.label }}
            </button>
          </div>
        </div>
        <AlixChatPanel
          :chat="chat"
          :dictation-client="voiceMode === 'dictation' ? dictationClient : undefined"
          :language="locale"
          :thread-id="threadId"
          :conversations="conversations.data.value"
          :conversations-loading="conversations.state.isLoading.value"
          :uploader="uploader"
          :voice="voiceMode"
          :max-file-mb="8"
          :title="text('chat.panelTitle')"
          accept="image/png,image/jpeg,image/webp,application/pdf,text/csv,text/plain"
          @action="onAction"
          @continue-in-app="onContinue"
          @download-file="onDownload"
          @new="create"
          @open-citation="onCitation"
          @open-file="onOpen"
          @select="select"
          @voice-cancel="onVoiceCancel"
          @voice-start="onVoiceStart"
          @voice-stop="onVoiceStop"
        >
          <template #attachments="{ item }">
            <DemoAttachmentImages
              :files="item.files"
              @download="onDownload"
              @open="onOpen"
            />
          </template>
          <template #diagram="{ block }">
            <DemoFlowchart :block="block as DiagramBlock" />
          </template>
        </AlixChatPanel>
      </div>
      <aside
        :aria-label="text('chat.rail')"
        class="alix-col-12 alix-col-lg-4 alix-flex-1 alix-d-flex alix-flex-column alix-ga-5 alix-demo-rail alix-sticky-lg-top"
      >
        <div class="alix-d-flex alix-flex-column alix-ga-3">
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ text("chat.rail") }}
          </h2>
          <ol class="alix-reset-list alix-d-flex alix-flex-column alix-ga-1">
            <li
              v-for="(prompt, index) in prompts"
              :key="prompt.id"
              :style="{ '--alix-demo-i': index }"
              class="alix-demo-stagger"
            >
              <button
                :disabled="busy"
                :title="prompt.prompt[locale]"
                class="alix-d-flex alix-align-start alix-ga-3 alix-w-100 alix-demo-prompt"
                type="button"
                @click="run(prompt)"
              >
                <span class="alix-font-mono alix-text-xs alix-text-muted alix-demo-prompt-index">
                  {{ String(index + 1).padStart(2, "0") }}
                </span>
                <span class="alix-d-flex alix-flex-column alix-ga-1 alix-min-w-0">
                  <span class="alix-font-medium">
                    {{ prompt.title[locale] }}
                  </span>
                  <span class="alix-text-muted alix-text-xs">
                    {{ prompt.shows[locale] }}
                  </span>
                </span>
                <DemoIcon
                  :size="16"
                  class="alix-flex-shrink-0 alix-demo-prompt-go"
                  name="arrow"
                />
              </button>
            </li>
          </ol>
        </div>
        <div class="alix-d-flex alix-flex-column alix-ga-3">
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
      </aside>
    </div>
  </DemoSection>
</template>
