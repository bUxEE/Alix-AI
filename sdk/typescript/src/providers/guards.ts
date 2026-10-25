import type {
  BillingNoticeAck,
  Budget,
  BudgetState,
  ConnectionStatus,
  EgressDisclosure,
  EgressPolicy,
  KillSwitch,
  OAuthStarted,
  ProviderBudgets,
  ProviderList,
  ProviderMode,
  EgressPolicyView,
  UsageExport,
} from "./types.js";

// The response guards of the providers area, in the style of src/guards.ts. They check what the SDK relies on: a closed top-level object
// (a member the document does not have is refused, so a server that adds a secret-looking member is not the server this SDK speaks to),
// every required member, the enumerations, and the cross-member rules the documents state in prose (a provider that is not connected has no kind
// and no connection time; a request that stays local lists nothing as leaving). The schemas stay the authority: the tests validate the
// fake's answers against them with ajv.

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

const closed = (value: Record<string, unknown>, known: readonly string[]): boolean => Object.keys(value).every((key) => known.includes(key));

const has = (value: Record<string, unknown>, required: readonly string[]): boolean => required.every((key) => key in value);

const text = (value: unknown): value is string => typeof value === "string" && value.length > 0;

const nullableText = (value: unknown): boolean => value === null || typeof value === "string";

const integer = (value: unknown): value is number => typeof value === "number" && Number.isInteger(value);

const oneOf = (value: unknown, allowed: readonly string[]): boolean => typeof value === "string" && allowed.includes(value);

const list = (value: unknown): value is unknown[] => Array.isArray(value);

const MODES = ["local", "hybrid", "external"] as const;
const STATES = ["not_connected", "pending_authorization", "connected", "expired", "revoked", "invalid", "quarantined", "reauthorization_required"] as const;
const HEALTH = ["healthy", "degraded", "unavailable", "unknown"] as const;
const LIFECYCLE = ["candidate", "qualified", "quarantined", "retired"] as const;
const BUDGET_STATUS = ["ok", "warning", "exhausted"] as const;
const EGRESS_LEVELS = ["deployment", "application", "tenant", "workspace", "source"] as const;
const MODE_LEVELS = ["deployment", "application", "tenant"] as const;

const isProviderRef = (value: unknown): boolean =>
  isRecord(value) && closed(value, ["profile_id", "provider_id", "model_id", "model_version"]) && has(value, ["profile_id", "provider_id", "model_id", "model_version"]) &&
  Object.values(value).every(text);

const isSafeAccount = (value: unknown): boolean =>
  isRecord(value) && closed(value, ["account_id", "organization_id", "project_id"]) && has(value, ["account_id", "organization_id", "project_id"]) && Object.values(value).every(nullableText);

const CONNECTION_KEYS = [
  "as_of", "provider", "lifecycle", "state", "kind", "account", "connected_at", "expires_at", "warnings", "health", "last_error_kind", "budget_state",
  "billing_notice_acknowledged",
] as const;

/** True when `value` is a ConnectionStatus: complete, closed, with the enumerations and the not-connected rule. It holds no credential member. */
export const isConnectionStatus = (value: unknown): value is ConnectionStatus => {
  if (!isRecord(value) || !closed(value, CONNECTION_KEYS) || !has(value, CONNECTION_KEYS)) return false;
  if (!text(value["as_of"]) || !isProviderRef(value["provider"]) || !isSafeAccount(value["account"])) return false;
  if (!oneOf(value["lifecycle"], LIFECYCLE) || !oneOf(value["state"], STATES) || !oneOf(value["health"], HEALTH)) return false;
  const kind = value["kind"];
  if (kind !== null && !oneOf(kind, ["oauth", "api_key"])) return false;
  if (!nullableText(value["connected_at"]) || !nullableText(value["expires_at"]) || !nullableText(value["last_error_kind"])) return false;
  if (!list(value["warnings"]) || !value["warnings"].every((w) => typeof w === "string")) return false;
  const budget = value["budget_state"];
  if (budget !== null && !oneOf(budget, BUDGET_STATUS)) return false;
  if (typeof value["billing_notice_acknowledged"] !== "boolean") return false;
  if (value["state"] === "not_connected") return kind === null && value["connected_at"] === null && value["expires_at"] === null;
  return kind !== null && (value["state"] !== "connected" || value["connected_at"] !== null);
};

/** True when `value` is `{ providers: ConnectionStatus[] }`. */
export const isProviderList = (value: unknown): value is ProviderList =>
  isRecord(value) && closed(value, ["providers"]) && list(value["providers"]) && value["providers"].every(isConnectionStatus);

/** True when `value` is a recorded billing notice acknowledgement with both confirmations true. */
export const isBillingNoticeAck = (value: unknown): value is BillingNoticeAck => {
  const keys = ["ack_id", "profile_id", "provider_id", "notice_version", "notice_digest", "scope", "confirmations", "acknowledged_by", "acknowledged_at"];
  if (!isRecord(value) || !closed(value, keys) || !has(value, keys)) return false;
  const confirmations = value["confirmations"];
  return (
    text(value["ack_id"]) && text(value["notice_version"]) && text(value["acknowledged_by"]) && text(value["acknowledged_at"]) && isRecord(value["scope"]) &&
    isRecord(confirmations) && confirmations["consumer_subscription_is_not_api_access"] === true && confirmations["separate_api_billing_understood"] === true
  );
};

const DISCLOSURE_KEYS = [
  "generated_at", "provider", "role", "external_enabled", "data_classes", "data_categories", "rag_excerpts_included", "regions", "terms", "cost", "on_unavailable",
] as const;

/** True when `value` is an egress disclosure; a request that stays local lists nothing as leaving, and the cost is integer micro-units. */
export const isEgressDisclosure = (value: unknown): value is EgressDisclosure => {
  if (!isRecord(value) || !closed(value, DISCLOSURE_KEYS) || !has(value, DISCLOSURE_KEYS)) return false;
  if (!text(value["generated_at"]) || !isProviderRef(value["provider"]) || !text(value["role"]) || typeof value["external_enabled"] !== "boolean") return false;
  const { data_classes: classes, data_categories: categories, regions } = value;
  if (!list(classes) || !list(categories) || !list(regions) || typeof value["rag_excerpts_included"] !== "boolean") return false;
  if (value["external_enabled"] === false && (classes.length > 0 || categories.length > 0 || value["rag_excerpts_included"] === true)) return false;
  const cost = value["cost"];
  if (!isRecord(cost) || !oneOf(cost["budget_state"], BUDGET_STATUS) || !text(cost["currency"])) return false;
  if (cost["estimate_upper_micro_units"] !== null && !(integer(cost["estimate_upper_micro_units"]) && cost["estimate_upper_micro_units"] >= 0)) return false;
  const onUnavailable = value["on_unavailable"];
  return isRecord(value["terms"]) && isRecord(onUnavailable) && oneOf(onUnavailable["behavior"], ["fail", "fallback"]);
};

/** True when `value` is the mode of the caller's scope. Under the kill switch the effective mode is `local`. */
export const isProviderMode = (value: unknown): value is ProviderMode => {
  const keys = ["effective", "configured", "decided_by", "kill_switch", "levels"];
  if (!isRecord(value) || !closed(value, keys) || !has(value, keys)) return false;
  if (!oneOf(value["effective"], MODES) || !oneOf(value["configured"], MODES) || !text(value["decided_by"]) || typeof value["kill_switch"] !== "boolean") return false;
  const levels = value["levels"];
  if (!isRecord(levels) || !closed(levels, MODE_LEVELS) || !Object.values(levels).every((m) => oneOf(m, MODES))) return false;
  return value["kill_switch"] === false || value["effective"] === "local";
};

const POLICY_KEYS = [
  "policy_id", "revision", "level", "target", "enabled", "source_mode", "source_class", "allowed_providers", "allowed_models", "allowed_classes", "allowed_regions",
  "files", "audio", "approval", "updated_at", "updated_by",
] as const;

const OPTIONAL_POLICY_KEYS: readonly string[] = ["source_class", "files", "audio", "approval"];

const EGRESS_ELIGIBLE = ["public", "internal", "confidential"] as const;

/** The files or audio setting, when present: exactly `{ enabled, max_class }`, and restricted is never a class. */
const isContentEgress = (value: unknown): boolean =>
  isRecord(value) && closed(value, ["enabled", "max_class"]) && typeof value["enabled"] === "boolean" && oneOf(value["max_class"], EGRESS_ELIGIBLE);

/** True when `value` is a stored egress policy. */
export const isEgressPolicy = (value: unknown): value is EgressPolicy => {
  const required = POLICY_KEYS.filter((key) => !OPTIONAL_POLICY_KEYS.includes(key));
  if (!isRecord(value) || !closed(value, POLICY_KEYS) || !has(value, required)) return false;
  if (("files" in value && !isContentEgress(value["files"])) || ("audio" in value && !isContentEgress(value["audio"]))) return false;
  return (
    text(value["policy_id"]) && integer(value["revision"]) && value["revision"] >= 1 && oneOf(value["level"], EGRESS_LEVELS) && isRecord(value["target"]) &&
    typeof value["enabled"] === "boolean" && text(value["source_mode"]) && list(value["allowed_providers"]) && list(value["allowed_models"]) &&
    list(value["allowed_classes"]) && list(value["allowed_regions"]) && text(value["updated_at"]) && text(value["updated_by"])
  );
};

/** True when `value` is `{ policy: EgressPolicy | null }`: the member must be present. */
export const isEgressPolicyView = (value: unknown): value is EgressPolicyView =>
  isRecord(value) && closed(value, ["policy"]) && "policy" in value && (value["policy"] === null || isEgressPolicy(value["policy"]));

/** True when `value` is a budget as stored; money is integer micro-units. */
export const isBudget = (value: unknown): value is Budget => {
  const keys = ["budget_id", "revision", "subject", "channel", "currency", "limits", "warn_at_percent", "quotas", "rate_limits", "updated_at", "updated_by"];
  const required = ["budget_id", "revision", "subject", "channel", "currency", "limits", "warn_at_percent", "updated_at", "updated_by"];
  if (!isRecord(value) || !closed(value, keys) || !has(value, required)) return false;
  const limits = value["limits"];
  return (
    text(value["budget_id"]) && integer(value["revision"]) && value["revision"] >= 1 && isRecord(value["subject"]) && oneOf(value["channel"], ["interactive", "background"]) &&
    text(value["currency"]) && isRecord(limits) && integer(limits["daily_micro_units"]) && integer(limits["monthly_micro_units"]) &&
    integer(value["warn_at_percent"]) && text(value["updated_at"]) && text(value["updated_by"])
  );
};

const isBudgetState = (value: unknown): value is BudgetState => {
  if (!isRecord(value) || !has(value, ["budget_id", "subject", "channel", "currency", "as_of", "state", "windows", "local_fallback_available"])) return false;
  return text(value["budget_id"]) && oneOf(value["state"], BUDGET_STATUS) && list(value["windows"]) && value["windows"].length === 2 && value["windows"].every(isRecord);
};

/** True when `value` is `{ budgets, states }`. */
export const isProviderBudgets = (value: unknown): value is ProviderBudgets =>
  isRecord(value) && closed(value, ["budgets", "states"]) && list(value["budgets"]) && value["budgets"].every(isBudget) && list(value["states"]) && value["states"].every(isBudgetState);

/** True when `value` is the kill switch; a reason is a fixed code. */
export const isKillSwitch = (value: unknown): value is KillSwitch =>
  isRecord(value) && closed(value, ["engaged", "reason", "changed_by", "changed_at"]) && typeof value["engaged"] === "boolean" &&
  (!("reason" in value) || (typeof value["reason"] === "string" && /^[a-z][a-z0-9_]{0,63}$/.test(value["reason"]))) &&
  (!("changed_by" in value) || typeof value["changed_by"] === "string") && (!("changed_at" in value) || typeof value["changed_at"] === "string");

/** True when `value` is the answer of startOAuth: an authorization URL and the consent to show. It has no verifier and no token member. */
export const isOAuthStarted = (value: unknown): value is OAuthStarted => {
  const keys = ["authorization_url", "redirect_uri", "expires_at", "consent"];
  if (!isRecord(value) || !closed(value, keys) || !has(value, keys)) return false;
  const consent = value["consent"];
  const consentKeys = ["provider_name", "account", "scopes", "data_access", "reauthorization", "switch_account"];
  return (
    text(value["authorization_url"]) && text(value["redirect_uri"]) && text(value["expires_at"]) && isRecord(consent) && closed(consent, consentKeys) &&
    has(consent, consentKeys) && text(consent["provider_name"]) && isSafeAccount(consent["account"]) && list(consent["scopes"]) &&
    consent["scopes"].every((s) => isRecord(s) && closed(s, ["name", "description", "data_access"]) && has(s, ["name", "description", "data_access"])) &&
    list(consent["data_access"]) && typeof consent["reauthorization"] === "boolean" && typeof consent["switch_account"] === "boolean"
  );
};

/** True when `value` is a JSON object (the usage export: records and totals). */
export const isUsageExport = (value: unknown): value is UsageExport => isRecord(value);
