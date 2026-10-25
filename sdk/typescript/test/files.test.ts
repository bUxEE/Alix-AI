import { describe, expect, it, vi } from "vitest";

import { AlixAIError, attachmentKind, createClient, isAlixAIError, turnText } from "../src/index.js";
import type { FileUpload } from "../src/index.js";
import { createHost, collect } from "./helpers/host.js";
import { json, settled, slowBody, stubbed } from "./helpers/stub.js";

// Scoped files and the composition of a message with attachments (master plan 15.1): upload, read, preview, download and delete a
// FileRef; a message is its attachments uploaded first and then one text turn that names the uploaded file ids (turn attachments: test/attachments.test.ts).

const caught = async (promise: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await promise;
  } catch (error) {
    expect(isAlixAIError(error), `not an AlixAIError: ${String(error)}`).toBe(true);
    return error as AlixAIError;
  }
  throw new Error("expected a failure");
};

const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4]);
const text = (value: string): Uint8Array => new TextEncoder().encode(value);
const png = (name = "diagram.png"): FileUpload => ({ data: PNG, name, mimeType: "image/png" });
const note = (body = "hello", treatment: FileUpload["treatment"] = "application_object"): FileUpload => ({
  data: text(body),
  name: "note.txt",
  mimeType: "text/plain",
  treatment,
});

describe("upload", () => {
  it("stores a file and returns its reference: opaque id, sniffed type, owner, permissions, expiry, no path or URL", async () => {
    const { client } = createHost();
    const ref = await client.files.upload(png());
    expect(ref).toMatchObject({
      file_id: expect.stringMatching(/^fil_[0-9A-HJKMNP-TV-Z]{26}$/),
      name: "diagram.png",
      mime_type: "image/png",
      size_bytes: PNG.length,
      origin: "upload",
      treatment: "ephemeral_context",
      previewable: true,
    });
    expect(ref.content_hash).toMatch(/^sha256:[0-9a-f]{64}$/);
    expect(JSON.stringify(ref)).not.toMatch(/https?:|\/tmp|C:\\/);
  });

  it("sends multipart with the treatment BEFORE the file, and lets fetch set the boundary", async () => {
    const { client, seen } = stubbed(() =>
      json(201, {
        file_id: "fil_01J0000000000000000000000A",
        name: "a.txt",
        mime_type: "text/plain",
        size_bytes: 1,
        owner: "u",
        origin: "upload",
        permissions: ["read"],
        previewable: true,
        expires_at: "2026-10-03T10:00:00Z",
      }),
    );
    await client.files.upload({ data: text("x"), name: "a.txt", mimeType: "text/plain", treatment: "rag_source" });
    const request = seen[0]!;
    expect(request.url).toBe("https://alixai.test/v1/files");
    expect(request.init.method).toBe("POST");
    const headers = request.init.headers as Record<string, string>;
    expect(Object.keys(headers).map((h) => h.toLowerCase())).not.toContain("content-type");
    const form = request.init.body as FormData;
    expect([...form.keys()]).toEqual(["treatment", "file"]);
    expect((form.get("file") as File).name).toBe("a.txt");
    expect((form.get("file") as File).type).toBe("text/plain");
  });

  it("refuses locally, before a token is spent: HTML/SVG/XML/script types, an empty file, a bad name, a bad treatment, an unknown member", async () => {
    const host = createHost();
    const bad: Array<[string, FileUpload | Record<string, unknown>]> = [
      ["html", { data: text("<p>"), name: "a.html", mimeType: "text/html" }],
      ["svg", { data: text("<svg/>"), name: "a.svg", mimeType: "image/svg+xml" }],
      ["xml", { data: text("<a/>"), name: "a.xml", mimeType: "application/xml" }],
      ["script", { data: text("1"), name: "a.js", mimeType: "application/javascript" }],
      ["empty", { data: new Uint8Array(), name: "a.txt", mimeType: "text/plain" }],
      ["no name", { data: text("x"), name: "", mimeType: "text/plain" }],
      ["control character", { data: text("x"), name: "a\u0000.txt", mimeType: "text/plain" }],
      ["a media type with parameters", { data: text("x"), name: "a.txt", mimeType: "text/plain; charset=utf-8" }],
      ["treatment", { data: text("x"), name: "a.txt", mimeType: "text/plain", treatment: "forever" }],
      ["unknown member", { data: text("x"), name: "a.txt", mimeType: "text/plain", path: "/etc/passwd" }],
      ["not bytes", { data: "x", name: "a.txt", mimeType: "text/plain" }],
    ];
    for (const [label, file] of bad) {
      expect((await caught(host.client.files.upload(file as FileUpload))).code, label).toBe("validation_failed");
    }
    expect(host.tokenRequests).toHaveLength(0);
    expect(host.fake.requests).toHaveLength(0);
  });

  it("accepts a Blob and an ArrayBuffer", async () => {
    const { client } = createHost();
    expect((await client.files.upload({ data: new Blob([PNG]), name: "b.png", mimeType: "image/png" })).size_bytes).toBe(PNG.length);
    expect((await client.files.upload({ data: PNG.buffer.slice(0) as ArrayBuffer, name: "c.png", mimeType: "image/png" })).size_bytes).toBe(PNG.length);
  });

  it("the core's refusals are typed: a type that is not what the bytes are, a type the policy does not allow, too large, the quota", async () => {
    const host = createHost({}, { files: { allowedTypes: ["image/png", "text/plain"], maxFileBytes: 64, quota: 2 } });
    const { client } = host;
    expect(await caught(client.files.upload({ data: text("not a png"), name: "a.png", mimeType: "image/png" }))).toMatchObject({ code: "validation_failed", details: { reason: "mime_mismatch" } });
    expect(await caught(client.files.upload({ data: PNG, name: "a.pdf", mimeType: "application/pdf" }))).toMatchObject({ details: { reason: "mime_not_allowed" } });
    expect(await caught(client.files.upload({ data: text("x".repeat(65)), name: "a.txt", mimeType: "text/plain" }))).toMatchObject({ status: 413, details: { reason: "too_large" } });
    await client.files.upload(note("one"));
    await client.files.upload(note("two"));
    expect(await caught(client.files.upload(note("three")))).toMatchObject({ code: "resource_exhausted", status: 507, retryable: true, details: { reason: "quota_files" } });
  });

  it("a treatment the host policy does not accept is permission_denied", async () => {
    const { client } = createHost({}, { files: { treatments: ["ephemeral_context"] } });
    expect(await caught(client.files.upload(note("x", "rag_source")))).toMatchObject({ code: "permission_denied", details: { reason: "treatment_not_allowed" } });
  });

  it("a network failure during an upload is uncertain_outcome: the file may have been stored", async () => {
    const { client } = stubbed(() => {
      throw new TypeError("connection reset");
    });
    expect((await caught(client.files.upload(note()))).code).toBe("uncertain_outcome");
  });

  it("names the session to the token provider: from the option, or from a bound client", async () => {
    const host = createHost();
    await host.client.files.upload(png(), { sessionId: "ses-1" });
    await host.client.forSession("ses-1").files.upload(png());
    await host.client.files.upload(png());
    expect(host.tokenRequests).toEqual([
      { operation: "uploadFile", sessionId: "ses-1" },
      { operation: "uploadFile", sessionId: "ses-1" },
      { operation: "uploadFile" },
    ]);
    expect((await caught(host.client.files.upload(png(), { sessionId: "bad id" as never }))).code).toBe("validation_failed");
  });
});

describe("get, preview, download, delete", () => {
  it("reads the reference again, and refuses an id that is not one", async () => {
    const host = createHost();
    const ref = await host.client.files.upload(png());
    expect(await host.client.files.get(ref.file_id)).toEqual(ref);
    expect((await caught(host.client.files.get("fil_nope"))).code).toBe("validation_failed");
    expect(host.tokenRequests.map((t) => t.operation)).toEqual(["uploadFile", "getFile"]);
    expect(host.tokenRequests[1]).toMatchObject({ fileId: ref.file_id });
  });

  it("an ephemeral context is previewable but has no download by default (permission_denied); an application object has both", async () => {
    const { client } = createHost();
    const ephemeral = await client.files.upload(png());
    expect((await client.files.preview(ephemeral.file_id)).data).toEqual(PNG);
    expect(await caught(client.files.download(ephemeral.file_id))).toMatchObject({ code: "permission_denied", details: { reason: "permission_missing" } });
    const object = await client.files.upload(note("hello world"));
    const content = await client.files.download(object.file_id);
    expect(new TextDecoder().decode(content.data)).toBe("hello world");
    expect(content).toMatchObject({ contentType: "text/plain", fileName: "note.txt", size: 11 });
  });

  it("a text preview is cut to the limit and says so; an image is served as itself", async () => {
    const { client } = createHost({}, { files: { previewTextBytes: 4 } });
    const object = await client.files.upload(note("hello world"));
    const preview = await client.files.preview(object.file_id);
    expect(new TextDecoder().decode(preview.data)).toBe("hell");
    expect(preview).toMatchObject({ contentType: "text/plain", truncated: true });
    const image = await client.files.upload(png());
    expect(await client.files.preview(image.file_id)).toMatchObject({ contentType: "image/png", truncated: false });
  });

  it("a type with no safe previewer is not_previewable", async () => {
    const { client } = createHost();
    const pdf = await client.files.upload({ data: text("%PDF-1.4"), name: "a.pdf", mimeType: "application/pdf", treatment: "application_object" });
    expect(pdf.previewable).toBe(false);
    expect(await caught(client.files.preview(pdf.file_id))).toMatchObject({ code: "validation_failed", status: 415, details: { reason: "not_previewable" } });
  });

  it("maxBytes refuses a file that is too large, by its Content-Length and again by what was read", async () => {
    const { client } = createHost();
    const object = await client.files.upload(note("hello world"));
    expect(await caught(client.files.download(object.file_id, { maxBytes: 5 }))).toMatchObject({ code: "resource_exhausted", details: { reason: "too_large", limit: 5 } });
    expect((await client.files.download(object.file_id, { maxBytes: 11 })).size).toBe(11);
    expect((await caught(client.files.download(object.file_id, { maxBytes: 0 }))).code).toBe("validation_failed");
  });

  it("deleting stops the reference resolving at once", async () => {
    const { client } = createHost();
    const ref = await client.files.upload(png());
    await expect(client.files.delete(ref.file_id)).resolves.toBeUndefined();
    expect(await caught(client.files.get(ref.file_id))).toMatchObject({ code: "scope_violation" });
  });

  it("a reference of another principal, or another session's ephemeral context, is one and the same scope_violation as one that never existed", async () => {
    const host = createHost();
    const ref = await host.client.files.upload(png());
    const other = (claims: { session_id: string; principal_id?: string }) =>
      createClient({ baseUrl: host.fake.baseUrl, fetch: host.fake.fetch, getContextToken: () => host.fake.issueToken(claims) });
    const stranger = await caught(other({ session_id: "ses-1", principal_id: "intruder" }).files.get(ref.file_id));
    const elsewhere = await caught(other({ session_id: "ses-2" }).files.get(ref.file_id));
    const never = await caught(host.client.files.get("fil_01J0000000000000000000000A"));
    for (const error of [stranger, elsewhere, never]) expect(error).toMatchObject({ code: "scope_violation", status: 403 });
    expect(stranger.message).toBe(never.message);
    expect(elsewhere.message).toBe(never.message);
  });

  it("a reference expires: expired (410), never a stale success", async () => {
    const host = createHost({}, { files: { ttlMs: 60_000 } });
    const ref = await host.client.files.upload(png());
    host.fake.advanceTime(61_000);
    expect(await caught(host.client.files.get(ref.file_id))).toMatchObject({ code: "expired", status: 410, retryable: false });
  });

  it("a package that is not active serves no files", async () => {
    const host = createHost();
    const ref = await host.client.files.upload(png());
    host.fake.setApplicationStatus("quarantined");
    expect((await caught(host.client.files.get(ref.file_id))).code).toBe("quarantined");
  });
});

describe("attachmentKind", () => {
  it("sorts a media type into image, audio, video or file", () => {
    expect(["image/png", "audio/mpeg", "video/mp4", "application/pdf", "text/plain", "IMAGE/JPEG"].map(attachmentKind)).toEqual(["image", "audio", "video", "file", "file", "image"]);
  });
});

describe("sendMessage: a message with attachments", () => {
  it("uploads each attachment first, then sends the text turn, and reports the references in order", async () => {
    const host = createHost();
    const stream = host.client.sendMessage("ses-1", {
      text: "what is in these?",
      attachments: [png("one.png"), { data: text("ID3"), name: "voice.mp3", mimeType: "audio/mpeg" }, { data: text("....ftypmp42"), name: "clip.mp4", mimeType: "video/mp4" }],
    });
    const result = await stream.result();
    const refs = await stream.attachments;
    expect(refs.map((r) => [r.name, attachmentKind(r.mime_type)])).toEqual([["one.png", "image"], ["voice.mp3", "audio"], ["clip.mp4", "video"]]);
    expect(host.fake.requests.map((r) => r.operation)).toEqual(["uploadFile", "uploadFile", "uploadFile", "sendTurn"]);
    expect(turnText(result.events)).toBe("fake: what is in these?");
    expect(host.fake.files().map((f) => f.file_id)).toEqual(refs.map((r) => r.file_id));
    // The turn names the uploaded files, in the order given.
    expect(host.fake.requests.at(-1)?.body).toEqual({ text: "what is in these?", attachments: refs.map((r) => r.file_id) });
    // Every upload told the token provider which session it was for.
    expect(host.tokenRequests.filter((t) => t.operation === "uploadFile").map((t) => t.sessionId)).toEqual(["ses-1", "ses-1", "ses-1"]);
  });

  it("a message without attachments is just a turn", async () => {
    const host = createHost();
    const stream = host.client.sendMessage("ses-1", { text: "hi", language: "it" });
    expect(await collect(stream)).toHaveLength(4);
    expect(await stream.attachments).toEqual([]);
    expect(host.fake.requests.map((r) => r.operation)).toEqual(["sendTurn"]);
    expect(host.fake.requests[0]?.body).toEqual({ text: "hi", language: "it" });
  });

  it("checks the whole message before anything is uploaded: no token, no stored file", async () => {
    const host = createHost();
    const attempts: Array<[string, unknown]> = [
      ["no text", { text: "", attachments: [png()] }],
      ["an attachment that is not storable", { text: "x", attachments: [png(), { data: text("<p>"), name: "a.html", mimeType: "text/html" }] }],
      ["too many attachments", { text: "x", attachments: Array.from({ length: 5 }, () => png()) }],
      ["an unknown member", { text: "x", attachment: png() }],
    ];
    for (const [label, message] of attempts) {
      const stream = host.client.sendMessage("ses-1", message as never);
      expect((await caught(stream.result())).code, label).toBe("validation_failed");
      expect((await caught(stream.attachments)).code, label).toBe("validation_failed");
    }
    expect(host.tokenRequests).toHaveLength(0);
    expect(host.fake.files()).toEqual([]);
  });

  it("when an upload fails nothing is sent and the files already stored are deleted", async () => {
    const host = createHost();
    const stream = host.client.sendMessage("ses-1", {
      text: "x",
      attachments: [png("ok.png"), { data: text("this is not a png"), name: "bad.png", mimeType: "image/png" }],
    });
    expect(await caught(stream.result())).toMatchObject({ code: "validation_failed", details: { reason: "mime_mismatch" } });
    expect(await caught(stream.attachments)).toMatchObject({ details: { reason: "mime_mismatch" } });
    expect(host.fake.requests.map((r) => r.operation)).toEqual(["uploadFile", "uploadFile", "deleteFile"]);
    expect(host.fake.files()).toEqual([]);
  });

  it("cancel abandons the message, whichever step it is on", async () => {
    const host = createHost();
    const stream = host.client.sendMessage("ses-1", { text: "x", attachments: [png()] });
    stream.cancel(new DOMException("never mind", "AbortError"));
    await expect(stream.result()).rejects.toMatchObject({ name: "AbortError" });
  });

  it("works on a client bound to a session, and refuses a bad session id without a token", async () => {
    const host = createHost();
    const bound = host.client.forSession("ses-1");
    await bound.sendMessage("ses-1", { text: "x", attachments: [png()] }).result();
    expect(host.tokenRequests.every((t) => t.sessionId === "ses-1")).toBe(true);
    host.tokenRequests.length = 0;
    expect((await caught(host.client.sendMessage("../x", { text: "x" }).result())).code).toBe("validation_failed");
    expect(host.tokenRequests).toHaveLength(0);
  });
});

describe("a download that takes longer than the request deadline", () => {
  const ID = "fil_01J00000000000000000000000";

  it("is bounded by the deadline only until the response head arrives, and the caller's signal still stops it", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      const slow = slowBody({ "Content-Type": "text/plain" });
      const client = createClient({ baseUrl: "https://alixai.test", getContextToken: () => "stub.token", fetch: slow.fetch, timeoutMs: 1000 });
      const download = settled(client.files.download(ID));
      await vi.advanceTimersByTimeAsync(5000); // well past the deadline of an ordinary request
      slow.finish("hello");
      const outcome = await download;
      expect("value" in outcome && new TextDecoder().decode(outcome.value.data)).toBe("hello");

      const preview = settled(client.files.preview(ID));
      await vi.advanceTimersByTimeAsync(5000);
      slow.finish("pre");
      expect(await preview).toHaveProperty("value");

      const controller = new AbortController();
      const stopped = settled(client.files.download(ID, { signal: controller.signal }));
      await vi.advanceTimersByTimeAsync(0);
      controller.abort(new DOMException("stop", "AbortError"));
      expect(await stopped).toMatchObject({ error: { name: "AbortError" } });
    } finally {
      vi.useRealTimers();
    }
  });

  it("still times out when the response head does not come", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      const never = (_url: string, init: RequestInit = {}): Promise<Response> =>
        new Promise((_resolve, reject) => init.signal?.addEventListener("abort", () => reject(init.signal?.reason), { once: true }));
      const client = createClient({ baseUrl: "https://alixai.test", getContextToken: () => "stub.token", fetch: never, timeoutMs: 1000 });
      const download = settled(client.files.download(ID));
      await vi.advanceTimersByTimeAsync(1000);
      expect(await download).toMatchObject({ error: { code: "timeout" } });
    } finally {
      vi.useRealTimers();
    }
  });
});
