<script setup lang="ts">
import type { voice } from "@alixai/sdk";
import { voiceOrbStateFor } from "@alixai/voice-orb";
import { computed, nextTick, onMounted, ref, useId, useTemplateRef, watch } from "vue";
import AlixIcon from "../components/common/AlixIcon.vue";
import { useReducedMotion } from "../composables/useBrowser";
import { useAlix } from "../i18n";
import AlixTranscriptEditor from "./AlixTranscriptEditor.vue";
import AlixVoiceButton from "./AlixVoiceButton.vue";
import AlixVoiceOrb from "./AlixVoiceOrb.vue";
import { useAlixVoiceText, type AlixVoiceMessageKey } from "./messages";
import type { AlixMicrophone, AlixPlayer, AlixVoiceEvent, AlixVoiceFallback, AlixVoicePhase, AlixVoiceReply, AlixVoiceTurn } from "./types";
import { useAlixVoice } from "./useAlixVoice";

// Voice Mode: a calm, full-attention surface for talking with the assistant, in two ways to use it.
//
//   * Pass a `session` (from `voice.createVoiceSession`, made by the host: only the host has the token provider and the socket
//     factory). The component opens the microphone, plays the verified speech, and shows everything from the session's own state.
//   * Or pass `state`, `levels`, `transcript` and `reply` yourself and handle `start` and `stop`: the component then owns no audio.
//
// It shows the state in words (never colour alone), the voice orb (it swells with the microphone level while listening, swirls
// while the assistant thinks and pulses with its speech; the `waveform` slot replaces it), the live transcript (which the person
// can correct before anything is confirmed), and the typed reply, always, whether it is also spoken or not. Turn taking, not full duplex: pressing
// talk while the assistant speaks stops the speech first. A spoken "yes" confirms nothing: a prepared action is confirmed with the
// confirmation card on screen, and the component says so. Where voice is not offered (no voice pack, no socket, no microphone) it
// falls back to text: the `fallback` slot is where the host puts its typed input.

const props = withDefaults(
  defineProps<{
    state?: AlixVoicePhase;
    /** Voice levels, 0 to 1, oldest first: the orb follows the newest. With a `session` the component reads them itself. */
    levels?: readonly number[];
    /** The transcript so far, shown as captions. */
    transcript?: string;
    /** The transcript is still the recogniser's guess. */
    interim?: boolean;
    /** The typed reply of the assistant. With a `session` this is the verified response of the last turn. */
    reply?: string;
    session?: voice.VoiceSession;
    microphone?: AlixMicrophone;
    /** `null` plays nothing: replies are only shown. */
    player?: AlixPlayer | null;
    /** The language of the person's speech (BCP 47). */
    language?: string;
    /** Open the connection when the component appears. Default true. */
    autoConnect?: boolean;
    /** The host already knows speech input is not offered here (its own 503 `text_input_only`). */
    unavailable?: boolean;
    /** Let the person correct the transcript. Default true. */
    editable?: boolean;
  }>(),
  { state: "idle", levels: () => [], transcript: "", interim: false, reply: "", autoConnect: true, unavailable: false, editable: true },
);

const handsFree = defineModel<boolean>("handsFree", { default: false });

const emit = defineEmits<{
  start: [];
  stop: [];
  cancel: [];
  end: [];
  restart: [];
  interrupt: [];
  correct: [text: string];
  "recording-change": [recording: boolean];
  transcript: [transcript: { text: string; final: boolean; corrected: boolean }];
  response: [reply: AlixVoiceReply];
  turn: [turn: AlixVoiceTurn];
  fallback: [reason: AlixVoiceFallback];
  error: [error: { code: string; message: string; retryable: boolean }];
}>();

const { t } = useAlix();
const vt = useAlixVoiceText();
const uid = useId();
const root = useTemplateRef<HTMLElement>("root");
const editor = useTemplateRef<InstanceType<typeof AlixTranscriptEditor>>("editor");
const editButton = useTemplateRef<HTMLButtonElement>("editButton");
const reducedQuery = useReducedMotion();
const reducedAttribute = ref(false);

// Discrete events of the controller become events of the component. None of them carries audio or a confirmation secret.
const onEvent = (event: AlixVoiceEvent): void => {
  switch (event.type) {
    case "recording":
      emit("recording-change", event.recording);
      break;
    case "transcript":
      emit("transcript", { text: event.text, final: event.final, corrected: event.corrected });
      break;
    case "response":
      emit("response", { text: event.text, spoken: event.spoken });
      break;
    case "turn":
      emit("turn", event.turn);
      break;
    case "interrupt":
      emit("interrupt");
      break;
    case "correct":
      emit("correct", event.text);
      break;
    case "fallback":
      emit("fallback", event.reason);
      break;
    case "ended":
      break;
    case "error":
      emit("error", { code: event.code, message: event.message, retryable: event.retryable });
      break;
  }
};

const { view, controller } = useAlixVoice(
  () =>
    props.session === undefined
      ? undefined
      : {
          session: props.session,
          autoConnect: props.autoConnect,
          unavailable: props.unavailable,
          handsFree: handsFree.value,
          ...(props.microphone !== undefined && { microphone: props.microphone }),
          ...(props.player !== undefined && { player: props.player }),
          ...(props.language !== undefined && { language: props.language }),
        },
  onEvent,
);

onMounted(() => {
  reducedAttribute.value = root.value?.closest('[data-reduced-motion="true"]') != null;
});
const still = computed(() => reducedQuery.value || reducedAttribute.value);

// ---- what to draw: the controller's view when there is a session, the props otherwise ----
const phase = computed<AlixVoicePhase>(() => (view.value?.phase ?? (props.unavailable ? "unavailable" : props.state)));
const transcriptText = computed(() => view.value?.transcript?.text ?? props.transcript);
const transcriptInterim = computed(() => view.value?.transcript?.interim ?? props.interim);
const transcriptCorrected = computed(() => view.value?.transcript?.corrected ?? false);
const replyText = computed(() => view.value?.reply?.text ?? props.reply);
const replySpoken = computed(() => view.value?.reply?.spoken ?? false);
const fallback = computed<AlixVoiceFallback | undefined>(() => view.value?.fallback ?? (phase.value === "unavailable" ? "text_input_only" : undefined));
const textOnly = computed(() => fallback.value === "text_input_only" || fallback.value === "connect_failed");
const bars = computed<readonly number[]>(() => {
  const source = view.value?.levels ?? props.levels;
  if (still.value) return Array.from({ length: 24 }, () => (phase.value === "listening" || phase.value === "speaking" ? 0.4 : 0.08));
  return source.length > 0 ? source : Array.from({ length: 24 }, () => 0.08);
});
// The orb follows the newest level (already shaped like a bar height by the controller) and the phase, through the mapping both
// kits share.
const orbLevel = computed(() => {
  const source = view.value?.levels ?? props.levels;
  return source.length > 0 ? (source[source.length - 1] ?? 0) : 0;
});
const orbState = computed(() => voiceOrbStateFor(phase.value, fallback.value));
const announcement = computed(() => (transcriptText.value !== "" && !transcriptInterim.value ? vt("voice.heard", { text: transcriptText.value }) : ""));

const buttonState = computed<"idle" | "listening" | "processing" | "unavailable">(() => {
  if (phase.value === "unavailable" || phase.value === "ended") return "unavailable";
  if (view.value?.micOpen === true && handsFree.value) return "listening"; // hands free stays on until the person stops it
  if (phase.value === "listening") return "listening";
  if (phase.value === "processing" || phase.value === "connecting" || phase.value === "reconnecting") return "processing";
  return "idle"; // including "speaking": pressing talk interrupts
});
const buttonMode = computed(() => (handsFree.value ? "toggle" : "push-to-talk"));
const speaking = computed(() => phase.value === "speaking");
const stateWord = computed(() => vt(`voice.state.${phase.value}` as AlixVoiceMessageKey));
const hint = computed(() => vt(handsFree.value ? "voice.keyHint.handsFree" : "voice.keyHint.hold"));
const noticeKey = computed<AlixVoiceMessageKey | undefined>(() => {
  if (view.value?.notice === "renewed") return "voice.renewed";
  if (view.value?.notice === "capture_limit") return "voice.captureLimit";
  return undefined;
});

// ---- actions ----
const start = (): void => {
  emit("start");
  void controller.value?.start();
};
const stop = (): void => {
  emit("stop");
  controller.value?.stop();
};
const cancel = (): void => {
  emit("cancel");
  controller.value?.cancel();
};
const interrupt = (): void => {
  if (controller.value === undefined) emit("interrupt");
  else controller.value.interrupt();
};
const retry = (): void => controller.value?.retry();

watch(handsFree, (on) => controller.value?.setHandsFree(on));

// ---- correcting the transcript ----
const editing = ref(false);
const draft = ref("");
const canEdit = computed(() => props.editable && transcriptText.value !== "" && !transcriptInterim.value && phase.value !== "unavailable");
const edit = async (): Promise<void> => {
  draft.value = transcriptText.value;
  editing.value = true;
  await nextTick();
  editor.value?.focus();
};
const closeEditor = async (): Promise<void> => {
  editing.value = false;
  await nextTick();
  editButton.value?.focus();
};
const correct = (text: string): void => {
  if (controller.value === undefined) emit("correct", text);
  else controller.value.correct(text);
  void closeEditor();
};
// A new transcript (the next utterance, or the core's reading of a correction) replaces whatever was being edited.
watch(transcriptText, () => {
  editing.value = false;
});

const keydown = (event: KeyboardEvent): void => {
  if (event.key === "Escape" && speaking.value) {
    event.preventDefault();
    interrupt();
  }
};
</script>

<template>
  <div class="alix-root alix-d-contents">
    <section
      :aria-label="t('voice.mode.title')"
      :aria-busy="phase === 'processing' || phase === 'connecting'"
      ref="root"
      class="alix-card alix-pa-5 alix-d-flex alix-flex-column alix-align-center alix-ga-4"
      @keydown="keydown"
    >
      <p
        class="alix-heading alix-d-flex alix-align-center alix-ga-2"
        role="status"
      >
        <span
          :data-phase="phase"
          class="alix-voice-dot"
          aria-hidden="true"
        />
        {{ stateWord }}
      </p>

      <p
        v-if="textOnly"
        class="alix-banner alix-w-100 alix-d-flex alix-flex-wrap alix-align-center alix-justify-between alix-ga-2"
        role="status"
      >
        {{ vt("voice.fallback.text") }}
        <button
          v-if="fallback === 'connect_failed'"
          class="alix-btn alix-btn-quiet"
          type="button"
          @click="retry"
        >
          {{ t("common.retry") }}
        </button>
      </p>
      <p
        v-else-if="fallback === 'microphone_unavailable'"
        class="alix-banner alix-tone-warning alix-w-100"
        role="status"
      >
        {{ vt("voice.fallback.microphone") }}
      </p>
      <p
        v-if="phase === 'reconnecting'"
        class="alix-banner alix-tone-warning alix-w-100"
        role="status"
      >
        {{ t("common.reconnecting") }}
      </p>
      <div
        v-if="phase === 'ended'"
        class="alix-banner alix-w-100 alix-d-flex alix-flex-wrap alix-align-center alix-justify-between alix-ga-2"
        role="status"
      >
        {{ vt("voice.sessionEnded") }}
        <button
          class="alix-btn"
          type="button"
          @click="emit('restart')"
        >
          {{ vt("voice.restart") }}
        </button>
      </div>
      <p
        v-if="noticeKey !== undefined"
        class="alix-banner alix-w-100"
        role="status"
      >
        {{ vt(noticeKey) }}
      </p>

      <slot
        :levels="bars"
        name="waveform"
      >
        <AlixVoiceOrb
          :level="orbLevel"
          :reducedMotion="still"
          :size="168"
          :state="orbState"
        />
      </slot>

      <p
        v-if="speaking"
        class="alix-ma-0"
      >
        <button
          class="alix-btn"
          type="button"
          @click="interrupt"
        >
          <AlixIcon
            :size="16"
            name="stop"
          />
          {{ vt("voice.interrupt") }}
        </button>
      </p>

      <div
        v-if="transcriptText !== ''"
        class="alix-w-100 alix-d-flex alix-flex-column alix-align-center alix-ga-2"
      >
        <p
          :class="{ 'alix-text-muted': transcriptInterim }"
          class="alix-ma-0 alix-text-center alix-break-word"
          aria-live="off"
        >
          <slot
            :text="transcriptText"
            name="transcript"
          >
            {{ transcriptText }}
          </slot>
        </p>
        <span
          v-if="transcriptCorrected"
          class="alix-badge alix-tone-accent"
        >
          {{ vt("voice.corrected") }}
        </span>
        <button
          v-if="canEdit && !editing"
          ref="editButton"
          class="alix-btn alix-btn-quiet"
          type="button"
          @click="edit"
        >
          {{ vt("voice.edit") }}
        </button>
      </div>
      <AlixTranscriptEditor
        v-if="editing"
        v-model="draft"
        :corrected="transcriptCorrected"
        :original="transcriptText"
        ref="editor"
        class="alix-w-100"
        @confirm="correct"
        @discard="closeEditor"
      >
        <template #submit>
          {{ vt("voice.sendCorrection") }}
        </template>
      </AlixTranscriptEditor>
      <p
        class="alix-sr-only"
        aria-live="polite"
      >
        {{ announcement }}
      </p>

      <div
        v-if="replyText !== ''"
        class="alix-w-100 alix-d-flex alix-flex-column alix-ga-1"
      >
        <p class="alix-ma-0 alix-text-muted alix-text-xs alix-font-semibold">
          {{ vt("voice.reply") }}
        </p>
        <div
          class="alix-break-word"
          aria-live="polite"
        >
          <slot
            :spoken="replySpoken"
            :text="replyText"
            name="reply"
          >
            {{ replyText }}
          </slot>
        </div>
      </div>
      <p
        v-if="view?.textReplies === true && !textOnly"
        class="alix-ma-0 alix-text-muted alix-text-sm alix-text-center"
      >
        {{ vt("voice.textReplies") }}
      </p>

      <p
        v-if="view?.confirmationRequired === true"
        class="alix-banner alix-tone-warning alix-w-100"
        role="status"
      >
        {{ vt("voice.confirmOnScreen") }}
      </p>
      <p
        v-if="view?.supersededPreparationId !== undefined"
        class="alix-banner alix-w-100"
        role="status"
      >
        {{ vt("voice.superseded") }}
      </p>

      <slot
        :reason="fallback"
        name="fallback"
      />

      <slot name="controls">
        <div class="alix-d-flex alix-flex-wrap alix-justify-center alix-align-center alix-ga-4">
          <label class="alix-d-flex alix-align-center alix-ga-2">
            <input
              v-model="handsFree"
              :disabled="textOnly || phase === 'ended'"
              class="no-validation"
              type="checkbox"
            >
            {{ t("voice.handsFree") }}
          </label>
          <AlixVoiceButton
            :aria-describedby="`${uid}-hint`"
            :mode="buttonMode"
            :state="buttonState"
            size="lg"
            @cancel="cancel"
            @start="start"
            @stop="stop"
          />
          <button
            class="alix-btn"
            type="button"
            @click="emit('end')"
          >
            {{ t("voice.end") }}
          </button>
        </div>
        <p
          v-if="!textOnly && phase !== 'ended'"
          :id="`${uid}-hint`"
          class="alix-ma-0 alix-text-muted alix-text-xs alix-text-center"
        >
          {{ hint }}
        </p>
      </slot>
    </section>
  </div>
</template>

<style scoped lang="scss">
// A dot beside the state word. It supports the word and never replaces it.
.alix-voice-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--alix-border-strong);

  &[data-phase="listening"] {
    background: var(--alix-accent);
  }

  &[data-phase="speaking"] {
    background: var(--alix-success);
  }

  &[data-phase="processing"],
  &[data-phase="connecting"],
  &[data-phase="reconnecting"] {
    background: var(--alix-warning);
  }

  &[data-phase="unavailable"],
  &[data-phase="ended"] {
    background: var(--alix-danger);
  }
}
</style>
