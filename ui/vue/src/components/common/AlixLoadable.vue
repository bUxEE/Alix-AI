<script setup lang="ts">
import { computed } from "vue";
// Skeletons and loaders are imported STATICALLY, never lazily: a placeholder that has to be fetched cannot cover the wait it is for.
import AlixSectionLoader from "./AlixSectionLoader.vue";

// The two loading situations, derived from two flags the caller owns (see useLoadState):
//   first load (loading, nothing ever loaded)  ->  the `skeleton` slot, shaped like the real content
//   refetch    (loading, content on screen)    ->  the content stays, AlixSectionLoader goes over it
// The wrapper is the positioned ancestor the overlay needs.

const props = defineProps<{
  loading: boolean;
  /** True once a load has succeeded. A retry after a failed first load is still a first load. */
  loaded: boolean;
}>();

const isFirstLoad = computed(() => props.loading && !props.loaded);
const isRefreshing = computed(() => props.loading && props.loaded);
</script>

<template>
  <div
    :aria-busy="loading"
    class="alix-position-relative"
  >
    <slot
      v-if="isFirstLoad"
      name="skeleton"
    />
    <slot v-else />
    <AlixSectionLoader v-if="isRefreshing" />
  </div>
</template>
