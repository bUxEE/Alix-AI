<script setup lang="ts">
import { computed, ref, useId } from "vue";
import type { PendingAttachment } from "../composables/useAttachmentQueue";
import { MAX_ATTACHMENTS } from "../composables/useAlixChat";
import { useAlix } from "../i18n";
import AlixIcon from "./common/AlixIcon.vue";

// The message box: text, pending files, send and stop. Data in, events out, the same contract as the React kit's composer: the
// host owns the files (`files`) and uploads them (`add-files` carries the Files the person chose, within the size limit;
// `remove-file` carries a file id). Enter sends, Shift+Enter starts a new line, and Enter while an input method is composing
// never sends. A message with files and no text is refused by the core, so Send waits for text.

const props = withDefaults(
  defineProps<{
    /** Files already chosen and waiting to go with the next message. */
    files?: readonly PendingAttachment[];
    /** True while a run is in flight: Send becomes Stop. */
    running?: boolean;
    disabled?: boolean;
    placeholder?: string;
    maxFiles?: number;
    /** The attachment size limit of the effective settings (`attachments.max_file_mb`). A convenience check: the core enforces it. */
    maxFileMb?: number;
    /** The `accept` list of the file picker, for example `image/*,.pdf`. */
    accept?: string;
    /** Shows the attach button. Turn it off when the effective settings do not allow uploads. */
    attachable?: boolean;
  }>(),
  { files: () => [], running: false, disabled: false, placeholder: undefined, maxFiles: MAX_ATTACHMENTS, maxFileMb: undefined, accept: undefined, attachable: true },
);

const text = defineModel<string>({ default: "" });

const emit = defineEmits<{
  send: [text: string];
  stop: [];
  "add-files": [files: File[]];
  "remove-file": [fileId: string];
}>();

const { t, formatSize } = useAlix();
const uid = useId();
const field = ref<HTMLTextAreaElement | null>(null);
const picker = ref<HTMLInputElement | null>(null);
const notice = ref("");
const dragging = ref(false);

const rows = computed(() => Math.min(8, Math.max(1, text.value.split("\n").length)));
const blocked = computed(() => props.files.some((file) => file.status !== "ready"));
const canSend = computed(() => !props.disabled && !props.running && text.value.trim() !== "" && !blocked.value);
const full = computed(() => props.files.length >= props.maxFiles);

const submit = (): void => {
  if (!canSend.value) return;
  emit("send", text.value.trim());
  text.value = "";
  notice.value = "";
};

const onKeydown = (event: KeyboardEvent): void => {
  if (event.key !== "Enter" || event.shiftKey || event.isComposing) return;
  event.preventDefault();
  submit();
};

const attach = (incoming: File[]): void => {
  if (!props.attachable || props.disabled || incoming.length === 0) return;
  notice.value = "";
  const accepted: File[] = [];
  for (const file of incoming) {
    if (props.files.length + accepted.length >= props.maxFiles) {
      notice.value = t("composer.tooMany", { max: props.maxFiles });
      break;
    }
    if (props.maxFileMb !== undefined && file.size > props.maxFileMb * 1024 * 1024) {
      notice.value = t("composer.tooLarge", { name: file.name, max: props.maxFileMb });
      continue;
    }
    accepted.push(file);
  }
  if (accepted.length > 0) emit("add-files", accepted);
};

const onPick = (event: Event): void => {
  const input = event.target as HTMLInputElement;
  attach(Array.from(input.files ?? []));
  input.value = "";
};

const onDrop = (event: DragEvent): void => {
  dragging.value = false;
  attach(Array.from(event.dataTransfer?.files ?? []));
};

const onPaste = (event: ClipboardEvent): void => {
  const pasted = Array.from(event.clipboardData?.files ?? []);
  if (pasted.length > 0 && props.attachable) {
    event.preventDefault();
    attach(pasted);
  }
};

defineExpose({ focus: (): void => field.value?.focus() });
</script>

<template>
  <form
    :aria-label="t('chat.composerLabel')"
    :aria-busy="running"
    class="alix-root alix-d-flex alix-flex-column alix-ga-2"
    @dragenter.prevent="dragging = attachable"
    @dragover.prevent
    @dragleave.prevent="dragging = false"
    @drop.prevent="onDrop"
    @submit.prevent="submit"
  >
    <ul
      v-if="files.length > 0"
      :aria-label="t('composer.files')"
      class="alix-reset-list alix-d-flex alix-flex-wrap alix-ga-2"
    >
      <li
        v-for="file in files"
        :key="file.fileId"
      >
        <slot
          :file="file"
          :remove="() => emit('remove-file', file.fileId)"
          name="attachment"
        >
          <span
            :class="file.status === 'error' ? 'alix-tone-danger' : ''"
            class="alix-badge alix-pa-2"
          >
            <span
              v-if="file.status === 'uploading'"
              class="alix-spinner"
              aria-hidden="true"
            />
            <AlixIcon
              v-else
              :name="file.status === 'error' ? 'alert' : 'file'"
              :size="16"
            />
            <span class="alix-text-truncate">
              {{ file.name }}
            </span>
            <span class="alix-text-muted">
              {{ file.status === "uploading" ? t("composer.uploading") : file.status === "error" ? t("composer.uploadFailed") : formatSize(file.sizeBytes) }}
            </span>
            <button
              :aria-label="t('composer.removeFile', { name: file.name })"
              class="alix-btn alix-btn-quiet alix-btn-icon"
              type="button"
              @click="emit('remove-file', file.fileId)"
            >
              <AlixIcon
                :size="14"
                name="x"
              />
            </button>
          </span>
        </slot>
      </li>
    </ul>
    <p
      v-if="notice"
      class="alix-banner alix-tone-warning alix-ma-0"
      role="alert"
    >
      {{ notice }}
    </p>
    <div
      :class="dragging ? 'alix-drop-active' : ''"
      class="alix-d-flex alix-align-end alix-ga-2 alix-position-relative"
    >
      <button
        v-if="attachable"
        :disabled="disabled || full"
        :aria-label="t('chat.attach')"
        class="alix-btn alix-btn-quiet alix-btn-icon alix-flex-shrink-0"
        type="button"
        @click="picker?.click()"
      >
        <AlixIcon name="paperclip" />
      </button>
      <input
        v-if="attachable"
        :accept="accept"
        ref="picker"
        class="alix-sr-only no-validation"
        type="file"
        tabindex="-1"
        aria-hidden="true"
        multiple
        @change="onPick"
      >
      <textarea
        v-model="text"
        :disabled="disabled"
        :placeholder="placeholder ?? t('chat.placeholder')"
        :rows="rows"
        :aria-label="t('chat.composerLabel')"
        :aria-describedby="`${uid}-hint`"
        ref="field"
        class="alix-field no-validation alix-flex-grow-1 alix-min-w-0"
        enterkeyhint="send"
        @keydown="onKeydown"
        @paste="onPaste"
      />
      <slot name="actions" />
      <button
        v-if="running"
        :aria-label="t('common.stop')"
        class="alix-btn alix-btn-primary alix-btn-icon alix-flex-shrink-0"
        type="button"
        @click="emit('stop')"
      >
        <AlixIcon name="stop" />
      </button>
      <button
        v-else
        :disabled="!canSend"
        :aria-label="t('common.send')"
        class="alix-btn alix-btn-primary alix-btn-icon alix-flex-shrink-0"
        type="submit"
      >
        <AlixIcon name="send" />
      </button>
    </div>
    <p
      :id="`${uid}-hint`"
      class="alix-text-muted alix-text-xs alix-ma-0"
    >
      <slot name="hint">
        {{ dragging ? t("composer.dropHere") : t("composer.hint") }}
      </slot>
    </p>
  </form>
</template>

<style scoped lang="scss">
// While files are dragged over the composer the field gets an accent outline; nothing moves.
.alix-drop-active :deep(.alix-field) {
  border-color: var(--alix-accent);
  outline: 2px dashed var(--alix-accent);
  outline-offset: 2px;
}
</style>
