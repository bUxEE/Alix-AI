<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, provide, ref, useSlots, watch } from "vue";
import type { AlixAttachment, AlixConfirmation, ChatItem } from "../agui/types";
import { useReducedMotion } from "../composables/useBrowser";
import { useAlix } from "../i18n";
import type {
  ActionCardBlock,
  ChartBlock,
  Citation,
  ClarificationChoicesBlock,
  ConfirmationPanelBlock,
  DiagramBlock,
  PermittedAction,
  TableBlock,
} from "../sdk-types";
import AlixActionCard from "./AlixActionCard.vue";
import AlixCitationList from "./AlixCitationList.vue";
import AlixConfirmationSheet from "./AlixConfirmationSheet.vue";
import AlixFilePreview from "./AlixFilePreview.vue";
import AlixReceiptCard from "./AlixReceiptCard.vue";
import AlixChartBlock from "./blocks/AlixChartBlock.vue";
import AlixClarificationChoices from "./blocks/AlixClarificationChoices.vue";
import AlixDiagramBlock from "./blocks/AlixDiagramBlock.vue";
import AlixMarkdown from "./blocks/AlixMarkdown.vue";
import AlixTableBlock from "./blocks/AlixTableBlock.vue";
import AlixIcon from "./common/AlixIcon.vue";
import { RUN_IN_FLIGHT } from "./run-state";

// The timeline of a conversation. It owns the flow (order, scrolling, announcements); what each entry looks like is the entry's
// component, replaceable per kind through a slot. The log keeps the reader's place: while they are at the bottom it follows new
// text, and once they scroll up it stops and offers "Jump to latest". `aria-busy` is set while a reply streams, so a screen reader
// announces the finished message instead of every fragment. The `diagram` slot is the renderer of diagram blocks (a host's own
// Mermaid renderer, given `block`); without it a diagram shows its text alternative and its source as text.

const props = withDefaults(
  defineProps<{
    items: readonly ChatItem[];
    /** True while a run is in flight. */
    running?: boolean;
    /** Shows link addresses in assistant text. Turn it off when the host owns routing. */
    links?: boolean;
    citationDisplay?: "inline" | "footnote" | "panel";
  }>(),
  { running: false, links: true, citationDisplay: "panel" },
);

const emit = defineEmits<{
  approve: [itemId: string];
  decline: [itemId: string];
  dismiss: [itemId: string];
  choose: [choiceId: string, itemId: string];
  "free-text": [text: string];
  "open-citation": [citation: Citation];
  action: [action: PermittedAction];
  "continue-in-app": [itemId: string];
  "open-file": [file: AlixAttachment];
  "download-file": [file: AlixAttachment];
  retry: [];
}>();

const { t } = useAlix();
const reduced = useReducedMotion();
provide(RUN_IN_FLIGHT, computed(() => props.running));
const scroller = ref<HTMLElement | null>(null);
const atBottom = ref(true);
const unseen = ref(false);

const NEAR = 48;

const onScroll = (): void => {
  const el = scroller.value;
  if (el === null) return;
  atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight <= NEAR;
  if (atBottom.value) unseen.value = false;
};

const toBottom = (smooth: boolean): void => {
  const el = scroller.value;
  if (el === null) return;
  if (typeof el.scrollTo === "function") el.scrollTo({ top: el.scrollHeight, behavior: smooth && !reduced.value ? "smooth" : "auto" });
  else el.scrollTop = el.scrollHeight;
  atBottom.value = true;
  unseen.value = false;
};

// Follows new content, in the same frame the text grows, only while the reader is at the bottom. A message the user just sent
// always brings them to it: they asked, so they look at the answer.
watch(
  () => [props.items.length, props.items[props.items.length - 1]],
  async () => {
    await nextTick();
    const last = props.items[props.items.length - 1];
    if (atBottom.value || (last?.kind === "message" && last.role === "user")) toBottom(false);
    else unseen.value = true;
  },
  { deep: true, flush: "post" },
);

// A chart, an image preview or a table grows after it is drawn, without a scroll event: while the reader is at the bottom the
// view stays pinned to it as the entries resize.
let resized: ResizeObserver | undefined;
const observeEntries = (): void => {
  const el = scroller.value;
  if (resized === undefined || el === null) return;
  resized.disconnect();
  for (const child of Array.from(el.children)) resized.observe(child);
};
watch(
  () => props.items.length,
  async () => {
    await nextTick();
    observeEntries();
  },
  { flush: "post" },
);
onMounted(() => {
  if (typeof ResizeObserver === "undefined") return;
  resized = new ResizeObserver(() => {
    if (atBottom.value) toBottom(false);
  });
  observeEntries();
});
onBeforeUnmount(() => resized?.disconnect());

watch(
  () => props.items.length,
  async (now, before) => {
    // A history load or a new conversation replaces the whole timeline: start at the end.
    if (before === 0 && now > 0) {
      await nextTick();
      toBottom(false);
    }
  },
);

onMounted(() => toBottom(false));

// A prepared action arrives twice: as a `confirmation_panel` block, where the answer places it, and as the interrupt that makes it
// answerable (explicit confirmation) or the hand-over to the application (step up). It is presented ONCE, where the panel is: the
// panel takes the interrupt's entry (its state, its id for the answer, the `confirmation` or `continue` slot) and that entry is
// not drawn a second time. A panel with neither (the run is still streaming, or its interrupt is gone) shows what would change and
// offers no answer. When the host draws blocks itself (the `block` slot) it owns that choice, and every entry is drawn as before.
type ConfirmationItem = Extract<ChatItem, { kind: "confirmation" }>;
type ContinueItem = Extract<ChatItem, { kind: "continue" }>;

interface PanelView {
  readonly key: string;
  /** What the panel itself says, for the sheet when no entry is bound to it. */
  readonly preview: AlixConfirmation;
  readonly confirmation: ConfirmationItem | undefined;
  readonly handOver: ContinueItem | undefined;
}

const slots = useSlots();
const panelPreparations = computed(() => {
  const ids = new Set<string>();
  if (slots.block !== undefined) return ids;
  for (const item of props.items) {
    if (item.kind === "block" && item.blockType === "confirmation_panel") ids.add((item.block as ConfirmationPanelBlock).preparation_id);
  }
  return ids;
});

const drawnByPanel = (item: ChatItem): boolean =>
  (item.kind === "confirmation" && panelPreparations.value.has(item.confirmation.preparationId)) ||
  (item.kind === "continue" && item.preparationId !== undefined && panelPreparations.value.has(item.preparationId));

const panelView = (id: string, block: ConfirmationPanelBlock): PanelView => ({
  key: `${id}_panel`,
  preview: {
    preparationId: block.preparation_id,
    capabilityId: "",
    summary: block.summary,
    effects: block.effects,
    riskLevel: block.risk_level,
    expiresAt: block.expires_at,
    mode: block.confirmation?.mode ?? "explicit",
  },
  confirmation: props.items.find((item): item is ConfirmationItem => item.kind === "confirmation" && item.confirmation.preparationId === block.preparation_id),
  handOver: props.items.find((item): item is ContinueItem => item.kind === "continue" && item.preparationId === block.preparation_id),
});

defineExpose({ toBottom: (): void => toBottom(true) });
</script>

<template>
  <div class="alix-root alix-position-relative alix-d-flex alix-flex-column alix-min-h-0 alix-flex-grow-1">
    <div
      :aria-busy="running"
      :aria-label="t('chat.log')"
      ref="scroller"
      class="alix-flex-grow-1 alix-overflow-auto alix-px-4 alix-py-4 alix-d-flex alix-flex-column alix-ga-4"
      role="log"
      aria-live="polite"
      aria-relevant="additions"
      tabindex="0"
      @scroll.passive="onScroll"
    >
      <slot
        v-if="items.length === 0"
        name="empty"
      >
        <div class="alix-my-auto alix-text-center alix-d-flex alix-flex-column alix-ga-2 alix-px-4">
          <p class="alix-heading alix-text-lg">
            {{ t("chat.empty.title") }}
          </p>
          <p class="alix-text-muted alix-ma-0 alix-text-balance">
            {{ t("chat.empty.body") }}
          </p>
        </div>
      </slot>
      <template
        v-for="item in items"
        :key="item.id"
      >
        <div
          v-if="item.kind === 'message'"
          :class="item.role === 'user' ? 'alix-self-end alix-bubble-user' : 'alix-w-100'"
          class="alix-enter alix-min-w-0"
        >
          <slot
            :item="item"
            name="message"
          >
            <p class="alix-sr-only">
              {{ item.role === "user" ? t("chat.you") : t("chat.assistant") }}
            </p>
            <p
              v-if="item.role === 'user'"
              class="alix-ma-0 alix-break-word alix-pre-wrap"
            >
              {{ item.text }}
            </p>
            <AlixMarkdown
              v-else
              :source="item.text"
              :links="links"
            />
          </slot>
        </div>
        <div
          v-else-if="item.kind === 'attachments'"
          class="alix-enter alix-self-end alix-d-flex alix-flex-wrap alix-ga-2"
        >
          <slot
            :item="item"
            name="attachments"
          >
            <AlixFilePreview
              v-for="file in item.files"
              :key="file.fileId"
              :file="file"
              @download="emit('download-file', $event)"
              @open="emit('open-file', $event)"
            />
          </slot>
        </div>
        <div
          v-else-if="item.kind === 'block'"
          class="alix-enter"
        >
          <slot
            :item="item"
            name="block"
          >
            <AlixTableBlock
              v-if="item.blockType === 'table'"
              :block="item.block as TableBlock"
            />
            <AlixChartBlock
              v-else-if="item.blockType === 'chart'"
              :block="item.block as ChartBlock"
            />
            <AlixDiagramBlock
              v-else-if="item.blockType === 'diagram'"
              :block="item.block as DiagramBlock"
            >
              <template
                v-if="slots.diagram"
                #renderer="scope"
              >
                <slot
                  name="diagram"
                  v-bind="scope"
                />
              </template>
            </AlixDiagramBlock>
            <AlixActionCard
              v-else-if="item.blockType === 'action_card'"
              :block="item.block as ActionCardBlock"
              @action="emit('action', $event)"
            />
            <AlixClarificationChoices
              v-else-if="item.blockType === 'clarification_choices'"
              :answered="item.answered"
              :block="item.block as ClarificationChoicesBlock"
              @choose="emit('choose', $event, item.id)"
              @free-text="emit('free-text', $event)"
            />
            <AlixCitationList
              v-else-if="item.blockType === 'citation_list'"
              :citations="(item.block as { citations: Citation[] }).citations"
              :display="citationDisplay"
              @open-citation="emit('open-citation', $event)"
            />
            <template
              v-for="view in item.blockType === 'confirmation_panel' ? [panelView(item.id, item.block as ConfirmationPanelBlock)] : []"
              :key="view.key"
            >
              <slot
                v-if="view.confirmation"
                :item="view.confirmation"
                name="confirmation"
              >
                <AlixConfirmationSheet
                  :confirmation="view.confirmation.confirmation"
                  :status="view.confirmation.status"
                  @approve="emit('approve', view.confirmation.id)"
                  @continue-in-app="emit('continue-in-app', view.confirmation.id)"
                  @decline="emit('decline', view.confirmation.id)"
                  @dismiss="emit('dismiss', view.confirmation.id)"
                />
              </slot>
              <slot
                v-else-if="view.handOver"
                :item="view.handOver"
                name="continue"
              >
                <AlixConfirmationSheet
                  :confirmation="view.preview"
                  @continue-in-app="emit('continue-in-app', view.handOver.id)"
                />
              </slot>
              <AlixConfirmationSheet
                v-else
                :confirmation="view.preview"
                @continue-in-app="emit('continue-in-app', item.id)"
              >
                <template
                  v-if="view.preview.mode !== 'step_up'"
                  #actions
                >
                  <p class="alix-text-muted alix-text-sm alix-ma-0">
                    {{ t("confirm.preparing") }}
                  </p>
                </template>
              </AlixConfirmationSheet>
            </template>
          </slot>
        </div>
        <div
          v-else-if="item.kind === 'citations'"
          class="alix-enter"
        >
          <slot
            :item="item"
            name="citations"
          >
            <AlixCitationList
              :citations="item.citations"
              :display="citationDisplay"
              @open-citation="emit('open-citation', $event)"
            />
          </slot>
        </div>
        <div
          v-else-if="item.kind === 'receipt'"
          class="alix-enter"
        >
          <slot
            :item="item"
            name="receipt"
          >
            <AlixReceiptCard :receipt="item.receipt" />
          </slot>
        </div>
        <div
          v-else-if="item.kind === 'confirmation' && !drawnByPanel(item)"
          class="alix-enter"
        >
          <slot
            :item="item"
            name="confirmation"
          >
            <AlixConfirmationSheet
              :confirmation="item.confirmation"
              :status="item.status"
              @approve="emit('approve', item.id)"
              @continue-in-app="emit('continue-in-app', item.id)"
              @decline="emit('decline', item.id)"
              @dismiss="emit('dismiss', item.id)"
            />
          </slot>
        </div>
        <div
          v-else-if="item.kind === 'continue' && !drawnByPanel(item)"
          class="alix-enter"
        >
          <slot
            :item="item"
            name="continue"
          >
            <div class="alix-banner alix-tone-warning alix-d-flex alix-flex-wrap alix-align-center alix-justify-between alix-ga-2">
              <span class="alix-break-word">
                {{ item.summary || t("confirm.stepUp.body") }}
              </span>
              <button
                class="alix-btn"
                type="button"
                @click="emit('continue-in-app', item.id)"
              >
                {{ t("confirm.stepUp.cta") }}
                <AlixIcon
                  :size="16"
                  name="external"
                />
              </button>
            </div>
          </slot>
        </div>
        <div
          v-else-if="item.kind === 'error'"
          class="alix-enter"
        >
          <slot
            :item="item"
            name="error"
          >
            <div
              class="alix-banner alix-tone-danger alix-d-flex alix-flex-wrap alix-align-center alix-justify-between alix-ga-2"
              role="alert"
            >
              <span class="alix-break-word">
                {{ item.message || t("chat.runFailed") }}
              </span>
              <button
                class="alix-btn"
                type="button"
                @click="emit('retry')"
              >
                <AlixIcon
                  :size="16"
                  name="refresh"
                />
                {{ t("common.retry") }}
              </button>
            </div>
          </slot>
        </div>
      </template>
    </div>
    <Transition name="alix-jump">
      <button
        v-if="!atBottom && (unseen || running)"
        class="alix-btn alix-jump alix-card-raised"
        type="button"
        @click="toBottom(true)"
      >
        <AlixIcon
          :size="16"
          name="arrow-down"
        />
        {{ t("chat.jump") }}
      </button>
    </Transition>
  </div>
</template>

<style scoped lang="scss">
// The user's own message: a quiet tinted bubble, right-aligned by the utility on the element. The assistant has no bubble.
.alix-bubble-user {
  max-width: min(80%, 60ch);
  padding: 10px 14px;
  border-radius: var(--alix-radius-lg);
  background: var(--alix-accent-soft);
}

.alix-pre-wrap {
  white-space: pre-wrap;
}

// "Jump to latest" floats above the log and rises in; the exit is faster than the enter.
.alix-jump {
  position: absolute;
  inset: auto auto 12px 50%;
  z-index: 3;
  translate: -50% 0;
}

.alix-jump-enter-active {
  transition: opacity var(--alix-dur-base) var(--alix-ease-out), transform var(--alix-dur-base) var(--alix-ease-out);
}

.alix-jump-leave-active {
  transition: opacity var(--alix-dur-press) ease;
}

.alix-jump-enter-from {
  opacity: 0;
  transform: translateY(8px);
}

.alix-jump-leave-to {
  opacity: 0;
}
</style>
