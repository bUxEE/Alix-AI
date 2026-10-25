<script setup lang="ts">
import { computed, useId, useTemplateRef } from "vue";
import { useAlix } from "../i18n";
import { useAlixVoiceText } from "./messages";

// What the microphone heard, shown as editable text BEFORE anything is confirmed. Speech recognition is a proposal like any other
// model output: the person corrects it here, and only `confirm` (with the corrected text) sends it. `discard` drops it.
// The editor changes no state of its own. In Voice Mode, `confirm` becomes a correction of the transcript (a new turn that
// replaces the earlier one); standalone, the host decides what to do with the text.
//
// Keyboard: Ctrl or Cmd + Enter sends, Escape discards. `original` is what the recogniser heard: when it differs from the text,
// "Back to what I heard" restores it.

const props = defineProps<{
  disabled?: boolean;
  /** What the recogniser heard. Shown as the way back when the text has been edited. */
  original?: string;
  /** The text is already a correction, not the recogniser's own words. */
  corrected?: boolean;
}>();

const text = defineModel<string>({ default: "" });

const emit = defineEmits<{
  confirm: [text: string];
  discard: [];
}>();

const { t } = useAlix();
const vt = useAlixVoiceText();
const uid = useId();
const field = useTemplateRef<HTMLTextAreaElement>("field");
const rows = computed(() => Math.min(8, Math.max(2, text.value.split("\n").length)));
const edited = computed(() => props.original !== undefined && text.value !== props.original);

const confirm = (): void => {
  const corrected = text.value.trim();
  if (corrected !== "" && props.disabled !== true) emit("confirm", corrected);
};
const reset = (): void => {
  if (props.original !== undefined) text.value = props.original;
  field.value?.focus();
};
const keydown = (event: KeyboardEvent): void => {
  if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
    event.preventDefault();
    confirm();
  } else if (event.key === "Escape") {
    event.preventDefault();
    emit("discard");
  }
};

defineExpose({ focus: () => field.value?.focus() });
</script>

<template>
  <div class="alix-root alix-d-contents">
    <form
      class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3"
      @submit.prevent="confirm"
    >
      <label
        :for="uid"
        class="alix-d-flex alix-flex-column alix-ga-1"
      >
        <span class="alix-d-flex alix-align-center alix-ga-2">
          <span class="alix-heading">
            <slot name="label">
              {{ t("voice.transcript") }}
            </slot>
          </span>
          <span
            v-if="corrected"
            class="alix-badge alix-tone-accent"
          >
            {{ vt("voice.corrected") }}
          </span>
        </span>
        <span class="alix-text-muted alix-text-sm">
          <slot name="hint">
            {{ t("voice.transcriptHint") }}
          </slot>
        </span>
      </label>
      <textarea
        v-model="text"
        :id="uid"
        :disabled="disabled"
        :rows="rows"
        ref="field"
        class="alix-field no-validation"
        @keydown="keydown"
      />
      <div class="alix-d-flex alix-flex-wrap alix-align-center alix-ga-2">
        <button
          v-if="edited"
          :disabled="disabled"
          class="alix-btn alix-btn-quiet"
          type="button"
          @click="reset"
        >
          {{ vt("voice.reset") }}
        </button>
        <span class="alix-ms-auto alix-d-flex alix-flex-wrap alix-justify-end alix-ga-2">
          <button
            :disabled="disabled"
            class="alix-btn"
            type="button"
            @click="emit('discard')"
          >
            {{ t("common.discard") }}
          </button>
          <button
            :disabled="disabled || text.trim() === ''"
            class="alix-btn alix-btn-primary"
            type="submit"
          >
            <slot name="submit">
              {{ t("common.send") }}
            </slot>
          </button>
        </span>
      </div>
    </form>
  </div>
</template>
