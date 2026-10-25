<script setup lang="ts">
import { computed, useId } from "vue";
import { useLoadFromProps } from "../composables/useLoadState";
import { useAlix, type MessageKey } from "../i18n";
import type { SettingsAuditFinding, SettingsAuditStatus, SettingsProviderHealth } from "../sdk-types";
import AlixIcon from "./common/AlixIcon.vue";
import AlixLoadable from "./common/AlixLoadable.vue";
import AlixSkeleton from "./common/AlixSkeleton.vue";

// What the audit of the installed integration package found, and how the model and service roles are doing. The audit is the
// core's (`GET /v1/settings/integration-audit`), the provider health is `GET /v1/settings/providers`; the panel shows them and
// decides nothing. Findings are ordered critical, warning, info, each with the remediation it states. Every word of a finding is the
// audit's own data and is shown as text.

const props = withDefaults(
  defineProps<{
    audit?: SettingsAuditStatus;
    providers?: SettingsProviderHealth;
    title?: string;
    loading?: boolean;
    failed?: boolean;
  }>(),
  { audit: undefined, providers: undefined, title: undefined, loading: false, failed: false },
);

const emit = defineEmits<{
  refresh: [];
}>();

const { t } = useAlix();
const uid = useId();
const state = useLoadFromProps({ loading: () => props.loading, failed: () => props.failed, hasData: () => props.audit !== undefined });

const ORDER = { critical: 0, warning: 1, info: 2 } as const;
const findings = computed<SettingsAuditFinding[]>(() => [...(props.audit?.findings ?? [])].sort((a, b) => ORDER[a.severity] - ORDER[b.severity]));
const severityTone = (severity: SettingsAuditFinding["severity"]): string =>
  severity === "critical" ? "alix-tone-danger" : severity === "warning" ? "alix-tone-warning" : "";
const statusTone = (status: string): string => (status === "ok" ? "alix-tone-success" : status === "degraded" ? "alix-tone-warning" : "alix-tone-danger");
const outcomeTone = computed(() => (props.audit?.outcome === "clear" ? "" : props.audit?.outcome === "blocked" ? "alix-tone-danger" : "alix-tone-warning"));
</script>

<template>
  <section
    :aria-labelledby="`${uid}-title`"
    class="alix-root alix-d-flex alix-flex-column alix-ga-3"
  >
    <header class="alix-d-flex alix-align-center alix-justify-between alix-ga-2">
      <h2
        :id="`${uid}-title`"
        class="alix-heading"
      >
        {{ title ?? t("audit.title") }}
      </h2>
      <button
        :disabled="loading"
        :aria-label="t('common.refresh')"
        class="alix-btn alix-btn-quiet alix-btn-icon"
        type="button"
        @click="emit('refresh')"
      >
        <AlixIcon name="refresh" />
      </button>
    </header>
    <AlixLoadable
      :loading="state.isLoading.value"
      :loaded="state.hasLoadedOnce.value"
    >
      <template #skeleton>
        <div class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-3">
          <AlixSkeleton width="35%" />
          <AlixSkeleton />
          <AlixSkeleton width="75%" />
        </div>
      </template>
      <div
        v-if="failed && !state.hasLoadedOnce.value"
        class="alix-banner alix-tone-danger alix-d-flex alix-align-center alix-justify-between alix-ga-2"
        role="alert"
      >
        {{ t("audit.failed") }}
        <button
          class="alix-btn"
          type="button"
          @click="emit('refresh')"
        >
          {{ t("common.retry") }}
        </button>
      </div>
      <div
        v-else-if="audit"
        class="alix-d-flex alix-flex-column alix-ga-3"
      >
        <div class="alix-card alix-pa-4 alix-d-flex alix-flex-column alix-ga-2">
          <div class="alix-d-flex alix-flex-wrap alix-align-center alix-ga-2">
            <span
              v-if="audit.outcome"
              :class="outcomeTone"
              class="alix-badge"
            >
              {{ t(`audit.outcome.${audit.outcome}` as MessageKey) }}
            </span>
            <span
              v-else
              class="alix-badge"
            >
              {{ t("audit.notAudited") }}
            </span>
            <span
              v-if="audit.package_version"
              class="alix-text-muted alix-text-sm alix-font-mono"
            >
              {{ audit.package_version }}
            </span>
          </div>
          <p class="alix-text-muted alix-text-sm alix-ma-0">
            {{ t("audit.counts", { critical: audit.counts.critical, warnings: audit.counts.warnings, info: audit.counts.info }) }}
          </p>
        </div>
        <ul
          v-if="findings.length > 0"
          :aria-label="t('audit.findings')"
          class="alix-reset-list alix-d-flex alix-flex-column alix-ga-2"
        >
          <li
            v-for="finding in findings"
            :key="finding.key"
            class="alix-card alix-pa-3 alix-d-flex alix-flex-column alix-ga-2"
          >
            <slot
              :finding="finding"
              name="finding"
            >
              <div class="alix-d-flex alix-flex-wrap alix-align-center alix-ga-2">
                <span
                  :class="severityTone(finding.severity)"
                  class="alix-badge"
                >
                  {{ t(`audit.severity.${finding.severity}` as MessageKey) }}
                </span>
                <span class="alix-badge">
                  {{ t(`audit.status.${finding.status}` as MessageKey) }}
                </span>
                <span
                  v-if="finding.capability"
                  class="alix-text-muted alix-text-sm alix-font-mono"
                >
                  {{ finding.capability }}
                </span>
              </div>
              <p class="alix-ma-0 alix-break-word">
                {{ finding.message }}
              </p>
              <p class="alix-text-muted alix-text-sm alix-ma-0 alix-break-word">
                {{ t("audit.remediation") }}: {{ finding.remediation }}
              </p>
            </slot>
          </li>
        </ul>
        <slot
          v-else
          name="empty"
        >
          <p class="alix-text-muted alix-ma-0">
            {{ audit.audited ? t("audit.noFindings") : t("audit.notAudited") }}
          </p>
        </slot>
        <section
          v-if="providers"
          :aria-label="t('audit.providers')"
          class="alix-d-flex alix-flex-column alix-ga-2"
        >
          <p class="alix-eyebrow">
            {{ t("audit.providers") }}
          </p>
          <ul class="alix-reset-list alix-card alix-divided">
            <li
              v-for="role in providers.roles"
              :key="role.role"
              class="alix-pa-3 alix-d-flex alix-flex-wrap alix-align-center alix-justify-between alix-ga-2"
            >
              <span class="alix-font-medium">
                {{ role.role }}
              </span>
              <span
                :class="statusTone(role.status)"
                class="alix-badge"
              >
                {{ role.configured ? t(`audit.provider.${role.status}` as MessageKey) : t("audit.provider.notConfigured") }}
              </span>
            </li>
          </ul>
        </section>
      </div>
    </AlixLoadable>
  </section>
</template>
