// The provider-area types of the client. The documents the core computes are the generated ones (schemas/providers), re-exported so a
// host imports them from one place; the request and answer shapes that live only in schemas/api/paths/providers.yaml and oauth.yaml
// are written here, with the member names of the document.
//
// External Provider Mode is opt-in and OFF by default. A connected provider is represented to the host only by a ConnectionStatus: the
// state, how it is connected and a safe account id. No type here can hold a credential reference, a fingerprint, a token or a key
// after it was sent. Money is integer micro-units (a JSON number that is an integer), never a float.

import type {
  Budget,
  BudgetID,
  BudgetState,
  ConnectionStatus,
  DataClassification,
  DeploymentMode,
  EgressEligibleClass,
  EgressLevel,
  EgressMode,
  EgressPolicy,
  EgressPolicyContentEgress,
  ModelRef,
  ProviderID,
  RegionCode,
  Role,
  SafeAccount,
  SourceID,
} from "../gen/providers/index.js";

export type {
  BillingNoticeAck,
  Budget,
  BudgetID,
  BudgetState,
  Channel,
  ConnectionStatus,
  DataClassification,
  DeploymentMode,
  EgressDisclosure,
  EgressEligibleClass,
  EgressLevel,
  EgressMode,
  EgressPolicy,
  EgressPolicyContentEgress,
  ModelRef,
  ProviderID,
  RegionCode,
  Role,
  SafeAccount,
  SourceID,
} from "../gen/providers/index.js";

/** Where a mode is configured, widest first. */
export type ModeLevel = "deployment" | "application" | "tenant";

/** The provider profiles of the caller's application with the connection status of the caller's tenant. */
export interface ProviderList {
  providers: ConnectionStatus[];
}

/** The mode of the caller's scope: configured at each level, and the effective one (the most restrictive level; `local` under the kill switch). */
export interface ProviderMode {
  effective: DeploymentMode;
  configured: DeploymentMode;
  /** The level that set the effective mode, `default` or `kill_switch`. */
  decided_by: string;
  kill_switch: boolean;
  /** The mode of each level that has a setting. */
  levels: Partial<Record<ModeLevel, DeploymentMode>>;
}

/** The answer of getEgressPolicy: the policy, or null when the level has none (external processing is disabled there). */
export interface EgressPolicyView {
  policy: EgressPolicy | null;
}

/**
 * The operator-supplied members of an egress policy. The target, id, approval and timestamps are minted by the core.
 * `expected_revision` is the revision the writer read, 0 to create; a stale one is `conflict`.
 */
export interface EgressPolicyUpdate {
  level: EgressLevel;
  /** The RAG source of a source-level policy, and only then. */
  source_id?: SourceID;
  expected_revision: number;
  enabled: boolean;
  source_mode: EgressMode;
  /** The classification of the source: for a source-level policy, and only then. */
  source_class?: DataClassification;
  allowed_providers?: ProviderID[];
  allowed_models?: ModelRef[];
  allowed_classes?: EgressEligibleClass[];
  allowed_regions?: RegionCode[];
  /** Whether an excerpt of an uploaded file may leave under this level, and up to which class. Left out, every excerpt stays local. Not at source level. */
  files?: EgressPolicyContentEgress;
  /** Whether the raw audio of an utterance may leave under this level (to an external speech recogniser), and up to which class. Left out, all audio stays local. Not at source level. */
  audio?: EgressPolicyContentEgress;
}

/** The budgets of the caller's scope with the current state of each. */
export interface ProviderBudgets {
  budgets: Budget[];
  states: BudgetState[];
}

/**
 * A budget to create or change. Leave `budget_id` out to create one; a given one must carry its current `revision`. The core sets the id, the
 * revision, `updated_at` and `updated_by`, so whatever is put there is ignored. Money is integer micro-units.
 */
export type BudgetInput = Omit<Budget, "budget_id" | "revision" | "updated_at" | "updated_by"> & {
  budget_id?: BudgetID;
  revision?: number;
};

/** The global external-disable switch. */
export interface KillSwitch {
  engaged: boolean;
  /** A fixed reason code, never free text. */
  reason?: string;
  changed_by?: string;
  changed_at?: string;
}

/** What an operator confirms when they acknowledge a provider's billing notice. Both statements must be `true`: the SDK sends nothing else. */
export interface BillingNoticeAckInput {
  /** The profile's current notice version. */
  notice_version: string;
  /** Confirms that a consumer chat subscription is not API access. */
  consumer_subscription_is_not_api_access: true;
  /** Confirms that API usage is billed separately. */
  separate_api_billing_understood: true;
}

/** A credential value in transit. It prints as `[redacted]` in `String()`, `JSON.stringify` and `util.inspect`; only the SDK can read it. */
export interface ProviderSecret {
  toString: () => string;
  toJSON: () => string;
}

/**
 * A manual credential. `key` is write-only: a string or a {@link ProviderSecret}. Prefer the secret, which prints as a placeholder in a log,
 * a JSON document and `util.inspect`. The SDK sends the key once and keeps no copy of it.
 */
export interface CredentialInput {
  key: string | ProviderSecret;
  /** An optional name for the connection (at most 128 characters). */
  label?: string;
  /** An optional expiry, an RFC 3339 time. */
  expires_at?: string;
  /** The provider scopes the capability needs; the validation probe asks for no more. */
  scopes?: string[];
}

/** The request an egress disclosure is for. Every member is optional; the server's defaults apply. */
export interface DisclosureQuery {
  /** The model role (default `conversational_controller`). */
  role?: Role;
  /** The RAG sources the request would draw on. */
  source?: string[];
  estimated_toks_in?: number;
  max_output_toks?: number;
}

/** The usage to export. Every member is optional. The SDK asks for JSON; the CSV rendering is not spoken. */
export interface UsageQuery {
  /** An RFC 3339 time. */
  from?: string;
  to?: string;
  provider?: string;
  channel?: "interactive" | "background";
  /** The caller's tenant (default) or the whole application (a deployment operator). */
  scope?: "tenant" | "application";
}

/** The usage records (no prompt and no response) with totals per provider, channel and currency. */
export type UsageExport = Record<string, unknown>;

/** Begins a browser OAuth authorization. */
export interface OAuthStartInput {
  /** The capabilities to connect for; the engine requests the smallest set of provider scopes they map to. */
  capabilities: string[];
  /** Where the provider sends the browser back. For callback `loopback` send `http://127.0.0.1/oauth/loopback`. */
  redirect_uri: string;
  /** An existing connection to renew (re-consent or account switch). */
  reauthorize?: string;
  switch_account?: boolean;
  label?: string;
  /** `redirect` (default) or `popup` (1.1.0). */
  presentation?: "redirect" | "popup";
  /** `host` (default) or `loopback` (1.1.0). */
  callback?: "host" | "loopback";
  /** Exact origin of the host page; required when presentation is `popup`. */
  opener_origin?: string;
}

/** One scope as the consent shows it. */
export interface OAuthScope {
  name: string;
  description: string;
  data_access: string;
}

/** What the host shows before the user is sent to the provider. It holds no secret. */
export interface OAuthConsent {
  provider_name: string;
  /** The known account when re-authorizing, otherwise null members (the user picks it at the provider). */
  account: SafeAccount;
  scopes: OAuthScope[];
  data_access: string[];
  reauthorization: boolean;
  switch_account: boolean;
}

/** The answer of startOAuth. */
export interface OAuthStarted {
  /** Open this in the user's browser. It holds the state, the PKCE challenge and the nonce, never a token or a verifier. */
  authorization_url: string;
  redirect_uri: string;
  expires_at: string;
  consent: OAuthConsent;
  /** Present when presentation was `popup`; poll getOAuthFlow with it. */
  flow_id?: string;
  /** Suggested polling interval when flow_id is set. */
  poll_interval_ms?: number;
}

/** Server-side status of a popup OAuth flow (1.1.0). */
export interface OAuthFlowStatus {
  flow_id: string;
  provider: string;
  status: "pending" | "completed" | "failed" | "expired" | "cancelled";
  reason?: string;
  connection?: ConnectionStatus;
  updated_at: string;
}

/** The redirect the browser followed, relayed by the host. */
export interface OAuthCallbackInput {
  /** The redirect URI the browser arrived on, without the query. */
  redirect_uri: string;
  /** The query string of the redirect (`code`, `state`, `error`, `iss`). It holds the authorization code: sent, never echoed or logged. */
  query: string;
}
