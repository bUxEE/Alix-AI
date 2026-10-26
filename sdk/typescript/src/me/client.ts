import type { Caller } from "../call.js";
import { sdkError } from "../errors.js";
import type { PrincipalLlm } from "../gen/providers/principal-llm.js";
import type { PrincipalLlmBindingUpdate } from "../gen/providers/principal-llm-binding-update.js";
import type { ProviderCatalog } from "../gen/providers/provider-catalog.js";
import type { CallOptions } from "../transport.js";
import type { MeOperationId } from "./operations.js";

// Per-principal LLM account (ADR 0041, 1.2.0). Scope is the verified token's principal. Never returns a secret.
//
// Manual keys are submitted only by the host server (`me.providers.submitCredential`). Do not call that method from
// browser code: the key must never sit in the page, in storage, or in a postMessage payload.

const ROLES = ["conversational_controller", "language_realization", "stt", "tts"] as const;
export type PrincipalLlmRole = (typeof ROLES)[number];

const PROVIDER_ID = /^[a-z][a-z0-9_-]{0,31}$/;

export interface PrincipalCredentialStatus {
  provider_id: string;
  status: string;
  account?: Record<string, unknown>;
}

export interface PrincipalCredentialInput {
  /** Write-only. Never echoed. Submit from the host server only, never from browser code. */
  key: string;
  label?: string;
}

export interface PrincipalBillingAckInput {
  notice_version: string;
  consumer_subscription_is_not_api_access: true;
  separate_api_billing_understood: true;
}

export interface PrincipalBillingAckResult {
  provider_id: string;
  notice_version: string;
  acknowledged_at: string;
}

export interface PrincipalOAuthStartInput {
  capabilities: readonly string[];
  redirect_uri: string;
  presentation?: "redirect" | "popup";
  callback?: "host" | "loopback";
  opener_origin?: string;
  label?: string;
}

export interface PrincipalOAuthStarted {
  authorization_url: string;
  redirect_uri: string;
  expires_at: string;
  flow_id?: string;
  poll_interval_ms?: number;
  consent?: Record<string, unknown>;
}

/** The per-principal LLM surface: `client.me`. */
export interface MeApi {
  /**
   * Operator-approved provider catalog a principal may pick (`GET /v1/providers/catalog`). No secrets.
   * Same as the brief's `providers.catalog()`.
   */
  catalog: (options?: CallOptions) => Promise<ProviderCatalog>;
  llm: {
    get: (options?: CallOptions) => Promise<PrincipalLlm>;
    putRole: (role: PrincipalLlmRole | string, body: PrincipalLlmBindingUpdate, options?: CallOptions) => Promise<PrincipalLlm>;
    deleteRole: (role: PrincipalLlmRole | string, options?: CallOptions) => Promise<PrincipalLlm>;
  };
  providers: {
    /**
     * Host-relayed manual key for the caller's principal. Write-only. Call from the host server only —
     * never from browser code.
     */
    submitCredential: (provider: string, input: PrincipalCredentialInput, options?: CallOptions) => Promise<PrincipalCredentialStatus>;
    deleteCredential: (provider: string, options?: CallOptions) => Promise<PrincipalCredentialStatus>;
    acknowledgeBillingNotice: (provider: string, input: PrincipalBillingAckInput, options?: CallOptions) => Promise<PrincipalBillingAckResult>;
    oauthStart: (provider: string, input: PrincipalOAuthStartInput, options?: CallOptions) => Promise<PrincipalOAuthStarted>;
  };
}

const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const providerId = (value: unknown): string => {
  if (typeof value !== "string" || !PROVIDER_ID.test(value)) {
    throw invalid("provider", "it must be a provider id: a lowercase letter, then lowercase letters, digits, _ or -, at most 32 characters");
  }
  return value;
};

const roleId = (value: unknown): string => {
  if (typeof value !== "string" || !(ROLES as readonly string[]).includes(value)) {
    throw invalid("role", "it must be conversational_controller, language_realization or stt");
  }
  return value;
};

const isProviderCatalog = (value: unknown): value is ProviderCatalog =>
  isRecord(value) && Array.isArray(value["entries"]);

const isPrincipalLlm = (value: unknown): value is PrincipalLlm =>
  isRecord(value) && typeof value["principal_id"] === "string" && typeof value["application_id"] === "string" && Array.isArray(value["bindings"]);

const isCredentialStatus = (value: unknown): value is PrincipalCredentialStatus =>
  isRecord(value) && typeof value["provider_id"] === "string" && typeof value["status"] === "string";

const isBillingAckResult = (value: unknown): value is PrincipalBillingAckResult =>
  isRecord(value) &&
  typeof value["provider_id"] === "string" &&
  typeof value["notice_version"] === "string" &&
  typeof value["acknowledged_at"] === "string";

const isOAuthStarted = (value: unknown): value is PrincipalOAuthStarted =>
  isRecord(value) &&
  typeof value["authorization_url"] === "string" &&
  typeof value["redirect_uri"] === "string" &&
  typeof value["expires_at"] === "string";

/** Builds the me area over a caller. */
export const createMeApi = (caller: Caller): MeApi => {
  const json = <T>(
    operation: MeOperationId,
    params: Record<string, string>,
    guard: (value: unknown) => value is T,
    what: string,
    options: CallOptions | undefined,
    body?: string,
  ): Promise<T> =>
    caller.json(operation, params, guard, what, options, body === undefined ? undefined : { body: { contentType: "application/json", payload: body } });

  return {
    catalog: async (options) => json("getProviderCatalog", {}, isProviderCatalog, "provider catalog", options),

    llm: {
      get: async (options) => json("getPrincipalLlm", {}, isPrincipalLlm, "principal llm", options),

      putRole: async (role, body, options) => {
        const id = roleId(role);
        if (!isRecord(body) || typeof body.profile_id !== "string" || body.profile_id.length === 0) {
          throw invalid("profile_id", "it is required");
        }
        const payload: Record<string, unknown> = { profile_id: body.profile_id };
        if (body.model_id !== undefined) {
          if (typeof body.model_id !== "string" || body.model_id.length === 0) throw invalid("model_id", "it must be a non-empty string when set");
          payload["model_id"] = body.model_id;
        }
        return json("putPrincipalLlmRole", { role: id }, isPrincipalLlm, "principal llm", options, JSON.stringify(payload));
      },

      deleteRole: async (role, options) => json("deletePrincipalLlmRole", { role: roleId(role) }, isPrincipalLlm, "principal llm", options),
    },

    providers: {
      submitCredential: async (provider, input, options) => {
        const id = providerId(provider);
        if (!isRecord(input) || typeof input.key !== "string" || input.key.length === 0 || input.key.length > 8192) {
          throw invalid("key", "it must be 1 to 8192 characters");
        }
        const key = input.key;
        const label = input.label;
        if (label !== undefined && (typeof label !== "string" || label.length === 0 || label.length > 80)) {
          throw invalid("label", "it must be 1 to 80 characters when set");
        }
        const payload = label === undefined ? { key } : { key, label };
        // Scrub: refuse an answer that repeats the key (same rule as the tenant providers client).
        const result = await json("putPrincipalProviderCredential", { provider: id }, isCredentialStatus, "principal credential", options, JSON.stringify(payload));
        if (key.length >= 8 && JSON.stringify(result).includes(key)) {
          throw sdkError("internal", "the server's answer repeats a value that must never be returned", {
            details: { operation: "putPrincipalProviderCredential", reason: "echoed_secret" },
          });
        }
        return result;
      },

      deleteCredential: async (provider, options) =>
        json("deletePrincipalProviderCredential", { provider: providerId(provider) }, isCredentialStatus, "principal credential", options),

      acknowledgeBillingNotice: async (provider, input, options) => {
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
        return json("ackPrincipalProviderBillingNotice", { provider: id }, isBillingAckResult, "billing acknowledgement", options, JSON.stringify(body));
      },

      oauthStart: async (provider, input, options) => {
        const id = providerId(provider);
        if (!isRecord(input)) throw invalid("input", "it must be an OAuth start request");
        if (!Array.isArray(input.capabilities) || input.capabilities.length === 0 || input.capabilities.length > 16 || !input.capabilities.every((c) => typeof c === "string" && c.length > 0)) {
          throw invalid("capabilities", "it must list 1 to 16 non-empty strings");
        }
        if (typeof input.redirect_uri !== "string" || input.redirect_uri.length === 0 || input.redirect_uri.length > 2048) {
          throw invalid("redirect_uri", "it is required, at most 2048 characters");
        }
        const body: Record<string, unknown> = {
          capabilities: [...input.capabilities],
          redirect_uri: input.redirect_uri,
        };
        if (input.presentation !== undefined) body["presentation"] = input.presentation;
        if (input.callback !== undefined) body["callback"] = input.callback;
        if (input.opener_origin !== undefined) body["opener_origin"] = input.opener_origin;
        if (input.label !== undefined) body["label"] = input.label;
        return json("startPrincipalOAuth", { provider: id }, isOAuthStarted, "principal oauth", options, JSON.stringify(body));
      },
    },
  };
};
