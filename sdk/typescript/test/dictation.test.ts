import { describe, expect, it } from "vitest";

import { isAlixAIError } from "../src/index.js";
import {
  DICTATION_OPERATIONS,
  createDictationClient,
  encodeWav16kMono,
  isDictationTranscript,
  isTranscriptionCapabilities,
} from "../src/voice/index.js";
import type { DictationTranscript, TranscriptionCapabilities } from "../src/voice/index.js";
import { describe as explain, loadSpec } from "./helpers/openapi.js";

const caps: TranscriptionCapabilities = {
  available: true,
  locality: "local",
  max_seconds: 60,
  sample_rate_hz: 16000,
  format: "audio/wav",
  languages: ["en", "it"],
};

const transcript: DictationTranscript = {
  text: "book Tuesday",
  language: "en",
  duration_ms: 1200,
  locality: "local",
  segment_kind: "user_recorded",
};

const silentError = {
  code: "validation_failed",
  message: "the clip is silent or too short",
  retryable: false,
  details: { reason: "silent" },
};

const unavailableCaps: TranscriptionCapabilities = {
  available: false,
  locality: "none",
  max_seconds: 60,
  sample_rate_hz: 16000,
  format: "audio/wav",
  languages: [],
};

const wav = encodeWav16kMono(new Float32Array(16000), 16000);

const fakeFetch = (routes: Record<string, (req: Request) => Promise<Response> | Response>): typeof fetch => {
  return async (input, init) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    const req = new Request(url, init);
    const bare = new URL(url).pathname;
    const handler = routes[bare];
    if (handler) return handler(req);
    return new Response(JSON.stringify({ code: "scope_violation", message: "no", retryable: false }), { status: 404 });
  };
};

describe("DICTATION_OPERATIONS matches OpenAPI", () => {
  const spec = loadSpec();
  it("every dictation operation exists with the same method and path", () => {
    for (const [operationId, op] of Object.entries(DICTATION_OPERATIONS)) {
      const declared = spec.operations.find((o) => o.operationId === operationId);
      expect(declared, `${operationId} is not in the OpenAPI document`).toBeDefined();
      expect({ method: declared?.method, path: declared?.path }).toEqual({ method: op.method, path: op.path });
    }
  });
});

describe("encodeWav16kMono", () => {
  it("writes a PCM WAV header at 16 kHz mono", () => {
    const bytes = encodeWav16kMono(new Float32Array([0.5, -0.5, 0]), 16000);
    expect(String.fromCharCode(...bytes.subarray(0, 4))).toBe("RIFF");
    expect(String.fromCharCode(...bytes.subarray(8, 12))).toBe("WAVE");
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    expect(view.getUint16(20, true)).toBe(1);
    expect(view.getUint16(22, true)).toBe(1);
    expect(view.getUint32(24, true)).toBe(16000);
    expect(view.getUint16(34, true)).toBe(16);
  });

  it("resamples from 48 kHz", () => {
    const at48 = new Float32Array(4800);
    for (let i = 0; i < at48.length; i += 1) at48[i] = Math.sin(i / 40);
    const bytes = encodeWav16kMono(at48, 48000);
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    expect(view.getUint32(24, true)).toBe(16000);
    expect(view.getUint32(40, true)).toBeGreaterThan(0);
  });
});

describe("guards", () => {
  it("accepts valid capabilities and transcripts", () => {
    expect(isTranscriptionCapabilities(caps)).toBe(true);
    expect(isTranscriptionCapabilities(unavailableCaps)).toBe(true);
    expect(isDictationTranscript(transcript)).toBe(true);
    expect(isTranscriptionCapabilities({ ...caps, sample_rate_hz: 8000 })).toBe(false);
    expect(isDictationTranscript({ ...transcript, segment_kind: "vad" })).toBe(false);
  });
});

describe("createDictationClient", () => {
  it("loads capabilities and creates a dictation", async () => {
    const tokens: string[] = [];
    const client = createDictationClient({
      baseUrl: "http://127.0.0.1:8080",
      getContextToken: async ({ operation, sessionId }) => {
        tokens.push(`${operation}:${sessionId ?? ""}`);
        return `tok_${operation}`;
      },
      fetch: fakeFetch({
        "/v1/dictations/capabilities": () => Response.json(caps),
        "/v1/sessions/ses_1/dictations": async (req) => {
          expect(req.headers.get("content-type")).toBe("audio/wav");
          expect(req.headers.get("authorization")).toBe("Bearer tok_createSessionDictation");
          const body = new Uint8Array(await req.arrayBuffer());
          expect(body.byteLength).toBeGreaterThan(44);
          return Response.json(transcript);
        },
      }),
    });
    await expect(client.capabilities()).resolves.toEqual(caps);
    await expect(client.create("ses_1", wav, { language: "en" })).resolves.toEqual(transcript);
    expect(tokens).toEqual(["getDictationCapabilities:", "createSessionDictation:ses_1"]);
  });

  it("maps silent clips and unavailable deployments to typed errors", async () => {
    const client = createDictationClient({
      baseUrl: "http://127.0.0.1:8080",
      getContextToken: async () => "tok_ok",
      fetch: fakeFetch({
        "/v1/dictations/capabilities": () => Response.json(unavailableCaps),
        "/v1/sessions/ses_1/dictations": () =>
          new Response(JSON.stringify(silentError), { status: 400, headers: { "Content-Type": "application/json" } }),
      }),
    });
    await expect(client.capabilities()).resolves.toMatchObject({ available: false, locality: "none" });
    try {
      await client.create("ses_1", wav);
      expect.unreachable();
    } catch (error) {
      expect(isAlixAIError(error)).toBe(true);
      if (isAlixAIError(error)) {
        expect(error.code).toBe("validation_failed");
        expect(error.details?.reason).toBe("silent");
      }
    }
  });

  it("refuses a bad session id before spending a token", async () => {
    let called = 0;
    const client = createDictationClient({
      baseUrl: "http://127.0.0.1:8080",
      getContextToken: async () => {
        called += 1;
        return "tok";
      },
      fetch: fakeFetch({}),
    });
    await expect(client.create("../x", wav)).rejects.toMatchObject({ code: "validation_failed" });
    expect(called).toBe(0);
  });
});

describe("OpenAPI response shapes", () => {
  const spec = loadSpec();
  it("accepts the fixtures against the document", () => {
    const cap = spec.responseValidator("getDictationCapabilities", 200)!;
    expect(cap(caps), explain(cap)).toBe(true);
    expect(cap(unavailableCaps), explain(cap)).toBe(true);
    const created = spec.responseValidator("createSessionDictation", 200)!;
    expect(created(transcript), explain(created)).toBe(true);
  });
});
