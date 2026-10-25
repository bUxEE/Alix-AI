<script setup lang="ts">
import {
  AlixIntegrationAuditPanel,
  AlixSettingsPanel,
  AlixSourceHealthPanel,
  type SettingsAuditStatus,
  type SettingsEffective,
  type SettingsProviderHealth,
  type SourceStatus,
  type UserSettings,
} from "@alixai/vue";
import type { SettingsUserRecord } from "@alixai/sdk";
import { onMounted, ref } from "vue";
import { DEMO_PATHS, fetchDemoJson } from "../../../demo-mock/src/index";
import DemoSection from "../components/DemoSection.vue";
import { useDemoResource } from "../composables/useDemoResource";
import { useDemoText } from "../state";

// The three panels a host puts on its settings and admin pages, fed by the mock's routes in the shapes of schemas/api: the
// user's settings over the effective ones (saved with `PUT /v1/settings/user` and the version they were read at), the health
// of the knowledge sources (refresh re-audits them first), and the integration audit with the providers' health.

const text = useDemoText();

const settings = useDemoResource(async (signal) => {
  const [effective, user] = await Promise.all([
    fetchDemoJson<SettingsEffective>(DEMO_PATHS.settingsEffective, "", { signal }),
    fetchDemoJson<SettingsUserRecord>(DEMO_PATHS.settingsUser, "", { signal }),
  ]);
  return { effective, user };
});
const sources = useDemoResource(async (signal) =>
  (await fetchDemoJson<{ sources: { status: SourceStatus }[] }>(DEMO_PATHS.ragSources, "", { signal })).sources.map((entry) => entry.status),
);
const audit = useDemoResource(async (signal) => {
  const [report, providers] = await Promise.all([
    fetchDemoJson<SettingsAuditStatus>(DEMO_PATHS.integrationAudit, "", { signal }),
    fetchDemoJson<SettingsProviderHealth>(DEMO_PATHS.providerHealth, "", { signal }),
  ]);
  return { report, providers };
});

onMounted(() => {
  void settings.refresh();
  void sources.refresh();
  void audit.refresh();
});

const saving = ref(false);
const notice = ref<{ tone: "success" | "danger"; message: string }>();

const save = async (next: UserSettings, expectedVersion: number): Promise<void> => {
  saving.value = true;
  notice.value = undefined;
  try {
    await fetchDemoJson<SettingsUserRecord>(DEMO_PATHS.settingsUser, "", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ expected_version: expectedVersion, settings: next }),
    });
    await settings.refresh();
    notice.value = { tone: "success", message: text("settings.saved", { version: settings.data.value?.user.version ?? expectedVersion + 1 }) };
  } catch (error) {
    notice.value = { tone: "danger", message: error instanceof Error ? error.message : String(error) };
  } finally {
    saving.value = false;
  }
};

const reaudit = async (): Promise<void> => {
  await fetch(DEMO_PATHS.ragReaudit, { method: "POST" });
  await sources.refresh();
};
</script>

<template>
  <DemoSection>
    <template #eyebrow>
      AlixSettingsPanel · AlixSourceHealthPanel · AlixIntegrationAuditPanel
    </template>
    <template #title>
      {{ text("nav.settings.title") }}
    </template>
    <template #lede>
      {{ text("settings.lede") }}
    </template>
    <div class="alix-row">
      <div class="alix-col-12 alix-col-lg-6 alix-d-flex alix-flex-column alix-ga-3 alix-sticky-lg-top">
        <p
          v-if="notice"
          :class="notice.tone === 'success' ? 'alix-tone-success' : 'alix-tone-danger'"
          class="alix-banner alix-ma-0"
          role="status"
        >
          {{ notice.message }}
        </p>
        <AlixSettingsPanel
          :settings="settings.data.value?.effective"
          :user="settings.data.value?.user.settings"
          :version="settings.data.value?.user.version"
          :loading="settings.state.isLoading.value"
          :failed="settings.error.value !== undefined"
          :saving="saving"
          @clear-history="notice = { tone: 'success', message: text('settings.cleared') }"
          @retry="settings.refresh()"
          @save="save"
        />
      </div>
      <div class="alix-col-12 alix-col-lg-6 alix-flex-1 alix-d-flex alix-flex-column alix-ga-4 alix-sticky-lg-top">
        <AlixSourceHealthPanel
          :sources="sources.data.value"
          :loading="sources.state.isLoading.value"
          :failed="sources.error.value !== undefined"
          @refresh="reaudit"
        />
        <AlixIntegrationAuditPanel
          :audit="audit.data.value?.report"
          :providers="audit.data.value?.providers"
          :loading="audit.state.isLoading.value"
          :failed="audit.error.value !== undefined"
          @refresh="audit.refresh()"
        />
      </div>
    </div>
  </DemoSection>
</template>
