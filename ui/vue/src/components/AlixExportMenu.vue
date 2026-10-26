<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { runExport, type AlixDownload, type AlixExporter, type ExportFormat, type ExportParams } from "../export";
import { useAlix } from "../i18n";
import AlixIcon from "./common/AlixIcon.vue";

// A small download menu. Hidden when there is no host exporter. Loading and error stay on the control; the kit never calls AlixAI.

const props = defineProps<{
  exporter: AlixExporter;
  download?: AlixDownload;
  params: Omit<ExportParams, "format">;
  formats: readonly ExportFormat[];
  label: string;
}>();

const { t } = useAlix();
const open = ref(false);
const busy = ref(false);
const error = ref("");
const root = ref<HTMLElement | null>(null);

const labels = computed(
  (): Record<ExportFormat, string> => ({
    csv: t("export.csv"),
    xlsx: t("export.xlsx"),
    html: t("export.html"),
    md: t("export.md"),
  }),
);

const close = (): void => {
  open.value = false;
};

const toggle = (): void => {
  if (busy.value) return;
  error.value = "";
  open.value = !open.value;
};

const onDocPointer = (event: Event): void => {
  if (!open.value || root.value === null) return;
  if (event.target instanceof Node && root.value.contains(event.target)) return;
  close();
};

const onDocKey = (event: KeyboardEvent): void => {
  if (event.key === "Escape" && open.value) {
    event.preventDefault();
    close();
  }
};

onMounted(() => {
  document.addEventListener("pointerdown", onDocPointer, true);
  document.addEventListener("keydown", onDocKey, true);
});
onBeforeUnmount(() => {
  document.removeEventListener("pointerdown", onDocPointer, true);
  document.removeEventListener("keydown", onDocKey, true);
});

const choose = async (format: ExportFormat): Promise<void> => {
  if (busy.value) return;
  busy.value = true;
  error.value = "";
  close();
  try {
    await runExport(props.exporter, { ...props.params, format }, props.download);
  } catch {
    error.value = t("export.failed");
  } finally {
    busy.value = false;
  }
};
</script>

<template>
  <div
    ref="root"
    class="alix-export alix-d-inline-flex alix-flex-column alix-ga-1 alix-position-relative"
  >
    <button
      :aria-expanded="open"
      :aria-busy="busy"
      :aria-label="label"
      :disabled="busy"
      class="alix-btn alix-btn-quiet alix-btn-icon"
      type="button"
      aria-haspopup="menu"
      @click="toggle"
    >
      <span
        v-if="busy"
        class="alix-spinner"
        aria-hidden="true"
      />
      <AlixIcon
        v-else
        :size="16"
        name="download"
      />
    </button>
    <ul
      v-if="open"
      :aria-label="label"
      class="alix-export-menu alix-reset-list alix-card alix-card-raised"
      role="menu"
    >
      <li
        v-for="format in formats"
        :key="format"
        role="none"
      >
        <button
          class="alix-export-item"
          type="button"
          role="menuitem"
          @click="choose(format)"
        >
          {{ labels[format] }}
        </button>
      </li>
    </ul>
    <p
      v-if="error"
      class="alix-text-sm alix-ma-0"
      role="alert"
    >
      {{ error }}
    </p>
  </div>
</template>

<style scoped lang="scss">
.alix-export-menu {
  position: absolute;
  inset: calc(100% + 4px) 0 auto auto;
  z-index: 5;
  min-width: 11rem;
  padding: 4px;
}

.alix-export-item {
  display: block;
  width: 100%;
  padding: 8px 10px;
  border: 0;
  border-radius: var(--alix-radius-sm);
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: start;
  cursor: pointer;

  &:hover,
  &:focus-visible {
    background: var(--alix-accent-soft);
    outline: none;
  }
}
</style>
