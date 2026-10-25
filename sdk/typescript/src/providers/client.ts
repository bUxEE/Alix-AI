import { timeout } from "../call.js";
import { errorFromResponse, isAlixAIError, sdkError } from "../errors.js";
import type { AlixAIError } from "../errors.js";
import type { CallOptions } from "../transport.js";
import { contextToken, normalizeBaseUrl } from "../validate.js";
import {
  isBillingNoticeAck,
  isBudget,
  isConnectionStatus,
  isEgressDisclosure,
  isEgressPolicy,
  isEgressPolicyView,
  isKillSwitch,
  isOAuthStarted,
  isProviderBudgets,
  isProviderList,
  isProviderMode,
  isUsageExport,
} from "./guards.js";
import { PROVIDERS_OPERATIONS } from "./operations.js";
import type { ProvidersOperationId } from "./operations.js";
import type {
  BillingNoticeAck,
  BillingNoticeAckInput,
  Budget,
  BudgetInput,
  ConnectionStatus,
  CredentialInput,
  DeploymentMode,
  DisclosureQuery,
  EgressDisclosure,
  EgressLevel,
  EgressPolicy,
  EgressPolicyUpdate,
  EgressPolicyView,
  KillSwitch,
  ModeLevel,
  OAuthCallbackInput,
  OAuthStarted,
  OAuthStartInput,
  ProviderBudgets,
  ProviderList,
  ProviderMode,
  ProviderSecret,
  UsageExport,
  UsageQuery,
} from "./types.js";

/** What the host's token provider is told about a providers request: the operationId, and the provider the path names, if it names one. */
export interface ProvidersContextTokenRequest {
  /** The OpenAPI operationId of the request. */
  operation: ProvidersOperationId;
  /** The provider id the path names, when it names one. */
  provider?: string;
}

/**
 * Supplies a fresh, single-use context token for one request. Called for every request and never cached by the SDK. Return the bare token (no
 * "Bearer " prefix). The caller must be an operator of the deployment, decided from the deployment's own configuration and not from the token.
 */
export type ProvidersContextTokenProvider = (request: ProvidersContextTokenRequest) => string | Promise<string>;

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface ProvidersClientOptions {
  /** The AlixAI service, e.g. "https://alixai.example.com" or "http://127.0.0.1:8080". A path prefix is kept. */
  baseUrl: string;
  /** Supplies a fresh single-use context token for each request. The SDK never mints, decodes, caches or retries with a token. */
  getContextToken: ProvidersContextTokenProvider;
  /** A fetch implementation. Defaults to the global `fetch`. */
  fetch?: FetchLike;
  /** Deadline for a request, in milliseconds. Default 30000; 0 disables it. */
  timeoutMs?: number;
  /** Allow plain http to a host that is not loopback. The context token is a bearer credential: leave this off. */
  allowInsecureHttp?: boolean;
}

/**
 * The operator-facing surface of External Provider Mode (additive plan 6.3, 11 and 16): provider connections, the deployment mode, the data-egress
 * policy, the egress disclosure, budgets and usage, the global kill switch and the browser OAuth flow.
 *
 * External mode is OFF by default. A deployment without the providers service (the local-only build, or external mode not enabled) answers every
 * method `unavailable`: that is the absence of the feature, never an empty result. Every method needs an operator (`permission_denied` for everybody
 * else). The application, tenant, workspace and principal are the verified context token and nothing an argument says.
 *
 * The SDK never persists, logs or echoes a provider credential. A manual key travels only inside {@link ProvidersClient.submitCredential}, and
 * a connected provider is represented to the host by a ConnectionStatus: the state and a safe account id. Money is integer micro-units.
 */
export interface ProvidersClient {
  /**
   * The provider profiles of the caller's application with the connection status of the caller's tenant: the state, how it is connected (`oauth` or
   * `api_key`), a safe account identifier, times, warnings, health, budget state and whether the billing notice was acknowledged. Never a credential
   * reference, a fingerprint or a secret.
   */
  list: (options?: CallOptions) => Promise<ProviderList>;
  /**
   * Sends a manual credential for a provider without a suitable OAuth flow. The one place a secret enters the system over HTTP, and write-only: the
   * key goes straight to the credential service, is validated with a minimally scoped provider request and stored in the secret backend; it is
   * never returned, logged or put in an error (the SDK refuses an answer that repeats it, and removes it from any error that quotes it). The status
   * shows the state and a safe account identifier, and a warning when the provider reports a key broader than needed (stored quarantined).
   * Prefer OAuth where the provider has it. A transport failure is `uncertain_outcome`: read {@link ProvidersClient.list} before sending again.
   */
  submitCredential: (provider: string, input: CredentialInput, options?: CallOptions) => Promise<ConnectionStatus>;
  /** Revokes the tenant's credential for a provider at once. Calls that need it fail closed and never fall back by themselves. */
  disconnect: (provider: string, options?: CallOptions) => Promise<ConnectionStatus>;
  /**
   * Records that the operator has seen the provider's billing notice (a consumer chat subscription is not API access; API usage is billed
   * separately) and confirms both statements. A provider is not usable until its notice is acknowledged. A stale notice version is `conflict`.
   */
  acknowledgeBillingNotice: (provider: string, input: BillingNoticeAckInput, options?: CallOptions) => Promise<BillingNoticeAck>;
  /**
   * What would leave the deployment for a request to a provider: which provider and model would process it, which data classes and categories may
   * leave, whether RAG excerpts are included, the provider's stated retention and training terms, an upper-bound cost and what happens when the
   * provider is unavailable. When any request-independent check fails (kill switch, policy, profile, allowlist, region) nothing is listed as leaving.
   */
  getDisclosure: (provider: string, query?: DisclosureQuery, options?: CallOptions) => Promise<EgressDisclosure>;
  /** The mode configured at each level (deployment, application, tenant), the effective mode (the most restrictive; `local` under the kill switch) and what decided it. Local is the default everywhere. */
  getMode: (options?: CallOptions) => Promise<ProviderMode>;
  /** Sets `local`, `hybrid` or `external` at one level. The tenant level needs a tenant administrator; the application and deployment levels a deployment operator. Audited. */
  setMode: (level: ModeLevel, mode: DeploymentMode, options?: CallOptions) => Promise<ProviderMode>;
  /** The data-egress policy (what may leave) at one level; `policy` is null for a level with none, and external processing is disabled there. `sourceId` names the source of a source-level policy, and only then. */
  getEgressPolicy: (level: EgressLevel, sourceId?: string, options?: CallOptions) => Promise<EgressPolicyView>;
  /** Creates or replaces the policy of one level and returns it as stored. A stale `expected_revision` is `conflict`. Credentials and raw application database content never leave, whatever a policy says. */
  setEgressPolicy: (update: EgressPolicyUpdate, options?: CallOptions) => Promise<EgressPolicy>;
  /** The per-provider, tenant, workspace and principal budgets (integer micro-units) of the caller's scope with the current state of each. */
  getBudgets: (options?: CallOptions) => Promise<ProviderBudgets>;
  /**
   * Creates (no `budget_id`) or changes (the `budget_id` and the `revision` that was read; a stale one is `conflict`) a budget and returns it as stored.
   * The subject, channel and currency of a budget never change. A provider-wide budget needs a deployment operator.
   */
  setBudget: (budget: BudgetInput, options?: CallOptions) => Promise<Budget>;
  /** The usage records (no prompt and no response) with totals per provider, channel and currency, for reconciliation with the provider's invoice. */
  exportUsage: (query?: UsageQuery, options?: CallOptions) => Promise<UsageExport>;
  /** Whether the global external-disable switch is engaged. A deployment operator only. */
  getKillSwitch: (options?: CallOptions) => Promise<KillSwitch>;
  /**
   * Engages or releases the global kill switch. Engaging stops every external provider request of the deployment at once, whatever any policy says,
   * and survives a restart; releasing it enables nothing by itself. `reason` is a fixed code, never free text. A deployment operator only.
   */
  setKillSwitch: (engaged: boolean, reason?: string, options?: CallOptions) => Promise<KillSwitch>;
  /**
   * Prepares the consent for the requested capabilities and begins the browser authorization. Open `authorization_url` in the user's browser and
   * show `consent` first. A provider without OAuth for API access answers `validation_failed` with `details.reason` `manual_fallback`
   * (test with {@link isManualFallback}): use {@link ProvidersClient.submitCredential} for it.
   */
  startOAuth: (provider: string, input: OAuthStartInput, options?: CallOptions) => Promise<OAuthStarted>;
  /**
   * Relays the redirect the browser followed, with a FRESH context token of the principal that started the flow (tokens are single use). A state that
   * is unknown, replayed, expired or issued to another scope is refused and spent. The code and the state are sent and never echoed or logged.
   */
  completeOAuth: (input: OAuthCallbackInput, options?: CallOptions) => Promise<ConnectionStatus>;
}

const DEFAULT_TIMEOUT_MS = 30_000;
const PLACEHOLDER = "[redacted]";
const PROVIDER_ID = /^[a-z][a-z0-9_-]{0,31}$/;
const KILL_REASON = /^[a-z][a-z0-9_]{0,63}$/;
const EGRESS_LEVELS: readonly string[] = ["deployment", "application", "tenant", "workspace", "source"];
const MODE_LEVELS: readonly string[] = ["deployment", "application", "tenant"];
const MODES: readonly string[] = ["local", "hybrid", "external"];

// ---- the secret ----

const SECRETS = new WeakMap<object, string>();
const INSPECT = Symbol.for("nodejs.util.inspect.custom");

/**
 * Wraps a credential value for {@link ProvidersClient.submitCredential}. The wrapper prints as `[redacted]` in `String()`, a template literal,
 * `JSON.stringify` and `util.inspect`, and holds the value where only this module can read it.
 */
export const providerSecret = (value: string): ProviderSecret => {
  if (typeof value !== "string") throw sdkError("validation_failed", "a secret is a string", { details: { argument: "value" } });
  const secret = Object.freeze({
    toString: (): string => PLACEHOLDER,
    toJSON: (): string => PLACEHOLDER,
    [Symbol.toPrimitive]: (): string => PLACEHOLDER,
    [INSPECT]: (): string => PLACEHOLDER,
  });
  SECRETS.set(secret, value);
  return secret;
};

/** The one place a key is read: while the request body is built. */
const revealKey = (key: unknown): string | undefined => {
  if (typeof key === "string") return key;
  if (typeof key === "object" && key !== null) return SECRETS.get(key);
  return undefined;
};

/** True for the answer of a provider without OAuth for API access: `validation_failed` with `details.reason` `manual_fallback`. */
export const isManualFallback = (error: unknown): boolean =>
  isAlixAIError(error) && error.code === "validation_failed" && error.details?.["reason"] === "manual_fallback";

// ---- validation, before a token is requested ----

const invalid = (argument: string, reason: string): AlixAIError => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });

const providerId = (value: unknown): string => {
  if (typeof value !== "string" || !PROVIDER_ID.test(value)) {
    throw invalid("provider", "it must be a provider id: a lowercase letter, then lowercase letters, digits, _ or -, at most 32 characters");
  }
  return value;
};

const bounded = (argument: string, value: unknown, max: number): string | undefined => {
  if (value === undefined) return undefined;
  if (typeof value !== "string" || value.length > max) throw invalid(argument, `it must be a string of at most ${max} characters`);
  return value;
};

const stringList = (argument: string, value: unknown, max: number, required = false): string[] | undefined => {
  if (value === undefined) {
    if (required) throw invalid(argument, "it is required");
    return undefined;
  }
  if (!Array.isArray(value) || value.length > max || (required && value.length === 0) || !value.every((v) => typeof v === "string" && v.length > 0)) {
    throw invalid(argument, `it must list ${required ? "1" : "0"} to ${max} non-empty strings`);
  }
  return value as string[];
};

const count = (argument: string, value: unknown): number | undefined => {
  if (value === undefined) return undefined;
  if (typeof value !== "number" || !Number.isInteger(value) || value < 0 || value > 2 ** 40) throw invalid(argument, "it must be a non-negative integer");
  return value;
};

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

const egressLevel = (level: unknown, sourceId: unknown): void => {
  if (typeof level !== "string" || !EGRESS_LEVELS.includes(level)) throw invalid("level", "it must be deployment, application, tenant, workspace or source");
  if ((level === "source") !== (sourceId !== undefined && sourceId !== "")) throw invalid("source_id", "it is for the source level and only for it");
  if (sourceId !== undefined && (typeof sourceId !== "string" || sourceId.length > 64)) throw invalid("source_id", "it must be a string of at most 64 characters");
};

// ---- the request ----

interface Sending<T> {
  operation: ProvidersOperationId;
  provider?: string;
  query?: URLSearchParams;
  body?: unknown;
  guard: (value: unknown) => value is T;
  /** A value that must appear nowhere in the answer or in an error (a credential, an authorization code). */
  secret?: string;
}

/** The cancellation and deadline of one call. */
const callScope = (operation: ProvidersOperationId, signal: AbortSignal | undefined, timeoutMs: number, safe: boolean) => {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const onAbort = (): void => controller.abort(signal?.reason);
  if (signal !== undefined) {
    if (signal.aborted) controller.abort(signal.reason);
    else signal.addEventListener("abort", onAbort, { once: true });
  }
  if (timeoutMs > 0 && !controller.signal.aborted) {
    timer = setTimeout(() => {
      controller.abort(
        safe
          ? sdkError("timeout", `the request timed out after ${timeoutMs} ms`, { details: { operation, timeout_ms: timeoutMs } })
          : sdkError("uncertain_outcome", `the request timed out after ${timeoutMs} ms and may have been processed: read the state before sending it again`, {
              details: { operation, phase: "timeout", timeout_ms: timeoutMs },
            }),
      );
    }, timeoutMs);
    (timer as { unref?: () => void }).unref?.();
  }
  return {
    signal: controller.signal,
    release: (): void => {
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
      signal?.removeEventListener("abort", onAbort);
    },
  };
};

/** An error that quotes the secret is replaced by a fixed one of the same code: the host never sees the value. */
const scrub = (error: unknown, secret: string | undefined): unknown => {
  if (secret === undefined || secret.length < 4 || !isAlixAIError(error)) return error;
  if (error.message.includes(secret) || JSON.stringify(error.details ?? {}).includes(secret)) {
    return sdkError(error.code, "the request was not accepted", { status: error.status, requestId: error.requestId, retryable: error.retryable });
  }
  return error;
};

/**
 * Creates the client of the providers and OAuth areas for one AlixAI service.
 *
 * Like the main client it uses `fetch` only, holds the credential in memory for the length of one call, follows no redirect, sends no cookie and
 * retries nothing. The shared transport (src/call.ts) is closed over the operations of the first SDK version, so this client sends its requests
 * itself, with the same rules.
 */
export const createProvidersClient = (options: ProvidersClientOptions): ProvidersClient => {
  if (typeof options !== "object" || options === null) throw sdkError("validation_failed", "createProvidersClient needs its options");
  const baseUrl = normalizeBaseUrl(options.baseUrl, options.allowInsecureHttp === true);
  if (typeof options.getContextToken !== "function") {
    throw sdkError("validation_failed", "getContextToken is required: the host supplies a fresh single-use token per request", {
      details: { argument: "getContextToken" },
    });
  }
  const requestTimeout = timeout(options.timeoutMs, DEFAULT_TIMEOUT_MS, "timeoutMs");

  const send = async <T>(sending: Sending<T>, callOptions: CallOptions | undefined): Promise<T> => {
    const { operation, provider, query, body, guard, secret } = sending;
    const spec = PROVIDERS_OPERATIONS[operation];
    const scope = callScope(operation, callOptions?.signal, timeout(callOptions?.timeoutMs, requestTimeout, "timeoutMs"), spec.safe);
    /** The error the host should see for whatever a fetch or a body read threw. */
    const failure = (error: unknown): unknown => {
      if (scope.signal.aborted) return scope.signal.reason;
      if (isAlixAIError(error)) return error;
      return spec.safe
        ? sdkError("unavailable", "the request could not be completed (network error)", { cause: error, details: { operation, phase: "transport" } })
        : sdkError("uncertain_outcome", "the request may not have completed (network error): read the state before sending it again", {
            cause: error,
            details: { operation, phase: "transport" },
          });
    };
    try {
      if (scope.signal.aborted) throw scope.signal.reason;
      let raw: unknown;
      try {
        raw = await options.getContextToken(provider === undefined ? { operation } : { operation, provider });
      } catch (cause) {
        if (scope.signal.aborted) throw scope.signal.reason;
        throw sdkError("unauthenticated", "getContextToken failed, so the request was not sent", { cause, details: { operation, reason: "token_provider_failed" } });
      }
      const token = contextToken(raw);
      // The call may have been aborted while the host was minting the token: do not spend it.
      if (scope.signal.aborted) throw scope.signal.reason;

      const path = spec.path.replace("{provider}", provider === undefined ? "" : encodeURIComponent(provider));
      const search = query !== undefined && query.size > 0 ? `?${query.toString()}` : "";
      const headers: Record<string, string> = { Accept: "application/json", Authorization: `Bearer ${token}` };
      if (body !== undefined) headers["Content-Type"] = "application/json";
      const fetchImpl: FetchLike = (input, init) => (options.fetch !== undefined ? options.fetch(input, init) : globalThis.fetch(input, init));
      let response: Response;
      try {
        response = await fetchImpl(`${baseUrl}${path}${search}`, {
          method: spec.method,
          headers,
          ...(body !== undefined && { body: JSON.stringify(body) }),
          signal: scope.signal,
          redirect: "error",
          credentials: "omit",
          cache: "no-store",
        });
      } catch (error) {
        throw failure(error);
      }
      if (!response.ok) {
        let refusal: unknown;
        try {
          refusal = await errorFromResponse(response);
        } catch (error) {
          refusal = failure(error);
        }
        throw refusal;
      }
      let text: string;
      try {
        text = await response.text();
      } catch (error) {
        throw failure(error);
      }
      const requestId = response.headers.get("X-Request-ID") ?? undefined;
      const unexpected = (reason: string, message: string) =>
        sdkError("internal", message, { status: response.status, requestId, details: { operation, reason } });
      // A server that repeats a secret anywhere in its answer is refused, and nothing of the answer reaches the host.
      if (secret !== undefined && secret.length >= 8 && text.includes(secret)) {
        throw unexpected("echoed_secret", "the server's answer repeats a value that must never be returned");
      }
      let parsed: unknown;
      try {
        parsed = JSON.parse(text);
      } catch (cause) {
        throw sdkError("internal", "the server answered with a body that is not JSON", { cause, status: response.status, requestId, details: { operation, reason: "not_json" } });
      }
      if (!guard(parsed)) throw unexpected("unexpected_shape", `the server's ${operation} response has an unexpected shape`);
      return parsed;
    } catch (error) {
      throw scrub(error, secret);
    } finally {
      scope.release();
    }
  };

  const get = <T>(operation: ProvidersOperationId, guard: (value: unknown) => value is T, callOptions: CallOptions | undefined, query?: URLSearchParams): Promise<T> =>
    send({ operation, guard, ...(query !== undefined && { query }) }, callOptions);

  return {
    list: async (callOptions) => get("listProviders", isProviderList, callOptions),

    submitCredential: async (provider, input, callOptions) => {
      const id = providerId(provider);
      if (!isRecord(input)) throw invalid("input", "it must be an object with a key");
      const key = revealKey(input.key);
      // The key is never named in a message.
      if (key === undefined || key.length === 0 || key.length > 4096 || /[\u0000-\u001f\u007f]/.test(key) || key.trim() !== key) {
        throw invalid("key", "it must be 1 to 4096 characters with no control character and no padding");
      }
      const label = bounded("label", input.label, 128);
      const expiresAt = bounded("expires_at", input.expires_at, 64);
      const scopes = stringList("scopes", input.scopes, 64);
      const body = { key, ...(label !== undefined && { label }), ...(expiresAt !== undefined && { expires_at: expiresAt }), ...(scopes !== undefined && { scopes }) };
      return send({ operation: "putProviderCredential", provider: id, body, guard: isConnectionStatus, secret: key }, callOptions);
    },

    disconnect: async (provider, callOptions) => send({ operation: "deleteProviderConnection", provider: providerId(provider), guard: isConnectionStatus }, callOptions),

    acknowledgeBillingNotice: async (provider, input, callOptions) => {
      const id = providerId(provider);
      if (!isRecord(input) || typeof input.notice_version !== "string" || input.notice_version.length === 0 || input.notice_version.length > 64) {
        throw invalid("notice_version", "it must be the notice version, 1 to 64 characters");
      }
      if (input.consumer_subscription_is_not_api_access !== true || input.separate_api_billing_understood !== true) {
        throw invalid("input", "both billing statements must be confirmed");
      }
      const body = {
        notice_version: input.notice_version,
        consumer_subscription_is_not_api_access: true,
        separate_api_billing_understood: true,
      };
      return send({ operation: "ackProviderBillingNotice", provider: id, body, guard: isBillingNoticeAck }, callOptions);
    },

    getDisclosure: async (provider, query = {}, callOptions) => {
      const id = providerId(provider);
      const params = new URLSearchParams();
      if (query.role !== undefined) {
        if (!["conversational_controller", "language_realization", "vision", "image_generation", "embedding", "stt", "tts"].includes(query.role)) {
          throw invalid("role", "it is not a known role");
        }
        params.set("role", query.role);
      }
      for (const source of stringList("source", query.source, 64) ?? []) {
        if (source.length > 64 || /[\s,]/.test(source)) throw invalid("source", "a source id is malformed");
        params.append("source", source);
      }
      const toksIn = count("estimated_toks_in", query.estimated_toks_in);
      if (toksIn !== undefined) params.set("estimated_toks_in", String(toksIn));
      const toksOut = count("max_output_toks", query.max_output_toks);
      if (toksOut !== undefined) params.set("max_output_toks", String(toksOut));
      return send({ operation: "getProviderDisclosure", provider: id, query: params, guard: isEgressDisclosure }, callOptions);
    },

    getMode: async (callOptions) => get("getProviderMode", isProviderMode, callOptions),

    setMode: async (level, mode, callOptions) => {
      if (!MODE_LEVELS.includes(level)) throw invalid("level", "it must be deployment, application or tenant");
      if (!MODES.includes(mode)) throw invalid("mode", "it must be local, hybrid or external");
      return send({ operation: "putProviderMode", body: { level, mode }, guard: isProviderMode }, callOptions);
    },

    getEgressPolicy: async (level, sourceId, callOptions) => {
      egressLevel(level, sourceId);
      const params = new URLSearchParams({ level });
      if (sourceId !== undefined && sourceId !== "") params.set("source_id", sourceId);
      return get("getProviderEgressPolicy", isEgressPolicyView, callOptions, params);
    },

    setEgressPolicy: async (update, callOptions) => {
      if (!isRecord(update)) throw invalid("update", "it must be an egress policy update");
      egressLevel(update.level, update.source_id);
      if (typeof update.source_mode !== "string" || !["local_only", "allow_excerpt", "allow_metadata", "deny"].includes(update.source_mode)) {
        throw invalid("source_mode", "it is not a known mode");
      }
      if (typeof update.enabled !== "boolean") throw invalid("enabled", "it must be a boolean");
      if (!Number.isInteger(update.expected_revision) || update.expected_revision < 0) throw invalid("expected_revision", "it must be a non-negative integer");
      for (const name of ["files", "audio"] as const) {
        const setting: unknown = update[name];
        if (setting === undefined) continue;
        if (!isRecord(setting) || !Object.keys(setting).every((key) => key === "enabled" || key === "max_class") || typeof setting["enabled"] !== "boolean" ||
          typeof setting["max_class"] !== "string" || !["public", "internal", "confidential"].includes(setting["max_class"])) {
          throw invalid(name, "it must be { enabled, max_class } with max_class public, internal or confidential");
        }
      }
      return send({ operation: "putProviderEgressPolicy", body: update, guard: isEgressPolicy }, callOptions);
    },

    getBudgets: async (callOptions) => get("getProviderBudgets", isProviderBudgets, callOptions),

    setBudget: async (budget, callOptions) => {
      if (!isRecord(budget) || !isRecord(budget.subject) || !["provider", "tenant", "workspace", "principal"].includes(String(budget.subject["kind"]))) {
        throw invalid("budget", "subject.kind, channel and currency are required");
      }
      if (!["interactive", "background"].includes(budget.channel) || typeof budget.currency !== "string" || budget.currency.length === 0) {
        throw invalid("budget", "subject.kind, channel and currency are required");
      }
      const limits = budget.limits;
      if (!isRecord(limits) || !Number.isInteger(limits.daily_micro_units) || !Number.isInteger(limits.monthly_micro_units) || limits.daily_micro_units < 0 || limits.monthly_micro_units < 0) {
        throw invalid("limits", "a limit is a non-negative integer number of micro-units");
      }
      if (!Number.isInteger(budget.warn_at_percent) || budget.warn_at_percent < 1 || budget.warn_at_percent > 99) throw invalid("warn_at_percent", "it must be 1 to 99");
      const exists = budget.budget_id !== undefined && budget.budget_id !== "";
      if (exists && !(Number.isInteger(budget.revision) && (budget.revision ?? 0) >= 1)) {
        throw invalid("revision", "a budget that exists is changed with the revision that was read");
      }
      // A new budget carries empty placeholders for what the core sets, as the server's create expects.
      const body = exists ? budget : { ...budget, budget_id: "", revision: 0, updated_at: "", updated_by: "" };
      return send({ operation: "putProviderBudget", body, guard: isBudget }, callOptions);
    },

    exportUsage: async (query = {}, callOptions) => {
      const params = new URLSearchParams();
      for (const name of ["from", "to"] as const) {
        const value = query[name];
        if (value !== undefined) {
          if (typeof value !== "string" || Number.isNaN(Date.parse(value))) throw invalid(name, "it must be an RFC 3339 time");
          params.set(name, value);
        }
      }
      if (query.provider !== undefined) params.set("provider", providerId(query.provider));
      if (query.channel !== undefined) {
        if (query.channel !== "interactive" && query.channel !== "background") throw invalid("channel", "it must be interactive or background");
        params.set("channel", query.channel);
      }
      if (query.scope !== undefined) {
        if (query.scope !== "tenant" && query.scope !== "application") throw invalid("scope", "it must be tenant or application");
        params.set("scope", query.scope);
      }
      params.set("format", "json");
      return get("exportProviderUsage", isUsageExport, callOptions, params);
    },

    getKillSwitch: async (callOptions) => get("getProviderKillSwitch", isKillSwitch, callOptions),

    setKillSwitch: async (engaged, reason, callOptions) => {
      if (typeof engaged !== "boolean") throw invalid("engaged", "it must be a boolean");
      if (reason !== undefined && reason !== "" && (typeof reason !== "string" || !KILL_REASON.test(reason))) {
        throw invalid("reason", "it must be a fixed code of lowercase letters, digits and _");
      }
      const body = reason === undefined || reason === "" ? { engaged } : { engaged, reason };
      return send({ operation: "putProviderKillSwitch", body, guard: isKillSwitch }, callOptions);
    },

    startOAuth: async (provider, input, callOptions) => {
      const id = providerId(provider);
      if (!isRecord(input)) throw invalid("input", "it must be an OAuth start request");
      const capabilities = stringList("capabilities", input.capabilities, 64, true);
      if (typeof input.redirect_uri !== "string" || input.redirect_uri.length === 0 || input.redirect_uri.length > 512) {
        throw invalid("redirect_uri", "it is required, at most 512 characters");
      }
      const label = bounded("label", input.label, 128);
      const body = {
        capabilities,
        redirect_uri: input.redirect_uri,
        ...(input.reauthorize !== undefined && { reauthorize: input.reauthorize }),
        ...(input.switch_account === true && { switch_account: true }),
        ...(label !== undefined && { label }),
      };
      return send({ operation: "startProviderOAuth", provider: id, body, guard: isOAuthStarted }, callOptions);
    },

    completeOAuth: async (input, callOptions) => {
      if (!isRecord(input) || typeof input.redirect_uri !== "string" || input.redirect_uri.length === 0 || input.redirect_uri.length > 512) {
        throw invalid("redirect_uri", "it is required, at most 512 characters");
      }
      const query = typeof input.query === "string" ? input.query.replace(/^\?/, "") : "";
      if (query.length === 0 || query.length > 4096) throw invalid("query", "it is required, at most 4096 characters");
      return send({ operation: "completeProviderOAuth", body: { redirect_uri: input.redirect_uri, query }, guard: isConnectionStatus, secret: query }, callOptions);
    },
  };
};
