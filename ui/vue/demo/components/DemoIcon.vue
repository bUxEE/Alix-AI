<script setup lang="ts">
import { computed } from "vue";

// The demo shell's icons: 24px outline glyphs on one stroke width, drawn from path data (no icon font, nothing fetched).

const PATHS = {
  chat: ["M4 5.5A2.5 2.5 0 0 1 6.5 3h11A2.5 2.5 0 0 1 20 5.5v8a2.5 2.5 0 0 1-2.5 2.5H10l-4.5 4v-4h0A1.5 1.5 0 0 1 4 14.5z", "M8 8.5h8", "M8 12h5"],
  conversations: ["M4 6h16", "M4 12h16", "M4 18h10"],
  voice: ["M12 3a3 3 0 0 1 3 3v6a3 3 0 0 1-6 0V6a3 3 0 0 1 3-3z", "M5.5 11a6.5 6.5 0 0 0 13 0", "M12 17.5V21"],
  files: ["M7 3h7l5 5v11.5A1.5 1.5 0 0 1 17.5 21h-10A1.5 1.5 0 0 1 6 19.5v-15A1.5 1.5 0 0 1 7.5 3z", "M14 3v5h5", "M9.5 13h5", "M9.5 16.5h5"],
  settings: ["M4 7h10", "M18 7h2", "M4 17h2", "M10 17h10", "M16 5v4", "M8 15v4"],
  blocks: ["M4 4h7v7H4z", "M13 4h7v4h-7z", "M13 10h7v10h-7z", "M4 13h7v7H4z"],
  system: ["M3.5 5h17v11h-17z", "M9 20h6", "M12 16v4"],
  light: ["M12 8a4 4 0 1 1 0 8 4 4 0 0 1 0-8z", "M12 2.5v2", "M12 19.5v2", "M2.5 12h2", "M19.5 12h2", "M5.3 5.3l1.4 1.4", "M17.3 17.3l1.4 1.4", "M5.3 18.7l1.4-1.4", "M17.3 6.7l1.4-1.4"],
  dark: ["M19.5 14.5A8 8 0 0 1 9.5 4.5a8 8 0 1 0 10 10z"],
  refresh: ["M19.5 12a7.5 7.5 0 1 1-2.2-5.3", "M19.5 4.5v4h-4"],
  plus: ["M12 5v14", "M5 12h14"],
  eye: ["M2.5 12s3.5-6.5 9.5-6.5 9.5 6.5 9.5 6.5-3.5 6.5-9.5 6.5S2.5 12 2.5 12z", "M12 9.5a2.5 2.5 0 1 1 0 5 2.5 2.5 0 0 1 0-5z"],
  download: ["M12 4v11", "M7.5 10.5 12 15l4.5-4.5", "M5 19.5h14"],
  arrow: ["M5 12h14", "M13 6l6 6-6 6"],
  spark: ["M12 3v4", "M12 17v4", "M3 12h4", "M17 12h4", "M12 8.5l1.2 2.3 2.3 1.2-2.3 1.2-1.2 2.3-1.2-2.3-2.3-1.2 2.3-1.2z"],
} as const;

export type DemoIconName = keyof typeof PATHS;

const props = withDefaults(
  defineProps<{
    name: DemoIconName;
    size?: number;
  }>(),
  { size: 20 },
);

const paths = computed(() => PATHS[props.name]);
</script>

<template>
  <svg
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.6"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path
      v-for="d in paths"
      :key="d"
      :d="d"
    />
  </svg>
</template>
