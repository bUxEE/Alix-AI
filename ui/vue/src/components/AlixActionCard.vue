<script setup lang="ts">
import { useAlix, type MessageKey } from "../i18n";
import { formatCanonicalValue } from "../agui/parse";
import type { ActionCardBlock, PermittedAction } from "../sdk-types";

// A structured application result or an offer of next steps: a title, the facts the host stated, and the actions the user may
// take from here. The facts are the application's, shown as text. An action is only ever reported (`action` event): the card
// invokes nothing itself, and confirming or rejecting a prepared action goes through the confirmation flow, never through here.

defineProps<{
  block: ActionCardBlock;
}>();

defineEmits<{
  action: [action: PermittedAction];
}>();

const { t } = useAlix();

const labelOf = (action: PermittedAction): string => action.label ?? t(`action.${action.action}` as MessageKey);
</script>

<template>
  <section class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
    <slot
      :block="block"
      name="header"
    >
      <h3 class="alix-heading">
        {{ block.title }}
      </h3>
    </slot>
    <dl
      v-if="block.facts.length > 0"
      class="alix-ma-0 alix-d-flex alix-flex-column alix-ga-2"
    >
      <div
        v-for="(fact, index) in block.facts"
        :key="`${fact.field}-${index}`"
        class="alix-d-flex alix-flex-wrap alix-justify-between alix-ga-2"
      >
        <slot
          :fact="fact"
          name="fact"
        >
          <dt class="alix-text-muted alix-text-sm">
            {{ fact.field }}
          </dt>
          <dd class="alix-ma-0 alix-font-medium alix-break-word">
            {{ fact.display ?? formatCanonicalValue(fact.value) }}
          </dd>
        </slot>
      </div>
    </dl>
    <div
      v-if="block.actions.length > 0"
      class="alix-d-flex alix-flex-wrap alix-ga-2"
    >
      <slot
        :actions="block.actions"
        name="actions"
      >
        <button
          v-for="(action, index) in block.actions"
          :key="`${action.action}-${index}`"
          class="alix-btn"
          type="button"
          @click="$emit('action', action)"
        >
          {{ labelOf(action) }}
        </button>
      </slot>
    </div>
  </section>
</template>
