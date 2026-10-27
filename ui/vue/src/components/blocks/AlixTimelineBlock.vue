<script setup lang="ts">
import type { TimelineBlock } from "../../sdk-types";
import AlixSourceLabel from "./AlixSourceLabel.vue";

defineProps<{
  block: TimelineBlock;
}>();
</script>

<template>
  <slot :block="block">
    <div
      class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3"
      data-alixai="timeline"
    >
      <div class="alix-d-flex alix-justify-space-between alix-align-center alix-ga-2">
        <h3
          v-if="block.title"
          class="alix-heading alix-ma-0 alix-text-md"
        >
          {{ block.title }}
        </h3>
        <AlixSourceLabel
          v-if="block.source"
          :source="block.source"
        />
      </div>
      <ol class="alix-ma-0 alix-pa-0 alix-d-flex alix-flex-column alix-ga-3 alix-list-none">
        <li
          v-for="(event, index) in block.events"
          :key="`${event.at}-${index}`"
          class="alix-d-flex alix-flex-column alix-ga-1"
        >
          <time
            :datetime="event.at"
            class="alix-text-xs alix-text-muted"
          >
            {{ event.at }}
          </time>
          <span class="alix-text-sm">{{ event.label }}</span>
          <span
            v-if="event.detail"
            class="alix-text-sm alix-text-muted"
          >{{ event.detail }}</span>
        </li>
      </ol>
    </div>
  </slot>
</template>
