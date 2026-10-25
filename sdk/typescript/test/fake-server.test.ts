import { describe, expect, it } from "vitest";

import { createClient, isAlixAIError } from "../src/index.js";
import { askChoice, createFakeServer, prepareAction, reply } from "../src/testing/index.js";
import { createHost } from "./helpers/host.js";

// The fake server is a deliverable (@alixai/sdk/testing): its own behaviour is tested here, apart from
// what the SDK does with it.

const status = async (fake: ReturnType<typeof createFakeServer>, path: string, init: RequestInit = {}): Promise<{ status: number; body: any; headers: Headers }> => {
  const response = await fake.fetch(`${fake.baseUrl}${path}`, init);
  const text = await response.text();
  return { status: response.status, body: text === "" ? undefined : JSON.parse(text), headers: response.headers };
};

const bearer = (token: string): RequestInit => ({ headers: { Authorization: `Bearer ${token}` } });

describe("the fake enforces the protocol the SDK relies on", () => {
  it("refuses a request without a bearer token (401 unauthenticated, WWW-Authenticate)", async () => {
    const fake = createFakeServer();
    const result = await status(fake, "/v1/health");
    expect(result).toMatchObject({ status: 401, body: { code: "unauthenticated", retryable: false } });
    expect(result.headers.get("WWW-Authenticate")).toBe("Bearer");
    expect(result.headers.get("X-Request-ID")).toMatch(/^req_/);
  });

  it("refuses a malformed token, a wrong scheme, and a token with missing claims", async () => {
    const fake = createFakeServer();
    for (const init of [bearer("not-a-token"), bearer("a.b.c"), { headers: { Authorization: "Basic abc" } }, { headers: { Authorization: "Bearer a b" } }]) {
      expect((await status(fake, "/v1/health", init)).body, JSON.stringify(init)).toMatchObject({ code: "unauthenticated" });
    }
    const noClaims = `${btoa("{}")}.${btoa(JSON.stringify({ jti: "x" }))}.sig`.replace(/=/g, "");
    expect((await status(fake, "/v1/health", bearer(noClaims))).body).toMatchObject({ code: "unauthenticated" });
  });

  it("refuses an expired token, with the fake's own clock", async () => {
    const fake = createFakeServer();
    const token = fake.issueToken({ session_id: "s", ttlSeconds: 30 });
    fake.advanceTime(31_000);
    expect(await status(fake, "/v1/health", bearer(token))).toMatchObject({ status: 401, body: { code: "expired" } });
  });

  it("a token is single use: the second presentation is 401 replayed, even for a different route", async () => {
    const fake = createFakeServer();
    const token = fake.issueToken({ session_id: "s" });
    expect((await status(fake, "/v1/health", bearer(token))).status).toBe(200);
    expect(await status(fake, "/v1/capabilities", bearer(token))).toMatchObject({ status: 401, body: { code: "replayed" } });
  });

  it("consumes the token even when the request then fails, as the real middleware does", async () => {
    const fake = createFakeServer();
    const token = fake.issueToken({ session_id: "s" });
    expect((await status(fake, "/v1/sessions/other", bearer(token))).body).toMatchObject({ code: "scope_violation" });
    expect((await status(fake, "/v1/sessions/s", bearer(token))).body).toMatchObject({ code: "replayed" });
  });

  it("issues distinct tokens (a new jti every time), unsigned and labelled as the fake's", () => {
    const fake = createFakeServer();
    const tokens = new Set(Array.from({ length: 20 }, () => fake.issueToken({ session_id: "s" })));
    expect(tokens.size).toBe(20);
    const [header, payload] = [...tokens][0]!.split(".");
    expect(JSON.parse(atob(header!.replace(/-/g, "+").replace(/_/g, "/")))).toEqual({ alg: "none", typ: "alixai-fake+jwt" });
    expect(JSON.parse(atob(payload!.replace(/-/g, "+").replace(/_/g, "/")))).toMatchObject({ session_id: "s", application_id: "fake-app" });
  });

  it("does not know other routes or methods", async () => {
    const fake = createFakeServer();
    expect((await status(fake, "/v2/anything")).status).toBe(404);
    expect((await status(fake, "/v1/health", { method: "DELETE" })).status).toBe(405);
    expect((await status(fake, "/healthz")).body).toEqual({ status: "ok" });
  });

  it("rejects a turn body that is not exactly one input, and a confirmation with a malformed secret", async () => {
    const fake = createFakeServer();
    const post = async (body: unknown) =>
      status(fake, "/v1/sessions/s/turns", {
        method: "POST",
        headers: { Authorization: `Bearer ${fake.issueToken({ session_id: "s" })}`, "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify(body),
      });
    for (const body of [{}, { text: "a", reject: { preparation_id: "p" } }, { text: "" }, { text: "a", x: 1 }, { confirmation: { preparation_id: "p", token: "short" } }, [], "x"]) {
      expect((await post(body)).body, JSON.stringify(body)).toMatchObject({ code: "validation_failed" });
    }
    expect((await post({ text: "ok" })).status).toBe(200);
  });

  it("answers JSON unless the client asks for text/event-stream", async () => {
    const { client, fake } = createHost();
    await client.sendTurnJson("ses-1", { text: "a" });
    await client.sendTurn("ses-1", { text: "b" }).result();
    expect(fake.requests.filter((r) => r.operation === "sendTurn").map((r) => r.accept)).toEqual(["application/json", "text/event-stream"]);
  });

  it("serves exactly the SSE framing: id is seq, event is the type, data is the event on one line, header only with a secret", async () => {
    const fake = createFakeServer();
    fake.onTurn(() => prepareAction({ capability_id: "demo.book", summary: "Book it" }));
    const response = await fake.fetch(`${fake.baseUrl}/v1/sessions/s/turns`, {
      method: "POST",
      headers: { Authorization: `Bearer ${fake.issueToken({ session_id: "s" })}`, "Content-Type": "application/json", Accept: "text/event-stream" },
      body: JSON.stringify({ text: "book" }),
    });
    expect(response.headers.get("Content-Type")).toBe("text/event-stream");
    expect(response.headers.get("X-AlixAI-Confirmation-Token")).toMatch(/^cfm_[A-Za-z0-9_-]{43}$/);
    const text = await response.text();
    const frames = text.split("\n\n").filter((f) => f !== "");
    expect(frames.length).toBeGreaterThan(3);
    frames.forEach((frame, i) => {
      const lines = frame.split("\n");
      expect(lines).toHaveLength(3);
      const event = JSON.parse(lines[2]!.slice("data: ".length));
      expect(lines[0]).toBe(`id: ${i}`);
      expect(lines[1]).toBe(`event: ${event.type}`);
      expect(event.seq).toBe(i);
    });
    expect(text).not.toContain(response.headers.get("X-AlixAI-Confirmation-Token")!);
    expect(text.endsWith("\n\n")).toBe(true);
  });
});

describe("scripting turns", () => {
  it("echoes by default, and onTurn(undefined) restores the default", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => reply("scripted"));
    expect((await client.sendTurnJson("ses-1", { text: "x" })).events.some((e) => e.type === "token" && e.text === "scripted")).toBe(true);
    fake.onTurn(undefined);
    expect((await client.sendTurnJson("ses-1", { text: "y" })).events.some((e) => e.type === "token" && e.text === "fake: y")).toBe(true);
  });

  it("gives the handler the turn number, the identity and what the session waited for", async () => {
    const { client, fake } = createHost({ principal_id: "alice", tenant_id: "t9" });
    const seen: Array<{ n: number; who: string; tenant: string; pending: string | undefined }> = [];
    fake.onTurn((turn) => {
      seen.push({ n: turn.turn_number, who: turn.session.principal_id, tenant: turn.session.tenant_id, pending: turn.pending?.kind });
      return seen.length === 1
        ? askChoice("field", "Which?", [{ choice_id: "a", label: "A" }, { choice_id: "b", label: "B" }])
        : reply("ok");
    });
    await client.sendTurnJson("ses-1", { text: "one" });
    await client.sendTurnJson("ses-1", { choice_id: "a" });
    expect(seen).toEqual([
      { n: 1, who: "alice", tenant: "t9", pending: undefined },
      { n: 2, who: "alice", tenant: "t9", pending: "clarification" },
    ]);
  });

  it("an async handler is awaited, and askChoice needs two choices", async () => {
    const { client, fake } = createHost();
    fake.onTurn(async () => {
      await new Promise((resolve) => setTimeout(resolve, 5));
      return reply("late");
    });
    expect((await client.sendTurnJson("ses-1", { text: "x" })).events.some((e) => e.type === "token" && e.text === "late")).toBe(true);
    expect(() => askChoice("f", "q", [{ choice_id: "a", label: "A" }])).toThrow(RangeError);
  });

  it("a plan can make the whole turn an HTTP error instead of a stream", async () => {
    const { client, fake } = createHost();
    const { rejectTurn } = await import("../src/testing/index.js");
    fake.onTurn(() => rejectTurn(429, { code: "rate_limited", message: "slow down", retryable: true }));
    await expect(client.sendTurn("ses-1", { text: "x" }).result()).rejects.toMatchObject({ code: "rate_limited", status: 429 });
    expect(fake.session("ses-1")?.turn_count ?? 0).toBe(0);
  });

  it("a preparation can be given a short validity, and expires on the fake's clock", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction({ capability_id: "demo.book", summary: "Book it", validForMs: 1000 }));
    const grant = (await client.sendTurn("ses-1", { text: "x" }).result()).confirmation!;
    expect((await client.getPendingState("ses-1"))?.kind).toBe("confirmation");
    fake.advanceTime(1001);
    expect(await client.getPendingState("ses-1")).toBeNull();
    await expect(client.confirmAction(grant.preparation_id, grant.token)).rejects.toMatchObject({ code: "expired" });
  });
});

describe("the fake keeps a record the host can assert on, without keeping secrets", () => {
  it("records every request with its operation, status and a redacted body", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction({ capability_id: "demo.book", summary: "Book it" }));
    const grant = (await client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    await client.sendTurn("ses-1", { confirmation: grant }).result();
    const turn = fake.requests.filter((r) => r.operation === "sendTurn");
    expect(turn[1]?.body).toEqual({ confirmation: { preparation_id: grant.preparation_id, token: "[redacted]" } });
    expect(JSON.stringify(fake.requests)).not.toContain(grant.token);
    expect(fake.requests.every((r) => r.tokenId !== undefined)).toBe(true);
    expect(fake.requests.map((r) => r.status)).toEqual([200, 200]);
  });

  it("reset() forgets sessions, preparations, redeemed tokens, requests and injected failures", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction({ capability_id: "demo.book", summary: "Book it" }));
    await client.sendTurn("ses-1", { text: "book" }).result();
    fake.failNext({ status: 503, error: { code: "unavailable", message: "x", retryable: true } });
    fake.setReady(false);
    fake.setApplicationStatus("rejected");
    fake.reset();
    expect(fake.requests).toHaveLength(0);
    expect(fake.tokensRedeemed).toBe(0);
    expect(fake.preparations()).toEqual([]);
    expect(fake.session("ses-1")).toBeUndefined();
    expect((await client.getHealth()).ready).toBe(true);
    expect((await client.createSession()).turn_count).toBe(0);
  });

  it("an injected failure applies once by default, or `times` times, to the named operation", async () => {
    const { client, fake } = createHost();
    fake.failNext({ operation: "getSession", times: 2, status: 503, error: { code: "unavailable", message: "x", retryable: true } });
    await client.createSession(); // not the named operation
    for (let i = 0; i < 2; i++) await expect(client.getSession("ses-1")).rejects.toMatchObject({ code: "unavailable" });
    expect((await client.getSession("ses-1")).session_id).toBe("ses-1");
  });
});

describe("serving the fake over loopback HTTP, for code that cannot inject fetch", () => {
  it("binds 127.0.0.1 only and serves the same protocol to the global fetch, streams included", async () => {
    const fake = createFakeServer({ baseUrl: "https://alixai.test/alixai", streamChunkBytes: 7 });
    const { baseUrl, close } = await fake.listen();
    try {
      const url = new URL(baseUrl);
      expect(url.hostname).toBe("127.0.0.1");
      expect(url.pathname).toBe("/alixai");
      fake.onTurn(() => prepareAction({ capability_id: "demo.book", summary: "Book it", text: "héllo 😀" }));
      const client = createClient({ baseUrl, getContextToken: () => fake.issueToken({ session_id: "ses-1" }) }); // the real global fetch
      expect((await client.createSession()).session_id).toBe("ses-1");
      const result = await client.sendTurn("ses-1", { text: "book" }).result();
      expect(result.confirmation?.token).toMatch(/^cfm_/);
      expect(result.events.some((e) => e.type === "token" && e.text === "héllo 😀")).toBe(true);
      const outcome = await client.confirmAction(result.confirmation!.preparation_id, result.confirmation!.token);
      expect(outcome.receipt.status).toBe("committed");
      const replay = await client.confirmAction(result.confirmation!.preparation_id, result.confirmation!.token).catch((e: unknown) => e);
      expect(isAlixAIError(replay) && replay.code).toBe("replayed");
      expect((await client.getHealth()).ready).toBe(true);
    } finally {
      await close();
    }
  });

  it("aborting a stream over real HTTP releases it", async () => {
    const fake = createFakeServer({ streamChunkBytes: 8 });
    const { baseUrl, close } = await fake.listen();
    try {
      fake.onTurn(() => reply("word ".repeat(400)));
      const client = createClient({ baseUrl, getContextToken: () => fake.issueToken({ session_id: "ses-1" }) });
      const controller = new AbortController();
      const stream = client.sendTurn("ses-1", { text: "x" }, { signal: controller.signal });
      const iterator = stream[Symbol.asyncIterator]();
      expect((await iterator.next()).done).toBe(false);
      controller.abort();
      await expect(iterator.next()).rejects.toMatchObject({ name: "AbortError" });
    } finally {
      await close();
    }
  });
});
