<script setup lang="ts">
import { computed, inject, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useAlix } from "../i18n";
import type { MemoryProposalStatus, MemoryProposalView } from "../agui/types";
import AlixIcon from "./common/AlixIcon.vue";
import { RUN_IN_FLIGHT } from "./run-state";

// An assistant-proposed long-term memory (AG-UI CUSTOM `alixai.memory_proposal`). The card only reports
// Remember / Don't remember; the host calls confirm/reject. Typed chat text never confirms.

const props = withDefaults(
  defineProps<{
    proposal: MemoryProposalView;
    status?: MemoryProposalStatus;
  }>(),
  { status: "pending" },
);

const emit = defineEmits<{
  confirm: [];
  reject: [];
}>();

const answered = ref(false);
const runInFlight = inject(RUN_IN_FLIGHT, undefined);
const blocked = computed(() => runInFlight?.value === true);
watch(
  () => props.status,
  (status) => {
    if (status === "pending" || status === "failed") answered.value = false;
  },
);

const answer = (kind: "confirm" | "reject"): void => {
  if (answered.value || blocked.value) return;
  answered.value = true;
  if (kind === "confirm") emit("confirm");
  else emit("reject");
};

const { t, now, formatDateTime } = useAlix();
const clock = ref(now());
let timer: ReturnType<typeof setTimeout> | undefined;

const expiresAt = computed(() => (props.proposal.expiresAt === undefined ? Number.NaN : Date.parse(props.proposal.expiresAt)));
const expired = computed(() => props.status === "expired" || (!Number.isNaN(expiresAt.value) && expiresAt.value <= clock.value));
const open = computed(() => props.status === "pending" || props.status === "failed");
const canAnswer = computed(() => open.value && !expired.value && !blocked.value);
const busy = computed(() => props.status === "submitting");

const schedule = (): void => {
  if (timer !== undefined) clearTimeout(timer);
  timer = undefined;
  clock.value = now();
  const remaining = expiresAt.value - clock.value;
  if (remaining > 0 && remaining < 2 ** 31 - 1) {
    timer = setTimeout(() => {
      clock.value = now();
    }, remaining + 25);
  }
};

onMounted(schedule);
onBeforeUnmount(() => {
  if (timer !== undefined) clearTimeout(timer);
});
watch(() => props.proposal.expiresAt, schedule);

const statusIcon = computed(() => (props.status === "remembered" ? "check" : props.status === "failed" ? "alert" : "x"));
</script>

<template>
  <section
    :aria-busy="busy"
    :class="{ 'alix-tone-danger': expired && open }"
    class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3"
  >
    <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
      <h3 class="alix-heading alix-ma-0">
        {{ t("memory.proposal.title") }}
      </h3>
      <span
        v-if="!Number.isNaN(expiresAt) && open && !expired"
        class="alix-text-muted alix-text-sm"
      >
        {{ t("memory.proposal.expires", { time: formatDateTime(expiresAt) }) }}
      </span>
    </header>

    <slot
      :proposal="proposal"
      name="body"
    >
      <p class="alix-ma-0 alix-break-word">
        {{ proposal.text }}
      </p>
    </slot>

    <p
      v-if="expired && open"
      class="alix-banner alix-tone-danger alix-ma-0 alix-text-sm"
      role="status"
    >
      {{ t("memory.proposal.expired") }}
    </p>
    <p
      v-else-if="status === 'failed'"
      class="alix-banner alix-tone-danger alix-ma-0 alix-text-sm"
      role="status"
    >
      {{ t("memory.proposal.failed") }}
    </p>
    <p
      v-else-if="status === 'remembered' || status === 'forgotten'"
      class="alix-d-flex alix-align-center alix-ga-2 alix-ma-0 alix-text-sm"
      role="status"
    >
      <AlixIcon
        :name="statusIcon"
        :size="16"
      />
      {{ status === "remembered" ? t("memory.proposal.remembered") : t("memory.proposal.forgotten") }}
    </p>

    <div
      v-if="open || busy"
      class="alix-d-flex alix-flex-wrap alix-ga-2"
    >
      <slot
        :busy="busy"
        :can-answer="canAnswer"
        :expired="expired"
        name="actions"
      >
        <button
          :disabled="!canAnswer || busy"
          class="alix-btn alix-btn-primary"
          type="button"
          @click="answer('confirm')"
        >
          {{ busy ? t("memory.proposal.submitting") : t("memory.proposal.remember") }}
        </button>
        <button
          :disabled="!canAnswer || busy"
          class="alix-btn"
          type="button"
          @click="answer('reject')"
        >
          {{ t("memory.proposal.forget") }}
        </button>
      </slot>
    </div>
  </section>
</template>
