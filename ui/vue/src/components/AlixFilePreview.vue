<script setup lang="ts">
import { computed } from "vue";
import type { AlixAttachment } from "../agui/types";
import { useAlix } from "../i18n";
import AlixIcon from "./common/AlixIcon.vue";

// A file the conversation refers to. The kit never fetches the bytes: the host reads the preview from the files service
// (`files.preview`, which returns a safe rendering) and hands it in as `src` (a raster image address, usually a blob: URL)
// or `text` (a text preview, cut to the deployment's limit). An address is accepted only if it cannot run anything: blob:,
// https:, or a raster data: URL. SVG, HTML and every other scheme are refused and the file shows as a plain card.

const props = defineProps<{
  file: AlixAttachment;
  /** A raster image to show for the file. */
  src?: string;
  /** A text preview of the file. */
  text?: string;
  /** True when the text preview was cut to its limit. */
  truncated?: boolean;
  /** True while the host is reading the preview. */
  loading?: boolean;
  /** Shows a remove button; reports `remove` with the file id. */
  removable?: boolean;
}>();

defineEmits<{
  open: [file: AlixAttachment];
  download: [file: AlixAttachment];
  remove: [fileId: string];
}>();

const { t, formatSize, formatDateTime } = useAlix();

const SAFE_IMAGE = /^(blob:|https:\/\/|data:image\/(png|jpe?g|gif|webp|avif);base64,)/i;
const safeSrc = computed(() => (props.src !== undefined && SAFE_IMAGE.test(props.src) ? props.src : undefined));
const isImage = computed(() => /^image\/(png|jpe?g|gif|webp|avif)$/i.test(props.file.mimeType));
</script>

<template>
  <figure class="alix-root alix-card alix-ma-0 alix-overflow-hidden alix-d-flex alix-flex-column">
    <slot
      :file="file"
      name="preview"
    >
      <img
        v-if="safeSrc && isImage"
        :src="safeSrc"
        :alt="t('file.preview', { name: file.name })"
        class="alix-w-100"
        loading="lazy"
        decoding="async"
      >
      <pre
        v-else-if="text !== undefined"
        class="alix-ma-0 alix-pa-3 alix-overflow-auto alix-font-mono alix-text-sm"
        tabindex="0"
      ><code>{{ text }}</code></pre>
      <div
        v-else
        class="alix-d-flex alix-align-center alix-justify-center alix-pa-6 alix-text-muted"
      >
        <span
          v-if="loading"
          :aria-label="t('common.loading')"
          class="alix-spinner"
          role="status"
        />
        <AlixIcon
          v-else
          :size="32"
          name="file"
        />
      </div>
    </slot>
    <figcaption class="alix-pa-3 alix-d-flex alix-flex-wrap alix-align-center alix-justify-between alix-ga-2">
      <div class="alix-min-w-0">
        <p class="alix-ma-0 alix-font-medium alix-text-truncate">
          {{ file.name }}
        </p>
        <p class="alix-ma-0 alix-text-muted alix-text-xs">
          {{ formatSize(file.sizeBytes) }}
          <template v-if="file.expiresAt">
            &middot; {{ t("file.expires", { time: formatDateTime(file.expiresAt) }) }}
          </template>
          <template v-if="truncated">
            &middot; {{ t("file.truncated") }}
          </template>
        </p>
      </div>
      <slot
        :file="file"
        name="actions"
      >
        <div class="alix-d-flex alix-ga-1">
          <button
            v-if="file.previewable"
            :aria-label="t('file.open', { name: file.name })"
            class="alix-btn alix-btn-quiet alix-btn-icon"
            type="button"
            @click="$emit('open', file)"
          >
            <AlixIcon name="external" />
          </button>
          <button
            v-if="removable"
            :aria-label="t('composer.removeFile', { name: file.name })"
            class="alix-btn alix-btn-quiet alix-btn-icon"
            type="button"
            @click="$emit('remove', file.fileId)"
          >
            <AlixIcon name="x" />
          </button>
          <button
            :aria-label="t('file.download', { name: file.name })"
            class="alix-btn alix-btn-quiet alix-btn-icon"
            type="button"
            @click="$emit('download', file)"
          >
            <AlixIcon name="arrow-down" />
          </button>
        </div>
      </slot>
    </figcaption>
  </figure>
</template>
