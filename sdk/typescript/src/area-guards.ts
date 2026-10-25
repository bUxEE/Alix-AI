import type {
  HistoryCleanupResult,
  HistoryExportRow,
  HistorySearchPage,
  HistorySegmentPage,
  HistorySessionCleared,
  SettingsAuditStatus,
  SettingsCapabilityView,
  SettingsDeploymentRecord,
  SettingsEffective,
  SettingsProfileList,
  SettingsProviderHealth,
  SettingsTenantRecord,
  SettingsUserRecord,
} from "./api-types.js";
import type { FileRef } from "./gen/conversation/index.js";
import { array, arrayOf, boolean, integer, nullable, object, oneOf, shape, string } from "./guards.js";

// Shallow response guards of the files, history and settings areas, in the style of src/guards.ts: they check that the members each
// type requires are present with the right JSON type before the data is handed to the host. They are not a validator; the schemas
// under schemas/ stay the authority for the deep structure (and the contract test checks the fake server against them).

const role = oneOf("user", "assistant");
const count: (v: unknown) => boolean = (v) => typeof v === "number" && Number.isInteger(v) && v >= 0;
const readiness = oneOf("ok", "degraded", "unavailable");

const fileRef = shape(
  {
    file_id: string,
    name: string,
    mime_type: string,
    size_bytes: count,
    owner: string,
    origin: oneOf("upload", "generated", "application"),
    permissions: arrayOf(oneOf("read", "preview", "download")),
    previewable: boolean,
    expires_at: string,
  },
  { content_hash: string, treatment: oneOf("ephemeral_context", "rag_source", "application_object") },
);

const historyTurn = shape(
  { turn_id: string, n: count, role, body: string, clipped: boolean, created_at: string },
  { act: string, withheld: boolean },
);

const exportRow = shape(
  { session_id: string, turn_id: string, n: count, role, body: string, clipped: boolean, created_at: string },
  { act: string, withheld: boolean },
);

const level = (name: string): ((v: unknown) => boolean) =>
  shape({ level: oneOf(name), version: count, schema: string, settings: object }, { updated_at: string, updated_by: string });

const settingsAccess = shape({ read: boolean, write: boolean });

const checks = {
  fileRef,
  historySegmentPage: shape({ session_id: string, turns: arrayOf(historyTurn), next_cursor: nullable(string), total: count }),
  historySessionCleared: shape({ session_id: string, rows_deleted: count }),
  historySearchPage: shape({
    hits: arrayOf(shape({ session_id: string, turn_id: string, n: count, role, created_at: string, snippet: string })),
    next_offset: nullable(integer),
  }),
  historyCleanup: shape({ rows_deleted: count }),
  exportRow,
  exportTrailer: shape({ complete: (v) => v === true, rows: count }),
  userRecord: level("user"),
  tenantRecord: level("tenant"),
  deploymentRecord: level("deployment"),
  effective: object,
  profiles: shape({ profiles: arrayOf(shape({ id: string, default: boolean, selected: boolean })) }),
  capabilityView: shape({
    capabilities: arrayOf(
      shape({
        id: string,
        risk_level: integer,
        enabled: boolean,
        disabled_by: array,
        requires_confirmation: boolean,
        requires_step_up: boolean,
        extra_confirmation: boolean,
        fields: array,
      }),
    ),
    settings_access: shape({ user: settingsAccess, tenant: settingsAccess, deployment: settingsAccess }),
  }),
  audit: shape(
    {
      application_id: string,
      state: oneOf("active", "quarantined", "rejected", "superseded", "not_installed"),
      audited: boolean,
      counts: shape({ critical: count, warnings: count, info: count, unverified: count }),
      findings: array,
    },
    { outcome: oneOf("blocked", "needs_acceptance", "clear") },
  ),
  providerHealth: shape({ status: readiness, roles: arrayOf(shape({ role: string, configured: boolean, status: readiness })) }),
};

export const isFileRef = checks.fileRef as (v: unknown) => v is FileRef;
export const isHistorySegmentPage = checks.historySegmentPage as (v: unknown) => v is HistorySegmentPage;
export const isHistorySessionCleared = checks.historySessionCleared as (v: unknown) => v is HistorySessionCleared;
export const isHistorySearchPage = checks.historySearchPage as (v: unknown) => v is HistorySearchPage;
export const isHistoryCleanupResult = checks.historyCleanup as (v: unknown) => v is HistoryCleanupResult;
export const isHistoryExportRow = checks.exportRow as (v: unknown) => v is HistoryExportRow;
export const isHistoryExportTrailer = checks.exportTrailer as (v: unknown) => v is { complete: true; rows: number };
export const isUserRecord = checks.userRecord as (v: unknown) => v is SettingsUserRecord;
export const isTenantRecord = checks.tenantRecord as (v: unknown) => v is SettingsTenantRecord;
export const isDeploymentRecord = checks.deploymentRecord as (v: unknown) => v is SettingsDeploymentRecord;
export const isEffectiveSettings = checks.effective as (v: unknown) => v is SettingsEffective;
export const isProfileList = checks.profiles as (v: unknown) => v is SettingsProfileList;
export const isCapabilityView = checks.capabilityView as (v: unknown) => v is SettingsCapabilityView;
export const isAuditStatus = checks.audit as (v: unknown) => v is SettingsAuditStatus;
export const isProviderHealth = checks.providerHealth as (v: unknown) => v is SettingsProviderHealth;
