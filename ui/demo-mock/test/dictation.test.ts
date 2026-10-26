import { voice } from "@alixai/sdk";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { DEMO_PATHS, dictationPath } from "../src/index";
import { expectSchema, startMock, type RunningMock } from "./helpers";

const SCHEMA_CAPS = "urn:alixai:schema:voice:transcription-capabilities:v1";
const SCHEMA_TRANSCRIPT = "urn:alixai:schema:voice:dictation-transcript:v1";

const toneWav = (seconds = 0.5, rate = 16000, amplitude = 8000): Uint8Array => {
  const n = Math.floor(rate * seconds);
  const pcm = new Int16Array(n);
  for (let i = 0; i < n; i += 1) pcm[i] = Math.sin(i / 20) * amplitude;
  return voice.encodeWav16kMono(Float32Array.from(pcm, (s) => s / 32768), rate);
};

const silentWav = (): Uint8Array => voice.encodeWav16kMono(new Float32Array(16000), 16000);

describe("dictation HTTP", () => {
  let mock: RunningMock;
  beforeAll(async () => {
    mock = await startMock({ pace: 0 });
  });
  afterAll(async () => {
    await mock.close();
  });

  it("reports capabilities and returns a scripted transcript", async () => {
    const client = voice.createDictationClient({
      baseUrl: mock.base,
      getContextToken: async () => "demo-context-token",
      allowInsecureHttp: true,
    });
    const caps = await client.capabilities();
    expectSchema(SCHEMA_CAPS, caps);
    expect(caps).toMatchObject({ available: true, locality: "local", sample_rate_hz: 16000, format: "audio/wav" });
    const transcript = await client.create("ses_demo_dictation", toneWav(), { language: "en" });
    expectSchema(SCHEMA_TRANSCRIPT, transcript);
    expect(transcript.text).toMatch(/Riverside/);
    expect(transcript.segment_kind).toBe("user_recorded");
  });

  it("refuses a silent clip with validation_failed", async () => {
    const client = voice.createDictationClient({
      baseUrl: mock.base,
      getContextToken: async () => "demo-context-token",
      allowInsecureHttp: true,
    });
    await expect(client.create("ses_demo_dictation", silentWav())).rejects.toMatchObject({
      code: "validation_failed",
      details: { reason: "silent" },
    });
  });
});

describe("dictation unavailable deployment", () => {
  let mock: RunningMock;
  beforeAll(async () => {
    mock = await startMock({ pace: 0, dictation: "unavailable" });
  });
  afterAll(async () => {
    await mock.close();
  });

  it("answers 503 unavailable on capabilities", async () => {
    const response = await fetch(`${mock.base}${DEMO_PATHS.dictationCapabilities}`);
    expect(response.status).toBe(503);
    const body = (await response.json()) as { code: string };
    expect(body.code).toBe("unavailable");
  });

  it("answers 503 unavailable on create", async () => {
    const wav = toneWav();
    const response = await fetch(`${mock.base}${dictationPath("ses_1")}`, {
      method: "POST",
      headers: { "Content-Type": "audio/wav" },
      body: Buffer.from(wav),
    });
    expect(response.status).toBe(503);
  });
});

describe("dictation disabled deployment", () => {
  let mock: RunningMock;
  beforeAll(async () => {
    mock = await startMock({ pace: 0, dictation: "disabled" });
  });
  afterAll(async () => {
    await mock.close();
  });

  it("reports available false so the host hides the control", async () => {
    const client = voice.createDictationClient({
      baseUrl: mock.base,
      getContextToken: async () => "demo-context-token",
      allowInsecureHttp: true,
    });
    const caps = await client.capabilities();
    expectSchema(SCHEMA_CAPS, caps);
    expect(caps).toMatchObject({ available: false, locality: "none", languages: [] });
  });
});
