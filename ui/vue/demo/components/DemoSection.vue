<script setup lang="ts">
// The frame of one showcase section: a header (the components it shows, a title, one line on what to try, optional actions)
// and the section's body. Every piece of content is a slot; the frame owns only the layout.
</script>

<template>
  <section class="alix-d-flex alix-flex-column alix-ga-6 alix-demo-section">
    <header class="alix-d-flex alix-flex-column alix-flex-md-row alix-align-start alix-justify-between alix-ga-4">
      <div class="alix-d-flex alix-flex-column alix-ga-2 alix-min-w-0">
        <p class="alix-ma-0 alix-font-mono alix-text-xs alix-text-muted alix-demo-eyebrow">
          <slot name="eyebrow" />
        </p>
        <h1 class="alix-ma-0 alix-demo-title">
          <slot name="title" />
        </h1>
        <p class="alix-ma-0 alix-text-muted alix-demo-lede">
          <slot name="lede" />
        </p>
      </div>
      <div class="alix-d-flex alix-flex-wrap alix-align-center alix-ga-2 alix-flex-shrink-0">
        <slot name="actions" />
      </div>
    </header>
    <slot />
  </section>
</template>
