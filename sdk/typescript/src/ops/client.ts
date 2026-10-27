import type { Caller } from "../call.js";
import { sdkError } from "../errors.js";
import type { PrivateLlmEndpoint } from "../gen/providers/private-llm-endpoint.js";
import type { PrivateLlmEndpointUpdate } from "../gen/providers/private-llm-endpoint-update.js";
import type { PrivateLlmPrincipalBinding } from "../gen/providers/private-llm-principal-binding.js";
import type { PrivateLlmPrincipalBindingUpdate } from "../gen/providers/private-llm-principal-binding-update.js";
import type { PrivateLlmTenantPin } from "../gen/providers/private-llm-tenant-pin.js";
import type { PrivateLlmTenantPinUpdate } from "../gen/providers/private-llm-tenant-pin-update.js";
import type { CallOptions } from "../transport.js";
import type { OpsOperationId } from "./operations.js";

export type ErasureLevel = "principal" | "tenant" | "workspace";
/** Tenant-local (default), application-wide, or one workspace; only with level principal. */
export type ErasureScope = "tenant" | "application" | "workspace";

export interface ErasureRequest {
  level: ErasureLevel;
  /** Only with level principal. `application` is deployment-operator only. */
  scope?: ErasureScope;
  tenant_id?: string;
  workspace_id?: string;
  principal_id?: string;
  reason?: string;
}

export interface ErasureJobAccepted {
  job_id: string;
  status: string;
}

export interface ErasureJobStatus {
  job_id: string;
  status: string;
  level: ErasureLevel | string;
  scope?: ErasureScope | string;
  tenant_id: string;
  workspace_id?: string;
  principal_id?: string;
  counts?: Record<string, unknown>;
  error?: string;
  finished_at?: string;
}

/** 1.4 callers may still pass a plain record; typed update is preferred (1.5.0). */
export type PrivateLlmEndpointBody = PrivateLlmEndpointUpdate | Record<string, unknown>;
export type PrivateLlmTenantPinBody = PrivateLlmTenantPinUpdate | Record<string, unknown>;
export type PrivateLlmPrincipalBindingBody = PrivateLlmPrincipalBindingUpdate | Record<string, unknown>;

export interface OpsApi {
  erasure: {
    start: (input: ErasureRequest, options?: CallOptions) => Promise<ErasureJobAccepted>;
    status: (jobId: string, options?: CallOptions) => Promise<ErasureJobStatus>;
  };
  privateLlm: {
    putEndpoint: {
      (endpointId: string, body: PrivateLlmEndpointUpdate, options?: CallOptions): Promise<PrivateLlmEndpoint>;
      (endpointId: string, body: Record<string, unknown>, options?: CallOptions): Promise<PrivateLlmEndpoint>;
    };
    getEndpoint: (endpointId: string, options?: CallOptions) => Promise<PrivateLlmEndpoint>;
    deleteEndpoint: (endpointId: string, options?: CallOptions) => Promise<void>;
    putTenantPin: {
      (tenantId: string, body: PrivateLlmTenantPinUpdate, options?: CallOptions): Promise<PrivateLlmTenantPin>;
      (tenantId: string, body: Record<string, unknown>, options?: CallOptions): Promise<PrivateLlmTenantPin>;
    };
    getTenantPin: (tenantId: string, options?: CallOptions) => Promise<PrivateLlmTenantPin>;
    deleteTenantPin: (tenantId: string, options?: CallOptions) => Promise<void>;
    putPrincipalBinding: {
      (principalId: string, body: PrivateLlmPrincipalBindingUpdate, options?: CallOptions): Promise<PrivateLlmPrincipalBinding>;
      (principalId: string, body: Record<string, unknown>, options?: CallOptions): Promise<PrivateLlmPrincipalBinding>;
    };
    getPrincipalBinding: (principalId: string, options?: CallOptions) => Promise<PrivateLlmPrincipalBinding>;
    deletePrincipalBinding: (principalId: string, options?: CallOptions) => Promise<void>;
  };
  /** Tenant analyst ability ceilings (ADR 0047). Tenant may only narrow. */
  analyst: {
    getTenant: (tenantId: string, options?: CallOptions) => Promise<Record<string, unknown>>;
    putTenant: (tenantId: string, body: Record<string, unknown>, options?: CallOptions) => Promise<Record<string, unknown>>;
  };
}

const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });
const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);
const JOB_ID = /^erj_[0-9A-HJKMNP-TV-Z]{26}$/;
const PLE_ID = /^ple_[0-9A-HJKMNP-TV-Z]{26}$/;
const LEVELS = ["principal", "tenant", "workspace"] as const;
const SCOPES = ["tenant", "application", "workspace"] as const;

const isAccepted = (value: unknown): value is ErasureJobAccepted =>
  isRecord(value) && typeof value["job_id"] === "string" && typeof value["status"] === "string";
const isStatus = (value: unknown): value is ErasureJobStatus =>
  isRecord(value) &&
  typeof value["job_id"] === "string" &&
  typeof value["status"] === "string" &&
  typeof value["level"] === "string" &&
  typeof value["tenant_id"] === "string";

/** Required members only — extra/optional fields from the server must not fail the guard. */
const isPrivateLlmEndpoint = (value: unknown): value is PrivateLlmEndpoint =>
  isRecord(value) &&
  typeof value["endpoint_id"] === "string" &&
  typeof value["origin"] === "string" &&
  typeof value["api_dialect"] === "string" &&
  Array.isArray(value["model_ids"]) &&
  Array.isArray(value["roles"]) &&
  typeof value["owner_kind"] === "string" &&
  typeof value["lifecycle"] === "string";

const isPrivateLlmTenantPin = (value: unknown): value is PrivateLlmTenantPin =>
  isRecord(value) && typeof value["tenant_id"] === "string" && typeof value["endpoint_id"] === "string";

const isPrivateLlmPrincipalBinding = (value: unknown): value is PrivateLlmPrincipalBinding =>
  isRecord(value) &&
  typeof value["principal_id"] === "string" &&
  typeof value["endpoint_id"] === "string" &&
  Array.isArray(value["roles"]);

/** Exported for unit tests of the required-member guards. */
export const privateLlmGuards = { isPrivateLlmEndpoint, isPrivateLlmTenantPin, isPrivateLlmPrincipalBinding };

export const createOpsApi = (caller: Caller): OpsApi => {
  const json = <T>(
    operation: OpsOperationId,
    params: Record<string, string>,
    guard: (value: unknown) => value is T,
    what: string,
    options: CallOptions | undefined,
    body?: string,
  ): Promise<T> =>
    caller.json(operation, params, guard, what, options, body === undefined ? undefined : { body: { contentType: "application/json", payload: body } });

  return {
    erasure: {
      start: async (input, options) => {
        if (!isRecord(input) || !(LEVELS as readonly string[]).includes(input.level)) {
          throw invalid("level", "it must be principal, tenant or workspace");
        }
        if (input.scope !== undefined && !(SCOPES as readonly string[]).includes(input.scope)) {
          throw invalid("scope", "it must be tenant, application or workspace");
        }
        const body: Record<string, unknown> = { level: input.level };
        if (input.scope !== undefined) body["scope"] = input.scope;
        if (input.tenant_id !== undefined) body["tenant_id"] = input.tenant_id;
        if (input.workspace_id !== undefined) body["workspace_id"] = input.workspace_id;
        if (input.principal_id !== undefined) body["principal_id"] = input.principal_id;
        if (input.reason !== undefined) body["reason"] = input.reason;
        return json("startErasure", {}, isAccepted, "erasure job", options, JSON.stringify(body));
      },
      status: async (jobId, options) => {
        if (typeof jobId !== "string" || !JOB_ID.test(jobId)) throw invalid("job_id", "it must be an erj_ ULID");
        return json("getErasureJob", { job_id: jobId }, isStatus, "erasure job", options);
      },
    },
    privateLlm: {
      putEndpoint: (async (endpointId: string, body: PrivateLlmEndpointBody, options?: CallOptions) => {
        if (typeof endpointId !== "string" || !PLE_ID.test(endpointId)) throw invalid("endpoint_id", "it must be a ple_ ULID");
        return json("putPrivateLlmEndpoint", { endpoint_id: endpointId }, isPrivateLlmEndpoint, "private LLM endpoint", options, JSON.stringify(body));
      }) as OpsApi["privateLlm"]["putEndpoint"],
      getEndpoint: async (endpointId, options) => {
        if (typeof endpointId !== "string" || !PLE_ID.test(endpointId)) throw invalid("endpoint_id", "it must be a ple_ ULID");
        return json("getPrivateLlmEndpoint", { endpoint_id: endpointId }, isPrivateLlmEndpoint, "private LLM endpoint", options);
      },
      deleteEndpoint: async (endpointId, options) => {
        if (typeof endpointId !== "string" || !PLE_ID.test(endpointId)) throw invalid("endpoint_id", "it must be a ple_ ULID");
        await caller.empty("deletePrivateLlmEndpoint", { endpoint_id: endpointId }, options);
      },
      putTenantPin: (async (tenantId: string, body: PrivateLlmTenantPinBody, options?: CallOptions) => {
        if (typeof tenantId !== "string" || tenantId === "") throw invalid("tenant_id", "it is required");
        return json("putTenantPrivateLlm", { tenant_id: tenantId }, isPrivateLlmTenantPin, "tenant private LLM pin", options, JSON.stringify(body));
      }) as OpsApi["privateLlm"]["putTenantPin"],
      getTenantPin: async (tenantId, options) => {
        if (typeof tenantId !== "string" || tenantId === "") throw invalid("tenant_id", "it is required");
        return json("getTenantPrivateLlm", { tenant_id: tenantId }, isPrivateLlmTenantPin, "tenant private LLM pin", options);
      },
      deleteTenantPin: async (tenantId, options) => {
        if (typeof tenantId !== "string" || tenantId === "") throw invalid("tenant_id", "it is required");
        await caller.empty("deleteTenantPrivateLlm", { tenant_id: tenantId }, options);
      },
      putPrincipalBinding: (async (principalId: string, body: PrivateLlmPrincipalBindingBody, options?: CallOptions) => {
        if (typeof principalId !== "string" || principalId === "") throw invalid("principal_id", "it is required");
        return json("putPrincipalPrivateLlm", { principal_id: principalId }, isPrivateLlmPrincipalBinding, "principal private LLM", options, JSON.stringify(body));
      }) as OpsApi["privateLlm"]["putPrincipalBinding"],
      getPrincipalBinding: async (principalId, options) => {
        if (typeof principalId !== "string" || principalId === "") throw invalid("principal_id", "it is required");
        return json("getPrincipalPrivateLlm", { principal_id: principalId }, isPrivateLlmPrincipalBinding, "principal private LLM", options);
      },
      deletePrincipalBinding: async (principalId, options) => {
        if (typeof principalId !== "string" || principalId === "") throw invalid("principal_id", "it is required");
        await caller.empty("deletePrincipalPrivateLlm", { principal_id: principalId }, options);
      },
    },
    analyst: {
      getTenant: async (tenantId, options) => {
        if (typeof tenantId !== "string" || tenantId === "") throw invalid("tenant_id", "it is required");
        return json("getTenantAnalyst", { tenant_id: tenantId }, isRecord, "tenant analyst", options);
      },
      putTenant: async (tenantId, body, options) => {
        if (typeof tenantId !== "string" || tenantId === "") throw invalid("tenant_id", "it is required");
        return json("putTenantAnalyst", { tenant_id: tenantId }, isRecord, "tenant analyst", options, JSON.stringify(body ?? {}));
      },
    },
  };
};
