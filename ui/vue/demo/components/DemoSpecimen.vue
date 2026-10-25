<script setup lang="ts">
// One exhibit of the blocks gallery: the component's name and a line on what it shows, then the component itself. All three are
// slots; the specimen owns the spacing only.
</script>

<template>
  <figure class="alix-ma-0 alix-d-flex alix-flex-column alix-ga-3 alix-min-w-0 alix-demo-specimen">
    <figcaption class="alix-d-flex alix-flex-wrap alix-align-baseline alix-ga-2">
      <code class="alix-font-mono alix-text-xs alix-demo-specimen-name">
        <slot name="name" />
      </code>
      <span class="alix-text-xs alix-text-muted">
        <slot name="note" />
      </span>
    </figcaption>
    <slot />
  </figure>
</template>
