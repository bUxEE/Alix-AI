<script setup lang="ts">
import { computed, inject, onBeforeUnmount, onMounted, ref, useId, watch } from "vue";
import { formatCanonicalValue } from "../agui/parse";
import type { AlixConfirmation, ConfirmationStatus } from "../agui/types";
import { useAlix, type MessageKey } from "../i18n";
import AlixIcon from "./common/AlixIcon.vue";
import { RUN_IN_FLIGHT } from "./run-state";

// The confirmation of a prepared action. What it shows (the summary, the exact effects, the risk and the expiry) is projected by
// the core from the PreparedAction, so the user confirms what the host validated and never what the model said. The sheet
// reports a decision (`approve`, `decline`, `dismiss`); it commits nothing, and the approval travels as a `resume` of the
// interrupt with the secret the host relay holds. A step-up action cannot be confirmed here at all: the sheet hands the user to
// the application's own interface (`continue-in-app`).
//
// Inline by default: a confirmation belongs in the conversation, where it stays as the record. `modal` presents it as a bottom
// sheet over the page, with focus kept inside, Escape to dismiss and a scrim.

const props = withDefaults(
  defineProps<{
    confirmation: AlixConfirmation;
    status?: ConfirmationStatus;
    modal?: boolean;
  }>(),
  { status: "pending", modal: false },
);

const emit = defineEmits<{
  approve: [];
  decline: [];
  dismiss: [];
  "continue-in-app": [];
}>();

// The answer is reported once. A second click (or a click while the host is still catching up) emits nothing; the flag clears
// only when the host says the answer did not go through (`status` back to pending or failed), so a retry still works. Inside a
// message stream no answer is taken while another run holds the thread: it would be refused, and nothing would clear the flag.
const answered = ref(false);
const runInFlight = inject(RUN_IN_FLIGHT, undefined);
const blocked = computed(() => runInFlight?.value === true);
watch(
  () => props.status,
  (status) => {
    if (status === "pending" || status === "failed") answered.value = false;
  },
);
const answer = (kind: "approve" | "decline" | "dismiss"): void => {
  if (answered.value || blocked.value) return;
  answered.value = true;
  if (kind === "approve") emit("approve");
  else if (kind === "decline") emit("decline");
  else emit("dismiss");
};

const { t, now, formatDateTime } = useAlix();
const uid = useId();
const root = ref<HTMLElement | null>(null);
const clock = ref(now());
let timer: ReturnType<typeof setTimeout> | undefined;

const expiresAt = computed(() => Date.parse(props.confirmation.expiresAt));
const expired = computed(() => !Number.isNaN(expiresAt.value) && expiresAt.value <= clock.value);
const stepUp = computed(() => props.confirmation.mode === "step_up");
const open = computed(() => props.status === "pending" || props.status === "failed");
const canAnswer = computed(() => open.value && !expired.value && !stepUp.value && !blocked.value);
const busy = computed(() => props.status === "submitting");

const riskTone = computed(() => {
  const level = props.confirmation.riskLevel;
  return level >= 3 ? "alix-tone-danger" : level === 2 ? "alix-tone-warning" : level === 1 ? "alix-tone-accent" : "";
});

// One timer, set for the moment of expiry: the approve button disables itself without the sheet polling.
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

const focusables = (): HTMLElement[] =>
  root.value === null ? [] : Array.from(root.value.querySelectorAll<HTMLElement>("button:not(:disabled), [href], [tabindex]:not([tabindex='-1'])"));

const onKeydown = (event: KeyboardEvent): void => {
  if (!props.modal) return;
  if (event.key === "Escape") {
    event.preventDefault();
    answer("dismiss");
    return;
  }
  if (event.key !== "Tab") return;
  const items = focusables();
  const first = items[0];
  const last = items[items.length - 1];
  if (first === undefined || last === undefined) return;
  const active = document.activeElement;
  if (event.shiftKey && (active === first || active === root.value)) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && active === last) {
    event.preventDefault();
    first.focus();
  }
};

onMounted(() => {
  schedule();
  if (props.modal) root.value?.focus();
});
onBeforeUnmount(() => {
  if (timer !== undefined) clearTimeout(timer);
});
watch(() => props.confirmation.expiresAt, schedule);

const statusIcon = computed(() => (props.status === "approved" ? "check" : props.status === "failed" ? "alert" : "x"));
</script>

<template>
  <div class="alix-root alix-d-contents">
    <div
      v-if="modal"
      class="alix-sheet-scrim"
      aria-hidden="true"
      @click="answer('dismiss')"
    />
    <section
      :aria-labelledby="`${uid}-title`"
      :aria-describedby="`${uid}-summary`"
      :class="{ 'alix-sheet-modal': modal }"
      :role="modal ? 'dialog' : 'group'"
      :aria-modal="modal ? 'true' : undefined"
      :tabindex="modal ? -1 : undefined"
      ref="root"
      class="alix-card alix-card-raised alix-pa-4 alix-d-flex alix-flex-column alix-ga-4"
      @keydown="onKeydown"
    >
      <slot name="header">
        <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-3">
          <div class="alix-d-flex alix-align-center alix-ga-2 alix-min-w-0">
            <AlixIcon name="shield" />
            <h3
              :id="`${uid}-title`"
              class="alix-heading"
            >
              {{ stepUp ? t("confirm.stepUp.title") : t("confirm.title") }}
            </h3>
          </div>
          <span
            :class="riskTone"
            class="alix-badge alix-flex-shrink-0"
          >
            {{ t("confirm.risk", { level: confirmation.riskLevel }) }}
          </span>
        </header>
      </slot>
      <p
        :id="`${uid}-summary`"
        class="alix-ma-0 alix-break-word"
      >
        {{ confirmation.summary }}
      </p>
      <section
        v-if="confirmation.effects.length > 0"
        :aria-label="t('confirm.effects')"
        class="alix-d-flex alix-flex-column alix-ga-2"
      >
        <p class="alix-eyebrow">
          {{ t("confirm.effects") }}
        </p>
        <ul class="alix-reset-list alix-card alix-divided">
          <li
            v-for="(effect, index) in confirmation.effects"
            :key="index"
            class="alix-pa-3 alix-d-flex alix-flex-column alix-ga-1"
          >
            <slot
              :effect="effect"
              name="effect"
            >
              <div class="alix-d-flex alix-flex-wrap alix-align-center alix-ga-2">
                <span class="alix-badge">
                  {{ t(`confirm.op.${effect.operation}` as MessageKey) }}
                </span>
                <span class="alix-font-medium alix-break-word">
                  {{ effect.description }}
                </span>
                <span
                  v-if="effect.external"
                  class="alix-badge alix-tone-warning"
                >
                  {{ t("confirm.external") }}
                </span>
              </div>
              <dl
                v-if="effect.changes.length > 0"
                class="alix-ma-0 alix-d-flex alix-flex-column alix-ga-1 alix-text-sm"
              >
                <div
                  v-for="(change, at) in effect.changes"
                  :key="`${change.field}-${at}`"
                  class="alix-d-flex alix-flex-wrap alix-ga-2"
                >
                  <dt class="alix-text-muted">
                    {{ change.field }}
                  </dt>
                  <dd class="alix-ma-0 alix-break-word">
                    {{ change.display ?? formatCanonicalValue(change.value) }}
                  </dd>
                </div>
              </dl>
            </slot>
          </li>
        </ul>
      </section>
      <p
        v-if="stepUp"
        class="alix-banner alix-tone-warning alix-ma-0"
      >
        {{ t("confirm.stepUp.body") }}
      </p>
      <p
        v-else-if="expired && open"
        class="alix-banner alix-tone-danger alix-ma-0"
        role="status"
      >
        {{ t("confirm.expired") }}
      </p>
      <p
        v-else-if="open"
        class="alix-d-flex alix-align-center alix-ga-1 alix-text-muted alix-text-sm alix-ma-0"
      >
        <AlixIcon
          :size="16"
          name="clock"
        />
        {{ t("confirm.expires", { time: formatDateTime(confirmation.expiresAt) }) }}
      </p>
      <p
        v-if="status === 'failed'"
        class="alix-banner alix-tone-danger alix-ma-0"
        role="alert"
      >
        {{ t("confirm.failed") }}
      </p>
      <slot
        :approve="() => answer('approve')"
        :busy="busy"
        :answerable="canAnswer"
        :decline="() => answer('decline')"
        :dismiss="() => answer('dismiss')"
        :status="status"
        name="actions"
      >
        <footer
          v-if="open || busy"
          class="alix-d-flex alix-flex-wrap alix-justify-end alix-ga-2"
        >
          <template v-if="stepUp">
            <button
              class="alix-btn alix-btn-primary"
              type="button"
              @click="emit('continue-in-app')"
            >
              {{ t("confirm.stepUp.cta") }}
              <AlixIcon
                :size="16"
                name="external"
              />
            </button>
          </template>
          <template v-else>
            <button
              :disabled="!open || busy || blocked"
              class="alix-btn"
              type="button"
              @click="answer('decline')"
            >
              {{ t("confirm.decline") }}
            </button>
            <button
              :disabled="!canAnswer || busy"
              :aria-busy="busy"
              class="alix-btn alix-btn-primary"
              type="button"
              @click="answer('approve')"
            >
              <span
                v-if="busy"
                class="alix-spinner"
                aria-hidden="true"
              />
              {{ busy ? t("confirm.submitting") : t("confirm.approve") }}
            </button>
          </template>
        </footer>
        <p
          v-else
          class="alix-d-flex alix-align-center alix-ga-2 alix-font-medium alix-ma-0"
          role="status"
        >
          <AlixIcon
            :name="statusIcon"
            :size="18"
          />
          {{ t(`confirm.${status}` as MessageKey) }}
        </p>
      </slot>
    </section>
  </div>
</template>

<style scoped lang="scss">
// The modal presentation: a bottom sheet over the page. The enter is a short slide with a strong ease-out; reduced motion
// collapses the duration through the token. The scrim only fades.
.alix-sheet-scrim {
  position: fixed;
  inset: 0;
  z-index: 19;
  background: var(--alix-scrim);
  animation: alix-sheet-fade var(--alix-dur-base) var(--alix-ease-out) both;
}

.alix-sheet-modal {
  position: fixed;
  inset: auto 0 0;
  z-index: 20;
  max-height: 85dvh;
  overflow: auto;
  border-end-start-radius: 0;
  border-end-end-radius: 0;
  outline: none;
  animation: alix-sheet-in var(--alix-dur-slow) var(--alix-ease-out) both;
}

@keyframes alix-sheet-in {
  from {
    transform: translateY(100%);
  }
}

@keyframes alix-sheet-fade {
  from {
    opacity: 0;
  }
}
</style>
