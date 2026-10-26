import type { Caller } from "../call.js";
import { sdkError } from "../errors.js";
import type { CallOptions } from "../transport.js";
import type { OpsOperationId } from "./operations.js";

export type ErasureLevel = "principal" | "tenant" | "workspace";
/** Tenant-local (default) vs application-wide GDPR erasure; only with level principal. */
export type ErasureScope = "tenant" | "application";

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

export interface OpsApi {
  erasure: {
    start: (input: ErasureRequest, options?: CallOptions) => Promise<ErasureJobAccepted>;
    status: (jobId: string, options?: CallOptions) => Promise<ErasureJobStatus>;
  };
}

const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });
const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);
const JOB_ID = /^erj_[0-9A-HJKMNP-TV-Z]{26}$/;
const LEVELS = ["principal", "tenant", "workspace"] as const;
const SCOPES = ["tenant", "application"] as const;

const isAccepted = (value: unknown): value is ErasureJobAccepted =>
  isRecord(value) && typeof value["job_id"] === "string" && typeof value["status"] === "string";
const isStatus = (value: unknown): value is ErasureJobStatus =>
  isRecord(value) &&
  typeof value["job_id"] === "string" &&
  typeof value["status"] === "string" &&
  typeof value["level"] === "string" &&
  typeof value["tenant_id"] === "string";

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
          throw invalid("scope", "it must be tenant or application");
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
  };
};
