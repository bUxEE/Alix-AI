<script setup lang="ts" generic="T extends { readonly id: number; readonly at: string }">
// What the kit handed to the host: opening a citation, continuing in the application, an action of a card, a file to open or
// download, a voice turn. The kit decides none of these itself; the host does, and this list shows each request as it arrives.
// Each entry's wording is the caller's (the `entry` slot); the list owns the layout and the empty state.

defineProps<{
  entries: readonly T[];
}>();

defineSlots<{
  entry: (props: { entry: T }) => unknown;
  empty: () => unknown;
}>();
</script>

<template>
  <ol
    v-if="entries.length > 0"
    class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2 alix-demo-log"
    aria-live="polite"
  >
    <li
      v-for="entry in entries"
      :key="entry.id"
      class="alix-d-flex alix-align-baseline alix-ga-3 alix-text-sm alix-demo-log-entry"
    >
      <time class="alix-font-mono alix-text-xs alix-text-muted alix-flex-shrink-0">
        {{ entry.at }}
      </time>
      <span class="alix-min-w-0 alix-break-word">
        <slot
          :entry="entry"
          name="entry"
        />
      </span>
    </li>
  </ol>
  <p
    v-else
    class="alix-ma-0 alix-text-sm alix-text-muted"
  >
    <slot name="empty" />
  </p>
</template>
