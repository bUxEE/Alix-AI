import { describe, expect, it } from "vitest";

import { OPERATIONS, createClient, isAlixAIError } from "../src/index.js";
import { FAKE_HISTORY_SCOPES, FAKE_SETTINGS_SCOPES, createFakeServer, prepareAction, reply } from "../src/testing/index.js";
import type { FakeAlixAIServer } from "../src/testing/index.js";
import { describe as explain, loadSpec } from "./helpers/openapi.js";
import type { Spec } from "./helpers/openapi.js";

// The SDK, the fake server and schemas/api/openapi.yaml must agree. The OpenAPI document is the
// contract: when it changes (or when the SDK or the fake drifts from it) this file fails.

const spec: Spec = loadSpec();

describe("the SDK's operation table matches schemas/api/openapi.yaml", () => {
  it("every operation the SDK speaks exists in the document with the same method, path and authentication", () => {
    for (const [operationId, op] of Object.entries(OPERATIONS)) {
      const declared = spec.operations.find((o) => o.operationId === operationId);
      expect(declared, `${operationId} is not in the OpenAPI document`).toBeDefined();
      expect({ method: declared?.method, path: declared?.path, authenticated: declared?.authenticated }, operationId).toEqual({
        method: op.method,
        path: op.path,
        authenticated: op.authenticated,
      });
    }
  });

  it("covers every operation of the areas this SDK version implements (sessions, actions, applications, files, history, settings, probes)", () => {
    // The RAG area is spoken too (src/rag, its own table, checked by test/rag.test.ts); voice, providers, oauth, decision and ops have no operations yet.
    const implemented = new Set(["root", "sessions", "actions", "apps", "files", "history", "settings"]);
    const missing = spec.operations.filter((o) => implemented.has(o.area) && !(o.operationId in OPERATIONS)).map((o) => o.operationId);
    expect(missing, "operations in the document that the SDK does not speak").toEqual([]);
  });

  it("has a row for every operation of the document whose area is implemented, and for nothing the document lacks", () => {
    const documented = spec.operations.filter((o) => ["root", "sessions", "actions", "apps", "files", "history", "settings"].includes(o.area));
    expect(Object.keys(OPERATIONS).sort()).toEqual(documented.map((o) => o.operationId).sort());
  });
});

describe("the validator really enforces the document (negative controls)", () => {
  const session = { session_id: "ses-1", status: "active", turn_count: 0, created_at: "2026-10-02T00:00:00Z", updated_at: "2026-10-02T00:00:00Z", pending: null };

  it("accepts a valid session and refuses one that is wrong", () => {
    const validate = spec.responseValidator("getSession", 200)!;
    expect(validate(session), explain(validate)).toBe(true);
    for (const bad of [{ ...session, status: "paused" }, { ...session, turn_count: -1 }, { ...session, extra: 1 }, { session_id: "ses-1" }]) {
      expect(validate(bad), JSON.stringify(bad)).toBe(false);
    }
  });

  it("resolves the shared schemas: errors and prepared actions are checked deeply", () => {
    const error = spec.responseValidator("getSession", 403)!;
    expect(error({ code: "scope_violation", message: "no", retryable: false })).toBe(true);
    expect(error({ code: "made_up", message: "no", retryable: false })).toBe(false);
    const action = spec.responseValidator("getAction", 200)!;
    expect(action({ preparation_id: "x" })).toBe(false);
  });

  it("knows which statuses each operation declares", () => {
    expect(spec.declaredStatuses("confirmAction").sort()).toEqual([200, 400, 401, 403, 409, 500, 503]);
    expect(spec.declaredStatuses("createSession")).not.toContain(404);
  });
});

/** A client over the fake whose every response is checked against the OpenAPI document. */
const conformance = (fake: FakeAlixAIServer, session = "ses-1") => {
  const violations: string[] = [];
  let validated = 0;
  const statuses = new Map<string, Set<number>>();
  const fetch = async (url: string, init?: RequestInit): Promise<Response> => {
    const response = await fake.fetch(url, init);
    const entry = fake.requests.at(-1);
    const operationId = entry?.operation;
    if (operationId === undefined) {
      violations.push(`${init?.method ?? "GET"} ${url}: no operation matched`);
      return response;
    }
    (statuses.get(operationId) ?? statuses.set(operationId, new Set()).get(operationId)!).add(response.status);
    if (operationId === "sendTurn" && typeof init?.body === "string") {
      const request: unknown = JSON.parse(init.body);
      const validateRequest = spec.componentValidator("TurnRequest");
      validated += 1;
      if (!validateRequest(request)) violations.push(`sendTurn request: ${explain(validateRequest)}
${init.body}`);
    }
    if (!spec.declaredStatuses(operationId).includes(response.status)) {
      violations.push(`${operationId}: the fake answered ${response.status}, which the document does not declare`);
    }
    const contentType = response.headers.get("Content-Type") ?? "";
    if (contentType.includes("application/json")) {
      const validate = spec.responseValidator(operationId, response.status);
      if (validate === undefined) violations.push(`${operationId} ${response.status}: no JSON schema declared`);
      else {
        const body: unknown = JSON.parse(await response.clone().text());
        validated += 1;
        if (!validate(body)) violations.push(`${operationId} ${response.status}: ${explain(validate)}\n${JSON.stringify(body)}`);
      }
    }
    if (contentType.includes("text/event-stream") && operationId !== "sendTurn" && operationId !== "streamTurnEvents") {
      violations.push(`${operationId}: an event stream from an operation that has none`);
    }
    return response;
  };
  const scopes = ["demo.lookup", "demo.book", "demo.close_account", "applications.register", ...FAKE_HISTORY_SCOPES, ...FAKE_SETTINGS_SCOPES];
  const client = createClient({ baseUrl: fake.baseUrl, fetch, getContextToken: () => fake.issueToken({ session_id: session, capability_scopes: scopes }) });
  return { client, violations, statuses, validated: () => validated };
};

describe("the TurnRequest of the document carries the attachments the SDK sends (negative controls)", () => {
  const validate = spec.componentValidator("TurnRequest");
  const id = "fil_0123456789ABCDEFGHJKMNPQRS";

  it("accepts up to four file ids with a text", () => {
    expect(validate({ text: "hi", attachments: [id] })).toBe(true);
    expect(validate({ text: "hi", language: "it", attachments: [id, id, id, id] })).toBe(true);
  });

  it("refuses five, a malformed id, and files with a choice, a confirmation or a rejection", () => {
    for (const bad of [
      { text: "hi", attachments: [id, id, id, id, id] },
      { text: "hi", attachments: ["../../etc/passwd"] },
      { choice_id: "c1", attachments: [id] },
      { confirmation: { preparation_id: "prep_1", token: "cfm_" + "A".repeat(43) }, attachments: [id] },
      { reject: { preparation_id: "prep_1" }, attachments: [id] },
    ]) {
      expect(validate(bad), JSON.stringify(bad)).toBe(false);
    }
  });
});

const BOOK = { capability_id: "demo.book", summary: "Book the 10:00 slot", text: "Shall I?" };

describe("every answer of the fake server conforms to the OpenAPI document", () => {
  it("across all operations, success and failure paths", async () => {
    const fake = createFakeServer();
    const { client, violations, statuses, validated } = conformance(fake);

    fake.onTurn((turn) => {
      const text = "text" in turn.input ? turn.input.text : "";
      if (text === "reply") return reply("hello");
      if (text.startsWith("commit:")) return prepareAction({ ...BOOK, commit: { status: text.slice(7) as "rejected" | "partial" | "uncertain" | "queued" } });
      if (text === "step-up") return prepareAction({ ...BOOK, risk_level: 3 });
      return prepareAction(BOOK);
    });

    await client.getLiveness();
    await client.getReadiness();
    await client.getHealth();
    await client.createSession();
    await client.getSession("ses-1");
    await client.getPendingState("ses-1");
    await client.listCapabilities();
    await client.getApplication("fake-app");
    await client.registerApplication(new Uint8Array([0x1f, 0x8b, 8, 0, 9]));
    await client.registerApplication(new Uint8Array([0x1f, 0x8b, 8, 0, 9]));
    await client.sendTurnJson("ses-1", { text: "reply" });
    await client.sendTurn("ses-1", { text: "reply" }).result();

    for (const status of ["committed", "rejected", "partial", "uncertain", "queued"]) {
      const text = status === "committed" ? "book" : `commit:${status}`;
      const grant = (await client.sendTurnJson("ses-1", { text })).confirmation!;
      expect(await client.getAction(grant.preparation_id)).toBeDefined();
      try {
        await client.confirmAction(grant.preparation_id, grant.token);
      } catch (error) {
        expect(isAlixAIError(error)).toBe(true);
      }
      await client.getActionReceipt(grant.preparation_id);
      await client.confirmAction(grant.preparation_id, grant.token).catch(() => undefined); // replayed
    }
    const rejected = (await client.sendTurnJson("ses-1", { text: "book" })).confirmation!;
    await client.rejectAction(rejected.preparation_id);
    const another = (await client.sendTurnJson("ses-1", { text: "book" })).confirmation!;
    await client.getActionReceipt(another.preparation_id).catch(() => undefined); // conflict: no outcome yet
    await client.sendTurnJson("ses-1", { text: "step-up" });

    // ---- reading a turn again: SSE from a cursor, the end of a finished turn (204), and the refusals ----
    const first = await client.sendTurnJson("ses-1", { text: "reply" });
    const lastSeq = first.events.length - 1;
    expect((await client.streamTurnEvents("ses-1", first.turn_id).result()).events).toEqual(first.events);
    expect((await client.streamTurnEvents("ses-1", first.turn_id, { cursor: 1 }).result()).events).toEqual(first.events.slice(2));
    expect((await client.streamTurnEvents("ses-1", first.turn_id, { cursor: lastSeq }).result()).events).toEqual([]); // 204
    await client.streamTurnEvents("ses-1", first.turn_id, { cursor: lastSeq + 5 }).result().catch(() => undefined); // 400
    await client.streamTurnEvents("ses-1", "trn_nope").result().catch(() => undefined); // 403
    const elsewhere = createClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: () => fake.issueToken({ session_id: "ses-9" }) });
    await elsewhere.streamTurnEvents("ses-9", first.turn_id).result().catch(() => undefined); // 403: the turn is another session's

    // ---- files: upload, read, preview, download (denied for an ephemeral context, allowed for an object), delete, refusals ----
    const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3]);
    const ref = await client.files.upload({ data: png, name: "diagram.png", mimeType: "image/png" });
    await client.files.get(ref.file_id);
    await client.files.preview(ref.file_id);
    await client.files.download(ref.file_id).catch(() => undefined); // permission_denied: an ephemeral context has no download
    const note = await client.files.upload({ data: new TextEncoder().encode("hello"), name: "note.txt", mimeType: "text/plain", treatment: "application_object" });
    await client.files.download(note.file_id);
    await client.files.preview(note.file_id);
    const pdf = await client.files.upload({ data: new TextEncoder().encode("%PDF-1.4"), name: "a.pdf", mimeType: "application/pdf", treatment: "application_object" });
    await client.files.preview(pdf.file_id).catch(() => undefined); // 415 not_previewable
    await client.files.upload({ data: new TextEncoder().encode("not a png"), name: "fake.png", mimeType: "image/png" }).catch(() => undefined); // 400 mime_mismatch
    await client.files.delete(note.file_id);
    await client.files.get(note.file_id).catch(() => undefined); // 403 scope_violation: deleted

    // ---- turn attachments: the ids travel in the request (validated against TurnRequest above); a deleted or unknown file is the one scope_violation ----
    await client.sendTurnJson("ses-1", { text: "reply", attachments: [ref.file_id] });
    await client.sendTurn("ses-1", { text: "reply", attachments: [ref.file_id, ref.file_id] }).result();
    await client.sendMessage("ses-1", { text: "reply", attachments: [{ data: png, name: "again.png", mimeType: "image/png" }] }).result();
    await client.sendTurnJson("ses-1", { text: "reply", attachments: [note.file_id] }).catch(() => undefined); // 403 scope_violation: deleted
    await client.sendTurnJson("ses-1", { text: "reply", attachments: ["fil_0123456789ABCDEFGHJKMNPQRS"] }).catch(() => undefined); // 403 scope_violation: unknown

    // ---- history: the turns above are the transcript ----
    const page = await client.history.segments("ses-1", { limit: 2 });
    expect(page.next_cursor).not.toBeNull();
    await client.history.segments("ses-1", { cursor: page.next_cursor! });
    await client.history.segments("ses-9").catch(() => undefined); // 403
    expect((await client.history.search({ text: "hello" })).hits.length).toBeGreaterThan(0);
    const exported = await client.history.export({ session_id: "ses-1" }).result();
    expect(exported.length).toBeGreaterThan(0);
    await client.history.export({ session_id: "bad id" }).result().catch(() => undefined); // 400
    await client.history.cleanup(new Date(Date.now() + 60_000)).catch(() => undefined); // 400: in the future
    expect((await client.history.cleanup(new Date(Date.now() - 3600_000))).rows_deleted).toBe(0);
    expect((await client.history.clear("ses-1")).rows_deleted).toBeGreaterThan(0);
    const noHistory = createClient({ baseUrl: fake.baseUrl, fetch, getContextToken: () => fake.issueToken({ session_id: "ses-1", capability_scopes: [] }) });
    await noHistory.history.segments("ses-1").catch(() => undefined); // 403 permission_denied

    // ---- settings: versioned documents, conflicts, refusals, the views ----
    const user = await client.settings.getUser();
    expect(user.version).toBe(0);
    const stored = await client.settings.putUser({ expected_version: 0, settings: { language: "it", model_profile: "lite" } });
    expect(stored.version).toBe(1);
    await client.settings.putUser({ expected_version: 0, settings: { language: "en" } }).catch(() => undefined); // 409 conflict
    await client.settings.putUser({ expected_version: 1, settings: { enabled_tools: ["x"] } as never }).catch(() => undefined); // 400: a member that does not exist
    await client.settings.putUser({ expected_version: 1, settings: { language: "it", model_profile: "lite" } }); // unchanged
    await client.settings.getTenant();
    await client.settings.putTenant({ expected_version: 0, settings: { default_language: "it" } });
    await client.settings.getDeployment();
    await client.settings.putDeployment({ expected_version: 0, settings: {} });
    expect((await client.settings.getEffective()).language).toBe("it");
    expect((await client.settings.listProfiles()).profiles.length).toBeGreaterThan(0);
    expect((await client.settings.getCapabilities()).capabilities.length).toBeGreaterThan(0);
    await client.settings.getIntegrationAudit();
    await client.settings.getProviderHealth();
    await noHistory.settings.getUser().catch(() => undefined); // 403 permission_denied

    // failure paths: scope, validation, authentication, availability, quarantine, ended session
    const other = createClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: () => fake.issueToken({ session_id: "ses-9" }) });
    await client.getSession("ses-9").catch(() => undefined); // scope_violation
    await client.getApplication("someone-else").catch(() => undefined);
    await other.getAction(another.preparation_id).catch(() => undefined);
    fake.failNext({ operation: "listCapabilities", status: 503, error: { code: "unavailable", message: "down", retryable: true } });
    await client.listCapabilities().catch(() => undefined);
    fake.setReady(false);
    await client.getHealth();
    await client.getReadiness();
    fake.setReady(true);
    fake.setApplicationStatus("quarantined");
    await client.createSession().catch(() => undefined);
    await client.sendTurnJson("ses-1", { text: "reply" }).catch(() => undefined);
    await client.listCapabilities().catch(() => undefined);
    fake.setApplicationStatus("active");
    await client.endSession("ses-1");
    await client.getSession("ses-1").catch(() => undefined); // 410

    expect(violations).toEqual([]);
    expect(validated(), "JSON bodies validated against the document").toBeGreaterThan(40);
    const exercised = new Set(statuses.keys());
    const unexercised = Object.keys(OPERATIONS).filter((id) => !exercised.has(id));
    expect(unexercised, "operations this scenario never called").toEqual([]);
    // The scenario really reached the interesting statuses.
    const all = new Set([...statuses.values()].flatMap((s) => [...s]));
    for (const status of [200, 201, 204, 400, 403, 409, 410, 500, 503]) expect(all.has(status), `status ${status} was never exercised`).toBe(true);
  });
});
