<script setup lang="ts">
import { computed, ref } from "vue";
import { useAlix } from "../../i18n";
import { runExport, saveExportBlob } from "../../export";
import type { AlixDownload, AlixExporter } from "../../export";
import type { DraftBlock } from "../../sdk-types";
import AlixMarkdown from "./AlixMarkdown.vue";
import AlixSourceLabel from "./AlixSourceLabel.vue";

const props = defineProps<{
  block: DraftBlock;
  sessionId?: string;
  turnId?: string;
  blockId?: string;
  exporter?: AlixExporter;
  download?: AlixDownload;
}>();

const { t } = useAlix();
const busy = ref(false);
const copied = ref(false);

const canHostExport = computed(() => props.exporter !== undefined && (props.sessionId ?? "") !== "");

const copy = async (): Promise<void> => {
  try {
    await navigator.clipboard.writeText(props.block.body);
    copied.value = true;
    window.setTimeout(() => {
      copied.value = false;
    }, 1500);
  } catch {
    /* host may deny clipboard */
  }
};

const downloadLocal = (): void => {
  const markdown = props.block.format === "markdown";
  saveExportBlob(
    new Blob([props.block.body], { type: markdown ? "text/markdown" : "text/plain" }),
    markdown ? "md" : "html",
    props.block.title || "draft",
  );
};

const downloadHost = async (): Promise<void> => {
  if (!canHostExport.value || props.exporter === undefined) return;
  busy.value = true;
  try {
    await runExport(
      props.exporter,
      {
        sessionId: props.sessionId!,
        turnId: props.turnId ?? props.blockId ?? "draft",
        blockIds: props.blockId !== undefined ? [props.blockId] : undefined,
        format: props.block.format === "markdown" ? "md" : "html",
      },
      props.download,
    );
  } finally {
    busy.value = false;
  }
};
</script>

<template>
  <slot :block="block">
    <div
      class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3"
      data-alixai="draft"
    >
      <div class="alix-d-flex alix-justify-space-between alix-align-center alix-ga-2 alix-flex-wrap">
        <h3 class="alix-heading alix-ma-0 alix-text-md">
          {{ block.title }}
        </h3>
        <AlixSourceLabel
          v-if="block.source"
          :source="block.source"
        />
      </div>
      <div class="alix-d-flex alix-ga-2 alix-flex-wrap">
        <button
          type="button"
          class="alix-btn alix-btn-ghost alix-text-sm"
          @click="copy"
        >
          {{ copied ? t("draft.copied") : t("draft.copy") }}
        </button>
        <button
          :disabled="busy"
          type="button"
          class="alix-btn alix-btn-ghost alix-text-sm"
          @click="canHostExport ? downloadHost() : downloadLocal()"
        >
          {{ t("draft.download") }}
        </button>
      </div>
      <AlixMarkdown
        v-if="block.format === 'markdown'"
        :source="block.body"
      />
      <pre
        v-else
        class="alix-ma-0 alix-pre-wrap alix-text-sm"
      >{{ block.body }}</pre>
    </div>
  </slot>
</template>
