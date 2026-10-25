<script setup lang="ts">
import { AlixFilePreview, type AlixAttachment } from "@alixai/vue";
import { onBeforeUnmount, reactive, watch } from "vue";
import { fetchDemoPreview } from "../../../demo-mock/src/index";

// The host's drawing of files in the conversation (the chat panel's `attachments` slot): each image is read through the files
// API's safe preview (`GET /v1/files/{id}/preview`, a blob: URL) and handed to AlixFilePreview as `src`; a text file gets its
// text preview; anything else stays a file card. The blob: URLs are released when the entry goes away.

const props = defineProps<{
  files: readonly AlixAttachment[];
}>();

const emit = defineEmits<{
  open: [file: AlixAttachment];
  download: [file: AlixAttachment];
}>();

interface Preview {
  loading: boolean;
  src?: string;
  text?: string;
  truncated?: boolean;
}

const previews = reactive<Record<string, Preview>>({});
const controller = new AbortController();

const read = async (file: AlixAttachment): Promise<void> => {
  if (!file.previewable || previews[file.fileId] !== undefined) return;
  previews[file.fileId] = { loading: true };
  try {
    const preview = await fetchDemoPreview(file.fileId, "", controller.signal);
    if (preview.kind === "image") previews[file.fileId] = { loading: false, src: preview.url };
    else if (preview.kind === "text") previews[file.fileId] = { loading: false, text: preview.text, truncated: preview.truncated };
    else previews[file.fileId] = { loading: false };
  } catch {
    previews[file.fileId] = { loading: false };
  }
};

watch(
  () => props.files,
  (files) => {
    for (const file of files) void read(file);
  },
  { immediate: true },
);

onBeforeUnmount(() => {
  controller.abort();
  for (const preview of Object.values(previews)) if (preview.src !== undefined) URL.revokeObjectURL(preview.src);
});
</script>

<template>
  <div class="alix-d-flex alix-flex-wrap alix-justify-end alix-ga-2">
    <AlixFilePreview
      v-for="file in files"
      :key="file.fileId"
      :file="file"
      :src="previews[file.fileId]?.src"
      :text="previews[file.fileId]?.text"
      :truncated="previews[file.fileId]?.truncated"
      :loading="previews[file.fileId]?.loading"
      class="alix-demo-attachment"
      @download="emit('download', $event)"
      @open="emit('open', $event)"
    />
  </div>
</template>
