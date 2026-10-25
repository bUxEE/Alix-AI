import { readFileSync } from "node:fs";
import { join } from "node:path";

import { parse } from "yaml";

// The application's own AlixAI integration package, read the way a loader reads
// it: plain YAML and JSON, no interpretation. The tests below compare it with
// the running application, so the package cannot promise what the app does not
// do.

export const PACKAGE_DIR = join(import.meta.dirname, "..", "..", "package");

export const readText = (path: string): string => readFileSync(join(PACKAGE_DIR, path), "utf8");
export const readYaml = <T>(path: string): T => parse(readText(path)) as T;
export const readJson = <T>(path: string): T => JSON.parse(readText(path)) as T;

export type AdapterCall = { endpoint: string; mcp_tool: string; result_schema: string; timeout_ms?: number };

export type Field = {
  name: string;
  type: string;
  required: boolean;
  sensitive?: boolean;
  entity_type?: string;
  values?: string[];
  min_length?: number;
  max_length?: number;
  minimum?: number;
  maximum?: number;
  default?: { source: string; value?: unknown; key?: string };
};

export type Capability = {
  id: string;
  version: string;
  audience: string;
  permissions: string[];
  fields: Field[];
  entity_types: Record<string, { description: string; resolve?: AdapterCall }>;
  ambiguity: { id: string; field: string; when: string; action: string }[];
  validation: { id: string; fields: string[]; error_code: string }[];
  failure_states: { code: string; description: string }[];
  effect_class: string;
  risk_level: number;
  operations: Partial<Record<"read" | "validate" | "prepare" | "commit" | "status", AdapterCall>>;
  confirmation: { mode: string };
  prepared_action?: { ttl_seconds: number; invalidated_by: string[] };
  idempotency?: { key_source: string; window_seconds: number; on_replay: string };
  response_obligations: { fact: string; when: string }[];
  next_actions: { id: string; after: string; capability_id: string }[];
  doc_refs: string[];
};

export type Contract = {
  capability_id: string;
  language: string;
  terminology: string[];
  field_prompts: { field: string; prompt: string }[];
  clarifications: { rule_id: string; prompt: string }[];
  corrections: { acknowledgement: string; correctable_fields: string[]; prepared_action_invalidated?: string };
  confirmations?: { request: string; declined: string; expired: string };
  success: { outcome: string; text: string }[];
  failures: { code: string; text: string }[];
  next_actions: { id: string; label: string }[];
};

export type Manifest = {
  application: { id: string; version: string };
  endpoints: Record<string, { transport: string; url: string; adapter_config: string }>;
  context_exchange: { issuer: string; audience: string; key_ids: string[]; max_lifetime_seconds: number };
  tenancy: { roles: Record<string, unknown> };
  languages: { default: string; supported: string[] };
  capabilities: Record<string, string>;
  rag_sources: Record<string, string>;
  integrity: { publisher_key_ids: string[] };
};

export type Policy = { role_grants: { role: string; capabilities: string[] }[] };

export type DocEntry = {
  id: string;
  capability_id: string;
  facet: string;
  language: string;
  location: { path: string; anchor?: string };
};

export type Suite = {
  fixtures: {
    provisioning: Record<"create_tenant" | "seed" | "cleanup", { mcp_tool: string }> & { seed_data: string };
    tenants: { ref: string; workspaces: string[] }[];
    principals: { ref: string; tenant: string; workspace: string; roles: string[] }[];
    records: { ref: string; entity_type: string; tenant: string; workspace: string }[];
  };
  scope_probes: {
    id: string;
    dimension: string;
    operation: "read" | "validate" | "prepare" | "commit" | "status" | "retrieve";
    capability_id?: string;
    principal: string;
    target_record?: string;
    target_source?: string;
    expect: "allow" | "deny";
    error_code?: string;
  }[];
  safety_probes: {
    id: string;
    capability_id: string;
    principal: string;
    kind: string;
    expect: { outcome: string; error_code?: string };
  }[];
  conversation_cases: {
    id: string;
    language: string;
    capability_id: string;
    principal: string;
    turns: { user: string; expect: { act: string; missing_fields?: string[] } }[];
  }[];
  smoke: {
    paths: { id: string; path: string; principal: string; capability_id?: string; utterance: string }[];
    verify_cleanup: boolean;
  };
};

export const manifest = readYaml<Manifest>("alixai.yaml");

export const capabilities: Record<string, Capability> = Object.fromEntries(
  Object.entries(manifest.capabilities).map(([id, file]) => [id, readYaml<Capability>(file)]),
);

export const contract = (capabilityId: string, language: string): Contract => {
  const file = (manifest.capabilities[capabilityId] ?? "").replace(/^capabilities\//u, "").replace(/\.yaml$/u, "");
  return readYaml<Contract>(`conversation-contracts/${file}.${language}.yaml`);
};

export const policy = readYaml<Policy>("policies/default-access.yaml");
export const docIndex = readYaml<{ entries: DocEntry[] }>("documentation/index.yaml");
export const suite = readYaml<Suite>("tests/clinic-safety.yaml");
export const adapterConfig = readYaml<{ server: string; tools: { allow: string[] } }>("schemas/adapters/clinic-mcp.yaml");

export const resultSchema = (path: string): object => readJson<object>(path);

// {fact} and {input.field} placeholders of a template; {{ and }} are literal braces.
export const placeholders = (template: string): string[] =>
  [...template.replaceAll("{{", "").replaceAll("}}", "").matchAll(/\{([^{}]+)\}/gu)].map((m) => m[1] ?? "");
