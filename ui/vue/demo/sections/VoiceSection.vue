<script setup lang="ts">
import { voice } from "@alixai/sdk";
import {
  AlixSwitch,
  AlixTranscriptEditor,
  AlixVoiceButton,
  AlixVoiceMode,
  createAlixMicrophone,
  createAlixPlayer,
  type AlixPlayer,
  type AlixVoiceFallback,
  type AlixVoiceReply,
  type AlixVoiceTurn,
} from "@alixai/vue";
import { computed, onActivated, onBeforeUnmount, onDeactivated, ref, shallowRef, watch } from "vue";
import { demoVoiceOptions } from "../../../demo-mock/src/index";
import DemoHostLog from "../components/DemoHostLog.vue";
import DemoSection from "../components/DemoSection.vue";
import { useHostLog } from "../composables/useHostLog";
import { locale, useDemoText } from "../state";
import { createSimulatedMicrophone } from "../voice";

// Voice Mode over the mock's voice socket, its orb first on the page (`WS /v1/sessions/{id}/voice`): the host makes the session (`voice.createVoiceSession`
// with the mock's options), the component does the rest. The microphone is simulated unless the reader turns the device's own
// on; replies are spoken through Web Audio unless they turn speech off. The session is rebuilt when the language or the speech
// setting changes, and closed when the section is left (the component never closes the host's session).
// Beside it: the voice button in each of its states, and the transcript editor on its own.

const text = useDemoText();
const SESSION_ID = "ses_demo_voice";

const useDevice = ref(false);
const speak = ref(true);
const simulated = createSimulatedMicrophone();
const device = createAlixMicrophone();
const microphone = computed(() => (useDevice.value ? device : simulated));
const player = shallowRef<AlixPlayer | null>(null);
const session = shallowRef<voice.VoiceSession>();
const active = ref(false);

const build = (): void => {
  session.value?.close();
  player.value?.close();
  player.value = speak.value ? createAlixPlayer() : null;
  session.value = voice.createVoiceSession({ ...demoVoiceOptions(SESSION_ID, { language: locale.value }), speak: speak.value });
};

const release = (): void => {
  session.value?.close();
  session.value = undefined;
  player.value?.close();
  player.value = null;
};

onActivated(() => {
  active.value = true;
  build();
});
onDeactivated(() => {
  active.value = false;
  release();
});
onBeforeUnmount(release);
watch([locale, speak], () => {
  if (active.value) build();
});

type VoiceEvent =
  | { kind: "heard"; text: string; corrected: boolean }
  | { kind: "reply"; text: string; spoken: boolean }
  | { kind: "turn"; act: string; confirm: boolean }
  | { kind: "fallback"; reason: AlixVoiceFallback }
  | { kind: "editor"; text: string };

const log = useHostLog<VoiceEvent>(10);
const onTranscript = (transcript: { text: string; final: boolean; corrected: boolean }): void => {
  if (transcript.final) log.push({ kind: "heard", text: transcript.text, corrected: transcript.corrected });
};
const onReply = (reply: AlixVoiceReply): void => log.push({ kind: "reply", text: reply.text, spoken: reply.spoken });
const onTurn = (turn: AlixVoiceTurn): void => log.push({ kind: "turn", act: turn.act, confirm: turn.confirmationRequired });
const onFallback = (reason: AlixVoiceFallback): void => log.push({ kind: "fallback", reason });

const describe = (entry: VoiceEvent): string => {
  switch (entry.kind) {
    case "heard":
      return text(entry.corrected ? "voice.log.corrected" : "voice.log.heard", { text: entry.text });
    case "reply":
      return text(entry.spoken ? "voice.log.spoken" : "voice.log.typed", { text: entry.text });
    case "turn":
      return text(entry.confirm ? "voice.log.turnConfirm" : "voice.log.turn", { act: entry.act });
    case "fallback":
      return text("voice.log.fallback", { reason: entry.reason });
    default:
      return text("voice.log.editor", { text: entry.text });
  }
};

const BUTTON_STATES = ["idle", "listening", "processing", "unavailable"] as const;
const heard = computed(() => (locale.value === "it" ? "Prenota un'ispezione antincendio a Riverside per martedì mattina" : "Book a fire-safety inspection at Riverside for Tuesday morning"));
const draft = ref(heard.value);
watch(heard, (next) => {
  draft.value = next;
});
</script>

<template>
  <DemoSection>
    <template #eyebrow>
      AlixVoiceMode · AlixVoiceOrb · AlixVoiceButton · AlixTranscriptEditor · voice.createVoiceSession
    </template>
    <template #title>
      {{ text("nav.voice.title") }}
    </template>
    <template #lede>
      {{ text("voice.lede") }}
    </template>
    <div class="alix-row">
      <div class="alix-col-12 alix-col-lg-7 alix-sticky-lg-top alix-d-flex alix-flex-column alix-ga-4">
        <div class="alix-card alix-demo-voice-stage">
          <AlixVoiceMode
            v-if="session"
            :key="`${locale}-${speak}`"
            :session="session"
            :microphone="microphone"
            :player="player"
            :language="locale"
            @fallback="onFallback"
            @response="onReply"
            @transcript="onTranscript"
            @turn="onTurn"
          />
        </div>
        <div class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-4">
          <AlixSwitch v-model="useDevice">
            {{ text("voice.device") }}
            <template #hint>
              {{ text("voice.device.hint") }}
            </template>
          </AlixSwitch>
          <AlixSwitch v-model="speak">
            {{ text("voice.speak") }}
            <template #hint>
              {{ text("voice.speak.hint") }}
            </template>
          </AlixSwitch>
        </div>
      </div>
      <div class="alix-col-12 alix-col-lg-5 alix-sticky-lg-top alix-flex-1 alix-d-flex alix-flex-column alix-ga-4">
        <section class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ text("voice.turns") }}
          </h2>
          <DemoHostLog :entries="log.entries.value">
            <template #entry="{ entry }">
              {{ describe(entry) }}
            </template>
            <template #empty>
              {{ text("voice.turns.empty") }}
            </template>
          </DemoHostLog>
        </section>
        <section class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ text("voice.buttons") }}
          </h2>
          <ul class="alix-reset-list alix-d-flex alix-flex-wrap alix-ga-5">
            <li
              v-for="state in BUTTON_STATES"
              :key="state"
              class="alix-d-flex alix-flex-column alix-align-center alix-ga-2"
            >
              <AlixVoiceButton
                :state="state"
                mode="toggle"
              />
              <span class="alix-font-mono alix-text-xs alix-text-muted">
                {{ state }}
              </span>
            </li>
            <li class="alix-d-flex alix-flex-column alix-align-center alix-ga-2">
              <AlixVoiceButton
                mode="push-to-talk"
                size="lg"
              />
              <span class="alix-font-mono alix-text-xs alix-text-muted">
                push-to-talk · lg
              </span>
            </li>
          </ul>
        </section>
        <section class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
          <h2 class="alix-ma-0 alix-demo-subtitle">
            {{ text("voice.editor") }}
          </h2>
          <AlixTranscriptEditor
            v-model="draft"
            :original="heard"
            @confirm="log.push({ kind: 'editor', text: $event })"
            @discard="draft = heard"
          />
        </section>
      </div>
    </div>
  </DemoSection>
</template>
