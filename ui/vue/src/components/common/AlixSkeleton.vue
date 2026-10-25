<script setup lang="ts">
// One placeholder bar. Skeletons are composed from these inside the real cards of a screen, so the card's border, radius and
// padding are on screen from the first paint and only the contents stand in. Decorative: hidden from assistive technology
// (the surrounding AlixLoadable announces `aria-busy`).

withDefaults(
  defineProps<{
    width?: string;
    height?: string;
  }>(),
  { width: "100%", height: "14px" },
);
</script>

<template>
  <span
    :style="{ width, height }"
    class="alix-skeleton"
    aria-hidden="true"
  />
</template>
