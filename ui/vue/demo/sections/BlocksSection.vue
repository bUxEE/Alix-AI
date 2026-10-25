<script setup lang="ts">
import {
  AlixActionCard,
  AlixChartBlock,
  AlixCitationList,
  AlixClarificationChoices,
  AlixConfirmationSheet,
  AlixDiagramBlock,
  AlixMarkdown,
  AlixReceiptCard,
  AlixSectionLoader,
  AlixSkeleton,
  AlixSwitch,
  AlixTableBlock,
  AlixVoiceOrb,
  type ConfirmationStatus,
  type VoiceOrbState,
} from "@alixai/vue";
import { computed, onActivated, onBeforeUnmount, onDeactivated, ref } from "vue";
import DemoFlowchart from "../components/DemoFlowchart.vue";
import DemoHostLog from "../components/DemoHostLog.vue";
import DemoSection from "../components/DemoSection.vue";
import DemoSpecimen from "../components/DemoSpecimen.vue";
import { useHostLog } from "../composables/useHostLog";
import {
  sampleBar,
  sampleCard,
  sampleChoices,
  sampleCitations,
  sampleConfirmation,
  sampleDiagram,
  sampleLine,
  sampleMarkdown,
  sampleReceipt,
  sampleTable,
} from "../samples";
import { locale, useDemoText } from "../state";
import { startSimulatedLevel } from "../voice";

// Every block and card of the kit on its own, with sample data in the shapes of the v1 schemas: what a host renders when it
// shows AlixAI's output outside the chat panel. The events each one reports go to the log at the end of the page.

const text = useDemoText();
const log = useHostLog<{ message: string }>(6);

const markdown = computed(() => sampleMarkdown(locale.value));
const bar = computed(() => sampleBar(locale.value));
const line = computed(() => sampleLine(locale.value));
const table = computed(() => sampleTable(locale.value));
const diagram = computed(() => sampleDiagram(locale.value));
const card = computed(() => sampleCard(locale.value));
const choices = computed(() => sampleChoices(locale.value));
const citations = computed(() => sampleCitations(locale.value));
const explicit = computed(() => sampleConfirmation(locale.value, "explicit"));
const stepUp = computed(() => sampleConfirmation(locale.value, "step_up"));

// The voice orb in each of its states. Listening and speaking follow a simulated voice level while the section is shown.
const ORB_STATES: readonly VoiceOrbState[] = ["idle", "listening", "thinking", "speaking", "muted", "error"];
const voiceLevel = ref(0);
let stopLevel = (): void => undefined;
const startLevel = (): void => {
  stopLevel();
  stopLevel = startSimulatedLevel((level) => {
    voiceLevel.value = level;
  });
};
onActivated(startLevel);
onDeactivated(() => stopLevel());
onBeforeUnmount(() => stopLevel());

const DISPLAYS = ["panel", "inline", "footnote"] as const;
const display = ref<(typeof DISPLAYS)[number]>("panel");
const RECEIPTS = ["committed", "queued", "rejected"] as const;
const receipt = ref<(typeof RECEIPTS)[number]>("committed");
const shownReceipt = computed(() => sampleReceipt(locale.value, receipt.value));

const answered = ref(false);
const status = ref<ConfirmationStatus>("pending");
const modalOpen = ref(false);
const modalStatus = ref<ConfirmationStatus>("pending");

// The sheet reports a decision and commits nothing: here the "host" pretends to send it and comes back after a moment.
const answer = (which: "inline" | "modal", outcome: "approved" | "declined" | "dismissed"): void => {
  const target = which === "inline" ? status : modalStatus;
  target.value = "submitting";
  log.push({ message: text(`blocks.log.${outcome}`) });
  setTimeout(() => {
    target.value = outcome;
  }, 900);
};

const openModal = (): void => {
  modalStatus.value = "pending";
  modalOpen.value = true;
};

const closeModalAfter = (outcome: "approved" | "declined" | "dismissed"): void => {
  answer("modal", outcome);
  setTimeout(() => {
    modalOpen.value = false;
  }, outcome === "dismissed" ? 0 : 1300);
};

const resetConfirmation = (): void => {
  status.value = "pending";
};

const loading = ref(true);
</script>

<template>
  <DemoSection>
    <template #eyebrow>
      blocks · cards · loaders
    </template>
    <template #title>
      {{ text("nav.blocks.title") }}
    </template>
    <template #lede>
      {{ text("blocks.lede") }}
    </template>
    <div class="alix-d-flex alix-flex-column alix-ga-8">
      <DemoSpecimen>
        <template #name>
          AlixVoiceOrb
        </template>
        <template #note>
          {{ text("blocks.orb") }}
        </template>
        <ul class="alix-card alix-reset-list alix-pa-6 alix-d-flex alix-flex-wrap alix-justify-center alix-ga-6">
          <li
            v-for="state in ORB_STATES"
            :key="state"
            class="alix-d-flex alix-flex-column alix-align-center alix-ga-2"
          >
            <AlixVoiceOrb
              :level="state === 'listening' || state === 'speaking' ? voiceLevel : 0"
              :size="112"
              :state="state"
            />
            <span class="alix-font-mono alix-text-xs alix-text-muted">
              {{ state }}
            </span>
          </li>
        </ul>
      </DemoSpecimen>
      <div class="alix-row alix-demo-gallery-row">
        <DemoSpecimen class="alix-col-12 alix-col-lg-5 alix-sticky-lg-top">
          <template #name>
            AlixMarkdown
          </template>
          <template #note>
            {{ text("blocks.markdown") }}
          </template>
          <div class="alix-card alix-pa-4">
            <AlixMarkdown :source="markdown" />
          </div>
        </DemoSpecimen>
        <DemoSpecimen class="alix-col-12 alix-col-lg-7 alix-flex-1 alix-sticky-lg-top">
          <template #name>
            AlixTableBlock
          </template>
          <template #note>
            {{ text("blocks.table") }}
          </template>
          <AlixTableBlock :block="table" />
        </DemoSpecimen>
      </div>
      <div class="alix-row alix-demo-gallery-row">
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-sticky-md-top">
          <template #name>
            AlixChartBlock · bar
          </template>
          <template #note>
            {{ text("blocks.chart") }}
          </template>
          <AlixChartBlock :block="bar" />
        </DemoSpecimen>
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-flex-1 alix-sticky-md-top">
          <template #name>
            AlixChartBlock · line
          </template>
          <template #note>
            {{ text("blocks.chart") }}
          </template>
          <AlixChartBlock :block="line" />
        </DemoSpecimen>
      </div>
      <div class="alix-row alix-demo-gallery-row">
        <DemoSpecimen class="alix-col-12 alix-col-lg-7 alix-sticky-lg-top">
          <template #name>
            AlixDiagramBlock · #renderer
          </template>
          <template #note>
            {{ text("blocks.diagram.renderer") }}
          </template>
          <AlixDiagramBlock :block="diagram">
            <template #renderer="{ block }">
              <DemoFlowchart :block="block" />
            </template>
          </AlixDiagramBlock>
        </DemoSpecimen>
        <DemoSpecimen class="alix-col-12 alix-col-lg-5 alix-flex-1 alix-sticky-lg-top">
          <template #name>
            AlixDiagramBlock
          </template>
          <template #note>
            {{ text("blocks.diagram.plain") }}
          </template>
          <AlixDiagramBlock :block="diagram" />
        </DemoSpecimen>
      </div>
      <div class="alix-row alix-demo-gallery-row">
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-sticky-md-top">
          <template #name>
            AlixActionCard
          </template>
          <template #note>
            {{ text("blocks.card") }}
          </template>
          <AlixActionCard
            :block="card"
            @action="log.push({ message: text('blocks.log.action', { action: $event.label ?? $event.action }) })"
          />
        </DemoSpecimen>
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-flex-1 alix-sticky-md-top">
          <template #name>
            AlixClarificationChoices
          </template>
          <template #note>
            {{ text("blocks.choices") }}
          </template>
          <AlixClarificationChoices
            :block="choices"
            :answered="answered"
            @choose="answered = true; log.push({ message: text('blocks.log.choice', { id: $event }) })"
            @free-text="log.push({ message: text('blocks.log.freeText', { text: $event }) })"
          />
          <button
            v-if="answered"
            class="alix-btn alix-btn-quiet alix-self-start alix-demo-btn-xs"
            type="button"
            @click="answered = false"
          >
            {{ text("blocks.reset") }}
          </button>
        </DemoSpecimen>
      </div>
      <div class="alix-row alix-demo-gallery-row">
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-sticky-md-top">
          <template #name>
            AlixConfirmationSheet
          </template>
          <template #note>
            {{ text("blocks.confirm") }}
          </template>
          <AlixConfirmationSheet
            :confirmation="explicit"
            :status="status"
            @approve="answer('inline', 'approved')"
            @decline="answer('inline', 'declined')"
            @dismiss="answer('inline', 'dismissed')"
          />
          <div class="alix-d-flex alix-flex-wrap alix-ga-2">
            <button
              class="alix-btn"
              type="button"
              @click="openModal"
            >
              {{ text("blocks.confirm.modal") }}
            </button>
            <button
              v-if="status !== 'pending'"
              class="alix-btn alix-btn-quiet"
              type="button"
              @click="resetConfirmation"
            >
              {{ text("blocks.reset") }}
            </button>
          </div>
          <AlixConfirmationSheet
            v-if="modalOpen"
            :confirmation="explicit"
            :status="modalStatus"
            modal
            @approve="closeModalAfter('approved')"
            @decline="closeModalAfter('declined')"
            @dismiss="closeModalAfter('dismissed')"
          />
        </DemoSpecimen>
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-flex-1 alix-sticky-md-top">
          <template #name>
            AlixConfirmationSheet · step_up
          </template>
          <template #note>
            {{ text("blocks.stepUp") }}
          </template>
          <AlixConfirmationSheet
            :confirmation="stepUp"
            @continue-in-app="log.push({ message: text('blocks.log.continue') })"
          />
        </DemoSpecimen>
      </div>
      <div class="alix-row alix-demo-gallery-row">
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-sticky-md-top">
          <template #name>
            AlixCitationList · {{ display }}
          </template>
          <template #note>
            {{ text("blocks.citations") }}
          </template>
          <div
            :aria-label="text('blocks.citations.display')"
            class="alix-d-flex alix-demo-segmented alix-self-start"
            role="radiogroup"
          >
            <button
              v-for="option in DISPLAYS"
              :key="option"
              :aria-checked="display === option"
              class="alix-demo-segment alix-font-mono alix-text-xs"
              role="radio"
              type="button"
              @click="display = option"
            >
              {{ option }}
            </button>
          </div>
          <AlixCitationList
            :citations="citations"
            :display="display"
            @open-citation="log.push({ message: text('blocks.log.citation', { name: $event.display_name }) })"
          />
        </DemoSpecimen>
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-flex-1 alix-sticky-md-top">
          <template #name>
            AlixReceiptCard · {{ receipt }}
          </template>
          <template #note>
            {{ text("blocks.receipt") }}
          </template>
          <div
            :aria-label="text('blocks.receipt.status')"
            class="alix-d-flex alix-demo-segmented alix-self-start"
            role="radiogroup"
          >
            <button
              v-for="option in RECEIPTS"
              :key="option"
              :aria-checked="receipt === option"
              class="alix-demo-segment alix-font-mono alix-text-xs"
              role="radio"
              type="button"
              @click="receipt = option"
            >
              {{ option }}
            </button>
          </div>
          <AlixReceiptCard :receipt="shownReceipt" />
        </DemoSpecimen>
      </div>
      <div class="alix-row alix-demo-gallery-row">
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-sticky-md-top">
          <template #name>
            AlixSkeleton · AlixSectionLoader
          </template>
          <template #note>
            {{ text("blocks.loaders") }}
          </template>
          <div class="alix-row">
            <div class="alix-col-12 alix-col-sm-6 alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-2">
              <AlixSkeleton width="40%" />
              <AlixSkeleton width="90%" />
              <AlixSkeleton width="75%" />
              <AlixSkeleton
                width="60%"
                height="10px"
              />
            </div>
            <div class="alix-col-12 alix-col-sm-6 alix-flex-1 alix-card alix-pa-4 alix-position-relative alix-d-flex alix-flex-column alix-ga-1">
              <p class="alix-ma-0 alix-font-medium">
                Harbor Point
              </p>
              <p class="alix-ma-0 alix-text-sm alix-text-muted">
                {{ text("blocks.loaders.content") }}
              </p>
              <AlixSectionLoader v-if="loading" />
            </div>
          </div>
          <AlixSwitch v-model="loading">
            {{ text("blocks.loaders.toggle") }}
            <template #hint>
              {{ text("blocks.loaders.hint") }}
            </template>
          </AlixSwitch>
        </DemoSpecimen>
        <DemoSpecimen class="alix-col-12 alix-col-md-6 alix-flex-1 alix-sticky-md-top">
          <template #name>
            {{ text("blocks.log") }}
          </template>
          <template #note>
            {{ text("blocks.log.note") }}
          </template>
          <div class="alix-card alix-pa-4">
            <DemoHostLog :entries="log.entries.value">
              <template #entry="{ entry }">
                {{ entry.message }}
              </template>
              <template #empty>
                {{ text("blocks.log.empty") }}
              </template>
            </DemoHostLog>
          </div>
        </DemoSpecimen>
      </div>
    </div>
  </DemoSection>
</template>
