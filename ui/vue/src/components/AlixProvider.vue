<script setup lang="ts">
import { computed } from "vue";
import { provideAlix, type Messages } from "../i18n";

// Wraps the kit's components with a locale, optional message overrides and the theming switches. Every value is a plain scalar or
// a data object: the provider sets attributes that the design tokens key off (`data-theme`, `data-contrast`,
// `data-reduced-motion`) and one token (`--alix-text-scale`). A host that wants its own look overrides `--alix-*` on this element
// or on any ancestor and edits no component. Without a provider the kit renders in English with the system's colour scheme.

const props = withDefaults(
  defineProps<{
    locale?: string;
    messages?: Record<string, Partial<Messages>>;
    /** `system` follows the operating system; `light` and `dark` force a scheme. */
    theme?: "system" | "light" | "dark";
    contrast?: "normal" | "more";
    /** True forces reduced motion on. The operating system's own setting is always respected as well. */
    reducedMotion?: boolean;
    /** The text size as a whole percentage of normal (the effective setting `accessibility.text_scale_pct`). */
    textScalePct?: number;
    timeZone?: string;
  }>(),
  { locale: "en", messages: undefined, theme: "system", contrast: "normal", reducedMotion: false, textScalePct: 100, timeZone: undefined },
);

provideAlix({ locale: () => props.locale, ...(props.messages !== undefined && { messages: props.messages }), ...(props.timeZone !== undefined && { timeZone: props.timeZone }) });

const scale = computed(() => String(Math.min(300, Math.max(100, props.textScalePct)) / 100));
</script>

<template>
  <div
    :lang="locale"
    :data-theme="theme === 'system' ? undefined : theme"
    :data-contrast="contrast === 'more' ? 'more' : undefined"
    :data-reduced-motion="reducedMotion ? 'true' : undefined"
    :style="{ '--alix-text-scale': scale }"
    class="alix-root"
  >
    <slot />
  </div>
</template>
