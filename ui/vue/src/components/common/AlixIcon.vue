<script setup lang="ts">
import { computed } from "vue";

// A small set of stroke icons drawn for the kit (no third-party icon font, no asset to fetch). They are decorative: the
// button or label next to an icon always carries the accessible name, so the icon is hidden from assistive technology.

const ICONS: Record<string, string[]> = {
  send: ["M21 3 10 14", "M21 3l-7 18-4-7-7-4 18-7z"],
  stop: ["M7 7h10v10H7z"],
  paperclip: ["M20 11.5 12.5 19a5 5 0 0 1-7-7l8-8a3.5 3.5 0 0 1 5 5l-8 8a2 2 0 0 1-3-3l7-7"],
  mic: ["M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3z", "M5 11a7 7 0 0 0 14 0", "M12 18v3"],
  x: ["M6 6l12 12", "M18 6 6 18"],
  check: ["M5 12.5l4.5 4.5L19 7.5"],
  alert: ["M12 4 2.5 20h19z", "M12 10v4", "M12 17.5v.01"],
  info: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", "M12 11v5", "M12 8v.01"],
  file: ["M7 3h7l5 5v13H7z", "M14 3v5h5"],
  "chevron-down": ["M6 9l6 6 6-6"],
  "arrow-down": ["M12 5v14", "M6 13l6 6 6-6"],
  refresh: ["M20 11a8 8 0 1 0-2.3 5.7", "M20 5v6h-6"],
  external: ["M14 4h6v6", "M20 4 10 14", "M18 14v6H4V6h6"],
  shield: ["M12 3l8 3v6c0 4.5-3.2 7.8-8 9-4.8-1.2-8-4.5-8-9V6z"],
  clock: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", "M12 7v5l3 2"],
  trash: ["M4 7h16", "M9 7V4h6v3", "M6 7l1 13h10l1-13"],
  plus: ["M12 5v14", "M5 12h14"],
  sliders: ["M4 7h10", "M18 7h2", "M4 17h2", "M10 17h10", "M14 4v6", "M6 14v6"],
  message: ["M4 5h16v11H9l-5 4z"],
  menu: ["M4 7h16", "M4 12h16", "M4 17h16"],
};

const props = withDefaults(
  defineProps<{
    name: string;
    size?: number;
  }>(),
  { size: 20 },
);

const paths = computed(() => ICONS[props.name] ?? []);
</script>

<template>
  <svg
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.75"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path
      v-for="(d, index) in paths"
      :key="index"
      :d="d"
    />
  </svg>
</template>
