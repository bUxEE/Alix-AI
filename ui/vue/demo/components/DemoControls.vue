<script setup lang="ts">
import type { DemoLocale } from "../../../demo-mock/src/index";
import { locale, theme, useDemoText, type DemoTheme } from "../state";
import DemoIcon from "./DemoIcon.vue";

// The two switches of the showcase: the language of the kit and of the mock's replies, and the colour scheme. Each is a small
// radio group of buttons (arrow keys are not needed for two or three options; every button is reachable with Tab).

withDefaults(
  defineProps<{
    compact?: boolean;
  }>(),
  { compact: false },
);

const text = useDemoText();
const LOCALES: readonly { value: DemoLocale; label: string }[] = [
  { value: "en", label: "EN" },
  { value: "it", label: "IT" },
];
const THEMES: readonly DemoTheme[] = ["system", "light", "dark"];
</script>

<template>
  <div
    :class="compact ? 'alix-flex-row alix-align-center' : 'alix-flex-column'"
    class="alix-d-flex alix-ga-3"
  >
    <div
      :aria-label="text('shell.language')"
      class="alix-d-flex alix-demo-segmented"
      role="radiogroup"
    >
      <button
        v-for="option in LOCALES"
        :key="option.value"
        :aria-checked="locale === option.value"
        :lang="option.value"
        :title="text(`shell.language.${option.value}`)"
        class="alix-demo-segment alix-font-mono alix-text-xs"
        role="radio"
        type="button"
        @click="locale = option.value"
      >
        {{ option.label }}
      </button>
    </div>
    <div
      :aria-label="text('shell.theme')"
      class="alix-d-flex alix-demo-segmented"
      role="radiogroup"
    >
      <button
        v-for="option in THEMES"
        :key="option"
        :aria-checked="theme === option"
        :aria-label="text(`shell.theme.${option}`)"
        :title="text(`shell.theme.${option}`)"
        class="alix-demo-segment"
        role="radio"
        type="button"
        @click="theme = option"
      >
        <DemoIcon
          :name="option"
          :size="16"
        />
      </button>
    </div>
  </div>
</template>
