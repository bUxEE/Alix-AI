import type {
  SettingsAuditStatus,
  SettingsCapabilityView,
  SettingsEffective,
  SettingsProfileList,
  SettingsProviderHealth,
  SettingsUserRecord,
  rag,
} from "@alixai/sdk";
import { fixedId, isoAt } from "../ids";
import { SOURCE_IDS } from "./northwind";

// What the panels show: the caller's settings at every level they can see, the RAG sources with mixed health, the integration
// audit of the installed package (findings that pass, warn and fail), and the health of the model roles. Every document has the
// shape of its route in schemas/api (the suite validates them against the OpenAPI document).

type UserSettings = SettingsUserRecord["settings"];
type SourceStatus = rag.SourceStatus;

export const USER_SETTINGS_SCHEMA = "urn:alixai:schema:settings:user-settings:v1";

/** The user's own document as the demo starts: a few choices made, the rest inherited. */
export const initialUserSettings = (): SettingsUserRecord => ({
  level: "user",
  version: 3,
  schema: USER_SETTINGS_SCHEMA,
  updated_at: isoAt(Date.now() - 6 * 24 * 60 * 60 * 1000),
  updated_by: "usr_demo_alex",
  settings: {
    language: "en",
    voice: { enabled: true, auto_play: true, speech_rate_pct: 110 },
    sources: { citation_display: "panel" },
    accessibility: { reduced_motion: false, captions: true },
    confirmation: { extra_from_risk_level: 2 },
  },
});

/** The tenant switched the sensor capability off for everybody; the operator capped attachments and retention. */
const TENANT_DISABLED = ["sensors.read_live"];
const MAX_FILE_MB = 8;
const MAX_RETENTION_DAYS = 180;

/** What the deployment, tenant and user levels add up to, for this user document. */
export const effectiveSettings = (record: SettingsUserRecord): SettingsEffective => {
  const user: UserSettings = record.settings;
  const disabled = Array.from(new Set([...TENANT_DISABLED, ...(user.capabilities?.disabled ?? [])]));
  const extra = user.confirmation?.extra_from_risk_level;
  return {
    levels: { deployment: 7, tenant: 4, user: record.version },
    language: user.language ?? "en",
    model_profile: user.model_profile ?? "balanced",
    voice: {
      allowed: true,
      enabled: user.voice?.enabled ?? false,
      voice_id: user.voice?.voice_id ?? "aria_neutral",
      speech_rate_pct: user.voice?.speech_rate_pct ?? 100,
      auto_play: user.voice?.auto_play ?? false,
      barge_in: user.voice?.barge_in ?? false,
    },
    history: { retention_days: Math.min(user.history?.retention_days ?? 90, MAX_RETENTION_DAYS), auto_cleanup: user.history?.auto_cleanup ?? true, export_allowed: true },
    privacy: { store_transcript: user.privacy?.store_transcript ?? false },
    attachments: { allowed: true, enabled: user.attachments?.accept_uploads ?? true, max_file_mb: Math.min(user.attachments?.max_file_mb ?? MAX_FILE_MB, MAX_FILE_MB) },
    sources: { include_attachments: user.sources?.include_attachments ?? true, citation_display: user.sources?.citation_display ?? "inline" },
    // The tenant's policy does not allow notifications: the switch shows, disabled, with the reason.
    notifications: { allowed: false, enabled: false, events: [] },
    streaming: { enabled: user.streaming?.enabled ?? true, unit: user.streaming?.unit ?? "token" },
    accessibility: {
      reduced_motion: user.accessibility?.reduced_motion ?? false,
      high_contrast: user.accessibility?.high_contrast ?? false,
      text_scale_pct: user.accessibility?.text_scale_pct ?? 100,
      screen_reader: user.accessibility?.screen_reader ?? false,
      captions: user.accessibility?.captions ?? false,
    },
    capabilities: { disabled },
    confirmation: extra === undefined ? {} : { extra_from_risk_level: extra },
    decided_by: {
      language: user.language === undefined ? "tenant" : "user",
      model_profile: user.model_profile === undefined ? "deployment" : "user",
      "voice.enabled": user.voice?.enabled === undefined ? "default" : "user",
      "history.retention_days": user.history?.retention_days === undefined ? "tenant" : "user",
      "history.export_allowed": "deployment",
      "privacy.store_transcript": user.privacy?.store_transcript === undefined ? "tenant" : "user",
      "attachments.enabled": user.attachments?.accept_uploads === undefined ? "default" : "user",
      "attachments.max_file_mb": "operator",
      "notifications.enabled": "tenant",
      ...(extra !== undefined && { "confirmation.extra_from_risk_level": "user" }),
    },
  };
};

export const settingsProfiles = (selected: string): SettingsProfileList => ({
  profiles: ["lite", "balanced", "precise-16g"].map((id) => ({ id, default: id === "balanced", selected: id === selected })),
});

const CAPABILITIES: readonly { id: string; risk: 0 | 1 | 2 | 3; fields: string[]; stepUp?: boolean }[] = [
  { id: "work_orders.list", risk: 0, fields: ["site", "status", "priority"] },
  { id: "work_orders.get", risk: 0, fields: ["work_order"] },
  { id: "work_orders.create", risk: 2, fields: ["site", "task", "priority", "scheduled_for"] },
  { id: "inspections.book", risk: 2, fields: ["site", "kind", "slot"] },
  { id: "reports.export", risk: 1, fields: ["report", "period", "format"] },
  { id: "energy.read", risk: 0, fields: ["site", "period"] },
  { id: "sensors.read_live", risk: 0, fields: ["site", "sensor"] },
  { id: "contracts.terminate", risk: 3, fields: ["contract", "effective_date"], stepUp: true },
];

export const settingsCapabilities = (effective: SettingsEffective): SettingsCapabilityView => ({
  capabilities: CAPABILITIES.map((capability) => {
    const extra = effective.confirmation.extra_from_risk_level;
    const native = capability.risk >= 2;
    const raised = extra !== undefined && capability.risk >= extra && capability.risk > 0;
    const off = effective.capabilities.disabled.includes(capability.id);
    return {
      id: capability.id,
      risk_level: capability.risk,
      enabled: !off,
      disabled_by: off ? (TENANT_DISABLED.includes(capability.id) ? ["tenant"] : ["user"]) : [],
      requires_confirmation: native || raised,
      requires_step_up: capability.stepUp === true,
      extra_confirmation: raised && !native,
      fields: capability.fields,
    };
  }),
  settings_access: { user: { read: true, write: true }, tenant: { read: false, write: false }, deployment: { read: true, write: false } },
});

/** The latest integration audit of the installed package: one critical finding the operator accepted, warnings, notes. */
export const integrationAudit = (): SettingsAuditStatus => ({
  application_id: "northwind_facilities",
  state: "active",
  package_version: "2.4.0",
  manifest_hash: "5f0c2a9d1e7b4c3a8f6e2d1c0b9a8f7e6d5c4b3a29181716151413121110a0b1",
  audited: true,
  run_id: fixedId("iar", 1),
  outcome: "needs_acceptance",
  counts: { critical: 1, warnings: 3, info: 2, unverified: 1 },
  findings: [
    {
      key: "contracts.terminate:step_up",
      check_id: "risk.step_up_declared",
      severity: "critical",
      capability: "contracts.terminate",
      message: "contracts.terminate is risk level 3 and its commit endpoint does not verify a step-up claim.",
      remediation: "Verify the step-up claim in the adapter's commit handler, or lower nothing and keep the capability on continue-in-app.",
      status: "accepted",
    },
    {
      key: "work_orders.create:idempotency",
      check_id: "commit.idempotency_key",
      severity: "warning",
      capability: "work_orders.create",
      path: "adapter/work_orders/commit",
      message: "The commit handler accepts a retried request without comparing the idempotency key's payload hash.",
      remediation: "Store the payload hash with the idempotency key and refuse a retry whose payload differs.",
      status: "open",
    },
    {
      key: "inspections.book:data_version",
      check_id: "prepare.data_versions",
      severity: "warning",
      capability: "inspections.book",
      message: "The prepare step returns no data version for the inspector's calendar, so a stale slot is not detected at commit.",
      remediation: "Return the calendar's version from prepare so the core can bind the confirmation to it.",
      status: "open",
    },
    {
      key: "reports.export:egress",
      check_id: "egress.declared_hosts",
      severity: "warning",
      capability: "reports.export",
      path: "manifest/egress",
      message: "The export job declares a wildcard storage host.",
      remediation: "Name the storage host exactly in the manifest's egress list.",
      status: "accepted",
    },
    {
      key: "energy.read:cache",
      check_id: "read.freshness_declared",
      severity: "info",
      capability: "energy.read",
      message: "energy.read declares a freshness of 1 hour; answers say how old the figures are.",
      remediation: "None needed.",
      status: "informational",
    },
    {
      key: "package:locales",
      check_id: "package.locales",
      severity: "info",
      message: "The package ships English and Italian texts for every capability.",
      remediation: "None needed.",
      status: "informational",
    },
  ],
});

/** The model roles: the core runs, the reranker is degraded, document OCR is not configured. */
export const providerHealth = (): SettingsProviderHealth => ({
  status: "degraded",
  roles: [
    { role: "controller", configured: true, status: "ok" },
    { role: "router", configured: true, status: "ok" },
    { role: "embedder", configured: true, status: "ok" },
    { role: "reranker", configured: true, status: "degraded", effect: "rerank_skipped" },
    { role: "stt", configured: true, status: "ok" },
    { role: "tts", configured: true, status: "ok" },
    { role: "ocr", configured: false, status: "degraded", effect: "scanned_documents_skipped" },
  ],
});

const hoursAgo = (hours: number): string => isoAt(Date.now() - hours * 60 * 60 * 1000);

/** The registered sources: ready, indexing, stale, failing and paused. */
export const ragSources = (): { sources: { declaration: string; package_version: string; serving: boolean; status: SourceStatus }[] } => ({
  sources: [
    {
      declaration: "maintenance_policy",
      package_version: "2.4.0",
      serving: true,
      status: {
        source_id: SOURCE_IDS.maintenancePolicy,
        display_name: "Preventive Maintenance Policy",
        state: "ready",
        availability: "available",
        staleness: "current",
        last_attempt_at: hoursAgo(2),
        last_success_at: hoursAgo(2),
        counts: { discovered: 1, indexed: 1, skipped: 0, failed: 0, tombstoned: 0 },
        active_generation_id: fixedId("gen", 1),
      },
    },
    {
      declaration: "supplier_contracts",
      package_version: "2.4.0",
      serving: true,
      status: {
        source_id: SOURCE_IDS.hvacContract,
        display_name: "Supplier contracts",
        state: "ready",
        availability: "available",
        staleness: "current",
        last_attempt_at: hoursAgo(5),
        last_success_at: hoursAgo(5),
        counts: { discovered: 14, indexed: 13, skipped: 1, failed: 0, tombstoned: 2 },
        active_generation_id: fixedId("gen", 2),
      },
    },
    {
      declaration: "inspection_schedules",
      package_version: "2.4.0",
      serving: true,
      status: {
        source_id: SOURCE_IDS.fireSchedule,
        display_name: "Inspection schedules",
        state: "indexing",
        availability: "available",
        staleness: "current",
        last_attempt_at: hoursAgo(0.1),
        last_success_at: hoursAgo(26),
        counts: { discovered: 9, indexed: 6, skipped: 0, failed: 0, tombstoned: 0 },
        active_generation_id: fixedId("gen", 3),
      },
    },
    {
      declaration: "site_requirements",
      package_version: "2.3.1",
      serving: true,
      status: {
        source_id: SOURCE_IDS.ventilation,
        display_name: "Site requirements",
        state: "ready",
        availability: "available",
        staleness: "stale",
        last_attempt_at: hoursAgo(30),
        last_success_at: hoursAgo(24 * 41),
        counts: { discovered: 22, indexed: 19, skipped: 0, failed: 3, tombstoned: 0 },
        active_generation_id: fixedId("gen", 4),
        error: { code: "validation_failed", message: "3 documents could not be parsed (scanned images without text).", retryable: false },
      },
    },
    {
      declaration: "supplier_portal",
      package_version: "2.4.0",
      serving: false,
      status: {
        source_id: SOURCE_IDS.supplierPortal,
        display_name: "Supplier portal",
        state: "paused",
        availability: "unavailable",
        staleness: "unknown",
        last_attempt_at: hoursAgo(1),
        last_success_at: null,
        counts: { discovered: 0, indexed: 0, skipped: 0, failed: 0, tombstoned: 0 },
        active_generation_id: null,
        error: { code: "unavailable", message: "The supplier portal did not answer (connection timed out).", retryable: true },
      },
    },
  ],
});

/** The readiness report: ready, degraded (the reranker; OCR is not configured). */
export const readyReport = (): { status: "degraded"; ready: true; roles: { role: string; configured: boolean; status: "ok" | "degraded" | "unavailable"; effect?: string }[]; checks: { name: string; ok: boolean }[] } => ({
  status: "degraded",
  ready: true,
  roles: providerHealth().roles.map((role) => ({ role: role.role, configured: role.configured, status: role.status, ...(role.effect !== undefined && { effect: role.effect }) })),
  checks: [
    { name: "store", ok: true },
    { name: "application_package", ok: true },
    { name: "audit_log", ok: true },
  ],
});
