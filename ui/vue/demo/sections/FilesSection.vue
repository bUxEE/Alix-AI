<script setup lang="ts">
import {
  AlixAttachmentComposer,
  AlixFilePreview,
  AlixLoadable,
  AlixSkeleton,
  useAttachmentQueue,
  type AlixAttachment,
  type OutgoingAttachment,
} from "@alixai/vue";
import { onBeforeUnmount, onMounted, reactive, ref, useTemplateRef, watch } from "vue";
import { demoUploader, fetchDemoPreview } from "../../../demo-mock/src/index";
import DemoIcon from "../components/DemoIcon.vue";
import DemoSection from "../components/DemoSection.vue";
import { useDemoResource } from "../composables/useDemoResource";
import { downloadDemoFile, fetchDemoContent, listDemoFiles } from "../files";
import { useDemoText } from "../state";

// Uploads and the files service. On the left, the attachment composer with the host's uploader (`POST /v1/files`): what a message
// would carry is shown below it (file ids only, never bytes). Three buttons show the refusals: the composer's own size check, and
// the files service refusing a file that is too large and an SVG. On the right, every file the mock holds, and a preview of the
// one picked: an image through the safe preview route, a CSV as text, a PDF from its content (the browser draws it).

const text = useDemoText();
const MAX_MB = 8;

// ---- the composer ----
interface Refusal {
  readonly id: number;
  readonly name: string;
  readonly reason: string;
}

const refusals = ref<Refusal[]>([]);
let refusalId = 0;
const upload = demoUploader();
const uploader = async (file: File, signal: AbortSignal): Promise<{ fileId: string }> => {
  try {
    const result = await upload(file, signal);
    void library.refresh();
    return result;
  } catch (error) {
    refusalId += 1;
    refusals.value = [{ id: refusalId, name: file.name, reason: error instanceof Error ? error.message : String(error) }, ...refusals.value].slice(0, 4);
    throw error;
  }
};

const queue = useAttachmentQueue({ uploader });
const draft = ref("");
const sent = ref<{ text: string; attachments: OutgoingAttachment[] }>();
const composer = useTemplateRef<HTMLElement>("composerHost");

const send = (message: string): void => {
  sent.value = { text: message, attachments: queue.outgoing.value };
  queue.clear();
};

const sized = (name: string, type: string, megabytes: number): File => new File([new Uint8Array(Math.round(megabytes * 1024 * 1024))], name, { type });

// The composer checks the size before anything is uploaded: drop a 9 MB file on it the way a reader would.
const dropTooLarge = (): void => {
  const form = composer.value?.querySelector("form");
  if (form === null || form === undefined) return;
  const transfer = new DataTransfer();
  transfer.items.add(sized("site-survey-scan.pdf", "application/pdf", 9));
  form.dispatchEvent(new DragEvent("drop", { dataTransfer: transfer, bubbles: true, cancelable: true }));
};

// Past the composer's check, straight to the files service: it refuses the size, and it refuses SVG whatever its size.
const uploadTooLarge = (): void => queue.add([sized("drone-footage-roof.png", "image/png", 9)]);
const uploadSvg = (): void =>
  queue.add([new File(['<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>'], "northwind-logo.svg", { type: "image/svg+xml" })]);

// ---- the library ----
const library = useDemoResource((signal) => listDemoFiles(signal));
onMounted(() => void library.refresh());

interface Preview {
  loading: boolean;
  src?: string;
  text?: string;
  truncated?: boolean;
  pdf?: string;
  failed?: boolean;
}

const picked = ref<AlixAttachment>();
const preview = reactive<Preview>({ loading: false });
let previewAbort: AbortController | undefined;

const releasePreview = (): void => {
  previewAbort?.abort();
  if (preview.src !== undefined) URL.revokeObjectURL(preview.src);
  if (preview.pdf !== undefined) URL.revokeObjectURL(preview.pdf);
  Object.assign(preview, { loading: false, src: undefined, text: undefined, truncated: undefined, pdf: undefined, failed: undefined });
};

watch(picked, async (file) => {
  releasePreview();
  if (file === undefined) return;
  const own = new AbortController();
  previewAbort = own;
  preview.loading = true;
  try {
    if (file.mimeType === "application/pdf") {
      const url = await fetchDemoContent(file.fileId, own.signal);
      if (own.signal.aborted) URL.revokeObjectURL(url);
      else preview.pdf = url;
    } else {
      const result = await fetchDemoPreview(file.fileId, "", own.signal);
      if (own.signal.aborted) return;
      if (result.kind === "image") preview.src = result.url;
      else if (result.kind === "text") Object.assign(preview, { text: result.text, truncated: result.truncated });
      else preview.failed = true;
    }
  } catch {
    if (!own.signal.aborted) preview.failed = true;
  } finally {
    if (!own.signal.aborted) preview.loading = false;
  }
});

onBeforeUnmount(releasePreview);

const isPdf = (file: AlixAttachment): boolean => file.mimeType === "application/pdf";
</script>

<template>
  <DemoSection>
    <template #eyebrow>
      AlixAttachmentComposer · useAttachmentQueue · AlixFilePreview
    </template>
    <template #title>
      {{ text("nav.files.title") }}
    </template>
    <template #lede>
      {{ text("files.lede", { max: MAX_MB }) }}
    </template>
    <div class="alix-row">
      <div class="alix-col-12 alix-col-lg-5 alix-d-flex alix-flex-column alix-ga-4 alix-sticky-lg-top">
        <section
          ref="composerHost"
          class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-4"
        >
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ text("files.compose") }}
          </h2>
          <AlixAttachmentComposer
            v-model="draft"
            :files="queue.files.value"
            :max-file-mb="MAX_MB"
            :placeholder="text('files.placeholder')"
            accept="image/png,image/jpeg,image/webp,application/pdf,text/csv,text/plain"
            @add-files="queue.add($event)"
            @remove-file="queue.remove($event)"
            @send="send"
          />
          <div class="alix-d-flex alix-flex-column alix-ga-2">
            <p class="alix-ma-0 alix-eyebrow">
              {{ text("files.refusals") }}
            </p>
            <div class="alix-d-flex alix-flex-wrap alix-ga-2">
              <button
                class="alix-btn"
                type="button"
                @click="dropTooLarge"
              >
                {{ text("files.refuse.client") }}
              </button>
              <button
                class="alix-btn"
                type="button"
                @click="uploadTooLarge"
              >
                {{ text("files.refuse.size") }}
              </button>
              <button
                class="alix-btn"
                type="button"
                @click="uploadSvg"
              >
                {{ text("files.refuse.svg") }}
              </button>
            </div>
            <TransitionGroup
              class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2"
              name="alix-demo-list"
              tag="ul"
            >
              <li
                v-for="refusal in refusals"
                :key="refusal.id"
                class="alix-banner alix-tone-danger alix-text-sm alix-d-flex alix-flex-column alix-ga-1"
              >
                <span class="alix-font-medium alix-break-word">
                  {{ text("files.refused", { name: refusal.name }) }}
                </span>
                <span class="alix-break-word">
                  {{ refusal.reason }}
                </span>
              </li>
            </TransitionGroup>
          </div>
        </section>
        <section class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ text("files.payload") }}
          </h2>
          <pre
            v-if="sent"
            class="alix-ma-0 alix-pa-3 alix-font-mono alix-text-xs alix-overflow-auto alix-demo-code"
            tabindex="0"
          ><code>{{ JSON.stringify({ role: "user", content: [{ type: "text", text: sent.text }, ...sent.attachments.map((file) => ({ type: file.kind, source: { type: "file", value: file.fileId } }))] }, null, 2) }}</code></pre>
          <p
            v-else
            class="alix-ma-0 alix-text-sm alix-text-muted"
          >
            {{ text("files.payload.empty") }}
          </p>
        </section>
      </div>
      <div class="alix-col-12 alix-col-lg-7 alix-flex-1 alix-d-flex alix-flex-column alix-ga-4 alix-sticky-lg-top">
        <section class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
          <div class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
            <h2 class="alix-ma-0 alix-demo-subtitle">
              {{ text("files.library") }}
            </h2>
            <button
              :disabled="library.state.isLoading.value"
              :aria-label="text('common.refresh')"
              class="alix-btn alix-btn-quiet alix-btn-icon"
              type="button"
              @click="library.refresh()"
            >
              <DemoIcon
                :size="18"
                name="refresh"
              />
            </button>
          </div>
          <AlixLoadable
            :loading="library.state.isLoading.value"
            :loaded="library.state.hasLoadedOnce.value"
          >
            <template #skeleton>
              <ul class="alix-reset-list alix-d-flex alix-flex-column alix-divided">
                <li
                  v-for="row in 5"
                  :key="row"
                  class="alix-d-flex alix-align-center alix-ga-3 alix-py-3"
                >
                  <AlixSkeleton
                    width="36px"
                    height="36px"
                  />
                  <span class="alix-d-flex alix-flex-column alix-ga-2 alix-flex-grow-1">
                    <AlixSkeleton width="55%" />
                    <AlixSkeleton
                      width="25%"
                      height="10px"
                    />
                  </span>
                </li>
              </ul>
            </template>
            <p
              v-if="library.error.value"
              class="alix-banner alix-tone-danger alix-ma-0"
              role="alert"
            >
              {{ library.error.value }}
            </p>
            <ul
              v-else
              class="alix-reset-list alix-d-flex alix-flex-column alix-divided"
            >
              <li
                v-for="file in library.data.value ?? []"
                :key="file.fileId"
              >
                <button
                  :aria-pressed="picked?.fileId === file.fileId"
                  class="alix-d-flex alix-align-center alix-ga-3 alix-w-100 alix-py-3 alix-demo-file-row"
                  type="button"
                  @click="picked = file"
                >
                  <span
                    :data-kind="isPdf(file) ? 'pdf' : file.mimeType.split('/')[0]"
                    class="alix-font-mono alix-text-xs alix-flex-shrink-0 alix-demo-file-kind"
                  >
                    {{ isPdf(file) ? "PDF" : file.mimeType.split("/")[1]?.toUpperCase() }}
                  </span>
                  <span class="alix-d-flex alix-flex-column alix-min-w-0 alix-flex-grow-1">
                    <span class="alix-font-medium alix-text-truncate">
                      {{ file.name }}
                    </span>
                    <span class="alix-text-xs alix-text-muted">
                      {{ file.previewable ? text("files.previewable") : isPdf(file) ? text("files.pdf") : text("files.noPreview") }}
                    </span>
                  </span>
                  <DemoIcon
                    :size="16"
                    class="alix-flex-shrink-0 alix-text-muted"
                    name="eye"
                  />
                </button>
              </li>
            </ul>
          </AlixLoadable>
        </section>
        <section class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ text("files.preview") }}
          </h2>
          <AlixFilePreview
            v-if="picked"
            :file="picked"
            :src="preview.src"
            :text="preview.text"
            :truncated="preview.truncated"
            :loading="preview.loading"
            @download="downloadDemoFile($event)"
          >
            <template
              v-if="preview.pdf"
              #preview
            >
              <object
                :data="preview.pdf"
                :aria-label="picked.name"
                class="alix-w-100 alix-demo-pdf"
                type="application/pdf"
              >
                <p class="alix-pa-4 alix-ma-0 alix-text-muted">
                  {{ text("files.pdf.fallback") }}
                </p>
              </object>
            </template>
          </AlixFilePreview>
          <p
            v-else
            class="alix-ma-0 alix-text-sm alix-text-muted"
          >
            {{ text("files.preview.empty") }}
          </p>
          <p
            v-if="preview.failed"
            class="alix-ma-0 alix-text-sm alix-text-muted"
          >
            {{ text("files.preview.none") }}
          </p>
        </section>
      </div>
    </div>
  </DemoSection>
</template>
