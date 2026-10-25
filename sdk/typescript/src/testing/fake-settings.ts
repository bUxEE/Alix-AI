import type { SettingsAuditStatus, SettingsCapabilityView, SettingsProviderHealth } from "../api-types.js";
import type { Capability } from "../api-types.js";
import { requireCapability } from "./context.js";
import type { FakeContext, FakeScope, Json } from "./context.js";

// The settings area of the fake server (schemas/api/paths/settings.yaml): versioned documents at user, tenant and deployment level
// with optimistic concurrency (a write names the version it replaces: a stale one is 409 `conflict` with `details.current_version`),
// the effective view, the approved profiles, the capability view and the read-only audit and provider health. Each operation needs its
// capability in the context token (`settings.user.read`, `settings.tenant.write`, ...), with no wildcard.
//
// What it does not do: validate a document deeply (the real service validates against schemas/settings and refuses a member that is
// not listed there; the fake refuses an unknown top-level member and anything that is not an object, which is the check a host's own
// mistakes trip over), or add the levels up with the real rules (the effective view takes the language and the profile from the user
// level and keeps the documented defaults for the rest).

type Level = "user" | "tenant" | "deployment";

const SCHEMA: Record<Level, string> = {
  user: "urn:alixai:schema:settings:user-settings:v1",
  tenant: "urn:alixai:schema:settings:tenant-settings:v1",
  deployment: "urn:alixai:schema:settings:deployment-settings:v1",
};

/** The members each level's document may have (schemas/settings): a document with another member is refused whole. */
const MEMBERS: Record<Level, readonly string[]> = {
  user: ["language", "model_profile", "voice", "history", "privacy", "attachments", "sources", "notifications", "streaming", "accessibility", "capabilities", "confirmation"],
  tenant: ["default_language", "default_model_profile", "voice", "history", "privacy", "attachments", "notifications", "capabilities", "confirmation"],
  deployment: ["default_language", "default_model_profile", "voice", "history", "privacy", "attachments", "notifications", "capabilities", "confirmation"],
};

interface Stored {
  version: number;
  settings: Json;
  updatedMs: number;
  updatedBy: string;
}

export interface FakeSettingsOptions {
  /** The approved profiles. Default `lite` and `balanced`, `lite` being the default. */
  profiles?: readonly string[];
}

export interface FakeSettings {
  handle: (
    operation:
      | "getUserSettings"
      | "putUserSettings"
      | "getTenantSettings"
      | "putTenantSettings"
      | "getDeploymentSettings"
      | "putDeploymentSettings"
      | "getEffectiveSettings"
      | "listSettingsProfiles"
      | "getSettingsCapabilities"
      | "getSettingsIntegrationAudit"
      | "getSettingsProviderHealth",
    scope: FakeScope,
    body: unknown,
  ) => Response;
  reset: () => void;
}

const isRecord = (value: unknown): value is Json => typeof value === "object" && value !== null && !Array.isArray(value);

export const createFakeSettings = (
  context: FakeContext,
  capabilities: () => readonly Capability[],
  applicationStatus: () => "active" | "quarantined" | "rejected" | "superseded" | "not_installed",
  options: FakeSettingsOptions = {},
): FakeSettings => {
  const profiles = options.profiles ?? ["lite", "balanced"];
  const store = new Map<string, Stored>();

  const keyOf = (level: Level, scope: FakeScope): string =>
    [level, scope.application_id, scope.tenant_id, level === "deployment" ? "" : scope.workspace_id, level === "user" ? scope.principal_id : ""].join("\u0000");

  const record = (level: Level, scope: FakeScope): Response => {
    const found = store.get(keyOf(level, scope));
    return context.json(200, {
      level,
      version: found?.version ?? 0,
      schema: SCHEMA[level],
      ...(found !== undefined && { updated_at: context.iso(found.updatedMs), updated_by: found.updatedBy }),
      settings: found?.settings ?? {},
    });
  };

  const write = (level: Level, scope: FakeScope, body: unknown): Response => {
    const bad = (message: string, details?: Json): Response => context.problem("validation_failed", message, details === undefined ? {} : { details });
    if (!isRecord(body) || Object.keys(body).some((k) => k !== "expected_version" && k !== "settings")) return bad("the request body must be {expected_version, settings}");
    const expected = body["expected_version"];
    if (typeof expected !== "number" || !Number.isInteger(expected) || expected < 0) return bad("expected_version must be a non-negative integer");
    const settings = body["settings"];
    if (!isRecord(settings)) return bad("settings must be an object");
    const unknown = Object.keys(settings).filter((k) => !MEMBERS[level].includes(k));
    // A document that names a member the API does not have (enabling a tool, a scope, egress, a credential, ...) is refused whole.
    if (unknown.length > 0) return bad(`the settings document names a member that does not exist: ${unknown[0]}`, { guard: "closed_document" });
    const key = keyOf(level, scope);
    const current = store.get(key);
    const version = current?.version ?? 0;
    if (expected !== version) {
      return context.problem("conflict", "the settings were changed since they were read", { details: { current_version: version } });
    }
    // A document identical to the stored one changes nothing and is not recorded.
    if (current !== undefined && JSON.stringify(current.settings) === JSON.stringify(settings)) return record(level, scope);
    store.set(key, { version: version + 1, settings, updatedMs: context.nowMs(), updatedBy: scope.principal_id });
    return record(level, scope);
  };

  const levels = (scope: FakeScope): { deployment: number; tenant: number; user: number } => ({
    deployment: store.get(keyOf("deployment", scope))?.version ?? 0,
    tenant: store.get(keyOf("tenant", scope))?.version ?? 0,
    user: store.get(keyOf("user", scope))?.version ?? 0,
  });

  const effective = (scope: FakeScope): Response => {
    const user = store.get(keyOf("user", scope))?.settings ?? {};
    const language = typeof user["language"] === "string" ? user["language"] : "en";
    const profile = typeof user["model_profile"] === "string" ? user["model_profile"] : (profiles[0] ?? "lite");
    return context.json(200, {
      levels: levels(scope),
      language,
      model_profile: profile,
      voice: { allowed: false, enabled: false, speech_rate_pct: 100, auto_play: false, barge_in: true },
      history: { retention_days: 90, auto_cleanup: true, export_allowed: true },
      privacy: { store_transcript: true },
      attachments: { allowed: true, enabled: true, max_file_mb: 10 },
      sources: { include_attachments: true, citation_display: "inline" },
      notifications: { allowed: true, enabled: true, events: ["action_ready", "job_finished", "source_indexed"] },
      streaming: { enabled: true, unit: "token" },
      accessibility: { reduced_motion: false, high_contrast: false, text_scale_pct: 100, screen_reader: false, captions: false },
      capabilities: { disabled: [] },
      confirmation: {},
      decided_by: {
        model_profile: typeof user["model_profile"] === "string" ? "user" : "default",
        "history.retention_days": "default",
        language: typeof user["language"] === "string" ? "user" : "default",
      },
    });
  };

  const handle: FakeSettings["handle"] = (operation, scope, body) => {
    const required: Record<string, string> = {
      getUserSettings: "settings.user.read",
      putUserSettings: "settings.user.write",
      getTenantSettings: "settings.tenant.read",
      putTenantSettings: "settings.tenant.write",
      getDeploymentSettings: "settings.deployment.read",
      putDeploymentSettings: "settings.deployment.write",
      getEffectiveSettings: "settings.user.read",
      listSettingsProfiles: "settings.user.read",
      getSettingsCapabilities: "settings.user.read",
      getSettingsIntegrationAudit: "settings.deployment.read",
      getSettingsProviderHealth: "settings.deployment.read",
    };
    const denied = requireCapability(context, scope, required[operation] ?? "");
    if (denied !== undefined) return denied;
    // An application that is not active serves no settings, except the audit, which matters most while it is quarantined.
    if (operation !== "getSettingsIntegrationAudit") {
      const refusal = context.quarantine();
      if (refusal !== undefined) return refusal;
    }
    switch (operation) {
      case "getUserSettings":
        return record("user", scope);
      case "putUserSettings":
        return write("user", scope, body);
      case "getTenantSettings":
        return record("tenant", scope);
      case "putTenantSettings":
        return write("tenant", scope, body);
      case "getDeploymentSettings":
        return record("deployment", scope);
      case "putDeploymentSettings":
        return write("deployment", scope, body);
      case "getEffectiveSettings":
        return effective(scope);
      case "listSettingsProfiles": {
        const user = store.get(keyOf("user", scope))?.settings ?? {};
        const selected = typeof user["model_profile"] === "string" ? user["model_profile"] : profiles[0];
        return context.json(200, { profiles: profiles.map((id, index) => ({ id, default: index === 0, selected: id === selected })) });
      }
      case "getSettingsCapabilities": {
        const user = store.get(keyOf("user", scope))?.settings ?? {};
        const off = isRecord(user["capabilities"]) && Array.isArray(user["capabilities"]["disabled"]) ? (user["capabilities"]["disabled"] as unknown[]) : [];
        const access = (level: Level): { read: boolean; write: boolean } => ({
          read: scope.capability_scopes.includes(`settings.${level}.read`),
          write: scope.capability_scopes.includes(`settings.${level}.write`),
        });
        const view: SettingsCapabilityView = {
          capabilities: capabilities()
            .filter((c) => scope.capability_scopes.includes(c.id))
            .map((c) => ({
              id: c.id,
              risk_level: c.risk_level,
              enabled: !off.includes(c.id),
              disabled_by: off.includes(c.id) ? ["user"] : [],
              requires_confirmation: c.requires_confirmation,
              requires_step_up: c.requires_step_up,
              extra_confirmation: false,
              fields: c.fields,
            })),
          settings_access: { user: access("user"), tenant: access("tenant"), deployment: access("deployment") },
        };
        return context.json(200, view);
      }
      case "getSettingsIntegrationAudit": {
        const state = applicationStatus();
        const status: SettingsAuditStatus = {
          application_id: scope.application_id,
          state,
          audited: false,
          counts: { critical: 0, warnings: 0, info: 0, unverified: 0 },
          findings: [],
        };
        return context.json(200, status);
      }
      default: {
        const health: SettingsProviderHealth = {
          status: "ok",
          roles: [
            { role: "controller", configured: true, status: "ok" },
            { role: "embedder", configured: true, status: "ok" },
          ],
        };
        return context.json(200, health);
      }
    }
  };

  return { handle, reset: () => store.clear() };
};
