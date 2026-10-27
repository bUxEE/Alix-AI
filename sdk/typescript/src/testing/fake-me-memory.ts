import type { CoreOperationId } from "../operations.js";
import type { FakeContext, FakeScope, Json } from "./context.js";

const ROLES = new Set(["conversational_controller", "language_realization", "stt"]);
const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** Scripted answers for per-principal LLM (ADR 0041) and long-term memory (ADR 0042). */
export const createFakeMeMemory = (ctx: FakeContext) => {
  const principalLlm = (scope: FakeScope, bindings: Json[] = []): Json => ({
    principal_id: scope.principal_id,
    application_id: scope.application_id,
    bindings,
    updated_at: ctx.iso(ctx.nowMs()),
  });

  const memoryItem = (scope: FakeScope, id: string, text: string, status: string, origin: string): Json => ({
    id,
    application_id: scope.application_id,
    tenant_id: scope.tenant_id,
    workspace_id: scope.workspace_id,
    principal_id: scope.principal_id,
    text,
    origin,
    status,
    data_class: "internal",
    created_at: ctx.iso(ctx.nowMs()),
    updated_at: ctx.iso(ctx.nowMs()),
  });

  const handle = (operation: CoreOperationId, scope: FakeScope, params: Record<string, string>, body: unknown): Response | undefined => {
    switch (operation) {
      case "getProviderCatalog":
        return ctx.json(200, { entries: [] });
      case "getPrincipalLlm":
        return ctx.json(200, principalLlm(scope));
      case "putPrincipalLlmRole": {
        const role = params["role"] ?? "";
        if (!ROLES.has(role)) return ctx.problem("validation_failed", "role is not a principal LLM role");
        if (!isRecord(body) || typeof body["profile_id"] !== "string" || body["profile_id"] === "") {
          return ctx.problem("validation_failed", "profile_id is required");
        }
        const model = typeof body["model_id"] === "string" && body["model_id"] !== "" ? body["model_id"] : "gpt-test";
        return ctx.json(
          200,
          principalLlm(scope, [
            {
              role,
              profile_id: body["profile_id"],
              provider_id: "openai",
              model_id: model,
              account_status: "missing",
              billing_notice_acked: false,
              updated_at: ctx.iso(ctx.nowMs()),
            },
          ]),
        );
      }
      case "deletePrincipalLlmRole":
        return ctx.json(200, principalLlm(scope));
      case "putPrincipalProviderCredential": {
        if (!isRecord(body) || typeof body["key"] !== "string" || body["key"] === "") {
          return ctx.problem("validation_failed", "key is required");
        }
        return ctx.json(200, { provider_id: params["provider"] ?? "openai", status: "active" });
      }
      case "deletePrincipalProviderCredential":
        return ctx.json(200, { provider_id: params["provider"] ?? "openai", status: "revoked" });
      case "ackPrincipalProviderBillingNotice": {
        if (
          !isRecord(body) ||
          typeof body["notice_version"] !== "string" ||
          body["consumer_subscription_is_not_api_access"] !== true ||
          body["separate_api_billing_understood"] !== true
        ) {
          return ctx.problem("validation_failed", "billing notice acknowledgement is incomplete");
        }
        return ctx.json(200, {
          provider_id: params["provider"] ?? "openai",
          notice_version: body["notice_version"],
          acknowledged_at: ctx.iso(ctx.nowMs()),
        });
      }
      case "startPrincipalOAuth": {
        if (!isRecord(body) || !Array.isArray(body["capabilities"]) || typeof body["redirect_uri"] !== "string" || body["redirect_uri"] === "") {
          return ctx.problem("validation_failed", "capabilities and redirect_uri are required");
        }
        return ctx.json(200, {
          authorization_url: "https://example.test/oauth",
          redirect_uri: body["redirect_uri"],
          expires_at: ctx.iso(ctx.nowMs() + 60_000),
          flow_id: "oaf_01J0000000000000000000000A",
          poll_interval_ms: 1500,
        });
      }
      case "completePrincipalOAuth": {
        if (!isRecord(body) || typeof body["redirect_uri"] !== "string" || typeof body["query"] !== "string") {
          return ctx.problem("validation_failed", "redirect_uri and query are required");
        }
        return ctx.json(200, { provider_id: "openai", status: "active" });
      }
      case "getPrincipalOAuthFlow":
        return ctx.json(200, {
          flow_id: params["flow_id"] ?? "oaf_01J0000000000000000000000A",
          provider: "openai",
          status: "pending",
          updated_at: ctx.iso(ctx.nowMs()),
        });
      case "cancelPrincipalOAuthFlow":
        return new Response(null, { status: 204, headers: { "Cache-Control": "no-store", "X-Request-ID": "req_fake" } });
      case "startErasure": {
        if (!isRecord(body) || typeof body["level"] !== "string") {
          return ctx.problem("validation_failed", "level is required");
        }
        return ctx.json(202, { job_id: "erj_01J0000000000000000000000A", status: "queued" });
      }
      case "getErasureJob":
        return ctx.json(200, {
          job_id: params["job_id"] ?? "erj_01J0000000000000000000000A",
          status: "completed",
          level: "principal",
          tenant_id: scope.tenant_id,
          principal_id: "usr_alice",
          counts: { audit_events_erased: 1, sessions_removed: 1 },
          finished_at: ctx.iso(ctx.nowMs()),
        });
      case "putPrivateLlmEndpoint":
      case "getPrivateLlmEndpoint":
        return ctx.json(200, {
          endpoint_id: params["endpoint_id"] ?? "ple_01J0000000000000000000000A",
          origin: "http://127.0.0.1:8080",
          api_dialect: "openai-compatible",
          model_ids: ["local-model"],
          roles: ["conversational_controller"],
          owner_kind: "shared",
          lifecycle: "ready",
          updated_at: ctx.iso(ctx.nowMs()),
        });
      case "deletePrivateLlmEndpoint":
      case "deleteTenantPrivateLlm":
      case "deletePrincipalPrivateLlm":
        return new Response(null, { status: 204, headers: { "Cache-Control": "no-store", "X-Request-ID": "req_fake" } });
      case "putTenantPrivateLlm":
      case "getTenantPrivateLlm":
        return ctx.json(200, {
          tenant_id: params["tenant_id"] ?? scope.tenant_id,
          endpoint_id: "ple_01J0000000000000000000000A",
          updated_at: ctx.iso(ctx.nowMs()),
        });
      case "putPrincipalPrivateLlm":
      case "getPrincipalPrivateLlm":
        return ctx.json(200, {
          principal_id: params["principal_id"] ?? scope.principal_id,
          endpoint_id: "ple_01J0000000000000000000000A",
          roles: ["conversational_controller"],
          updated_at: ctx.iso(ctx.nowMs()),
        });
      case "getMeUsage":
        return ctx.json(200, { groups: [], next_cursor: null });
      case "getMeBudget":
        return ctx.json(200, { budgets: [], states: [] });
      case "listMemory":
        return ctx.json(200, { items: [] });
      case "createMemory": {
        if (!isRecord(body) || typeof body["text"] !== "string" || body["text"] === "") {
          return ctx.problem("validation_failed", "text is required");
        }
        return ctx.json(201, memoryItem(scope, "mem_01J0000000000000000000000A", body["text"], "active", "user_explicit"));
      }
      case "patchMemory": {
        const text = isRecord(body) && typeof body["text"] === "string" ? body["text"] : "updated";
        return ctx.json(200, memoryItem(scope, params["id"] ?? "mem_01J0000000000000000000000A", text, "active", "user_explicit"));
      }
      case "deleteMemory":
        return new Response(null, { status: 204, headers: { "Cache-Control": "no-store", "X-Request-ID": "req_fake" } });
      case "confirmMemory":
        return ctx.json(200, memoryItem(scope, params["id"] ?? "mem_01J0000000000000000000000A", "proposed fact", "active", "assistant_proposed"));
      case "rejectMemory":
        return ctx.json(200, memoryItem(scope, params["id"] ?? "mem_01J0000000000000000000000A", "proposed fact", "rejected", "assistant_proposed"));
      default:
        return undefined;
    }
  };

  return { handle };
};
