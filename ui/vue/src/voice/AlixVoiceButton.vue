<script setup lang="ts">
import { computed, ref } from "vue";
import AlixIcon from "../components/common/AlixIcon.vue";
import { useAlixVoiceText } from "./messages";

// The microphone button. It reports `start`, `stop` and `cancel` and owns no audio: Voice Mode (or the host, with `useAlixVoice`)
// captures and sends.
//
// Two ways to use it. `toggle`: press to start, press again to stop. `push-to-talk`: hold to talk, release to stop, with the
// keyboard too (hold Space or Enter). Escape while listening cancels: nothing is sent. While listening the button is a pressed
// toggle button, so a screen reader hears the state. A screen reader's own "activate" is a click with no pointer and no key, so in
// push-to-talk it toggles instead (holding is not possible with a virtual cursor). Losing focus or the pointer while holding
// releases, so the microphone never stays open behind the person's back.

const props = withDefaults(
  defineProps<{
    state?: "idle" | "listening" | "processing" | "unavailable";
    mode?: "toggle" | "push-to-talk";
    size?: "md" | "lg";
  }>(),
  { state: "idle", mode: "toggle", size: "md" },
);

const emit = defineEmits<{
  start: [];
  stop: [];
  cancel: [];
}>();

const t = useAlixVoiceText();

const listening = computed(() => props.state === "listening");
const inert = computed(() => props.state === "unavailable" || props.state === "processing");
const label = computed(() => {
  if (props.state === "unavailable") return t("voice.unavailable");
  if (props.state === "processing") return t("voice.processing");
  if (props.mode === "push-to-talk") return t("voice.hold");
  return listening.value ? t("voice.stop") : t("voice.start");
});

const toggle = (): void => {
  if (listening.value) emit("stop");
  else emit("start");
};
const click = (event: MouseEvent): void => {
  if (inert.value) return;
  if (props.mode === "toggle") toggle();
  else if (event.detail === 0) toggle();
};
// The press is remembered here, not read back from `state`: the host may take a moment to open the microphone, and a release in
// that moment must still be reported, or the microphone would open after the person let go.
const held = ref(false);
const down = (event?: PointerEvent): void => {
  if (props.mode !== "push-to-talk" || inert.value || listening.value || held.value) return;
  if (event !== undefined && event.button > 0) return;
  try {
    // Keep receiving the pointer when the finger or mouse slides off the button: only lifting it releases.
    (event?.currentTarget as HTMLElement | undefined)?.setPointerCapture?.(event?.pointerId ?? 0);
  } catch {
    // a synthetic or already-released pointer
  }
  held.value = true;
  emit("start");
};
const up = (): void => {
  if (!held.value) return;
  held.value = false;
  emit("stop");
};
const keydown = (event: KeyboardEvent): void => {
  if (event.key === "Escape" && (listening.value || held.value)) {
    event.preventDefault();
    held.value = false; // the release that follows is not a "send"
    emit("cancel");
    return;
  }
  if (props.mode === "push-to-talk" && (event.key === " " || event.key === "Enter")) {
    event.preventDefault();
    if (!event.repeat) down();
  }
};
const keyup = (event: KeyboardEvent): void => {
  if (props.mode === "push-to-talk" && (event.key === " " || event.key === "Enter")) {
    event.preventDefault();
    up();
  }
};
const contextmenu = (event: Event): void => {
  // A long press is how this button is used on a touch screen: no browser menu over it.
  if (props.mode === "push-to-talk") event.preventDefault();
};
</script>

<template>
  <button
    :aria-label="label"
    :aria-pressed="listening"
    :aria-disabled="inert"
    :aria-keyshortcuts="mode === 'push-to-talk' ? 'Space Enter' : undefined"
    :data-mode="mode"
    :data-size="size"
    :data-state="state"
    class="alix-root alix-voice-talk alix-flex-shrink-0"
    type="button"
    @blur="up"
    @click="click"
    @contextmenu="contextmenu"
    @keydown="keydown"
    @keyup="keyup"
    @pointercancel="up"
    @pointerdown="down"
    @pointerup="up"
  >
    <slot name="icon">
      <AlixIcon
        :name="state === 'listening' && mode === 'toggle' ? 'stop' : 'mic'"
        :size="size === 'lg' ? 28 : 20"
      />
    </slot>
    <span class="alix-sr-only">
      <slot>
        {{ label }}
      </slot>
    </span>
  </button>
</template>

<style scoped lang="scss">
@use "../styles/mixins";

// A round button. It carries its own look (it is also the root, so the kit's `.alix-root .alix-btn` rules do not reach it). The
// ring is a pseudo-element that fades and settles in: transform and opacity only, never from nothing.
.alix-voice-talk {
  @include mixins.pressable;
  @include mixins.focus-ring;

  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  padding: 0;
  border: 1px solid var(--alix-border-strong);
  border-radius: 50%;
  background: var(--alix-surface);
  color: var(--alix-text);
  cursor: pointer;
  user-select: none;
  -webkit-user-select: none;
  -webkit-touch-callout: none;
  touch-action: manipulation;

  &[data-mode="push-to-talk"] {
    touch-action: none;
  }

  &[data-size="lg"] {
    width: 64px;
    height: 64px;
  }

  @include mixins.hover {
    background: var(--alix-surface-2);
  }

  &::after {
    content: "";
    position: absolute;
    inset: -6px;
    border: 2px solid var(--alix-accent);
    border-radius: 50%;
    opacity: 0;
    transform: scale(0.92);
    pointer-events: none;
    transition: opacity var(--alix-dur-base) var(--alix-ease-out), transform var(--alix-dur-base) var(--alix-ease-out);
  }

  &[data-state="listening"] {
    border-color: var(--alix-accent);
    background: var(--alix-accent);
    color: var(--alix-accent-contrast);

    @include mixins.hover {
      background: var(--alix-accent-hover);
    }

    &::after {
      opacity: 0.4;
      transform: scale(1);
    }
  }

  &[aria-disabled="true"] {
    opacity: 0.5;
    cursor: not-allowed;
  }
}
</style>
