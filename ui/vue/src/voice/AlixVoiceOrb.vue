<script setup lang="ts">
import { createVoiceOrb, readCssColors, watchPresentation, type VoiceOrb, type VoiceOrbColors, type VoiceOrbRenderer, type VoiceOrbState } from "@alixai/voice-orb";
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, useTemplateRef, watch } from "vue";
import { useReducedMotion } from "../composables/useBrowser";
import { useAlixVoiceText, type AlixVoiceMessageKey } from "./messages";

// The voice orb (@alixai/voice-orb): a soft, luminous sphere that breathes when idle, swells with the person's voice while
// listening, swirls while the assistant thinks and pulses while it speaks. The React kit's AlixVoiceOrb draws the same orb.
//
// It is an image to a screen reader, named after its state; the words of Voice Mode carry the state for everyone else too. Its
// colours are the kit's tokens (accent, surface, danger), read again when the theme changes. Under reduced motion (the system's,
// the provider's, or this prop) it holds still and shows the state through size and brightness only. The orb, its canvas and its
// GPU context are created after mount and released on unmount: nothing runs on a server.

const props = withDefaults(
  defineProps<{
    state?: VoiceOrbState;
    /** The voice level, 0 to 1: the microphone while listening, the speech while speaking. */
    level?: number;
    /** The orb's side: a number of pixels or any CSS length. */
    size?: number | string;
    /** Replaces the state's name for screen readers. */
    label?: string;
    /** Hold still even where motion is allowed. Reduced motion asked by the system or the provider applies anyway. */
    reducedMotion?: boolean;
    /** Colours instead of the kit's tokens, as CSS colours. */
    colors?: VoiceOrbColors;
  }>(),
  { state: "idle", level: 0, size: 160, label: undefined, reducedMotion: false, colors: undefined },
);

const TOKENS = { accent: "--alix-accent", surface: "--alix-surface", error: "--alix-danger" };

const vt = useAlixVoiceText();
const host = useTemplateRef<HTMLElement>("host");
const orb = shallowRef<VoiceOrb>();
const renderer = ref<VoiceOrbRenderer>();
const prefersStill = useReducedMotion();
const providerStill = ref(false);
const still = computed(() => props.reducedMotion || prefersStill.value || providerStill.value);
const side = computed(() => (typeof props.size === "number" ? `${props.size}px` : props.size));
const name = computed(() => props.label ?? vt(`voice.orb.${props.state}` as AlixVoiceMessageKey));

const colorsOf = (element: HTMLElement): VoiceOrbColors => ({ ...readCssColors(element, TOKENS), ...props.colors });
let stopWatching = (): void => undefined;

onMounted(() => {
  const element = host.value;
  if (element === null) return;
  providerStill.value = element.closest('[data-reduced-motion="true"]') !== null;
  const created = createVoiceOrb(element, {
    state: props.state,
    level: props.level,
    colors: colorsOf(element),
    reducedMotion: still.value,
    onRendererChange: (next) => {
      renderer.value = next;
    },
  });
  orb.value = created;
  renderer.value = created.renderer;
  stopWatching = watchPresentation(element, () => {
    providerStill.value = element.closest('[data-reduced-motion="true"]') !== null;
    created.setColors(colorsOf(element));
  });
});

watch(
  () => props.state,
  (state) => orb.value?.setState(state),
);
watch(
  () => props.level,
  (level) => orb.value?.setLevel(level),
);
watch(still, (on) => orb.value?.setReducedMotion(on));
watch(
  () => props.colors,
  () => {
    if (host.value !== null) orb.value?.setColors(colorsOf(host.value));
  },
  { deep: true },
);
watch(side, () => orb.value?.resize());

onBeforeUnmount(() => {
  stopWatching();
  orb.value?.destroy();
  orb.value = undefined;
});
</script>

<template>
  <span
    :aria-label="name"
    :data-renderer="renderer"
    :data-state="state"
    :style="{ width: side, height: side }"
    ref="host"
    class="alix-voice-orb"
    role="img"
  />
</template>

<style scoped lang="scss">
// The orb paints its own canvas inside this box; the box only reserves the space, so nothing moves when the canvas arrives.
.alix-voice-orb {
  display: inline-block;
  flex-shrink: 0;
  max-width: 100%;
  aspect-ratio: 1;
  pointer-events: none;
}
</style>
