<script setup lang="ts">
import { computed, ref, useId, watch } from "vue";
import { useLoadFromProps } from "../composables/useLoadState";
import { useAlix, type MessageKey } from "../i18n";
import type { SettingsEffective, UserSettings } from "../sdk-types";
import { CITATION_DISPLAYS, CONFIRMATION_LEVELS, SETTINGS_SECTIONS, TEXT_SCALES, readPath, writePath, type SwitchField } from "../settings-fields";
import AlixButtonSpinner from "./common/AlixButtonSpinner.vue";
import AlixLoadable from "./common/AlixLoadable.vue";
import AlixSkeleton from "./common/AlixSkeleton.vue";
import AlixSwitch from "./common/AlixSwitch.vue";

// Settings and privacy controls. Given only the effective settings it is a read-only view of what the deployment, the tenant and
// the user add up to (the same contract as the React kit's panel, with the host's own controls in the default slot). Given the
// user's own document as well (`user`, with its `version`) it becomes the editor of that document and reports `save` with the
// whole document and the version it replaces; the host writes it with `settings.putUser`. A setting can only select among approved
// things, switch something off for oneself or ask for MORE confirmation, so there is no control here that lowers a protection.
// A feature the policy forbids is shown switched off with the reason, never hidden.

const props = withDefaults(
  defineProps<{
    settings?: SettingsEffective;
    user?: UserSettings;
    version?: number;
    title?: string;
    loading?: boolean;
    failed?: boolean;
    saving?: boolean;
    /** The languages offered: their own names, not translated. */
    languages?: readonly { value: string; label: string }[];
  }>(),
  {
    settings: undefined,
    user: undefined,
    version: 0,
    title: undefined,
    loading: false,
    failed: false,
    saving: false,
    languages: () => [
      { value: "en", label: "English" },
      { value: "it", label: "Italiano" },
    ],
  },
);

const emit = defineEmits<{
  save: [settings: UserSettings, expectedVersion: number];
  "clear-history": [];
  retry: [];
}>();

const { t } = useAlix();
const uid = useId();
const state = useLoadFromProps({ loading: () => props.loading, failed: () => props.failed, hasData: () => props.settings !== undefined });
const draft = ref<UserSettings>(JSON.parse(JSON.stringify(props.user ?? {})) as UserSettings);
const editable = computed(() => props.user !== undefined);
const dirty = computed(() => JSON.stringify(draft.value) !== JSON.stringify(props.user ?? {}));

watch(
  () => props.user,
  (next) => {
    draft.value = JSON.parse(JSON.stringify(next ?? {})) as UserSettings;
  },
);

const valueOf = (field: SwitchField): boolean => {
  const own = readPath(draft.value, field.path);
  if (typeof own === "boolean") return own;
  return props.settings === undefined ? false : (field.effective(props.settings) ?? false);
};
const allowed = (field: SwitchField): boolean => props.settings === undefined || field.allowed === undefined || field.allowed(props.settings);
const setValue = (path: string, value: unknown): void => {
  draft.value = writePath(draft.value, path, value);
};

const select = (path: string, event: Event, parse: (raw: string) => unknown): void => {
  const raw = (event.target as HTMLSelectElement).value;
  setValue(path, raw === "" ? undefined : parse(raw));
};

const scale = computed(() => (readPath(draft.value, "accessibility.text_scale_pct") as number | undefined) ?? props.settings?.accessibility.text_scale_pct ?? 100);
const display = computed(() => (readPath(draft.value, "sources.citation_display") as string | undefined) ?? props.settings?.sources.citation_display ?? "panel");
const level = computed(() => (readPath(draft.value, "confirmation.extra_from_risk_level") as number | undefined) ?? "");
const language = computed(() => (readPath(draft.value, "language") as string | undefined) ?? props.settings?.language ?? "");

const reset = (): void => {
  draft.value = JSON.parse(JSON.stringify(props.user ?? {})) as UserSettings;
};
</script>

<template>
  <section
    :aria-labelledby="`${uid}-title`"
    class="alix-root alix-d-flex alix-flex-column alix-ga-4"
  >
    <h2
      :id="`${uid}-title`"
      class="alix-heading"
    >
      {{ title ?? t("settings.title") }}
    </h2>
    <AlixLoadable
      :loading="state.isLoading.value"
      :loaded="state.hasLoadedOnce.value"
    >
      <template #skeleton>
        <div class="alix-row">
          <div
            v-for="n in 4"
            :key="n"
            class="alix-col-12 alix-col-md-6"
          >
            <div class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
              <AlixSkeleton width="40%" />
              <AlixSkeleton />
              <AlixSkeleton width="80%" />
            </div>
          </div>
        </div>
      </template>
      <div
        v-if="failed && !state.hasLoadedOnce.value"
        class="alix-banner alix-tone-danger alix-d-flex alix-align-center alix-justify-between alix-ga-2"
        role="alert"
      >
        {{ t("settings.failed") }}
        <button
          class="alix-btn"
          type="button"
          @click="emit('retry')"
        >
          {{ t("common.retry") }}
        </button>
      </div>
      <form
        v-else-if="settings"
        class="alix-d-flex alix-flex-column alix-ga-4"
        @submit.prevent="emit('save', draft, version)"
      >
        <div class="alix-row">
          <fieldset
            v-for="section in SETTINGS_SECTIONS"
            :key="section.id"
            class="alix-col-12 alix-col-md-6 alix-card alix-pa-4 alix-ma-0 alix-d-flex alix-flex-column alix-ga-3 alix-min-w-0"
          >
            <legend class="alix-eyebrow alix-px-2">
              {{ t(section.title) }}
            </legend>
            <template
              v-for="field in section.fields"
              :key="field.path"
            >
              <AlixSwitch
                v-if="editable"
                :model-value="valueOf(field)"
                :disabled="!allowed(field)"
                @update:model-value="setValue(field.path, $event)"
              >
                {{ t(field.label) }}
                <template #hint>
                  {{ !allowed(field) ? t("settings.policyOff") : field.hint ? t(field.hint) : "" }}
                </template>
              </AlixSwitch>
              <div
                v-else
                class="alix-d-flex alix-justify-between alix-ga-3"
              >
                <span>
                  {{ t(field.label) }}
                </span>
                <span class="alix-font-medium">
                  {{ !allowed(field) ? t("settings.policyOff") : valueOf(field) ? t("common.on") : t("common.off") }}
                </span>
              </div>
            </template>
            <template v-if="section.id === 'replies'">
              <label
                v-if="editable"
                class="alix-d-flex alix-flex-column alix-ga-1"
              >
                <span class="alix-font-medium">
                  {{ t("settings.language") }}
                </span>
                <select
                  :value="language"
                  class="alix-field no-validation"
                  @change="select('language', $event, (raw) => raw)"
                >
                  <option value="">
                    {{ t("settings.default") }}
                  </option>
                  <option
                    v-for="entry in languages"
                    :key="entry.value"
                    :value="entry.value"
                  >
                    {{ entry.label }}
                  </option>
                </select>
              </label>
              <label
                v-if="editable"
                class="alix-d-flex alix-flex-column alix-ga-1"
              >
                <span class="alix-font-medium">
                  {{ t("settings.citations") }}
                </span>
                <select
                  :value="display"
                  class="alix-field no-validation"
                  @change="select('sources.citation_display', $event, (raw) => raw)"
                >
                  <option
                    v-for="entry in CITATION_DISPLAYS"
                    :key="entry"
                    :value="entry"
                  >
                    {{ t(`settings.citations.${entry}` as MessageKey) }}
                  </option>
                </select>
              </label>
              <div
                v-else
                class="alix-d-flex alix-justify-between alix-ga-3"
              >
                <span>
                  {{ t("settings.citations") }}
                </span>
                <span class="alix-font-medium">
                  {{ t(`settings.citations.${display}` as MessageKey) }}
                </span>
              </div>
            </template>
            <template v-if="section.id === 'accessibility'">
              <label
                v-if="editable"
                class="alix-d-flex alix-flex-column alix-ga-1"
              >
                <span class="alix-font-medium">
                  {{ t("settings.a11y.textScale") }}
                </span>
                <select
                  :value="scale"
                  class="alix-field no-validation"
                  @change="select('accessibility.text_scale_pct', $event, Number)"
                >
                  <option
                    v-for="entry in TEXT_SCALES"
                    :key="entry"
                    :value="entry"
                  >
                    {{ entry }}%
                  </option>
                </select>
              </label>
              <div
                v-else
                class="alix-d-flex alix-justify-between alix-ga-3"
              >
                <span>
                  {{ t("settings.a11y.textScale") }}
                </span>
                <span class="alix-font-medium alix-font-mono">
                  {{ scale }}%
                </span>
              </div>
            </template>
            <template v-if="section.id === 'privacy'">
              <p class="alix-text-muted alix-text-sm alix-ma-0">
                {{ t("settings.history.retention", { days: settings.history.retention_days }) }}
              </p>
              <label
                v-if="editable"
                class="alix-d-flex alix-flex-column alix-ga-1"
              >
                <span class="alix-font-medium">
                  {{ t("settings.confirmation") }}
                </span>
                <select
                  :value="level"
                  class="alix-field no-validation"
                  @change="select('confirmation.extra_from_risk_level', $event, Number)"
                >
                  <option value="">
                    {{ t("settings.default") }}
                  </option>
                  <option
                    v-for="entry in CONFIRMATION_LEVELS"
                    :key="entry"
                    :value="entry"
                  >
                    {{ t("settings.confirmation.from", { level: entry }) }}
                  </option>
                </select>
                <span class="alix-text-muted alix-text-sm">
                  {{ t("settings.confirmation.hint") }}
                </span>
              </label>
              <button
                v-if="editable"
                class="alix-btn alix-btn-danger alix-self-start"
                type="button"
                @click="emit('clear-history')"
              >
                {{ t("settings.clearHistory") }}
              </button>
            </template>
          </fieldset>
        </div>
        <slot />
        <div
          v-if="editable"
          class="alix-d-flex alix-justify-end alix-ga-2"
        >
          <button
            :disabled="!dirty || saving"
            class="alix-btn"
            type="button"
            @click="reset"
          >
            {{ t("common.discard") }}
          </button>
          <button
            :disabled="!dirty || saving"
            :aria-busy="saving"
            class="alix-btn alix-btn-primary"
            type="submit"
          >
            <AlixButtonSpinner v-if="saving" />
            {{ t("common.save") }}
          </button>
        </div>
      </form>
    </AlixLoadable>
  </section>
</template>
