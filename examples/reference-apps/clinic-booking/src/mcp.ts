import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import * as z from "zod";

import { isRefusal, refuse } from "./errors.ts";
import { APPLICATION_VERSION, CONTRACT_VERSION, inspect, parseAspects, type RuntimeInfo } from "./inspect.ts";
import { rejectedResult, unavailableResult, type HostResult } from "./results.ts";
import type { ClinicService } from "./domain/service.ts";
import type { FaultMode, Instrumentation } from "./instrumentation.ts";
import {
  changeRecord,
  cleanupDisposableTenant,
  createDisposableTenant,
  requireDisposable,
  seedRecords,
  type TestSupportEnv,
} from "./testsupport.ts";

// The MCP surface of the clinic application (streamable HTTP, JSON responses).
//
// WIRE CONTRACT (also in README.md; the package's adapter configuration states
// the same):
//
//   * Every capability tool takes `identity`: the minimum identity AlixAI sends
//     (application_id, tenant_id, workspace_id, principal_id, roles, session_id).
//     The host reauthorises each call from it; roles are checked against the
//     host's own directory, never trusted.
//   * read, validate and prepare take `fields`, the capability's candidate
//     field values. commit takes `idempotency_key` and `prepared`, the whole
//     PreparedAction as the core holds it. status takes `idempotency_key`.
//   * Every capability tool answers with `structuredContent`: the host members
//     of a DeterministicResult (status, identifiers, values, errors, freshness,
//     allowed_next_actions, and for prepare also preparation). `operation` and
//     `capability_id` are never stated: the core knows which call it made. The
//     same JSON is mirrored as text for clients that only show text.
//   * A refusal is a result with status `rejected` and typed errors, never an
//     MCP tool error. A tool error means the reply was lost or the call was
//     malformed, and after a commit it is an uncertain outcome that `status`
//     resolves.

export type McpContext = {
  service: ClinicService;
  instrumentation: Instrumentation;
  testSupport: boolean;
  runtime: () => RuntimeInfo;
};

const identitySchema = z
  .object({
    application_id: z.string().describe("The application the identity belongs to."),
    tenant_id: z.string().describe("The tenant (clinic group)."),
    workspace_id: z.string().describe("The workspace (clinic site)."),
    principal_id: z.string().describe("The principal making the request."),
    roles: z.array(z.string()).describe("The roles the principal holds."),
    session_id: z.string().describe("The AlixAI session the request belongs to."),
  })
  .describe("The minimum identity context AlixAI passes with every call.");

const idempotencyKeySchema = z.string().describe("The idempotency key minted at prepare time.");

const preparedSchema = z
  .looseObject({
    effects: z.array(
      z.looseObject({
        operation: z.string(),
        entity_type: z.string(),
        entity_id: z.string().optional(),
        changes: z
          .array(
            z.looseObject({
              field: z.string(),
              value: z.looseObject({ kind: z.string(), value: z.union([z.string(), z.number(), z.boolean()]) }),
            }),
          )
          .optional(),
      }),
    ),
    data_versions: z.array(z.looseObject({ entity_type: z.string(), entity_id: z.string(), version: z.string() })),
    expires_at: z.string().optional(),
    capability_id: z.string().optional(),
  })
  .describe("The PreparedAction the core holds: exact effects and the data versions it was validated against.");

const factSchema = z.looseObject({
  field: z.string(),
  value: z.looseObject({ kind: z.string() }),
  display: z.string().optional(),
});

// The host members of a DeterministicResult, for the tool's output schema.
const resultShape = {
  status: z.enum(["ok", "rejected", "queued", "partial", "uncertain", "unavailable"]),
  identifiers: z.array(z.looseObject({ entity_type: z.string(), entity_id: z.string() })),
  values: z.array(factSchema),
  errors: z.array(z.looseObject({ code: z.string(), message: z.string(), field: z.string().optional() })),
  freshness: z.looseObject({ as_of: z.string(), version: z.string() }).optional(),
  allowed_next_actions: z.array(z.looseObject({ action: z.string() })),
  follow_up: z.looseObject({ reference: z.string(), state: z.string(), as_of: z.string().optional() }).optional(),
  preparation: z
    .looseObject({
      summary: z.string(),
      effects: z.array(z.looseObject({})),
      data_versions: z.array(z.looseObject({})),
      declared_risk_level: z.number(),
      valid_for_seconds: z.number().optional(),
    })
    .optional(),
};

type ToolInput = Record<string, unknown>;

type ToolDefinition = {
  name: string;
  title: string;
  description: string;
  // Which capability operation the tool implements, for the control-surface
  // comparison with the package. Absent for lifecycle and test tools.
  capability?: string;
  operation?: "read" | "validate" | "prepare" | "commit" | "status" | "resolve";
  readOnly: boolean;
  inputShape: z.ZodRawShape;
  outputShape?: z.ZodRawShape;
  testOnly?: boolean;
  run: (args: ToolInput) => unknown;
};

const fieldsOf = (shape: z.ZodRawShape, description: string) => z.looseObject(shape).partial().describe(description);

const readTool = (
  name: string,
  title: string,
  capability: string,
  description: string,
  fields: z.ZodRawShape,
  run: (identity: unknown, fields: unknown) => HostResult,
): ToolDefinition => ({
  name,
  title,
  description,
  capability,
  operation: "read",
  readOnly: true,
  inputShape: { identity: identitySchema, fields: fieldsOf(fields, "Candidate field values.") },
  outputShape: resultShape,
  run: (args) => run(args.identity, args.fields),
});

// The resolution read of the core: the reserved arguments entity_type, mention
// and candidate_id travel in `fields` like any other read's fields.
const resolveTool = (
  name: string,
  title: string,
  entityType: string,
  description: string,
  run: (identity: unknown, fields: unknown) => HostResult,
): ToolDefinition => ({
  name,
  title,
  description,
  operation: "resolve",
  readOnly: true,
  inputShape: {
    identity: identitySchema,
    fields: fieldsOf(
      {
        entity_type: z.string().optional().describe(`The entity type being resolved; only "${entityType}" is answered here.`),
        mention: z.string().optional().describe("What the user said that names the record."),
        candidate_id: z
          .string()
          .optional()
          .describe("A record identifier the model proposed. Alone it re-resolves exactly that record, access-checked; with a mention it changes nothing."),
      },
      "The reserved arguments of an entity resolution.",
    ),
  },
  outputShape: resultShape,
  run: (args) => run(args.identity, args.fields),
});

const actionTools = (
  prefix: string,
  capability: string,
  title: string,
  fields: z.ZodRawShape,
  ops: ClinicService["book"],
): ToolDefinition[] => [
  {
    name: `${prefix}_validate`,
    title: `${title}: validate`,
    description: `Check candidate values for ${capability} without staging anything. On unchanged data freshness.version equals the one prepare reported.`,
    capability,
    operation: "validate",
    readOnly: true,
    inputShape: { identity: identitySchema, fields: fieldsOf(fields, "Candidate field values.") },
    outputShape: resultShape,
    run: (args) => ops.validate(args.identity, args.fields),
  },
  {
    name: `${prefix}_prepare`,
    title: `${title}: prepare`,
    description: `Validate and stage ${capability}: returns the exact effects, the data versions used and a validity period. Applies nothing.`,
    capability,
    operation: "prepare",
    readOnly: true,
    inputShape: { identity: identitySchema, fields: fieldsOf(fields, "Candidate field values.") },
    outputShape: resultShape,
    run: (args) => ops.prepare(args.identity, args.fields),
  },
  {
    name: `${prefix}_commit`,
    title: `${title}: commit`,
    description: `Apply a prepared ${capability} exactly once per idempotency key; a repeat returns the original receipt.`,
    capability,
    operation: "commit",
    readOnly: false,
    inputShape: { identity: identitySchema, idempotency_key: idempotencyKeySchema, prepared: preparedSchema },
    outputShape: resultShape,
    run: (args) => ops.commit(args.identity, args.idempotency_key, args.prepared),
  },
  {
    name: `${prefix}_status`,
    title: `${title}: status`,
    description: `What was recorded for an idempotency key of ${capability}; used to recover from a lost commit reply.`,
    capability,
    operation: "status",
    readOnly: true,
    inputShape: { identity: identitySchema, idempotency_key: idempotencyKeySchema },
    outputShape: resultShape,
    run: (args) => ops.status(args.identity, args.idempotency_key),
  },
];

export const CAPABILITY_TOOLS = (service: ClinicService): ToolDefinition[] => [
  readTool(
    "availability_list",
    "List availability",
    "availability.list",
    "List the free appointments of the caller's site.",
    {
      date: z.string().optional().describe("First day to search, YYYY-MM-DD in the site's time zone."),
      days: z.number().optional().describe("How many days to search."),
      doctor: z.string().optional().describe("Only this doctor (identifier)."),
      max_results: z.number().optional().describe("How many appointments to list."),
    },
    service.availabilityList,
  ),
  readTool(
    "patients_find",
    "Find a patient",
    "patients.find",
    "Find patients the caller may see, by name, optionally narrowed by date of birth.",
    {
      query: z.string().describe("Part of the patient's name, or a patient identifier."),
      date_of_birth: z.string().optional().describe("The patient's date of birth, YYYY-MM-DD."),
      max_results: z.number().optional().describe("How many patients to list."),
    },
    service.patientsFind,
  ),
  resolveTool(
    "resolve_patient",
    "Resolve a patient",
    "patient",
    "Resolve a mention to patients the caller may see; several matches are returned as candidates with a label each.",
    service.resolvePatient,
  ),
  resolveTool(
    "resolve_doctor",
    "Resolve a doctor",
    "doctor",
    "Resolve a mention to doctors of the caller's site.",
    service.resolveDoctor,
  ),
  resolveTool(
    "resolve_slot",
    "Resolve an appointment slot",
    "slot",
    "Resolve a slot identifier, YYYY-MM-DD [HH:MM] or \"the next available one\" to free slots of the caller's site.",
    service.resolveSlot,
  ),
  resolveTool(
    "resolve_appointment",
    "Resolve an appointment",
    "appointment",
    "Resolve an identifier, a reference, a date or a patient name to appointments the caller may see.",
    service.resolveAppointment,
  ),
  ...actionTools(
    "appointments_book",
    "appointments.book",
    "Book an appointment",
    {
      patient: z.string().describe("The patient (identifier)."),
      slot: z.string().describe("The appointment slot (identifier)."),
      reason: z.string().optional().describe("The reason for the visit, free text."),
    },
    service.book,
  ),
  ...actionTools(
    "appointments_cancel",
    "appointments.cancel",
    "Cancel an appointment",
    {
      appointment: z.string().describe("The appointment (identifier)."),
      reason: z.string().optional().describe("Why it is cancelled: patient_request, provider_unavailable, scheduling_error or other."),
    },
    service.cancel,
  ),
  ...actionTools(
    "patients_update_contact",
    "patients.update_contact",
    "Update contact details",
    {
      patient: z.string().describe("The patient (identifier)."),
      phone: z.string().optional().describe("The new phone number."),
      email: z.string().optional().describe("The new email address."),
    },
    service.contact,
  ),
];

const lifecycleTools = (context: McpContext, tools: ToolDefinition[]): ToolDefinition[] => [
  {
    name: "describe",
    title: "Describe the application",
    description: "The contract versions and the operations the application maps per capability.",
    readOnly: true,
    inputShape: {},
    run: () => {
      const byCapability = new Map<string, Set<string>>();
      for (const tool of tools) {
        if (tool.capability !== undefined && tool.operation !== undefined) {
          const set = byCapability.get(tool.capability) ?? new Set<string>();
          set.add(tool.operation);
          byCapability.set(tool.capability, set);
        }
      }
      return {
        contract_versions: [CONTRACT_VERSION],
        application_version: APPLICATION_VERSION,
        capabilities: [...byCapability.entries()]
          .sort(([a], [b]) => a.localeCompare(b))
          .map(([capability_id, operations]) => ({ capability_id, operations: [...operations].sort() })),
        unmapped: [],
      };
    },
  },
  {
    name: "health",
    title: "Health",
    description: "Whether the application can serve requests.",
    readOnly: true,
    inputShape: {},
    run: () => {
      const report = inspect(context.service.env.db, context.runtime(), ["health"]);
      const status = report.reports[0]?.facts.find((f) => f.field === "status");
      return { status: status !== undefined && "value" in status.value && status.value.value === "ok" ? "ok" : "unavailable", application_version: APPLICATION_VERSION };
    },
  },
  {
    name: "inspect",
    title: "Inspect the runtime",
    description: "Runtime and system facts the application chooses to expose to the integration audit, per aspect.",
    readOnly: true,
    inputShape: { aspects: z.array(z.string()).optional().describe("Aspects wanted; empty or absent means all.") },
    run: (args) => inspect(context.service.env.db, context.runtime(), parseAspects(args.aspects)),
  },
];

// The failures the audit may ask for, and the one-shot fault of the instrumentation each one is.
const AUDIT_FAULTS: Record<string, FaultMode> = { dependency_down: "unavailable", reply_lost: "error_after_commit" };

const testTools = (context: McpContext): ToolDefinition[] => {
  const env: TestSupportEnv = context.service.env;
  return [
    {
      name: "test_create_tenant",
      title: "Create a disposable tenant",
      description: "Creates a disposable tenant with workspaces and principals for the audit's runtime probes.",
      readOnly: false,
      testOnly: true,
      inputShape: {
        tenant_id: z.string().optional(),
        name: z.string().optional(),
        workspaces: z.array(z.looseObject({ workspace_id: z.string(), name: z.string().optional(), timezone: z.string().optional() })),
        principals: z
          .array(z.looseObject({ principal_id: z.string(), workspace_id: z.string(), roles: z.array(z.string()).optional() }))
          .optional(),
      },
      run: (args) => ({ status: "ok", ...createDisposableTenant(env, args) }),
    },
    {
      name: "test_seed",
      title: "Seed synthetic records",
      description: "Creates synthetic patients, doctors, slots and appointments in a disposable tenant and returns their identifiers by reference.",
      readOnly: false,
      testOnly: true,
      inputShape: {
        tenant_id: z.string(),
        records: z.array(
          z.looseObject({
            ref: z.string(),
            entity_type: z.string(),
            workspace_id: z.string(),
            principal_id: z.string().optional(),
          }),
        ),
      },
      run: (args) => ({ status: "ok", ...seedRecords(env, args) }),
    },
    {
      name: "test_change_record",
      title: "Change a record of a disposable tenant",
      description:
        "Moves the revision counter of one patient, slot or appointment of a disposable tenant without changing anything else about it, so the audit can show that validate notices a changed record and that an earlier preparation is refused as stale.",
      readOnly: false,
      testOnly: true,
      inputShape: { tenant_id: z.string(), entity_type: z.string(), entity_id: z.string() },
      run: (args) => ({ status: "ok", ...changeRecord(env, args) }),
    },
    {
      name: "test_inject_fault",
      title: "Arm one failure",
      description:
        "Arms one failure of the next request of an operation of a capability, for requests of the named disposable tenants only: dependency_down answers unavailable and applies nothing; reply_lost (commit only) applies the commit and loses its reply.",
      readOnly: false,
      testOnly: true,
      inputShape: {
        operation: z.string(),
        capability_id: z.string(),
        kind: z.string(),
        tenant_ids: z.array(z.string()),
      },
      run: (args) => {
        const kind = typeof args.kind === "string" && Object.hasOwn(AUDIT_FAULTS, args.kind) ? args.kind : undefined;
        if (kind === undefined) {
          throw refuse("validation_failed", "kind must be dependency_down or reply_lost", { field: "kind" });
        }
        if (kind === "reply_lost" && args.operation !== "commit") {
          throw refuse("validation_failed", "a reply can only be lost after a commit", { field: "operation" });
        }
        const tool = CAPABILITY_TOOLS(context.service).find(
          (t) => t.capability === args.capability_id && t.operation === args.operation,
        );
        if (tool === undefined) {
          throw refuse("validation_failed", "the capability has no such operation", { field: "operation" });
        }
        const tenants = args.tenant_ids;
        if (!Array.isArray(tenants) || tenants.length === 0 || tenants.length > 64) {
          throw refuse("validation_failed", "tenant_ids must name between 1 and 64 disposable tenants", { field: "tenant_ids" });
        }
        const disposable = tenants.map((tenant) => requireDisposable(env, tenant));
        context.instrumentation.injectFault(tool.name, AUDIT_FAULTS[kind] as FaultMode, 1, disposable);
        return { status: "ok", armed: true, tool: tool.name };
      },
    },
    {
      name: "test_cleanup",
      title: "Remove a disposable tenant",
      description: "Removes a disposable tenant and everything created for it, and reports that nothing is left.",
      readOnly: false,
      testOnly: true,
      inputShape: { tenant_id: z.string() },
      run: (args) => ({ status: "ok", ...cleanupDisposableTenant(env, args.tenant_id) }),
    },
  ];
};

export const allTools = (context: McpContext): ToolDefinition[] => {
  const capability = CAPABILITY_TOOLS(context.service);
  return [
    ...capability,
    ...lifecycleTools(context, capability),
    ...(context.testSupport ? testTools(context) : []),
  ];
};

// ---- calling a tool ---------------------------------------------------------

type ToolResult = {
  content: { type: "text"; text: string }[];
  structuredContent?: Record<string, unknown>;
  isError?: boolean;
};

const asResult = (value: unknown): ToolResult => ({
  content: [{ type: "text", text: JSON.stringify(value) }],
  structuredContent: value as Record<string, unknown>,
});

// The lost reply: the operation was applied, the caller learns nothing.
const lostReply = (): ToolResult => ({
  content: [{ type: "text", text: "internal error" }],
  isError: true,
});

// The tenant a request names. It only decides whether a fault armed for tenants applies; the host
// reauthorises the identity itself before it does anything.
const tenantOf = (identity: unknown): string | undefined =>
  typeof identity === "object" && identity !== null && typeof (identity as { tenant_id?: unknown }).tenant_id === "string"
    ? (identity as { tenant_id: string }).tenant_id
    : undefined;

export const callTool = (context: McpContext, tool: ToolDefinition, args: ToolInput): ToolResult => {
  context.instrumentation.recordCall(tool.name);
  const fault = context.testSupport ? context.instrumentation.takeFault(tool.name, tenantOf(args.identity)) : undefined;
  if (fault === "unavailable") {
    return asResult(unavailableResult("unavailable", "a dependency of the clinic application is down"));
  }
  let value: unknown;
  try {
    value = tool.run(args);
  } catch (error) {
    // Capability tools never throw (the service turns every failure into a
    // result). Lifecycle and test tools do: a refusal becomes a typed result.
    if (isRefusal(error)) {
      const { code, message, field } = error.refusal;
      value = tool.outputShape !== undefined ? rejectedResult(code, message, field === undefined ? {} : { field }) : {
        status: "rejected",
        errors: [{ code, message, ...(field === undefined ? {} : { field }) }],
      };
    } else {
      process.stderr.write(`${JSON.stringify({ level: "error", event: "tool_error", tool: tool.name })}\n`);
      return lostReply();
    }
  }
  if (fault === "error_after_commit") {
    return lostReply();
  }
  return asResult(value);
};

export const createMcpServer = (context: McpContext): McpServer => {
  const server = new McpServer({ name: "clinic-booking", version: APPLICATION_VERSION });
  for (const tool of allTools(context)) {
    server.registerTool(
      tool.name,
      {
        title: tool.title,
        description: tool.description,
        inputSchema: tool.inputShape,
        ...(tool.outputShape === undefined ? {} : { outputSchema: tool.outputShape }),
        annotations: { readOnlyHint: tool.readOnly, destructiveHint: false, openWorldHint: false },
      },
      // The SDK validated the arguments against inputShape before this runs.
      async (args: ToolInput) => callTool(context, tool, args),
    );
  }
  return server;
};
