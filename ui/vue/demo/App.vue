<script setup lang="ts">
import alixIcon from "../../../docs/assets/brand/alix-icon.svg";
import { AlixProvider } from "@alixai/vue";
import { computed, onBeforeUnmount, onMounted, ref, watch, type Component } from "vue";
import DemoControls from "./components/DemoControls.vue";
import DemoMockStatus from "./components/DemoMockStatus.vue";
import DemoNav from "./components/DemoNav.vue";
import BlocksSection from "./sections/BlocksSection.vue";
import ChatSection from "./sections/ChatSection.vue";
import ConversationsSection from "./sections/ConversationsSection.vue";
import FilesSection from "./sections/FilesSection.vue";
import SettingsSection from "./sections/SettingsSection.vue";
import VoiceSection from "./sections/VoiceSection.vue";
import { SECTIONS, isSectionId, type SectionId } from "./sections";
import { locale, theme, useDemoText } from "./state";

// The showcase shell: a sidebar of sections on wide screens, a top bar with the same sections on narrow ones, and the language
// and theme switches. Everything sits inside AlixProvider, so the kit and the shell switch language and scheme together.

const text = useDemoText();

const VIEWS: Record<SectionId, Component> = {
  chat: ChatSection,
  conversations: ConversationsSection,
  voice: VoiceSection,
  files: FilesSection,
  settings: SettingsSection,
  blocks: BlocksSection,
};

const fromHash = (): SectionId => {
  const id = globalThis.location.hash.replace(/^#\/?/, "");
  return isSectionId(id) ? id : "chat";
};

const current = ref<SectionId>(fromHash());
const view = computed(() => VIEWS[current.value]);

const onHash = (): void => {
  current.value = fromHash();
};

onMounted(() => globalThis.addEventListener("hashchange", onHash));
onBeforeUnmount(() => globalThis.removeEventListener("hashchange", onHash));

// The page behind the provider (overscroll, the scrollbar gutter) follows the chosen scheme too.
watch(
  theme,
  (next) => {
    const root = document.documentElement;
    if (next === "system") delete root.dataset["theme"];
    else root.dataset["theme"] = next;
  },
  { immediate: true },
);
watch(
  [locale, current],
  ([nextLocale, section]) => {
    document.documentElement.lang = nextLocale;
    document.title = `${text(`nav.${section}.title`)} · AlixAI Vue kit`;
  },
  { immediate: true },
);
</script>

<template>
  <AlixProvider
    :locale="locale"
    :theme="theme"
    class="alix-demo-app"
  >
    <div class="alix-d-flex alix-demo-viewport">
      <aside
        :aria-label="text('shell.sections')"
        class="alix-d-none alix-d-md-flex alix-flex-column alix-ga-6 alix-pa-5 alix-flex-shrink-0 alix-demo-sidebar"
      >
        <a
          class="alix-d-flex alix-align-center alix-ga-3 alix-demo-brand"
          href="#/chat"
        >
          <img
            :src="alixIcon"
            class="alix-demo-mark"
            alt=""
            aria-hidden="true"
          >
          <span class="alix-d-flex alix-flex-column">
            <span class="alix-font-semibold">
              AlixAI
            </span>
            <span class="alix-text-muted alix-text-xs alix-font-mono">
              @alixai/vue
            </span>
          </span>
        </a>
        <DemoNav
          :current="current"
          :sections="SECTIONS"
        />
        <div class="alix-d-flex alix-flex-column alix-ga-4 alix-demo-sidebar-foot">
          <DemoControls />
          <DemoMockStatus />
        </div>
      </aside>
      <div class="alix-d-flex alix-flex-column alix-flex-grow-1 alix-min-w-0">
        <header class="alix-d-flex alix-d-md-none alix-flex-column alix-ga-3 alix-px-4 alix-pt-4 alix-pb-2 alix-demo-topbar">
          <div class="alix-d-flex alix-align-center alix-justify-between alix-ga-3">
            <a
              class="alix-d-flex alix-align-center alix-ga-2 alix-demo-brand"
              href="#/chat"
            >
              <img
                :src="alixIcon"
                class="alix-demo-mark"
                alt=""
                aria-hidden="true"
              >
              <span class="alix-font-semibold">
                AlixAI
              </span>
            </a>
            <DemoControls compact />
          </div>
          <DemoNav
            :current="current"
            :sections="SECTIONS"
            horizontal
          />
        </header>
        <main class="alix-flex-grow-1 alix-min-w-0 alix-demo-main">
          <KeepAlive>
            <component
              :is="view"
              :key="current"
            />
          </KeepAlive>
        </main>
      </div>
    </div>
  </AlixProvider>
</template>
