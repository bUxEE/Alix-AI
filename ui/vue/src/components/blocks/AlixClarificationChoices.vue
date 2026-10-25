<script setup lang="ts">
import { ref, useId } from "vue";
import { useAlix } from "../../i18n";
import type { ClarificationChoicesBlock } from "../../sdk-types";

// A question the user answers by selecting one choice. Selecting reports the choice's id (a structured answer, no model involved);
// when free text is allowed the user may instead type an answer, reported as `free-text`, which the core validates like any other
// turn. The wording of each choice is the host's data; the question is the model's wording, verified against the directive.

withDefaults(
  defineProps<{
    block: ClarificationChoicesBlock;
    /** True once the question has been answered: the choices stay visible but can no longer be used. */
    answered?: boolean;
  }>(),
  { answered: false },
);

const emit = defineEmits<{
  choose: [choiceId: string];
  "free-text": [text: string];
}>();

const { t } = useAlix();
const uid = useId();
const draft = ref("");

const submit = (): void => {
  const text = draft.value.trim();
  if (text === "") return;
  emit("free-text", text);
  draft.value = "";
};
</script>

<template>
  <div
    :aria-labelledby="`${uid}-q`"
    class="alix-root alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3"
    role="group"
  >
    <p
      :id="`${uid}-q`"
      class="alix-heading"
    >
      {{ block.prompt ?? t("clarification.title") }}
    </p>
    <ul class="alix-reset-list alix-d-flex alix-flex-wrap alix-ga-2">
      <li
        v-for="choice in block.choices"
        :key="choice.choice_id"
      >
        <slot
          :answered="answered"
          :choice="choice"
          name="choice"
        >
          <button
            :disabled="answered"
            :title="choice.description"
            class="alix-chip"
            type="button"
            @click="emit('choose', choice.choice_id)"
          >
            {{ choice.label }}
          </button>
        </slot>
      </li>
    </ul>
    <form
      v-if="block.allow_free_text && !answered"
      class="alix-d-flex alix-ga-2"
      @submit.prevent="submit"
    >
      <input
        v-model="draft"
        :aria-label="t('clarification.freeText')"
        :placeholder="t('clarification.freeText')"
        class="alix-field no-validation alix-flex-grow-1 alix-min-w-0"
        type="text"
        autocomplete="off"
      >
      <button
        :disabled="draft.trim() === ''"
        class="alix-btn"
        type="submit"
      >
        {{ t("common.send") }}
      </button>
    </form>
  </div>
</template>
