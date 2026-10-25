<script setup lang="ts">
import type { SectionId } from "../sections";
import { useDemoText } from "../state";
import DemoIcon from "./DemoIcon.vue";

// The section links. A vertical list with a hint under each title in the sidebar; a scrolling row of pills in the narrow top bar.

withDefaults(
  defineProps<{
    sections: readonly SectionId[];
    current: SectionId;
    horizontal?: boolean;
  }>(),
  { horizontal: false },
);

const text = useDemoText();
</script>

<template>
  <nav :aria-label="text('shell.sections')">
    <ul
      :class="horizontal ? 'alix-flex-row alix-overflow-auto alix-demo-nav-row' : 'alix-flex-column'"
      class="alix-reset-list alix-d-flex alix-ga-1"
    >
      <li
        v-for="id in sections"
        :key="id"
        class="alix-flex-shrink-0"
      >
        <a
          :aria-current="id === current ? 'page' : undefined"
          :href="`#/${id}`"
          :class="horizontal ? 'alix-demo-nav-pill' : 'alix-demo-nav-link'"
          class="alix-d-flex alix-align-start alix-ga-3"
        >
          <DemoIcon
            :name="id"
            class="alix-flex-shrink-0 alix-demo-nav-icon"
          />
          <span class="alix-d-flex alix-flex-column alix-min-w-0">
            <span class="alix-font-medium">
              {{ text(`nav.${id}.title`) }}
            </span>
            <span
              v-if="!horizontal"
              class="alix-text-muted alix-text-xs alix-demo-nav-hint"
            >
              {{ text(`nav.${id}.hint`) }}
            </span>
          </span>
        </a>
      </li>
    </ul>
  </nav>
</template>
