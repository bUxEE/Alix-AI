<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from "vue";
import type { voice } from "@alixai/sdk";
import type { AguiClient } from "../agui/client";
import type { AlixAttachment, AlixUiCall, ChatItem, ConversationSummary } from "../agui/types";
import { useBodyScrollLock } from "../composables/useBodyScrollLock";
import { useOnline } from "../composables/useBrowser";
import { useFocusTrap } from "../composables/useFocusTrap";
import { useAlixChat, type AlixChat } from "../composables/useAlixChat";
import { useAttachmentQueue, type AttachmentQueue } from "../composables/useAttachmentQueue";
import AlixDictationButton from "../dictation/AlixDictationButton.vue";
import { insertAtCaret, type VoiceModeOption } from "../dictation";
import type { AlixDownload, AlixExporter } from "../export";
import { useAlix } from "../i18n";
import type { Citation, DiagramBlock, PermittedAction } from "../sdk-types";
import AlixVoiceButton from "../voice/AlixVoiceButton.vue";
import AlixAttachmentComposer from "./AlixAttachmentComposer.vue";
import AlixConversationList from "./AlixConversationList.vue";
import AlixMessageStream from "./AlixMessageStream.vue";
import AlixIcon from "./common/AlixIcon.vue";
import AlixLoadable from "./common/AlixLoadable.vue";
import AlixSkeleton from "./common/AlixSkeleton.vue";

// The complete, embeddable chat panel: header, conversation switcher, timeline, progress, suggestions and the composer, over the
// AG-UI endpoint, with no CopilotKit. Give it a `client` (see createAguiClient) and a `threadId` (the session id the host's context
// token carries), or a `chat` the host built with useAlixChat. The panel is a client of the public API and nothing more: it never
// commits an action (an approval is a `resume` of the interrupt, with the secret kept by the host relay), never hides a citation,
// never opens a source itself (`open-citation` goes to the host, which resolves it under the user's scope) and never routes.

const props = withDefaults(
  defineProps<{
    client?: AguiClient;
    threadId?: string;
    /** A controller the host created with useAlixChat; replaces `client` and `threadId`. */
    chat?: AlixChat;
    title?: string;
    language?: string;
    /** Conversations to switch between. Without them the switcher is not shown. */
    conversations?: readonly ConversationSummary[];
    conversationsLoading?: boolean;
    /** Uploads one file to the files service and returns its id. Without it the panel takes no attachments. */
    uploader?: (file: File, signal: AbortSignal) => Promise<{ fileId: string }>;
    maxFileMb?: number;
    accept?: string;
    /** Runs a UI action the model proposed for a tool the host registered; the return value goes back as the tool result. */
    onUiAction?: (call: AlixUiCall) => unknown | Promise<unknown>;
    /** Loads the thread's history when the panel mounts. */
    autoConnect?: boolean;
    citationDisplay?: "inline" | "footnote" | "panel";
    links?: boolean;
    /**
     * Voice surface on the composer. Default `off` (1.0 behaviour: chat only).
     * `dictation` shows record-to-text; `conversation` shows the talk button for voice-chat mode.
     */
    voice?: VoiceModeOption;
    /** Required when `voice` is `dictation`: the SDK dictation client for this deployment. */
    dictationClient?: voice.DictationClient;
    /**
     * When true, render `AlixMemoryProposal` for `alixai.memory_proposal` CUSTOM events.
     * Default false (1.1 compatibility). The kit never calls AlixAI: provide confirm/reject handlers.
     */
    memory?: boolean;
    /** Host confirms a proposed memory id (calls POST /v1/memory/{id}/confirm). */
    onMemoryConfirm?: (id: string) => void | Promise<void>;
    /** Host rejects a proposed memory id (calls POST /v1/memory/{id}/reject). */
    onMemoryReject?: (id: string) => void | Promise<void>;
    /**
     * Controlled fullscreen. Omit for uncontrolled (internal state). Use with `v-model:fullscreen`.
     * Default behaviour when unbound: not fullscreen.
     */
    fullscreen?: boolean;
    /**
     * Overrides `--alix-fullscreen-z` (default 2400) for the fullscreen overlay.
     * Body-teleported dialogs/menus from inside the panel must use a higher z-index.
     */
    fullscreenZIndex?: number;
    /**
     * When true, show download menus on table/chart blocks and assistant answers.
     * Default false (1.1 compatibility). Requires `exporter`.
     */
    exports?: boolean;
    /** Host creates an export file (POST /v1/sessions/.../exports). The kit never calls AlixAI. */
    exporter?: AlixExporter;
    /** Host downloads a file id the exporter returned (GET /v1/files/{id}/content). */
    download?: AlixDownload;
  }>(),
  {
    client: undefined,
    threadId: undefined,
    chat: undefined,
    title: undefined,
    language: undefined,
    conversations: undefined,
    conversationsLoading: false,
    uploader: undefined,
    maxFileMb: undefined,
    accept: undefined,
    onUiAction: undefined,
    autoConnect: true,
    citationDisplay: "panel",
    links: true,
    voice: "off",
    dictationClient: undefined,
    memory: false,
    onMemoryConfirm: undefined,
    onMemoryReject: undefined,
    fullscreen: undefined,
    fullscreenZIndex: undefined,
    exports: false,
    exporter: undefined,
    download: undefined,
  },
);

const emit = defineEmits<{
  "open-citation": [citation: Citation];
  action: [action: PermittedAction];
  "continue-in-app": [itemId: string];
  "open-file": [file: AlixAttachment];
  "download-file": [file: AlixAttachment];
  select: [id: string];
  new: [];
  "voice-start": [];
  "voice-stop": [];
  "voice-cancel": [];
  "update:fullscreen": [value: boolean];
}>();

const { t } = useAlix();
const online = useOnline();

// The timeline's own slots (see AlixMessageStream), passed through to it. Only the ones the host fills are forwarded: an empty
// pass-through would replace the stream's default rendering with nothing. The slots are typed for the host (each entry kind its
// own `item`); the one dynamic pass-through in the template carries them all, so it casts its scope.
const STREAM_SLOTS = ["empty", "message", "attachments", "block", "diagram", "citations", "receipt", "confirmation", "continue", "memory-proposal", "error"] as const;
type Entry<K extends ChatItem["kind"]> = Extract<ChatItem, { kind: K }>;
const slots = defineSlots<{
  title?: () => unknown;
  "header-actions"?: () => unknown;
  conversations?: () => unknown;
  "composer-actions"?: () => unknown;
  "dictation-button"?: () => unknown;
  "voice-button"?: () => unknown;
  empty?: () => unknown;
  message?: (props: { item: Entry<"message"> }) => unknown;
  attachments?: (props: { item: Entry<"attachments"> }) => unknown;
  block?: (props: { item: Entry<"block"> }) => unknown;
  diagram?: (props: { block: DiagramBlock }) => unknown;
  citations?: (props: { item: Entry<"citations"> }) => unknown;
  receipt?: (props: { item: Entry<"receipt"> }) => unknown;
  confirmation?: (props: { item: Entry<"confirmation"> }) => unknown;
  continue?: (props: { item: Entry<"continue"> }) => unknown;
  "memory-proposal"?: (props: { item: Entry<"memory_proposal"> }) => unknown;
  error?: (props: { item: Entry<"error"> }) => unknown;
}>();
const forwarded = computed(() => STREAM_SLOTS.filter((name) => slots[name] !== undefined));

const createController = (): AlixChat => {
  if (props.chat !== undefined) return props.chat;
  if (props.client === undefined) throw new Error("AlixChatPanel needs a `client` or a `chat`.");
  return useAlixChat({
    client: props.client,
    threadId: () => props.threadId ?? "",
    language: () => props.language,
    memory: () => props.memory,
    ...(props.onUiAction !== undefined && { onUiAction: props.onUiAction }),
  });
};
const chat = createController();
const queue: AttachmentQueue | undefined = props.uploader === undefined ? undefined : useAttachmentQueue({ uploader: props.uploader });

const draft = ref("");
const showList = ref(false);
const localFullscreen = ref(false);
const panelRoot = ref<HTMLElement | null>(null);
const fullscreenToggle = ref<HTMLElement | null>(null);
const composer = ref<{
  focus: () => void;
  selectionStart: () => number;
  selectionEnd: () => number;
  setSelection: (start: number, end?: number) => void;
} | null>(null);

const isFullscreen = computed(() => props.fullscreen ?? localFullscreen.value);
const fullscreenStyle = computed(() =>
  props.fullscreenZIndex === undefined ? undefined : { "--alix-fullscreen-z": String(props.fullscreenZIndex) },
);
const setFullscreen = (value: boolean): void => {
  localFullscreen.value = value;
  emit("update:fullscreen", value);
};
const toggleFullscreen = (): void => setFullscreen(!isFullscreen.value);
useBodyScrollLock(isFullscreen);
useFocusTrap(panelRoot, isFullscreen, fullscreenToggle);

const canExport = computed(() => props.exports === true && props.exporter !== undefined);
const sessionId = computed(() => props.threadId ?? chat.state.threadId ?? "");

const onFullscreenKeydown = (event: KeyboardEvent): void => {
  if (!isFullscreen.value || event.key !== "Escape" || event.defaultPrevented) return;
  const target = event.target;
  if (target instanceof Element) {
    const dialog = target.closest('[role="dialog"][aria-modal="true"]');
    if (dialog !== null && dialog !== panelRoot.value) return;
  }
  event.preventDefault();
  setFullscreen(false);
};

const insertDictation = (text: string): void => {
  const start = composer.value?.selectionStart() ?? draft.value.length;
  const end = composer.value?.selectionEnd() ?? start;
  const next = insertAtCaret(draft.value, text, start, end);
  draft.value = next.text;
  void nextTick(() => {
    composer.value?.setSelection(next.caret);
    composer.value?.focus();
  });
};

const hasList = computed(() => props.conversations !== undefined);
const state = chat.state;
// The pipeline stages the turn-event schema names have words of their own; another name is shown as it is, made readable.
const STAGES = ["interpreting", "retrieving", "sources", "validating", "preparing", "committing", "tool_call", "queued", "generating", "verifying"] as const;
type Stage = (typeof STAGES)[number];
const isStage = (value: string): value is Stage => (STAGES as readonly string[]).includes(value);
const stage = computed(() => {
  const step = state.step ?? "";
  return isStage(step) ? t(`chat.stage.${step}`) : step.replace(/[_-]+/g, " ");
});
// The share done of the running stage, when the stream reports one (`alixai.progress` with `percent`), as a whole number 0-100.
const percent = computed(() => {
  const current = state.step === undefined ? undefined : state.progress[state.step];
  const value = current?.percent ?? Object.values(state.progress).find((entry) => entry.state === "running" && entry.percent !== undefined)?.percent;
  return value === undefined || !Number.isFinite(value) ? undefined : Math.round(Math.min(100, Math.max(0, value)));
});
const canSuggest = computed(() => !chat.running.value && state.outcome === "success" && state.suggestions.length > 0);
// The goal still needs the user while it is being filled in or an action waits for an answer; one whose capability just
// succeeded or failed waits for nothing.
const awaitingUser = computed(() => {
  const pending = state.pending;
  return pending !== undefined && !chat.running.value && pending.phase !== "success" && pending.phase !== "failure";
});

const send = async (text: string): Promise<void> => {
  const attachments = queue?.outgoing.value ?? [];
  queue?.clear();
  await chat.send(text, { attachments });
};

const answerMemory = async (itemId: string, kind: "confirm" | "reject"): Promise<void> => {
  const memoryId = chat.memoryIdOf(itemId);
  if (memoryId === undefined) return;
  const handler = kind === "confirm" ? props.onMemoryConfirm : props.onMemoryReject;
  if (handler === undefined) return;
  chat.setMemoryStatus(itemId, "submitting");
  try {
    await handler(memoryId);
    chat.setMemoryStatus(itemId, kind === "confirm" ? "remembered" : "forgotten");
  } catch {
    chat.setMemoryStatus(itemId, "failed");
  }
};
const onConfirmMemory = (itemId: string): void => {
  void answerMemory(itemId, "confirm");
};
const onRejectMemory = (itemId: string): void => {
  void answerMemory(itemId, "reject");
};

const choose = (choiceId: string, itemId: string): void => {
  void chat.choose(itemId, choiceId);
};

const onAction = (action: PermittedAction): void => {
  if (action.action === "retry" || action.action === "refresh") void chat.retry();
  emit("action", action);
};

const connect = (): void => {
  if (props.autoConnect && (props.chat !== undefined || (props.threadId ?? "") !== "")) void chat.connect();
};

onMounted(connect);
watch(
  () => props.threadId,
  (next, previous) => {
    if (props.chat !== undefined || next === previous) return;
    chat.reset();
    connect();
  },
);

const select = (id: string): void => {
  showList.value = false;
  emit("select", id);
};

defineExpose({ chat });
</script>

<template>
  <Teleport
    :disabled="!isFullscreen"
    to="body"
  >
    <section
      :aria-label="title ?? t('chat.title')"
      :aria-modal="isFullscreen ? 'true' : undefined"
      :class="{ 'alix-chat-fullscreen': isFullscreen }"
      :role="isFullscreen ? 'dialog' : undefined"
      :style="fullscreenStyle"
      :tabindex="isFullscreen ? -1 : undefined"
      ref="panelRoot"
      class="alix-root alix-card alix-d-flex alix-flex-column alix-h-100 alix-min-h-0 alix-overflow-hidden"
      @keydown="onFullscreenKeydown"
    >
    <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-2 alix-px-4 alix-py-3">
      <div class="alix-d-flex alix-align-center alix-ga-2 alix-min-w-0">
        <button
          v-if="hasList"
          :aria-expanded="showList"
          :aria-label="t('chat.conversations')"
          class="alix-btn alix-btn-quiet alix-btn-icon"
          type="button"
          @click="showList = !showList"
        >
          <AlixIcon name="menu" />
        </button>
        <slot name="title">
          <h2 class="alix-heading alix-text-truncate">
            {{ title ?? t("chat.title") }}
          </h2>
        </slot>
      </div>
      <div class="alix-d-flex alix-align-center alix-ga-1 alix-flex-shrink-0">
        <slot name="header-actions" />
        <button
          :aria-label="isFullscreen ? t('chat.fullscreenExit') : t('chat.fullscreen')"
          :aria-pressed="isFullscreen"
          ref="fullscreenToggle"
          class="alix-btn alix-btn-quiet alix-btn-icon"
          type="button"
          @click="toggleFullscreen"
        >
          <AlixIcon :name="isFullscreen ? 'minimize' : 'maximize'" />
        </button>
      </div>
    </header>
    <p
      v-if="!online"
      class="alix-banner alix-tone-warning alix-mx-4 alix-ma-0"
      role="status"
    >
      {{ t("common.offline") }}
    </p>
    <p
      v-else-if="chat.reconnecting.value"
      class="alix-banner alix-tone-warning alix-mx-4 alix-ma-0"
      role="status"
    >
      {{ t("common.reconnecting") }}
    </p>
    <div class="alix-d-flex alix-flex-grow-1 alix-min-h-0">
      <aside
        v-if="hasList && showList"
        class="alix-pa-3 alix-overflow-auto alix-flex-shrink-0 alix-conversations"
      >
        <slot name="conversations">
          <AlixConversationList
            :active-id="threadId"
            :items="conversations"
            :loading="conversationsLoading"
            @new="emit('new')"
            @select="select"
          />
        </slot>
      </aside>
      <AlixLoadable
        :loading="chat.load.isLoading.value"
        :loaded="chat.load.hasLoadedOnce.value"
        class="alix-d-flex alix-flex-column alix-flex-grow-1 alix-min-w-0"
      >
        <template #skeleton>
          <div class="alix-pa-4 alix-d-flex alix-flex-column alix-ga-4">
            <div class="alix-d-flex alix-flex-column alix-ga-2">
              <AlixSkeleton width="70%" />
              <AlixSkeleton width="90%" />
              <AlixSkeleton width="45%" />
            </div>
            <div class="alix-self-end alix-d-flex alix-flex-column alix-ga-2 alix-w-100">
              <AlixSkeleton
                class="alix-self-end"
                width="50%"
              />
            </div>
          </div>
        </template>
        <AlixMessageStream
          :citation-display="citationDisplay"
          :download="download"
          :exporter="exporter"
          :exports="canExport"
          :items="chat.items.value"
          :links="links"
          :running="chat.running.value"
          :session-id="sessionId"
          @action="onAction"
          @approve="chat.approve($event)"
          @choose="choose"
          @confirm-memory="onConfirmMemory"
          @continue-in-app="emit('continue-in-app', $event)"
          @decline="chat.decline($event)"
          @dismiss="chat.dismiss($event)"
          @download-file="emit('download-file', $event)"
          @free-text="send"
          @open-citation="emit('open-citation', $event)"
          @open-file="emit('open-file', $event)"
          @reject-memory="onRejectMemory"
          @retry="chat.retry()"
        >
          <template
            v-for="name in forwarded"
            #[name]="scope"
          >
            <slot
              :name="name"
              v-bind="(scope ?? {}) as { item: never; block: never }"
            />
          </template>
        </AlixMessageStream>
      </AlixLoadable>
    </div>
    <div class="alix-px-4 alix-pb-4 alix-d-flex alix-flex-column alix-ga-2">
      <p
        v-if="chat.running.value && stage"
        class="alix-d-flex alix-align-center alix-ga-2 alix-text-muted alix-text-sm alix-ma-0"
        role="status"
      >
        <span
          class="alix-spinner"
          aria-hidden="true"
        />
        {{ percent === undefined ? t("chat.working", { stage }) : t("chat.workingPercent", { stage, percent }) }}
      </p>
      <div
        v-if="chat.running.value && stage && percent !== undefined"
        :aria-label="t('chat.progress')"
        :aria-valuenow="percent"
        class="alix-progress"
        role="progressbar"
        aria-valuemin="0"
        aria-valuemax="100"
      >
        <span
          :style="{ transform: `scaleX(${percent / 100})` }"
          class="alix-progress-fill"
        />
      </div>
      <p
        v-else-if="awaitingUser && state.pending"
        class="alix-text-muted alix-text-sm alix-ma-0"
      >
        {{ t("chat.pending", { capability: state.pending.capabilityId }) }}
      </p>
      <ul
        v-if="canSuggest"
        :aria-label="t('chat.suggestions')"
        class="alix-reset-list alix-d-flex alix-flex-wrap alix-ga-2"
      >
        <li
          v-for="suggestion in state.suggestions"
          :key="suggestion.message"
        >
          <button
            class="alix-chip"
            type="button"
            @click="send(suggestion.message)"
          >
            {{ suggestion.title }}
          </button>
        </li>
      </ul>
      <AlixAttachmentComposer
        v-model="draft"
        :accept="accept"
        :attachable="queue !== undefined"
        :disabled="!online"
        :files="queue?.files.value ?? []"
        :max-file-mb="maxFileMb"
        :running="chat.running.value"
        ref="composer"
        @add-files="queue?.add($event)"
        @remove-file="queue?.remove($event)"
        @send="send"
        @stop="chat.stop()"
      >
        <template #actions>
          <slot name="composer-actions" />
          <slot
            v-if="voice === 'dictation' && dictationClient && (threadId ?? '') !== ''"
            name="dictation-button"
          >
            <AlixDictationButton
              :client="dictationClient"
              :language="language"
              :session-id="threadId ?? ''"
              @transcript="insertDictation"
            />
          </slot>
          <slot
            v-else-if="voice === 'conversation'"
            name="voice-button"
          >
            <AlixVoiceButton
              @cancel="emit('voice-cancel')"
              @start="emit('voice-start')"
              @stop="emit('voice-stop')"
            />
          </slot>
        </template>
      </AlixAttachmentComposer>
    </div>
    </section>
  </Teleport>
</template>

<style scoped lang="scss">
// The conversation switcher is a fixed-width column beside the timeline; its width is a size, not a layout decision.
.alix-conversations {
  width: 260px;
  border-inline-end: 1px solid var(--alix-border);
}

// Fullscreen: same panel instance, moved to body. Safe-area inset keeps controls clear of notches; reduced-motion
// collapses the enter duration through the token.
.alix-chat-fullscreen {
  position: fixed;
  inset: env(safe-area-inset-top, 0px) env(safe-area-inset-right, 0px) env(safe-area-inset-bottom, 0px) env(safe-area-inset-left, 0px);
  z-index: var(--alix-fullscreen-z, 2400);
  width: auto;
  height: auto;
  max-height: 100dvh;
  border-radius: 0;
  outline: none;
  animation: alix-fullscreen-in var(--alix-dur-base) var(--alix-ease-out) both;
}

@keyframes alix-fullscreen-in {
  from {
    opacity: 0;
    transform: scale(0.98);
  }
}

// The share done of the running stage: a thin track whose fill grows from the start edge. Only the transform moves.
.alix-progress {
  height: 4px;
  overflow: hidden;
  border-radius: 999px;
  background: var(--alix-surface-3);
}

.alix-progress-fill {
  display: block;
  height: 100%;
  background: var(--alix-accent);
  transform-origin: left center;
  transition: transform var(--alix-dur-slow) var(--alix-ease-out);

  &:dir(rtl) {
    transform-origin: right center;
  }
}
</style>
