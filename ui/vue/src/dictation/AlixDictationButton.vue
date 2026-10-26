<script setup lang="ts">
import { computed } from "vue";
import type { voice } from "@alixai/sdk";
import AlixIcon from "../components/common/AlixIcon.vue";
import { useAlix } from "../i18n";
import { useAlixDictation } from "./useAlixDictation";

// Record-to-text control for the composer. Emits `transcript` with recognised text and never sends a turn.

const props = withDefaults(
  defineProps<{
    sessionId: string;
    client: voice.DictationClient;
    language?: string;
    size?: "md" | "lg";
  }>(),
  { language: undefined, size: "md" },
);

const emit = defineEmits<{
  transcript: [text: string];
}>();

const { t } = useAlix();

const dictation = useAlixDictation({
  sessionId: () => props.sessionId,
  client: props.client,
  language: () => props.language,
  onTranscript: (text) => emit("transcript", text),
});

const recording = computed(() => dictation.phase.value === "recording");
const busy = computed(() => dictation.phase.value === "requesting" || dictation.phase.value === "transcribing");
const label = computed(() => {
  if (dictation.phase.value === "requesting") return t("dictation.requesting");
  if (dictation.phase.value === "transcribing") return t("dictation.transcribing");
  if (recording.value) return t("dictation.stop");
  return t("dictation.start");
});
const live = computed(() => {
  if (recording.value) return t("dictation.recording", { seconds: dictation.elapsedSeconds.value });
  if (dictation.phase.value === "transcribing") return t("dictation.transcribing");
  if (dictation.phase.value === "error") return dictation.errorMessage.value;
  return "";
});

const toggle = (): void => {
  if (busy.value) return;
  if (recording.value) void dictation.stop();
  else void dictation.start();
};

const onKeydown = (event: KeyboardEvent): void => {
  if (event.key === "Escape" && (recording.value || busy.value)) {
    event.preventDefault();
    dictation.cancel();
  }
};
</script>

<template>
  <span
    v-if="dictation.known.value && dictation.available.value"
    class="alix-d-inline-flex alix-align-center alix-ga-2 alix-flex-wrap"
  >
    <button
      :aria-label="label"
      :aria-pressed="recording"
      :aria-busy="busy"
      :aria-disabled="busy"
      :data-size="size"
      :data-state="dictation.phase.value"
      class="alix-root alix-dictation-btn alix-flex-shrink-0"
      type="button"
      @click="toggle"
      @keydown="onKeydown"
    >
      <span
        v-if="busy"
        class="alix-spinner"
        aria-hidden="true"
      />
      <AlixIcon
        v-else
        :name="recording ? 'stop' : 'mic'"
        :size="size === 'lg' ? 28 : 20"
      />
      <span class="alix-sr-only">
        {{ label }}
      </span>
    </button>
    <span
      v-if="recording"
      class="alix-dictation-elapsed alix-text-muted alix-text-sm"
      aria-hidden="true"
    >
      {{ dictation.elapsedSeconds.value }}s
    </span>
    <span
      class="alix-sr-only"
      aria-live="polite"
    >
      {{ live }}
    </span>
    <p
      v-if="dictation.phase.value === 'error' && dictation.errorMessage.value"
      class="alix-banner alix-tone-warning alix-ma-0 alix-dictation-error"
      role="alert"
    >
      {{ dictation.errorMessage.value }}
    </p>
  </span>
</template>

<style scoped lang="scss">
@use "../styles/mixins";

.alix-dictation-btn {
  @include mixins.pressable;
  @include mixins.focus-ring;

  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  padding: 0;
  border: 1px solid var(--alix-border-strong);
  border-radius: 50%;
  background: var(--alix-surface);
  color: var(--alix-text);
  cursor: pointer;

  &[data-size="lg"] {
    width: 64px;
    height: 64px;
  }

  @include mixins.hover {
    background: var(--alix-surface-2);
  }

  &[data-state="recording"] {
    border-color: var(--alix-accent);
    background: var(--alix-accent);
    color: var(--alix-accent-contrast);
  }

  &[aria-busy="true"] {
    opacity: 0.7;
    cursor: wait;
  }

  @media (prefers-reduced-motion: reduce) {
    transition: none;
  }
}

.alix-dictation-error {
  flex-basis: 100%;
}
</style>
