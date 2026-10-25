<script setup lang="ts">
import { useId } from "vue";

// An on/off control: a native checkbox with the `switch` role, so keyboard (Space), focus and screen-reader behaviour are the
// browser's own. The label and the hint are slots (the caller owns the words). It validates nothing, so it carries the
// `no-validation` class that keeps any host stylesheet from stamping a success tick on it.

defineProps<{
  disabled?: boolean;
}>();

const checked = defineModel<boolean>({ default: false });
const uid = useId();
</script>

<template>
  <div class="alix-root alix-switch alix-d-flex alix-align-start alix-ga-3">
    <input
      v-model="checked"
      :id="uid"
      :disabled="disabled"
      :aria-describedby="`${uid}-hint`"
      class="alix-switch-input no-validation"
      type="checkbox"
      role="switch"
    >
    <label
      :for="uid"
      class="alix-d-flex alix-flex-column alix-ga-1"
    >
      <span class="alix-font-medium">
        <slot />
      </span>
      <span
        :id="`${uid}-hint`"
        class="alix-text-muted alix-text-sm"
      >
        <slot name="hint" />
      </span>
    </label>
  </div>
</template>

<style scoped lang="scss">
@use "../../styles/mixins";

// The track and knob. The knob moves with a transform (never left/width) and a strong ease-out; reduced motion collapses it.
.alix-switch-input {
  @include mixins.focus-ring;

  position: relative;
  flex: none;
  width: 40px;
  height: 24px;
  margin: 0;
  border-radius: 999px;
  background: var(--alix-surface-3);
  border: 1px solid var(--alix-border-strong);
  appearance: none;
  cursor: pointer;
  transition: background-color var(--alix-dur-fast) ease, border-color var(--alix-dur-fast) ease;

  &::after {
    content: "";
    position: absolute;
    inset: 2px auto 2px 2px;
    width: 18px;
    border-radius: 50%;
    background: var(--alix-text-muted);
    transition: transform var(--alix-dur-base) var(--alix-ease-out), background-color var(--alix-dur-fast) ease;
  }

  &:checked {
    background: var(--alix-accent);
    border-color: var(--alix-accent);

    &::after {
      background: var(--alix-accent-contrast);
      transform: translateX(16px);
    }
  }

  &:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
}

label {
  cursor: pointer;
}

.alix-switch-input:disabled + label {
  cursor: not-allowed;
}
</style>
