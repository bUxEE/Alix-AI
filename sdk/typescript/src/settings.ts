import type {
  SettingsAuditStatus,
  SettingsCapabilityView,
  SettingsDeploymentRecord,
  SettingsEffective,
  SettingsProfileList,
  SettingsProviderHealth,
  SettingsTenantRecord,
  SettingsUpdate,
  SettingsUserRecord,
} from "./api-types.js";
import {
  isAuditStatus,
  isCapabilityView,
  isDeploymentRecord,
  isEffectiveSettings,
  isProfileList,
  isProviderHealth,
  isTenantRecord,
  isUserRecord,
} from "./area-guards.js";
import type { Caller } from "./call.js";
import { sdkError } from "./errors.js";
import type { DeploymentSettings, TenantSettings, UserSettings } from "./gen/settings/index.js";
import type { OperationId } from "./operations.js";
import type { CallOptions } from "./transport.js";

// Settings, model/profile and privacy preferences (master plan 14.2). Three levels of typed, versioned documents (user, tenant,
// deployment), the approved profiles, and read-only views of what the levels add up to. A setting can only select among things the
// operator approved, switch something off for itself or ask for MORE confirmation: there is no member, and so no method here, that
// enables an undeclared tool, lowers a confirmation, changes data egress, holds a credential or reaches the runtime.
//
// The host authorises each operation by putting its capability in the context token: `settings.user.read` and `settings.user.write`,
// the same pair for `tenant` and `deployment`. A write succeeds only when `expected_version` is still the stored version.

/** The settings area of the client: `client.settings`. */
export interface SettingsApi {
  /** The caller's own preferences with their revision. Version 0 and the empty document when there is none yet. Needs `settings.user.read`. */
  getUser: (options?: CallOptions) => Promise<SettingsUserRecord>;
  /**
   * Replaces the whole user document (a member left out means inherit). Needs `settings.user.write`. A stale `expected_version` is
   * `conflict` with `error.details.current_version`; nothing is retried. A document that names a member the API does not have
   * (enabling a tool, lowering a confirmation, external mode, egress, a credential) is refused whole (`validation_failed`).
   */
  putUser: (update: SettingsUpdate<UserSettings>, options?: CallOptions) => Promise<SettingsUserRecord>;
  /** The settings of the caller's tenant. Needs `settings.tenant.read`. */
  getTenant: (options?: CallOptions) => Promise<SettingsTenantRecord>;
  /** Replaces the tenant document. Needs `settings.tenant.write`. */
  putTenant: (update: SettingsUpdate<TenantSettings>, options?: CallOptions) => Promise<SettingsTenantRecord>;
  /** The settings of the caller's application deployment. Needs `settings.deployment.read`. */
  getDeployment: (options?: CallOptions) => Promise<SettingsDeploymentRecord>;
  /** Replaces the deployment document. Needs `settings.deployment.write`. */
  putDeployment: (update: SettingsUpdate<DeploymentSettings>, options?: CallOptions) => Promise<SettingsDeploymentRecord>;
  /** What the three levels add up to for the caller, with the level that decided each value. Read only. Needs `settings.user.read`. */
  getEffective: (options?: CallOptions) => Promise<SettingsEffective>;
  /** The profiles the operator approved, the default and the one the caller's effective settings select. Needs `settings.user.read`. */
  listProfiles: (options?: CallOptions) => Promise<SettingsProfileList>;
  /** The capabilities enabled for the caller, what settings did to each, and which settings operations the token permits. Needs `settings.user.read`. */
  getCapabilities: (options?: CallOptions) => Promise<SettingsCapabilityView>;
  /** The latest integration audit of the caller's application, read only. Needs `settings.deployment.read`. */
  getIntegrationAudit: (options?: CallOptions) => Promise<SettingsAuditStatus>;
  /** Role, status and degradation effect of each model role, with nothing about what runs behind it. Needs `settings.deployment.read`. */
  getProviderHealth: (options?: CallOptions) => Promise<SettingsProviderHealth>;
}

const MAX_VERSION = Number.MAX_SAFE_INTEGER;

const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** The body of a settings write, built from the two members the API accepts and nothing else. */
const updateBody = (update: unknown): string => {
  if (!isRecord(update)) throw invalid("update", "it must be an object with expected_version and settings");
  for (const key of Object.keys(update)) if (key !== "expected_version" && key !== "settings") throw invalid("update", `unknown member "${key}"`);
  const version = update["expected_version"];
  if (typeof version !== "number" || !Number.isInteger(version) || version < 0 || version > MAX_VERSION) {
    throw invalid("expected_version", "it must be the version that was read, or 0 to create the document");
  }
  if (!isRecord(update["settings"])) throw invalid("settings", "it must be the settings document");
  return JSON.stringify({ expected_version: version, settings: update["settings"] });
};

/** Builds the settings area over a caller. */
export const createSettingsApi = (caller: Caller): SettingsApi => {
  const read = <T>(operation: OperationId, guard: (value: unknown) => value is T, what: string, options: CallOptions | undefined): Promise<T> =>
    caller.json(operation, {}, guard, what, options);
  const write = <T>(operation: OperationId, guard: (value: unknown) => value is T, what: string, update: unknown, options: CallOptions | undefined): Promise<T> =>
    caller.json(operation, {}, guard, what, options, { body: { contentType: "application/json", payload: updateBody(update) } });

  return {
    getUser: async (options) => read("getUserSettings", isUserRecord, "user settings", options),
    putUser: async (update, options) => write("putUserSettings", isUserRecord, "user settings", update, options),
    getTenant: async (options) => read("getTenantSettings", isTenantRecord, "tenant settings", options),
    putTenant: async (update, options) => write("putTenantSettings", isTenantRecord, "tenant settings", update, options),
    getDeployment: async (options) => read("getDeploymentSettings", isDeploymentRecord, "deployment settings", options),
    putDeployment: async (update, options) => write("putDeploymentSettings", isDeploymentRecord, "deployment settings", update, options),
    getEffective: async (options) => read("getEffectiveSettings", isEffectiveSettings, "effective settings", options),
    listProfiles: async (options) => read("listSettingsProfiles", isProfileList, "profile list", options),
    getCapabilities: async (options) => read("getSettingsCapabilities", isCapabilityView, "capability view", options),
    getIntegrationAudit: async (options) => read("getSettingsIntegrationAudit", isAuditStatus, "integration audit", options),
    getProviderHealth: async (options) => read("getSettingsProviderHealth", isProviderHealth, "provider health", options),
  };
};
