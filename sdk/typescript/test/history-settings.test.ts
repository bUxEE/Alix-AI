import { describe, expect, it, vi } from "vitest";

import { AlixAIError, createClient, isAlixAIError } from "../src/index.js";
import { FAKE_HISTORY_SCOPES, FAKE_SETTINGS_SCOPES } from "../src/testing/index.js";
import { collect, createHost } from "./helpers/host.js";
import { json, settled, slowBody, stubbed } from "./helpers/stub.js";

// History, search, export and clearing; settings, profiles and privacy preferences (master plan 15.1, 14.2 and 19).

const caught = async (promise: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await promise;
  } catch (error) {
    expect(isAlixAIError(error), `not an AlixAIError: ${String(error)}`).toBe(true);
    return error as AlixAIError;
  }
  throw new Error("expected a failure");
};

const hostWith = (scopes: readonly string[], options = {}) => createHost({ capability_scopes: [...scopes] }, options);
const history = () => hostWith([...FAKE_HISTORY_SCOPES]);
const settings = () => hostWith([...FAKE_SETTINGS_SCOPES, "demo.lookup", "demo.book"]);

/** Three turns of conversation, so that there is a transcript. */
const converse = async (host: ReturnType<typeof createHost>): Promise<void> => {
  for (const said of ["warranty period", "return policy", "opening hours"]) await host.client.sendTurnJson("ses-1", { text: said });
};

describe("history", () => {
  it("reads the transcript page by page, whole turns, oldest first, from the cursor each page names", async () => {
    const host = history();
    await converse(host);
    const first = await host.client.history.segments("ses-1", { limit: 2 });
    expect(first).toMatchObject({ session_id: "ses-1", total: 6, next_cursor: "2" });
    expect(first.turns.map((t) => [t.n, t.role])).toEqual([[1, "user"], [1, "assistant"], [2, "user"], [2, "assistant"]]);
    expect(first.turns[0]).toMatchObject({ body: "warranty period", clipped: false });
    expect(first.turns[1]).toMatchObject({ body: "fake: warranty period", act: "present_result" });
    const second = await host.client.history.segments("ses-1", { limit: 2, cursor: first.next_cursor! });
    expect(second.turns.map((t) => t.n)).toEqual([3, 3]);
    expect(second.next_cursor).toBeNull();
  });

  it("checks its arguments before a token is spent", async () => {
    const host = history();
    for (const call of [
      () => host.client.history.segments("../x"),
      () => host.client.history.segments("ses-1", { limit: 0 }),
      () => host.client.history.segments("ses-1", { limit: 101 }),
      () => host.client.history.segments("ses-1", { cursor: "abc" }),
      () => host.client.history.search({ text: "" }),
      () => host.client.history.search({ text: "x".repeat(513) }),
      () => host.client.history.search({ text: "x", limit: 51 }),
      () => host.client.history.search({ text: "x", offset: 1001 }),
      () => host.client.history.search({ text: "x", extra: 1 } as never),
      () => host.client.history.cleanup("yesterday"),
      () => host.client.history.cleanup(new Date(Number.NaN)),
    ]) {
      expect((await caught(call())).code).toBe("validation_failed");
    }
    expect((await caught(host.client.history.export({ session_id: "bad id" }).result())).code).toBe("validation_failed");
    expect(host.tokenRequests).toHaveLength(0);
  });

  it("a session that is not the token's is scope_violation; a token without the capability is permission_denied before anything is read", async () => {
    const host = history();
    await converse(host);
    expect(await caught(host.client.history.segments("ses-2"))).toMatchObject({ code: "scope_violation" });
    const without = hostWith([]);
    for (const call of [
      () => without.client.history.segments("ses-1"),
      () => without.client.history.clear("ses-1"),
      () => without.client.history.search({ text: "x" }),
      () => without.client.history.export().result(),
      () => without.client.history.cleanup(new Date(0)),
    ]) {
      expect(await caught(call())).toMatchObject({ code: "permission_denied", status: 403 });
    }
    // The match is exact: reading is not searching.
    const readOnly = hostWith(["history.read"]);
    expect((await caught(readOnly.client.history.search({ text: "x" }))).code).toBe("permission_denied");
  });

  it("searches the caller's own rows: every word must match, accents and case are ignored, pages come with an offset", async () => {
    const host = history();
    await converse(host);
    await host.client.sendTurnJson("ses-1", { text: "Garantía del producto" });
    const hits = await host.client.history.search({ text: "garantia producto" });
    expect(hits.hits.map((h) => [h.role, h.n]).sort()).toEqual([["assistant", 4], ["user", 4]]);
    expect(hits.next_offset).toBeNull();
    const everything = await host.client.history.search({ text: "fake", limit: 2 });
    expect(everything.hits).toHaveLength(2);
    expect(everything.next_offset).toBe(2);
    const next = await host.client.history.search({ text: "fake", limit: 2, offset: everything.next_offset! });
    expect(next.hits.map((h) => h.turn_id)).not.toContain(everything.hits[0]!.turn_id);
    expect((await host.client.history.search({ text: "nothing like this" })).hits).toEqual([]);
    // The text travels in the body, never in the URL (a URL is logged).
    expect(host.fake.requests.filter((r) => r.operation === "searchHistory").every((r) => !r.path.includes("garantia"))).toBe(true);
  });

  it("another principal's history is not visible to a search", async () => {
    const host = history();
    await converse(host);
    const other = createClient({
      baseUrl: host.fake.baseUrl,
      fetch: host.fake.fetch,
      getContextToken: () => host.fake.issueToken({ session_id: "ses-1", principal_id: "someone-else", capability_scopes: [...FAKE_HISTORY_SCOPES] }),
    });
    expect((await other.history.search({ text: "warranty" })).hits).toEqual([]);
    expect((await other.history.segments("ses-1")).turns).toEqual([]);
  });

  it("exports the rows as they arrive and verifies the trailer", async () => {
    const host = history();
    await converse(host);
    const stream = host.client.history.export({ session_id: "ses-1" });
    const rows = await collect(stream);
    expect(rows).toHaveLength(6);
    expect(stream.rows).toBe(6);
    expect(rows.map((r) => r.n)).toEqual([1, 1, 2, 2, 3, 3]);
    expect((await host.client.history.export().result()).length).toBe(6);
    expect(await host.client.history.export({ since: new Date(Date.now() + 3_600_000).toISOString() }).result()).toEqual([]);
    expect(host.fake.requests.filter((r) => r.operation === "exportHistory").at(-1)?.accept).toBe("application/x-ndjson");
  });

  const ROW = { session_id: "ses-1", turn_id: "trn_1", n: 1, role: "user", body: "hi", clipped: false, created_at: "2026-10-03T10:00:00Z" };
  const ndjson = (...lines: unknown[]): Response =>
    new Response(lines.map((l) => (typeof l === "string" ? l : JSON.stringify(l))).join("\n") + "\n", { status: 200, headers: { "Content-Type": "application/x-ndjson" } });

  it("an export that stops before its trailer, or whose trailer counts other rows, is incomplete and retryable: never a quiet end", async () => {
    const cut = stubbed(() => ndjson(ROW, ROW));
    const seen: unknown[] = [];
    const failure = await caught(
      (async () => {
        for await (const row of cut.client.history.export()) seen.push(row);
      })(),
    );
    expect(failure).toMatchObject({ code: "unavailable", retryable: true, details: { reason: "export_incomplete", rows: 2 } });
    expect(seen).toHaveLength(2); // what arrived was delivered
    const miscounted = stubbed(() => ndjson(ROW, { complete: true, rows: 5 }));
    expect(await caught(miscounted.client.history.export().result())).toMatchObject({ details: { reason: "export_incomplete", rows: 1, trailer_rows: 5 } });
    const trailing = stubbed(() => ndjson(ROW, { complete: true, rows: 1 }, ROW));
    expect(await caught(trailing.client.history.export().result())).toMatchObject({ code: "internal" });
    const garbage = stubbed(() => ndjson(ROW, "{not json"));
    expect(await caught(garbage.client.history.export().result())).toMatchObject({ code: "internal" });
    const unknownLine = stubbed(() => ndjson({ surprise: true }));
    expect(await caught(unknownLine.client.history.export().result())).toMatchObject({ code: "internal" });
    const whole = stubbed(() => ndjson(ROW, { complete: true, rows: 1 }));
    expect((await whole.client.history.export().result()).map((r) => r.body)).toEqual(["hi"]);
  });

  it("an export longer than the request deadline is bounded by it only until the response head arrives", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      const slow = slowBody({ "Content-Type": "application/x-ndjson" });
      const client = createClient({ baseUrl: "https://alixai.test", getContextToken: () => "stub.token", fetch: slow.fetch, timeoutMs: 1000 });
      const exported = settled(client.history.export().result());
      await vi.advanceTimersByTimeAsync(5000); // well past the deadline of an ordinary request
      slow.finish(`${JSON.stringify(ROW)}\n${JSON.stringify({ complete: true, rows: 1 })}\n`);
      const outcome = await exported;
      expect("value" in outcome && outcome.value.map((row) => row.body)).toEqual(["hi"]);

      const controller = new AbortController();
      const stopped = settled(client.history.export(undefined, { signal: controller.signal }).result());
      await vi.advanceTimersByTimeAsync(0);
      controller.abort(new DOMException("stop", "AbortError"));
      expect(await stopped).toMatchObject({ error: { name: "AbortError" } });
    } finally {
      vi.useRealTimers();
    }
  });

  it("an export error status is typed, and cancel releases the connection", async () => {
    const refused = stubbed(() => json(403, { code: "permission_denied", message: "no", retryable: false }));
    expect(await caught(refused.client.history.export().result())).toMatchObject({ code: "permission_denied" });
    const host = history();
    await converse(host);
    const stream = host.client.history.export();
    stream.cancel(new DOMException("enough", "AbortError"));
    await expect(stream.result()).rejects.toMatchObject({ name: "AbortError" });
  });

  it("clears one session's transcript and says how many rows went; clearing nothing is not an error", async () => {
    const host = history();
    await converse(host);
    expect(await host.client.history.clear("ses-1")).toEqual({ session_id: "ses-1", rows_deleted: 6 });
    expect((await host.client.history.segments("ses-1")).total).toBe(0);
    expect((await host.client.history.clear("ses-1")).rows_deleted).toBe(0);
    // Only the transcript goes: the session itself stays.
    expect(host.fake.session("ses-1")?.status).toBe("active");
  });

  it("cleans the whole workspace's history older than a time, never one in the future", async () => {
    const host = history();
    await converse(host);
    expect((await host.client.history.cleanup(new Date(Date.now() - 86_400_000))).rows_deleted).toBe(0);
    expect(await caught(host.client.history.cleanup(new Date(Date.now() + 3_600_000)))).toMatchObject({ code: "validation_failed", status: 400 });
    host.fake.advanceTime(2 * 60_000); // the fake's clock is two minutes on: a cutoff one minute from now is in its past
    expect((await host.client.history.cleanup(new Date(Date.now() + 60_000))).rows_deleted).toBe(6);
  });

  it("a clearing is not retried by the SDK: a lost response is uncertain_outcome", async () => {
    const { client, seen } = stubbed(() => {
      throw new TypeError("reset");
    });
    expect((await caught(client.history.clear("ses-1"))).code).toBe("uncertain_outcome");
    expect(seen).toHaveLength(1);
    // A search only reads: a failure is plain unavailability.
    expect((await caught(client.history.search({ text: "x" }))).code).toBe("unavailable");
  });
});

describe("settings", () => {
  it("a level with no document answers version 0 and the empty document", async () => {
    const { client } = settings();
    expect(await client.settings.getUser()).toMatchObject({ level: "user", version: 0, settings: {}, schema: "urn:alixai:schema:settings:user-settings:v1" });
    expect(await client.settings.getTenant()).toMatchObject({ level: "tenant", version: 0 });
    expect(await client.settings.getDeployment()).toMatchObject({ level: "deployment", version: 0 });
  });

  it("writes the whole document against the version that was read, and the version goes up by one", async () => {
    const { client, fake } = settings();
    const first = await client.settings.putUser({ expected_version: 0, settings: { language: "it", privacy: { store_transcript: false } } });
    expect(first).toMatchObject({ version: 1, updated_by: "user-1", settings: { language: "it" } });
    const second = await client.settings.putUser({ expected_version: first.version, settings: { language: "en" } });
    expect(second).toMatchObject({ version: 2, settings: { language: "en" } }); // a member left out means inherit: privacy is gone
    expect(second.settings).not.toHaveProperty("privacy");
    expect((await client.settings.getUser()).version).toBe(2);
    // An identical document changes nothing.
    expect((await client.settings.putUser({ expected_version: 2, settings: { language: "en" } })).version).toBe(2);
    expect(fake.requests.filter((r) => r.operation === "putUserSettings").at(-1)?.body).toEqual({ expected_version: 2, settings: { language: "en" } });
  });

  it("a stale version is conflict with the stored version, and the SDK never retries a write by itself", async () => {
    const { client, fake } = settings();
    await client.settings.putUser({ expected_version: 0, settings: { language: "it" } });
    const writes = () => fake.requests.filter((r) => r.operation === "putUserSettings").length;
    const before = writes();
    const conflict = await caught(client.settings.putUser({ expected_version: 0, settings: { language: "en" } }));
    expect(conflict).toMatchObject({ code: "conflict", status: 409, retryable: false, details: { current_version: 1 } });
    expect(writes() - before).toBe(1);
    // Read, merge, write again: the documented way.
    const current = await client.settings.getUser();
    expect((await client.settings.putUser({ expected_version: conflict.details!["current_version"] as number, settings: { ...current.settings, language: "en" } })).version).toBe(2);
  });

  it("a document that names a member that does not exist is refused whole, and nothing is stored", async () => {
    const { client } = settings();
    for (const forbidden of [{ enabled_tools: ["x"] }, { scopes: ["admin"] }, { egress: "open" }, { provider_credential: "k" }]) {
      const error = await caught(client.settings.putUser({ expected_version: 0, settings: forbidden as never }));
      expect(error).toMatchObject({ code: "validation_failed", status: 400, details: { guard: "closed_document" } });
    }
    expect((await client.settings.getUser()).version).toBe(0);
    // The tenant's document has its own members, not the user's.
    expect((await caught(client.settings.putTenant({ expected_version: 0, settings: { language: "it" } as never }))).code).toBe("validation_failed");
  });

  it("checks a write before a token is spent", async () => {
    const host = settings();
    for (const update of [
      null,
      {},
      { settings: {} },
      { expected_version: -1, settings: {} },
      { expected_version: 1.5, settings: {} },
      { expected_version: 0, settings: "x" },
      { expected_version: 0, settings: {}, extra: 1 },
    ]) {
      expect((await caught(host.client.settings.putUser(update as never))).code).toBe("validation_failed");
    }
    expect(host.tokenRequests).toHaveLength(0);
  });

  it("each level and each direction needs its own capability: there is no wildcard, and reading is not writing", async () => {
    const readOnly = hostWith(["settings.user.read", "settings.tenant.read", "settings.deployment.read"]);
    await readOnly.client.settings.getUser();
    expect(await caught(readOnly.client.settings.putUser({ expected_version: 0, settings: {} }))).toMatchObject({ code: "permission_denied" });
    expect(await caught(readOnly.client.settings.putDeployment({ expected_version: 0, settings: {} }))).toMatchObject({ code: "permission_denied" });
    const none = hostWith([]);
    for (const call of [
      () => none.client.settings.getUser(),
      () => none.client.settings.getEffective(),
      () => none.client.settings.listProfiles(),
      () => none.client.settings.getCapabilities(),
      () => none.client.settings.getIntegrationAudit(),
      () => none.client.settings.getProviderHealth(),
    ]) {
      expect((await caught(call())).code).toBe("permission_denied");
    }
  });

  it("settings are per principal and per tenant: another user does not see this one's preferences", async () => {
    const host = settings();
    await host.client.settings.putUser({ expected_version: 0, settings: { language: "it" } });
    const other = createClient({
      baseUrl: host.fake.baseUrl,
      fetch: host.fake.fetch,
      getContextToken: () => host.fake.issueToken({ session_id: "ses-1", principal_id: "someone-else", capability_scopes: [...FAKE_SETTINGS_SCOPES] }),
    });
    expect(await other.settings.getUser()).toMatchObject({ version: 0, settings: {} });
  });

  it("the effective view, the approved profiles and the capability view follow the user's choices", async () => {
    const host = settings();
    expect((await host.client.settings.listProfiles()).profiles).toEqual([
      { id: "lite", default: true, selected: true },
      { id: "balanced", default: false, selected: false },
    ]);
    await host.client.settings.putUser({ expected_version: 0, settings: { language: "it", model_profile: "balanced", capabilities: { disabled: ["demo.book"] } } });
    expect(await host.client.settings.getEffective()).toMatchObject({ language: "it", model_profile: "balanced", levels: { user: 1 } });
    expect((await host.client.settings.listProfiles()).profiles.find((p) => p.selected)?.id).toBe("balanced");
    const view = await host.client.settings.getCapabilities();
    expect(view.capabilities.find((c) => c.id === "demo.book")).toMatchObject({ enabled: false, disabled_by: ["user"] });
    expect(view.capabilities.find((c) => c.id === "demo.lookup")).toMatchObject({ enabled: true, disabled_by: [] });
    expect(view.settings_access).toEqual({ user: { read: true, write: true }, tenant: { read: true, write: true }, deployment: { read: true, write: true } });
  });

  it("tenant and deployment documents are separate levels", async () => {
    const { client } = settings();
    expect((await client.settings.putTenant({ expected_version: 0, settings: { default_language: "it" } })).level).toBe("tenant");
    expect((await client.settings.putDeployment({ expected_version: 0, settings: { default_model_profile: "lite" } })).level).toBe("deployment");
    expect((await client.settings.getUser()).version).toBe(0);
    expect(await client.settings.getEffective()).toMatchObject({ levels: { deployment: 1, tenant: 1, user: 0 } });
  });

  it("the audit and the provider health are read only, and the audit answers while the application is quarantined", async () => {
    const host = settings();
    expect(await host.client.settings.getProviderHealth()).toMatchObject({ status: "ok", roles: [{ role: "controller" }, { role: "embedder" }] });
    expect(await host.client.settings.getIntegrationAudit()).toMatchObject({ state: "active", audited: false, counts: { critical: 0 } });
    host.fake.setApplicationStatus("quarantined");
    expect(await host.client.settings.getIntegrationAudit()).toMatchObject({ state: "quarantined" });
    expect((await caught(host.client.settings.getUser())).code).toBe("quarantined");
  });

  it("does not accept a response that is not the document it asked for", async () => {
    const { client } = stubbed(() => json(200, { surprise: true }));
    for (const call of [() => client.settings.getUser(), () => client.settings.listProfiles(), () => client.history.segments("ses-1"), () => client.files.get("fil_01J0000000000000000000000A")]) {
      expect(await caught(call())).toMatchObject({ code: "internal", details: { reason: "unexpected_shape" } });
    }
  });
});
