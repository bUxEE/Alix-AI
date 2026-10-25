import { describe, expect, it } from "vitest";

import * as sdk from "../src/index.js";
import { isAlixAIError } from "../src/index.js";
import { describe as explain, loadSpec } from "./helpers/openapi.js";

// The RAG namespace of the SDK (task M2-11): its operation table against schemas/api/paths/rag.yaml, the
// client against a stub of the server, and the rules that make this area what it is: provenance, the
// closed result and no internal metadata on the client.

const { rag } = sdk;
const spec = loadSpec();

const SRC = "src_01J0000000000000000000000A";
const GEN = "gen_01J0000000000000000000000B";
const FIL = "fil_01J0000000000000000000000C";
const JOB = "job_01J0000000000000000000000D";
const ULID = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

const status = {
  source_id: SRC,
  display_name: "handbook",
  state: "registered",
  availability: "unavailable",
  staleness: "unknown",
  last_attempt_at: null,
  last_success_at: null,
  counts: { discovered: 0, indexed: 0, skipped: 0, failed: 0, tombstoned: 0 },
  active_generation_id: null,
  error: { code: "unavailable", message: "the source has not been scanned yet", retryable: true },
};
const source = { declaration: "handbook", package_version: "1.0.0", serving: true, status };
const generation = {
  generation_id: GEN,
  source_id: SRC,
  state: "active",
  components: { parser: { name: "alixai-parse", version: "1" }, ocr: null, chunker: { name: "alixai-chunk", version: "1" }, embedder: null },
  counts: { documents: 3, chunks: 12, embedded_chunks: 0 },
  created_at: "2026-10-03T08:00:00.000Z",
  activated_at: "2026-10-03T08:00:00.000Z",
  retired_at: null,
};
const citation = {
  source_id: SRC,
  display_name: "Warranty terms",
  version: "v7",
  locator: { section: "Section 2" },
  availability: "available",
  staleness: "current",
  open_ref: { kind: "file", file_id: FIL },
  claims: ["The kettle has a warranty of 24 months."],
};
const answered = {
  outcome: "answered",
  claims: [{ text: "The kettle has a warranty of 24 months.", citation_indexes: [0] }],
  citations: [citation],
  notices: [],
};
const hit = {
  evidence_id: "e1",
  rank: 1,
  source_id: SRC,
  display_name: "Warranty terms",
  version: "v7",
  availability: "available",
  staleness: "current",
  open_ref: { kind: "source" },
  excerpt: "The kettle has a warranty of 24 months.",
};
const resolution = {
  source_id: SRC,
  display_name: "Warranty terms",
  version: "v7",
  availability: "available",
  staleness: "current",
  open_ref: { kind: "source" },
  preview: "The kettle has a warranty of 24 months.",
  expires_at: "2026-10-03T09:15:00.000Z",
};

interface Seen {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: string | undefined;
}

/** A stub of the server: answers what the test says and remembers what it was sent. */
const stub = (reply: (seen: Seen) => Response) => {
  const seen: Seen[] = [];
  const tokens: string[] = [];
  const fetch = async (url: string, init?: RequestInit): Promise<Response> => {
    const entry: Seen = {
      method: init?.method ?? "GET",
      url,
      headers: init?.headers as Record<string, string>,
      body: init?.body as string | undefined,
    };
    seen.push(entry);
    return reply(entry);
  };
  const client = rag.createRagClient({
    baseUrl: "http://127.0.0.1:8080",
    fetch,
    getContextToken: (request) => {
      tokens.push(request.operation);
      return `tok${tokens.length}.aaa.bbb`;
    },
  });
  return { client, seen, tokens };
};

const json = (body: unknown, status = 200): Response =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const failure = (code: string, status: number, details?: Record<string, unknown>): Response =>
  json({ code, message: "no", retryable: ["unavailable", "timeout", "rate_limited"].includes(code), ...(details && { details }) }, status);

describe("the operation table matches schemas/api/paths/rag.yaml", () => {
  const documented = spec.operations.filter((o) => o.area === "rag");

  it("has a row for every operation of the document, with its method, path and authentication", () => {
    expect(documented.map((o) => o.operationId).sort()).toEqual(Object.keys(rag.RAG_OPERATIONS).sort());
    for (const op of documented) {
      const row = rag.RAG_OPERATIONS[op.operationId as keyof typeof rag.RAG_OPERATIONS];
      expect({ method: row.method, path: row.path, authenticated: op.authenticated }, op.operationId).toEqual({
        method: op.method,
        path: op.path,
        authenticated: true,
      });
    }
  });

  it("marks a request safe to repeat exactly when it only reads", () => {
    for (const [name, row] of Object.entries(rag.RAG_OPERATIONS)) {
      expect(row.safe, name).toBe(row.method === "GET" || name === "searchRag" || name === "answerRag");
    }
  });

  it("validates its sample responses against the document", () => {
    const check = (operation: string, status: number, body: unknown) => {
      const validate = spec.responseValidator(operation, status);
      expect(validate, `${operation} ${status} declares a body`).toBeDefined();
      expect(validate!(body), `${operation}: ${explain(validate!)}`).toBe(true);
    };
    check("registerRagSource", 200, source);
    check("listRagSources", 200, { sources: [source] });
    check("getRagSource", 200, source);
    check("reauditRagSources", 200, { checked: 1, rebound: 1, queued: 0, removed: 0, failed: 0 });
    check("syncRagSource", 202, { source_id: SRC, job_id: JOB });
    check("rebuildRagSource", 202, { source_id: SRC, job_id: JOB });
    check("listRagGenerations", 200, { generations: [generation] });
    check("invalidateRagGeneration", 200, { ...generation, state: "invalidated", retired_at: "2026-10-03T09:00:00.000Z" });
    check("searchRag", 200, { results: [hit], notices: ["reduced_semantic_recall"] });
    check("answerRag", 200, answered);
    check("answerRag", 200, { outcome: "no_supported_evidence", reason: "no_match", notices: [] });
    check("resolveRagCitation", 200, resolution);
  });

  it("and the document refuses what the SDK refuses: a hit with internal metadata", () => {
    const validate = spec.responseValidator("searchRag", 200)!;
    expect(validate({ results: [{ ...hit, source_egress: "local_only" }], notices: [] })).toBe(false);
    expect(validate({ results: [{ ...hit, source_classification: "restricted" }], notices: [] })).toBe(false);
  });
});

describe("the namespace", () => {
  it("exports the client and the narrowing helpers, and no evidence record", () => {
    expect(Object.keys(rag).sort()).toEqual(["RAG_OPERATIONS", "createRagClient", "isAnswered", "isNoSupportedEvidence"]);
    for (const internal of ["EvidenceItem", "EvidenceCitation", "EvidencePackage"]) expect(Object.keys(rag)).not.toContain(internal);
  });

  it("narrows an AnswerResult", () => {
    expect(rag.isAnswered(answered as never)).toBe(true);
    expect(rag.isNoSupportedEvidence(answered as never)).toBe(false);
    expect(rag.isNoSupportedEvidence({ outcome: "no_supported_evidence", reason: "no_match", notices: [] })).toBe(true);
  });
});

describe("every method sends the request of the document, with a fresh token", () => {
  it("sources", async () => {
    const { client, seen, tokens } = stub((s) => {
      if (s.method === "DELETE") return new Response(null, { status: 204 });
      if (s.url.endsWith("/reaudit")) return json({ checked: 1, rebound: 1, queued: 0, removed: 0, failed: 0 });
      if (s.method === "POST") return json(source);
      return s.url.endsWith("/sources") ? json({ sources: [source] }) : json(source);
    });
    expect((await client.registerSource("handbook")).declaration).toBe("handbook");
    expect(await client.listSources()).toHaveLength(1);
    expect((await client.getSource(SRC)).status.source_id).toBe(SRC);
    await expect(client.removeSource(SRC)).resolves.toBeUndefined();
    expect((await client.reauditSources()).checked).toBe(1);
    expect(seen.map((s) => `${s.method} ${s.url.replace("http://127.0.0.1:8080", "")}`)).toEqual([
      "POST /v1/rag/sources",
      "GET /v1/rag/sources",
      `GET /v1/rag/sources/${SRC}`,
      `DELETE /v1/rag/sources/${SRC}`,
      "POST /v1/rag/sources/reaudit",
    ]);
    expect(JSON.parse(seen[0]!.body!)).toEqual({ declaration: "handbook" });
    expect(tokens).toEqual(["registerRagSource", "listRagSources", "getRagSource", "removeRagSource", "reauditRagSources"]);
    for (const [i, s] of seen.entries()) {
      expect(s.headers["Authorization"], `request ${i}`).toBe(`Bearer tok${i + 1}.aaa.bbb`);
      expect(s.headers["Accept"]).toBe("application/json");
    }
  });

  it("indexing", async () => {
    const { client, seen } = stub((s) => {
      if (s.url.endsWith("/sync") || s.url.endsWith("/rebuild")) return json({ source_id: SRC, job_id: JOB }, 202);
      if (s.url.endsWith("/generations")) return json({ generations: [generation] });
      return json({ ...generation, state: "invalidated", retired_at: "2026-10-03T09:00:00.000Z" });
    });
    expect((await client.syncSource(SRC)).job_id).toBe(JOB);
    expect((await client.rebuildSource(SRC)).source_id).toBe(SRC);
    expect(await client.listGenerations(SRC)).toHaveLength(1);
    expect((await client.invalidateGeneration(SRC, GEN)).state).toBe("invalidated");
    expect(seen.map((s) => `${s.method} ${s.url.replace("http://127.0.0.1:8080", "")}`)).toEqual([
      `POST /v1/rag/sources/${SRC}/sync`,
      `POST /v1/rag/sources/${SRC}/rebuild`,
      `GET /v1/rag/sources/${SRC}/generations`,
      `POST /v1/rag/sources/${SRC}/generations/${GEN}/invalidate`,
    ]);
  });

  it("questions", async () => {
    const { client, seen } = stub((s) => {
      if (s.url.endsWith("/search")) return json({ results: [hit], notices: [] });
      if (s.url.endsWith("/answer")) return json(answered);
      return json(resolution);
    });
    const found = await client.search({ query: "warranty", source_ids: [SRC], limit: 5 });
    expect(found.results[0]?.excerpt).toContain("24 months");
    const result = await client.answer({ question: "How long is the warranty?" });
    expect(rag.isAnswered(result) && result.citations[0]?.open_ref).toEqual({ kind: "file", file_id: FIL });
    expect((await client.resolveCitation(FIL)).preview).toContain("24 months");
    expect(JSON.parse(seen[0]!.body!)).toEqual({ query: "warranty", source_ids: [SRC], limit: 5 });
    expect(JSON.parse(seen[1]!.body!)).toEqual({ question: "How long is the warranty?" });
    expect(seen[2]!.url).toBe(`http://127.0.0.1:8080/v1/rag/citations/${FIL}`);
    expect(seen[2]!.headers["Content-Type"]).toBeUndefined();
  });
});

describe("a malformed argument never costs a token or reaches the wire", () => {
  const cases: Array<[string, (c: sdk.rag.RagClient) => Promise<unknown>]> = [
    ["a blank query", (c) => c.search({ query: "  " })],
    ["a query over 2000 characters", (c) => c.search({ query: "a".repeat(2001) })],
    ["2001 astral characters", (c) => c.search({ query: "\u{1F600}".repeat(2001) })],
    ["a NUL", (c) => c.answer({ question: "a\u0000b" })],
    ["33 sources", (c) => c.search({ query: "x", source_ids: Array.from({ length: 33 }, (_, i) => "src_" + "0".repeat(24) + ULID[Math.floor(i / 32)] + ULID[i % 32]) })],
    ["a repeated source", (c) => c.answer({ question: "x", source_ids: [SRC, SRC] })],
    ["a malformed source", (c) => c.answer({ question: "x", source_ids: ["src_nope"] })],
    ["an empty source list", (c) => c.answer({ question: "x", source_ids: [] })],
    ["a limit of 0", (c) => c.search({ query: "x", limit: 0 })],
    ["a limit of 51", (c) => c.search({ query: "x", limit: 51 })],
    ["a fractional limit", (c) => c.search({ query: "x", limit: 1.5 })],
    ["a tenant in a search", (c) => c.search({ query: "x", tenant_id: "ten_other" } as never)],
    ["a principal in an answer", (c) => c.answer({ question: "x", principal_id: "usr_x" } as never)],
    ["an unknown member", (c) => c.search({ query: "x", scope: {} } as never)],
    ["no request", (c) => c.search(undefined as never)],
    ["an empty declaration", (c) => c.registerSource("")],
    ["a padded declaration", (c) => c.registerSource(" handbook")],
    ["a declaration with a control character", (c) => c.registerSource("hand\nbook")],
    ["a declaration over 128 characters", (c) => c.registerSource("a".repeat(129))],
    ["a path in place of a source id", (c) => c.getSource("../../etc")],
    ["a malformed source id", (c) => c.removeSource("src_nope")],
    ["a malformed generation id", (c) => c.invalidateGeneration(SRC, "gen_nope")],
    ["a generation id for a source id", (c) => c.invalidateGeneration(GEN, GEN)],
    ["a malformed citation reference", (c) => c.resolveCitation("fil_nope")],
    ["a source id as a citation reference", (c) => c.resolveCitation(SRC)],
  ];
  for (const [name, run] of cases) {
    it(name, async () => {
      const { client, seen, tokens } = stub(() => json({}));
      const err = await run(client).then(
        () => undefined,
        (e: unknown) => e,
      );
      expect(isAlixAIError(err) && err.code, name).toBe("validation_failed");
      expect(seen).toHaveLength(0);
      expect(tokens).toHaveLength(0);
    });
  }

  it("counts characters, not UTF-16 units: 2000 astral characters are accepted", async () => {
    const { client, seen } = stub(() => json({ results: [], notices: [] }));
    await client.search({ query: "\u{1F600}".repeat(2000) });
    expect(seen).toHaveLength(1);
  });

  it("builds the body from the members it knows", async () => {
    const { client, seen } = stub(() => json({ results: [], notices: [] }));
    await client.search({ query: "x", source_ids: undefined, limit: undefined });
    expect(JSON.parse(seen[0]!.body!)).toEqual({ query: "x" });
  });
});

describe("a response that does not satisfy the contract is not handed to the host", () => {
  const bad = async (operation: (c: sdk.rag.RagClient) => Promise<unknown>, body: unknown) => {
    const { client } = stub(() => json(body));
    const err = await operation(client).then(
      () => undefined,
      (e: unknown) => e,
    );
    expect(isAlixAIError(err) && [err.code, err.details?.["reason"]]).toEqual(["internal", "unexpected_shape"]);
  };

  it("an answer that is neither answered nor the typed absence", async () => {
    for (const body of [{}, { outcome: "answered" }, { outcome: "maybe" }, { answer: "yes" }, [], "text", null]) {
      await bad((c) => c.answer({ question: "x" }), body);
    }
  });

  it("an answer without provenance: no claim, no citation, a claim without a citation, a citation out of range", async () => {
    await bad((c) => c.answer({ question: "x" }), { ...answered, claims: [] });
    await bad((c) => c.answer({ question: "x" }), { ...answered, citations: [] });
    await bad((c) => c.answer({ question: "x" }), { ...answered, claims: [{ text: "x", citation_indexes: [] }] });
    await bad((c) => c.answer({ question: "x" }), { ...answered, claims: [{ text: "x", citation_indexes: [1] }] });
    await bad((c) => c.answer({ question: "x" }), { ...answered, claims: [{ text: "x", citation_indexes: [-1] }] });
    await bad((c) => c.answer({ question: "x" }), { ...answered, citations: [{ ...citation, claims: [] }] });
    await bad((c) => c.answer({ question: "x" }), { ...answered, citations: [{ ...citation, open_ref: "https://example.com/x" }] });
  });

  it("an absence with an unlisted reason", async () => {
    await bad((c) => c.answer({ question: "x" }), { outcome: "no_supported_evidence", reason: "because", notices: [] });
  });

  it("anything that carries a source's egress mode or classification", async () => {
    await bad((c) => c.answer({ question: "x" }), { ...answered, citations: [{ ...citation, source_egress: "local_only" }] });
    await bad((c) => c.answer({ question: "x" }), { ...answered, citations: [{ ...citation, source_classification: "restricted" }] });
    await bad((c) => c.search({ query: "x" }), { results: [{ ...hit, source_egress: "local_only" }], notices: [] });
    await bad((c) => c.search({ query: "x" }), { results: [hit], notices: [], extra: { nested: [{ source_classification: "internal" }] } });
    await bad((c) => c.getSource(SRC), { ...source, source_egress: "deny" });
  });

  it("a search, a source, a generation and a resolution of the wrong shape", async () => {
    await bad((c) => c.search({ query: "x" }), { results: "none", notices: [] });
    await bad((c) => c.search({ query: "x" }), { results: [{ ...hit, excerpt: "" }], notices: [] });
    await bad((c) => c.search({ query: "x" }), { results: [{ ...hit, rank: 0 }], notices: [] });
    await bad((c) => c.search({ query: "x" }), { results: [], notices: ["made_up"] });
    await bad((c) => c.getSource(SRC), { declaration: "x" });
    await bad((c) => c.listSources(), { sources: [{ ...source, status: { ...status, state: "paused-ish" } }] });
    await bad((c) => c.listGenerations(SRC), { generations: [{ ...generation, state: "ready" }] });
    await bad((c) => c.syncSource(SRC), { source_id: SRC });
    await bad((c) => c.reauditSources(), { checked: -1, rebound: 0, queued: 0, removed: 0, failed: 0 });
    await bad((c) => c.resolveCitation(FIL), { ...resolution, expires_at: undefined });
  });

  it("a body that is not JSON", async () => {
    const { client } = stub(() => new Response("<html>proxy</html>", { status: 200 }));
    const err = await client.search({ query: "x" }).then(
      () => undefined,
      (e: unknown) => e,
    );
    expect(isAlixAIError(err) && [err.code, err.details?.["reason"]]).toEqual(["internal", "not_json"]);
  });
});

describe("errors keep their typed codes", () => {
  it("maps the statuses of the document", async () => {
    const cases: Array<[Response, string, boolean]> = [
      [failure("permission_denied", 403), "permission_denied", false],
      [failure("scope_violation", 403), "scope_violation", false],
      [failure("quarantined", 403), "quarantined", false],
      [failure("validation_failed", 404, { reason: "not_found" }), "validation_failed", false],
      [failure("validation_failed", 400, { reason: "not_declared" }), "validation_failed", false],
      [failure("conflict", 409), "conflict", false],
      [failure("expired", 410), "expired", false],
      [failure("unavailable", 503), "unavailable", true],
      [failure("timeout", 504), "timeout", true],
      [failure("rate_limited", 429), "rate_limited", true],
      [failure("schema_invalid", 502), "schema_invalid", false],
      [new Response("<html>bad gateway</html>", { status: 502 }), "unavailable", true],
    ];
    for (const [response, code, retryable] of cases) {
      const { client } = stub(() => response.clone());
      const err = await client.getSource(SRC).then(
        () => undefined,
        (e: unknown) => e,
      );
      expect(isAlixAIError(err) && [err.code, err.retryable], code).toEqual([code, retryable]);
    }
  });

  it("a 404 for an undeclared source carries the reason", async () => {
    const { client } = stub(() => failure("validation_failed", 400, { reason: "not_declared" }));
    const err = await client.registerSource("payroll").then(
      () => undefined,
      (e: unknown) => e,
    );
    expect(isAlixAIError(err) && err.details?.["reason"]).toBe("not_declared");
  });

  it("a request that may have been carried out and is not safe to repeat is uncertain; a read is unavailable", async () => {
    const down = async () => {
      throw new TypeError("fetch failed");
    };
    const mk = (fetch: (u: string, i?: RequestInit) => Promise<Response>) =>
      rag.createRagClient({ baseUrl: "http://127.0.0.1:8080", fetch, getContextToken: () => "tok.aaa.bbb" });
    const code = async (run: Promise<unknown>) => run.then(() => undefined, (e: unknown) => isAlixAIError(e) && e.code);
    expect(await code(mk(down).registerSource("handbook"))).toBe("uncertain_outcome");
    expect(await code(mk(down).removeSource(SRC))).toBe("uncertain_outcome");
    expect(await code(mk(down).syncSource(SRC))).toBe("uncertain_outcome");
    expect(await code(mk(down).getSource(SRC))).toBe("unavailable");
    expect(await code(mk(down).search({ query: "x" }))).toBe("unavailable");
    expect(await code(mk(down).answer({ question: "x" }))).toBe("unavailable");
  });

  it("a deadline: a read times out, a mutation is uncertain", async () => {
    const slow = (_url: string, init?: RequestInit): Promise<Response> =>
      new Promise((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal?.reason)));
    const client = rag.createRagClient({ baseUrl: "http://127.0.0.1:8080", fetch: slow, getContextToken: () => "tok.aaa.bbb", timeoutMs: 20 });
    const code = async (run: Promise<unknown>) => run.then(() => undefined, (e: unknown) => isAlixAIError(e) && e.code);
    expect(await code(client.search({ query: "x" }))).toBe("timeout");
    expect(await code(client.registerSource("handbook"))).toBe("uncertain_outcome");
  });

  it("a host that aborts gets its own reason, and no token is spent after the abort", async () => {
    const tokens: string[] = [];
    const client = rag.createRagClient({
      baseUrl: "http://127.0.0.1:8080",
      fetch: async () => json({}),
      getContextToken: (r) => {
        tokens.push(r.operation);
        return "tok.aaa.bbb";
      },
    });
    const controller = new AbortController();
    controller.abort(new Error("stop"));
    await expect(client.search({ query: "x" }, { signal: controller.signal })).rejects.toThrow("stop");
    expect(tokens).toEqual([]);
  });

  it("a token provider that fails or returns something unusable is unauthenticated, and nothing is sent", async () => {
    let sent = 0;
    const fetch = async () => {
      sent += 1;
      return json({});
    };
    for (const getContextToken of [
      () => {
        throw new Error("no session");
      },
      () => "Bearer abc.def",
      () => "",
      () => "has space",
    ]) {
      const client = rag.createRagClient({ baseUrl: "http://127.0.0.1:8080", fetch, getContextToken });
      const err = await client.search({ query: "x" }).then(
        () => undefined,
        (e: unknown) => e,
      );
      expect(isAlixAIError(err) && err.code).toBe("unauthenticated");
    }
    expect(sent).toBe(0);
  });

  it("refuses a base URL the main client refuses", () => {
    for (const baseUrl of ["", "ftp://x", "http://example.com", "https://user:pw@example.com", "https://example.com/?q=1"]) {
      expect(() => rag.createRagClient({ baseUrl, getContextToken: () => "t" }), baseUrl).toThrow();
    }
    expect(() => rag.createRagClient({ baseUrl: "http://example.com", getContextToken: () => "t", allowInsecureHttp: true })).not.toThrow();
    expect(() => rag.createRagClient({ baseUrl: "http://127.0.0.1:1", getContextToken: undefined as never })).toThrow();
  });

  it("never sends redirects, cookies or cached responses", async () => {
    let init: RequestInit | undefined;
    const client = rag.createRagClient({
      baseUrl: "http://127.0.0.1:8080/alixai/",
      fetch: async (url, i) => {
        init = i;
        expect(url.startsWith("http://127.0.0.1:8080/alixai/v1/rag/")).toBe(true);
        return json({ results: [], notices: [] });
      },
      getContextToken: () => "tok.aaa.bbb",
    });
    await client.search({ query: "x" });
    expect(init).toMatchObject({ redirect: "error", credentials: "omit", cache: "no-store" });
  });
});
