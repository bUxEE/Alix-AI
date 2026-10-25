<script setup lang="ts">
import { onMounted, ref } from "vue";
import { DEMO_PATHS, fetchDemoJson } from "../../../demo-mock/src/index";
import { useDemoText } from "../state";

// Whether the scripted backend answers (`GET /healthz` on the same origin). It is the only server the demo talks to.

const text = useDemoText();
const state = ref<"checking" | "ok" | "down">("checking");

const check = async (): Promise<void> => {
  state.value = "checking";
  try {
    await fetchDemoJson<{ status: string }>(DEMO_PATHS.healthz);
    state.value = "ok";
  } catch {
    state.value = "down";
  }
};

onMounted(check);
</script>

<template>
  <p
    class="alix-d-flex alix-align-center alix-ga-2 alix-ma-0 alix-text-xs alix-text-muted"
    role="status"
  >
    <span
      :class="`alix-demo-dot-${state}`"
      class="alix-demo-dot"
      aria-hidden="true"
    />
    <span>
      {{ text(`shell.mock.${state}`) }}
    </span>
    <button
      v-if="state === 'down'"
      class="alix-btn alix-btn-quiet alix-demo-btn-xs"
      type="button"
      @click="check"
    >
      {{ text("common.retry") }}
    </button>
  </p>
</template>
