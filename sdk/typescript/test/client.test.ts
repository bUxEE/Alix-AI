import { describe, expect, it } from "vitest";

import { createClient, isAlixAIError } from "../src/index.js";
import type { AlixAIError } from "../src/index.js";
import { prepareAction } from "../src/testing/index.js";
import { createHost } from "./helpers/host.js";
import { json, stubbed } from "./helpers/stub.js";

const caught = async (promise: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await promise;
  } catch (error) {
    expect(isAlixAIError(error), `not an AlixAIError: ${String(error)}`).toBe(true);
    return error as AlixAIError;
  }
  throw new Error("expected a failure");
};

const BOOK = { capability_id: "demo.book", summary: "Book the 10:00 slot", text: "Shall I?" };
const SECRET = `cfm_${"A".repeat(43)}`;

describe("context tokens are single use: asked for per request, never cached", () => {
  it("calls getContextToken once per request, with what the request concerns", async () => {
    const { client, tokenRequests } = createHost();
    await client.createSession();
    await client.getSession("ses-1");
    await client.getPendingState("ses-1");
    await client.listCapabilities();
    await client.getHealth();
    await client.endSession("ses-1");
    expect(tokenRequests).toEqual([
      { operation: "createSession" },
      { operation: "getSession", sessionId: "ses-1" },
      { operation: "getPendingState", sessionId: "ses-1" },
      { operation: "listCapabilities" },
      { operation: "getHealth" },
      { operation: "endSession", sessionId: "ses-1" },
    ]);
  });

  it("calls it once per stream open, and once per confirmation", async () => {
    const { client, fake, tokenRequests } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const grant = (await client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    await client.confirmAction(grant.preparation_id, grant.token);
    expect(tokenRequests).toEqual([
      { operation: "sendTurn", sessionId: "ses-1" },
      { operation: "confirmAction", preparationId: grant.preparation_id },
    ]);
  });

  it("N requests redeem N distinct tokens", async () => {
    const { client, fake } = createHost();
    for (let i = 0; i < 7; i++) await client.getSession("ses-1");
    expect(fake.tokensRedeemed).toBe(7);
    expect(new Set(fake.requests.map((r) => r.tokenId)).size).toBe(7);
  });

  it("a host that caches its token is refused by the service on the second request (replayed)", async () => {
    const fake = (await import("../src/testing/index.js")).createFakeServer();
    const cached = fake.issueToken({ session_id: "ses-1" });
    const client = createClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: () => cached });
    await client.getSession("ses-1");
    const failure = await caught(client.getSession("ses-1"));
    expect(failure).toMatchObject({ code: "replayed", status: 401, retryable: false });
  });

  it("does not retry on its own: a replayed answer is one request, not two", async () => {
    const { client, fake } = createHost();
    fake.failNext({ operation: "getSession", status: 503, error: { code: "unavailable", message: "down", retryable: true } });
    await caught(client.getSession("ses-1"));
    expect(fake.requests.filter((r) => r.operation === "getSession")).toHaveLength(1);
  });

  it("sends the token only as Authorization: Bearer, never in the URL, and never follows redirects or sends cookies", async () => {
    const stub = stubbed(() => json(200, { status: "active", session_id: "ses-1", turn_count: 0, created_at: "2026-10-02T00:00:00Z", updated_at: "2026-10-02T00:00:00Z", pending: null }));
    await stub.client.createSession();
    const { url, init } = stub.seen[0]!;
    const headers = new Headers(init.headers);
    expect(headers.get("Authorization")).toBe("Bearer stub.token.1");
    expect(url).toBe("https://alixai.test/v1/sessions");
    expect(url).not.toContain("stub.token");
    expect(init.redirect).toBe("error");
    expect(init.credentials).toBe("omit");
    expect(init.cache).toBe("no-store");
  });

  it("does not call getContextToken, and sends no Authorization header, for liveness and readiness", async () => {
    const { client, fake, tokenRequests } = createHost();
    expect(await client.getLiveness()).toEqual({ status: "ok" });
    expect((await client.getReadiness()).ready).toBe(true);
    expect(tokenRequests).toHaveLength(0);
    expect(fake.requests.every((r) => !r.authenticated)).toBe(true);
  });

  it("a token provider that throws means nothing is sent", async () => {
    const stub = (() => {
      const seen: string[] = [];
      const client = createClient({
        baseUrl: "https://alixai.test",
        getContextToken: () => {
          throw new Error("the host's session store is down");
        },
        fetch: async (url) => {
          seen.push(url);
          return json(200, {});
        },
      });
      return { client, seen };
    })();
    const failure = await caught(stub.client.createSession());
    expect(failure).toMatchObject({ code: "unauthenticated", details: { reason: "token_provider_failed" } });
    expect((failure.cause as Error).message).toContain("session store");
    expect(stub.seen).toHaveLength(0);
  });

  it("an async token provider is awaited", async () => {
    const fake = (await import("../src/testing/index.js")).createFakeServer();
    const client = createClient({
      baseUrl: fake.baseUrl,
      fetch: fake.fetch,
      getContextToken: async () => {
        await new Promise((resolve) => setTimeout(resolve, 5));
        return fake.issueToken({ session_id: "ses-1" });
      },
    });
    expect((await client.createSession()).session_id).toBe("ses-1");
  });

  it("refuses a token that would corrupt the header, without sending anything or echoing it", async () => {
    for (const bad of ["", "Bearer abc.def.ghi", "abc def", "abc\r\nX-Evil: 1", 42 as unknown as string, undefined as unknown as string]) {
      const seen: string[] = [];
      const client = createClient({ baseUrl: "https://alixai.test", getContextToken: () => bad, fetch: async (url) => (seen.push(url), json(200, {})) });
      const failure = await caught(client.createSession());
      expect(failure.code, JSON.stringify(bad)).toBe("unauthenticated");
      expect(failure.message).not.toContain("X-Evil");
      expect(seen).toHaveLength(0);
    }
  });

  it("an argument that fails local validation never costs a token", async () => {
    const { client, fake, tokenRequests } = createHost();
    const bad = ["", ".", "..", "a/b", "a b", "a?x=1", "a#b", "x".repeat(129), "café", "ses-1/../ses-2", "%2e%2e"];
    for (const id of bad) {
      expect((await caught(client.getSession(id))).code, JSON.stringify(id)).toBe("validation_failed");
      expect((await caught(client.getActionReceipt(id))).code, JSON.stringify(id)).toBe("validation_failed");
      expect((await caught(client.getApplication(id))).code, JSON.stringify(id)).toBe("validation_failed");
    }
    expect((await caught(client.confirmAction("prep_x", "nope"))).code).toBe("validation_failed");
    expect((await caught(client.registerApplication(new Uint8Array([1, 2, 3])))).code).toBe("validation_failed");
    expect(tokenRequests).toHaveLength(0);
    expect(fake.requests).toHaveLength(0);
  });
});

describe("baseUrl", () => {
  it("keeps a path prefix (a reverse proxy mount)", async () => {
    const { client, fake } = createHost({}, { baseUrl: "https://example.test/alixai/" });
    expect(fake.baseUrl).toBe("https://example.test/alixai");
    await client.createSession();
    await client.getSession("ses-1");
    expect(fake.requests.map((r) => r.path)).toEqual(["/v1/sessions", "/v1/sessions/ses-1"]);
  });

  it("joins by concatenation: a prefix is never dropped", async () => {
    const stub = stubbed(() => json(200, { status: "ok" }), "https://example.test/a/b//");
    await stub.client.getLiveness();
    expect(stub.seen[0]?.url).toBe("https://example.test/a/b/healthz");
  });

  it("refuses plain http to a host that is not loopback, unless asked", () => {
    const options = { getContextToken: () => "a.b.c" };
    expect(() => createClient({ ...options, baseUrl: "http://alixai.example.com" })).toThrowError(/https/);
    expect(() => createClient({ ...options, baseUrl: "http://alixai.example.com", allowInsecureHttp: true })).not.toThrow();
    for (const loopback of ["http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080", "http://alixai.localhost"]) {
      expect(() => createClient({ ...options, baseUrl: loopback }), loopback).not.toThrow();
    }
  });

  it("refuses a base URL that is not absolute http(s), or that carries credentials, a query or a fragment", () => {
    const options = { getContextToken: () => "a.b.c" };
    for (const baseUrl of ["", "alixai.test", "/v1", "ftp://alixai.test", "https://user:pw@alixai.test", "https://alixai.test?x=1", "https://alixai.test#x"]) {
      expect(() => createClient({ ...options, baseUrl }), baseUrl).toThrowError(expect.objectContaining({ code: "validation_failed" }));
    }
  });

  it("requires a token provider", () => {
    expect(() => createClient({ baseUrl: "https://alixai.test" } as never)).toThrowError(expect.objectContaining({ code: "validation_failed" }));
  });

  it("a fetch that is not the fake's origin fails like a network error", async () => {
    const fake = (await import("../src/testing/index.js")).createFakeServer();
    const client = createClient({ baseUrl: "https://elsewhere.test", fetch: fake.fetch, getContextToken: () => fake.issueToken({ session_id: "s" }) });
    expect(await caught(client.getSession("s"))).toMatchObject({ code: "unavailable", retryable: true });
  });
});

describe("sessions", () => {
  it("opens a session, always answering 200 for the session the token names", async () => {
    const { client, fake } = createHost();
    const first = await client.createSession();
    const again = await client.createSession();
    expect(first).toMatchObject({ session_id: "ses-1", status: "active", turn_count: 0, pending: null });
    expect(again.session_id).toBe("ses-1");
    expect(fake.requests.filter((r) => r.operation === "createSession").map((r) => r.status)).toEqual([200, 200]);
  });

  it("resumes a session with its turn count and pending state", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    await client.sendTurn("ses-1", { text: "book" }).result();
    const session = await client.getSession("ses-1");
    expect(session.turn_count).toBe(1);
    expect(session.pending).toMatchObject({ kind: "confirmation", capability_id: "demo.book", phase: "awaiting_confirmation" });
    expect(session.pending?.preparation).toMatchObject({ state: "prepared", risk_level: 2 });
    expect(await client.getPendingState("ses-1")).toEqual(session.pending);
  });

  it("a path session that is not the token's is scope_violation", async () => {
    const { client } = createHost();
    for (const call of [client.getSession("ses-2"), client.getPendingState("ses-2"), client.endSession("ses-2")]) {
      expect(await caught(call)).toMatchObject({ code: "scope_violation", status: 403, retryable: false });
    }
  });

  it("a session cannot change principal", async () => {
    const fake = (await import("../src/testing/index.js")).createFakeServer();
    const as = (principal: string) =>
      createClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: () => fake.issueToken({ session_id: "ses-1", principal_id: principal }) });
    await as("alice").createSession();
    expect(await caught(as("mallory").createSession())).toMatchObject({ code: "scope_violation" });
  });

  it("ending a session invalidates what it waited on, and ending twice is the same as ending once", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const grant = (await client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    expect(await client.endSession("ses-1")).toEqual({ session_id: "ses-1", status: "ended" });
    expect(await client.endSession("ses-1")).toEqual({ session_id: "ses-1", status: "ended" });
    expect(fake.preparations()[0]?.state).toBe("invalidated");
    expect(await caught(client.confirmAction(grant.preparation_id, grant.token))).toMatchObject({ code: "stale" });
    expect(await caught(client.getSession("ses-1"))).toMatchObject({ code: "expired", status: 410 });
    expect(await caught(client.createSession())).toMatchObject({ code: "expired", status: 410 });
  });
});

describe("actions", () => {
  const prepared = async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction(BOOK));
    const grant = (await host.client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    return { ...host, grant };
  };

  it("confirms with the secret: 200 with the receipt and the committed preparation", async () => {
    const { client, grant } = await prepared();
    const outcome = await client.confirmAction(grant.preparation_id, grant.token);
    expect(outcome.receipt).toMatchObject({ status: "committed", preparation_id: grant.preparation_id });
    expect(outcome.preparation?.state).toBe("committed");
    expect(await client.getActionReceipt(grant.preparation_id)).toEqual(outcome.receipt);
    expect(await client.getPendingState("ses-1")).toBeNull();
  });

  it("a queued commit is a 200 whose receipt says queued", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction({ ...BOOK, commit: { status: "queued" } }));
    const grant = (await host.client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    const outcome = await host.client.confirmAction(grant.preparation_id, grant.token);
    expect(outcome.receipt).toMatchObject({ status: "queued", follow_up: { state: "queued" } });
  });

  it("the second presentation of the secret is 409 replayed, a bare error", async () => {
    const { client, grant } = await prepared();
    await client.confirmAction(grant.preparation_id, grant.token);
    const failure = await caught(client.confirmAction(grant.preparation_id, grant.token));
    expect(failure).toMatchObject({ code: "replayed", status: 409, retryable: false });
    expect(failure.receipt).toBeUndefined();
  });

  it("a wrong secret is permission_denied and does not use the real one up", async () => {
    const { client, grant } = await prepared();
    expect(await caught(client.confirmAction(grant.preparation_id, SECRET))).toMatchObject({ code: "permission_denied", status: 403 });
    expect((await client.confirmAction(grant.preparation_id, grant.token)).receipt.status).toBe("committed");
  });

  it("a host that refused the commit is 409 with the receipt beside the error", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction({ ...BOOK, commit: { status: "rejected", error: { code: "conflict", message: "the slot was taken" } } }));
    const grant = (await host.client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    const failure = await caught(host.client.confirmAction(grant.preparation_id, grant.token));
    expect(failure).toMatchObject({ code: "conflict", status: 409, message: "the slot was taken" });
    expect(failure.receipt).toMatchObject({ status: "rejected", preparation_id: grant.preparation_id });
    expect(failure.preparation).toBeDefined();
    expect(failure.requestId).toMatch(/^req_/);
  });

  it("a partial or unknown outcome is 500 with the receipt, and is not retryable", async () => {
    for (const [status, code] of [["partial", "partial_failure"], ["uncertain", "uncertain_outcome"]] as const) {
      const host = createHost();
      host.fake.onTurn(() => prepareAction({ ...BOOK, commit: { status } }));
      const grant = (await host.client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
      const failure = await caught(host.client.confirmAction(grant.preparation_id, grant.token));
      expect(failure, status).toMatchObject({ code, status: 500, retryable: false });
      expect(failure.receipt, status).toMatchObject({ status });
    }
  });

  it("an expired preparation is refused as expired", async () => {
    const { client, fake, grant } = await prepared();
    fake.advanceTime(11 * 60 * 1000);
    expect(await caught(client.confirmAction(grant.preparation_id, grant.token))).toMatchObject({ code: "expired" });
    expect((await client.getAction(grant.preparation_id)).state).toBe("expired");
  });

  it("rejects a preparation, and rejecting twice returns it rejected", async () => {
    const { client, grant } = await prepared();
    expect((await client.rejectAction(grant.preparation_id)).state).toBe("rejected");
    expect((await client.rejectAction(grant.preparation_id)).state).toBe("rejected");
    expect(await caught(client.confirmAction(grant.preparation_id, grant.token))).toMatchObject({ code: "conflict" });
    expect(await client.getPendingState("ses-1")).toBeNull();
  });

  it("a preparation of another session is scope_violation, the same as one that does not exist", async () => {
    const { fake, grant } = await prepared();
    const other = createClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: () => fake.issueToken({ session_id: "ses-2" }) });
    expect(await caught(other.getAction(grant.preparation_id))).toMatchObject({ code: "scope_violation" });
    expect(await caught(other.confirmAction(grant.preparation_id, grant.token))).toMatchObject({ code: "scope_violation" });
    expect(await caught(other.getAction("prep_01J0000000000000000000000A"))).toMatchObject({ code: "scope_violation" });
  });

  it("the receipt is a conflict until there is an outcome", async () => {
    const { client, grant } = await prepared();
    expect(await caught(client.getActionReceipt(grant.preparation_id))).toMatchObject({ code: "conflict", status: 409 });
  });

  it("a confirmation as a turn input commits too, and spends the secret", async () => {
    const { client, grant } = await prepared();
    const result = await client.sendTurn("ses-1", { confirmation: grant }).result();
    expect(result.events.find((e) => e.type === "receipt")).toMatchObject({ receipt: { status: "committed" } });
    expect(await caught(client.confirmAction(grant.preparation_id, grant.token))).toMatchObject({ code: "replayed" });
  });

  it("a rejection as a turn input cancels the preparation", async () => {
    const { client, grant } = await prepared();
    const result = await client.sendTurn("ses-1", { reject: { preparation_id: grant.preparation_id } }).result();
    expect(result.events.at(-1)).toMatchObject({ type: "done", finish_reason: "cancelled" });
    expect((await client.getAction(grant.preparation_id)).state).toBe("rejected");
  });

  it("a text turn never confirms anything", async () => {
    const { client, fake, grant } = await prepared();
    fake.onTurn(() => prepareAction({ ...BOOK, summary: "another" }));
    await client.sendTurn("ses-1", { text: "yes, confirm it" }).result();
    expect((await client.getAction(grant.preparation_id)).state).toBe("prepared");
  });
});

describe("health", () => {
  it("returns the report with the caller's application", async () => {
    const { client } = createHost();
    expect(await client.getHealth()).toMatchObject({ status: "ok", ready: true, application: { application_id: "fake-app", status: "active" } });
  });

  it("a service that is not ready (503) returns its report instead of throwing", async () => {
    const { client, fake } = createHost();
    fake.setReady(false);
    const health = await client.getHealth();
    expect(health).toMatchObject({ status: "unavailable", ready: false });
    expect(health.checks.some((c) => !c.ok)).toBe(true);
    expect((await client.getReadiness()).ready).toBe(false);
    expect(fake.requests.filter((r) => r.operation === "getHealth" || r.operation === "getReadiness").map((r) => r.status)).toEqual([503, 503]);
  });

  it("a 503 that is not a report (a proxy page) is a typed unavailable error", async () => {
    const stub = stubbed(() => new Response("<html>Bad gateway</html>", { status: 503, headers: { "Content-Type": "text/html" } }));
    expect(await caught(stub.client.getReadiness())).toMatchObject({ code: "unavailable", status: 503, retryable: true });
    const stub2 = stubbed(() => json(503, { code: "unavailable", message: "nope", retryable: true }));
    expect(await caught(stub2.client.getHealth())).toMatchObject({ code: "unavailable", message: "nope" });
  });

  it("reports the application's state when its package is quarantined", async () => {
    const { client, fake } = createHost();
    fake.setApplicationStatus("quarantined");
    expect((await client.getHealth()).application.status).toBe("quarantined");
  });
});

describe("applications and capabilities", () => {
  it("lists the capabilities the caller may see", async () => {
    const { client } = createHost();
    const { capabilities } = await client.listCapabilities();
    expect(capabilities.map((c) => [c.id, c.risk_level])).toEqual([["demo.lookup", 0], ["demo.book", 2], ["demo.close_account", 3]]);
  });

  it("a quarantined application serves no capabilities", async () => {
    const { client, fake } = createHost();
    fake.setApplicationStatus("quarantined");
    expect(await caught(client.listCapabilities())).toMatchObject({ code: "quarantined", status: 403 });
    expect(await caught(client.createSession())).toMatchObject({ code: "quarantined" });
  });

  it("inspects the caller's application, and another application is scope_violation", async () => {
    const { client } = createHost();
    expect(await client.getApplication("fake-app")).toMatchObject({ application_id: "fake-app", status: "active", health: { status: "ok" } });
    expect(await caught(client.getApplication("someone-else"))).toMatchObject({ code: "scope_violation" });
  });

  it("registers a package into quarantine: 201 once, 200 for the same content", async () => {
    const { client, fake } = createHost({ capability_scopes: ["applications.register"] });
    const archive = new Uint8Array([0x1f, 0x8b, 8, 0, 1, 2, 3, 4]);
    const first = await client.registerApplication(archive);
    expect(first).toMatchObject({ application_id: "fake-app", created: true, package: { state: "quarantined" } });
    expect((await client.registerApplication(archive)).created).toBe(false);
    expect(await client.registerApplication(archive.buffer.slice(0) as ArrayBuffer)).toMatchObject({ created: false });
    expect(fake.requests.filter((r) => r.operation === "registerApplication").map((r) => r.status)).toEqual([201, 200, 200]);
    expect((await client.getApplication("fake-app")).packages).toHaveLength(2);
  });

  it("registering needs exactly the applications.register scope (default deny, no wildcard)", async () => {
    for (const claims of [{}, { capability_scopes: ["demo.lookup"] }, { capability_scopes: ["*"] }, { capability_scopes: ["applications.*"] }]) {
      const { client } = createHost(claims);
      expect(await caught(client.registerApplication(new Uint8Array([0x1f, 0x8b, 8]))), JSON.stringify(claims)).toMatchObject({ code: "permission_denied" });
    }
  });

  it("a capability the token does not list is not visible: scopes are exact ids, never a wildcard", async () => {
    const { client } = createHost({ capability_scopes: ["demo.lookup"] });
    expect((await client.listCapabilities()).capabilities.map((c) => c.id)).toEqual(["demo.lookup"]);
    const wild = createHost({ capability_scopes: ["*"] });
    expect((await wild.client.listCapabilities()).capabilities).toEqual([]);
    const none = createHost({ capability_scopes: [] });
    expect((await none.client.listCapabilities()).capabilities).toEqual([]);
  });

  it("a turn that prepares a capability the token does not permit fails closed, as a typed error event", async () => {
    const { client, fake } = createHost({ capability_scopes: ["demo.lookup"] });
    fake.onTurn(() => prepareAction(BOOK));
    const result = await client.sendTurn("ses-1", { text: "book" }).result();
    expect(result.events.find((e) => e.type === "error")).toMatchObject({ error: { code: "permission_denied" } });
    expect(result.events.at(-1)).toMatchObject({ type: "done", finish_reason: "failed" });
    expect(result.confirmation).toBeUndefined();
    expect(fake.preparations()).toEqual([]);
  });
});

describe("errors are typed with the ErrCode strings", () => {
  it("carries code, message, retryable, details, status and the request id of an error document", async () => {
    const stub = stubbed(() =>
      json(429, { code: "rate_limited", message: "slow down", retryable: true, details: { request_id: "req_1", limit: 5 } }, { "X-Request-ID": "req_1", "Retry-After": "3" }),
    );
    const failure = await caught(stub.client.getSession("s"));
    expect(failure).toMatchObject({ name: "AlixAIError", code: "rate_limited", message: "slow down", retryable: true, status: 429, requestId: "req_1", retryAfterMs: 3000 });
    expect(failure.details).toEqual({ request_id: "req_1", limit: 5 });
    expect(failure).toBeInstanceOf(Error);
  });

  it("a body that is not an error document becomes a typed fallback chosen by the status", async () => {
    const cases: Array<[number, string, boolean]> = [[400, "validation_failed", false], [401, "unauthenticated", false], [403, "permission_denied", false], [409, "conflict", false], [410, "expired", false], [429, "rate_limited", true], [502, "unavailable", true], [503, "unavailable", true], [504, "timeout", true], [500, "internal", false], [418, "internal", false]];
    for (const [status, code, retryable] of cases) {
      const stub = stubbed(() => new Response("<html>nope</html>", { status, headers: { "Content-Type": "text/html" } }));
      const failure = await caught(stub.client.getSession("s"));
      expect(failure, String(status)).toMatchObject({ code, retryable, status, details: { reason: "non_conforming_error_body" } });
    }
  });

  it("a JSON body with an unknown code is not trusted", async () => {
    const stub = stubbed(() => json(500, { code: "kaboom", message: "x", retryable: true }));
    expect(await caught(stub.client.getSession("s"))).toMatchObject({ code: "internal", retryable: false });
  });

  it("a 200 whose body has the wrong shape is a typed error, not an undefined member later", async () => {
    for (const body of [{}, { session_id: 7 }, [], null, "ok"]) {
      const stub = stubbed(() => json(200, body));
      expect((await caught(stub.client.createSession())).code, JSON.stringify(body)).toBe("internal");
    }
    const stub = stubbed(() => new Response("<html>login</html>", { status: 200, headers: { "Content-Type": "text/html" } }));
    expect(await caught(stub.client.createSession())).toMatchObject({ code: "internal", details: { reason: "not_json" } });
  });

  it("a confirmation answered 200 without a receipt is not a success", async () => {
    const stub = stubbed(() => json(200, { preparation: undefined }));
    expect((await caught(stub.client.confirmAction("prep_x", SECRET))).code).toBe("internal");
  });

  it("a network failure is unavailable for a read, and uncertain_outcome for anything that may have been processed", async () => {
    const { client, fake } = createHost();
    fake.failNext({ operation: "getSession", networkError: true });
    expect(await caught(client.getSession("ses-1"))).toMatchObject({ code: "unavailable", retryable: true, details: { phase: "transport" } });
    fake.failNext({ operation: "endSession", networkError: true });
    expect(await caught(client.endSession("ses-1"))).toMatchObject({ code: "uncertain_outcome", retryable: false });
    fake.failNext({ operation: "sendTurn", networkError: true });
    expect(await caught(client.sendTurn("ses-1", { text: "x" }).result())).toMatchObject({ code: "uncertain_outcome", retryable: false });
  });

  it("a request that outlives its timeout fails with timeout (a read) or uncertain_outcome (a write)", async () => {
    const hang = (_url: string, init: RequestInit) =>
      new Promise<Response>((_resolve, reject) => init.signal?.addEventListener("abort", () => reject(init.signal?.reason)));
    const read = stubbed(hang);
    expect(await caught(read.client.getSession("s", { timeoutMs: 20 }))).toMatchObject({ code: "timeout", retryable: true });
    const write = stubbed(hang);
    expect(await caught(write.client.endSession("s", { timeoutMs: 20 }))).toMatchObject({ code: "uncertain_outcome", retryable: false });
  });

  it("an aborted request throws the abort reason, not a wrapped error", async () => {
    const hang = (_url: string, init: RequestInit) =>
      new Promise<Response>((_resolve, reject) => init.signal?.addEventListener("abort", () => reject(init.signal?.reason)));
    const { client } = stubbed(hang);
    const controller = new AbortController();
    const pending = client.getSession("s", { signal: controller.signal });
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
  });

  it("rejects a timeout that is not a non-negative number", async () => {
    const { client } = createHost();
    for (const timeoutMs of [-1, Number.NaN, "5" as unknown as number]) {
      expect((await caught(client.getSession("ses-1", { timeoutMs }))).code).toBe("validation_failed");
    }
  });
});
