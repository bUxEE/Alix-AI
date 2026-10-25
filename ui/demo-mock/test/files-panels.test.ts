import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { loadSpec } from "../../../sdk/typescript/test/helpers/openapi";
import { SETTINGS_SECTIONS } from "../../vue/src/settings-fields";
import { DEMO_PATHS, demoUploader, fetchDemoPreview } from "../src/index";
import { energyChartPng } from "../src/files/fixtures";
import { SEEDED_FILES } from "../src/files/store";
import { expectSchema, kindsOf, runVue, SCHEMA, startMock, uid, userMessage, type RunningMock } from "./helpers";

// The REST side: every answer is validated against the response schema its operation declares in schemas/api (the OpenAPI
// document the gateway's own contract test reads), and the files the kits show go through the same checks the core makes.

const spec = loadSpec();

let mock: RunningMock;
beforeAll(async () => {
  mock = await startMock({ pace: 0 });
});
afterAll(async () => {
  await mock.close();
});

const call = async (operationId: string, path: string, init: RequestInit = {}, expected = 200): Promise<unknown> => {
  const response = await fetch(`${mock.base}${path}`, init);
  expect(response.status, `${operationId} ${path}`).toBe(expected);
  const body: unknown = await response.json();
  const validate = spec.responseValidator(operationId, expected);
  expect(validate, `${operationId} declares a JSON body for ${expected}`).toBeDefined();
  if (!validate!(body)) throw new Error(`${operationId}: ${JSON.stringify(validate!.errors?.slice(0, 5), null, 1)}`);
  return body;
};

const upload = (bytes: Uint8Array, name: string, type: string, base = mock.base): Promise<Response> => {
  const form = new FormData();
  form.append("file", new Blob([Uint8Array.from(bytes)], { type }), name);
  return fetch(`${base}${DEMO_PATHS.files}`, { method: "POST", body: form });
};

const refusal = async (response: Response): Promise<{ code: string; details?: { reason?: string } }> => {
  const body = (await response.json()) as { code: string; details?: { reason?: string } };
  expectSchema(SCHEMA.error, body);
  return body;
};

describe("files", () => {
  it("stores an upload, previews it, and keeps its bytes from being downloaded", async () => {
    const png = energyChartPng();
    const response = await upload(png, "../energy chart.png", "image/png");
    expect(response.status).toBe(201);
    const ref = (await response.json()) as { file_id: string; name: string; previewable: boolean; permissions: string[] };
    expectSchema(SCHEMA.fileRef, ref);
    expect(spec.responseValidator("uploadFile", 201)!(ref)).toBe(true);
    expect(response.headers.get("Location")).toBe(`/v1/files/${ref.file_id}`);
    expect(ref.name).toBe("energy chart.png");
    expect(ref.permissions).toEqual(["read", "preview"]);
    await call("getFile", `/v1/files/${ref.file_id}`);
    const preview = await fetch(`${mock.base}/v1/files/${ref.file_id}/preview`);
    expect(preview.headers.get("Content-Type")).toBe("image/png");
    expect(preview.headers.get("X-Content-Type-Options")).toBe("nosniff");
    expect(Buffer.from(await preview.arrayBuffer()).equals(png)).toBe(true);
    const content = await fetch(`${mock.base}/v1/files/${ref.file_id}/content`);
    expect(content.status).toBe(403);
    expect((await refusal(content)).code).toBe("permission_denied");
    expect((await fetch(`${mock.base}/v1/files/${ref.file_id}`, { method: "DELETE" })).status).toBe(204);
    expect((await refusal(await fetch(`${mock.base}/v1/files/${ref.file_id}`))).code).toBe("scope_violation");
  });

  it("serves the application's files: a PDF to download (no preview), a CSV previewed as text", async () => {
    const pdf = await fetch(`${mock.base}/v1/files/${SEEDED_FILES.maintenancePolicy}/content`);
    expect(pdf.headers.get("Content-Disposition")).toBe('attachment; filename="preventive-maintenance-policy-v12.pdf"');
    expect(Buffer.from(await pdf.arrayBuffer()).subarray(0, 5).toString()).toBe("%PDF-");
    const noPreview = await fetch(`${mock.base}/v1/files/${SEEDED_FILES.maintenancePolicy}/preview`);
    expect(noPreview.status).toBe(415);
    expect((await refusal(noPreview)).details?.reason).toBe("not_previewable");
    const csv = await fetch(`${mock.base}/v1/files/${SEEDED_FILES.workOrdersCsv}/preview`);
    expect(csv.headers.get("Content-Type")).toContain("text/plain");
    expect(await csv.text()).toContain("WO-2291,Harbor Point Office Tower");
    expect((await refusal(await fetch(`${mock.base}/v1/files/${SEEDED_FILES.floorPlan}`, { method: "DELETE" }))).code).toBe("permission_denied");
    const list = (await (await fetch(`${mock.base}${DEMO_PATHS.fileList}`)).json()) as { files: unknown[] };
    expect(list.files.length).toBeGreaterThanOrEqual(7);
    for (const file of list.files) expectSchema(SCHEMA.fileRef, file);
  });

  it("refuses what the core refuses", async () => {
    const reason = async (response: Promise<Response>): Promise<string | undefined> => (await refusal(await response)).details?.reason;
    expect(await reason(upload(Buffer.from("<svg xmlns='http://www.w3.org/2000/svg'/>"), "logo.svg", "image/svg+xml"))).toBe("mime_not_allowed");
    expect(await reason(upload(energyChartPng(), "photo.jpg", "image/jpeg"))).toBe("mime_mismatch");
    expect(await reason(upload(Buffer.from("plain words"), "fake.png", "image/png"))).toBe("mime_mismatch");
    expect(await reason(upload(new Uint8Array(0), "empty.txt", "text/plain"))).toBe("empty");
    expect(await reason(upload(Buffer.from("x"), "blob.bin", "application/octet-stream"))).toBe("mime_declared");
    expect((await fetch(`${mock.base}${DEMO_PATHS.files}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" })).status).toBe(415);
    expect((await fetch(`${mock.base}/v1/files/not-an-id`)).status).toBe(400);
    const small = await startMock({ pace: 0, maxFileMb: 1 });
    try {
      const big = await upload(new Uint8Array(1536 * 1024).fill(65), "big.txt", "text/plain", small.base);
      expect(big.status).toBe(413);
      expect((await refusal(big)).details?.reason).toBe("too_large");
    } finally {
      await small.close();
    }
  });

  it("works with the demo helpers the apps use (uploader, preview as a blob URL)", async () => {
    const upload = demoUploader(mock.base);
    const { fileId } = await upload(new File([Uint8Array.from(energyChartPng())], "chart.png", { type: "image/png" }), new AbortController().signal);
    expect(fileId).toMatch(/^fil_[0-9A-HJKMNP-TV-Z]{26}$/);
    const preview = await fetchDemoPreview(fileId, mock.base);
    expect(preview.kind).toBe("image");
    if (preview.kind === "image") expect(preview.url.startsWith("blob:")).toBe(true);
    await expect(upload(new File(["<html></html>"], "page.html", { type: "text/html" }), new AbortController().signal)).rejects.toThrow(/not accepted/);
    const text = await fetchDemoPreview(SEEDED_FILES.workOrdersCsv, mock.base);
    expect(text).toMatchObject({ kind: "text", truncated: false });
  });

  it("shows the files of a message as an attachments activity at the start of its run", async () => {
    const response = await upload(energyChartPng(), "chart.png", "image/png");
    const ref = (await response.json()) as { file_id: string };
    const run = await runVue(mock.base, { threadId: uid("ses_files"), messages: [userMessage("What do you make of this?", [{ fileId: ref.file_id, kind: "image" }])] });
    expect(kindsOf(run.state.items)).toEqual(["attachments", "message"]);
    expect(run.events[1]).toMatchObject({ type: "ACTIVITY_SNAPSHOT", activityType: "alixai.attachments" });
    const files = run.state.items[0] as { files: { fileId: string; previewable: boolean }[] };
    expect(files.files[0]).toMatchObject({ fileId: ref.file_id, previewable: true });
    const unknown = await fetch(`${mock.base}/v1/agui`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ threadId: uid("ses"), runId: uid("run"), messages: [userMessage("look", [{ fileId: "fil_01JNWDEM0F1XTVRE0000000077", kind: "image" }])] }),
    });
    expect(unknown.status).toBe(403);
  });
});

describe("settings, sources, audit and health", () => {
  it("answers every panel route with the documents of its schema", async () => {
    await call("getUserSettings", DEMO_PATHS.settingsUser);
    const effective = (await call("getEffectiveSettings", DEMO_PATHS.settingsEffective)) as Parameters<(typeof SETTINGS_SECTIONS)[number]["fields"][number]["effective"]>[0];
    await call("listSettingsProfiles", DEMO_PATHS.settingsProfiles);
    await call("getSettingsCapabilities", DEMO_PATHS.settingsCapabilities);
    const audit = (await call("getSettingsIntegrationAudit", DEMO_PATHS.integrationAudit)) as { findings: { severity: string; status: string }[] };
    const health = (await call("getSettingsProviderHealth", DEMO_PATHS.providerHealth)) as { roles: { status: string }[] };
    const sources = (await call("listRagSources", DEMO_PATHS.ragSources)) as { sources: { status: { state: string; staleness: string; error?: unknown } }[] };
    await call("reauditRagSources", DEMO_PATHS.ragReaudit, { method: "POST" });
    await call("getLiveness", DEMO_PATHS.healthz);
    await call("getReadiness", DEMO_PATHS.readyz);
    await call("getAguiCapabilities", DEMO_PATHS.aguiCapabilities);
    // What the panels have to show: every switch reads a value, findings of every severity, mixed health.
    for (const section of SETTINGS_SECTIONS) for (const field of section.fields) expect(typeof field.effective(effective), field.path).toBe("boolean");
    expect(SETTINGS_SECTIONS.flatMap((section) => section.fields).some((field) => field.allowed?.(effective) === false)).toBe(true);
    expect(new Set(audit.findings.map((finding) => finding.severity))).toEqual(new Set(["critical", "warning", "info"]));
    expect(new Set(audit.findings.map((finding) => finding.status))).toEqual(new Set(["open", "accepted", "informational"]));
    expect(new Set(health.roles.map((role) => role.status))).toEqual(new Set(["ok", "degraded"]));
    expect(new Set(sources.sources.map((source) => source.status.state))).toEqual(new Set(["ready", "indexing", "paused"]));
    expect(sources.sources.some((source) => source.status.staleness === "stale")).toBe(true);
    expect(sources.sources.filter((source) => source.status.error !== undefined)).toHaveLength(2);
  });

  it("writes the user's settings with the version it read, and the effective settings follow", async () => {
    const before = (await call("getUserSettings", DEMO_PATHS.settingsUser)) as { version: number; settings: Record<string, unknown> };
    const put = (expected_version: number, settings: unknown): RequestInit => ({ method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ expected_version, settings }) });
    const next = { ...before.settings, streaming: { enabled: false } };
    const after = (await call("putUserSettings", DEMO_PATHS.settingsUser, put(before.version, next))) as { version: number };
    expect(after.version).toBe(before.version + 1);
    const effective = (await call("getEffectiveSettings", DEMO_PATHS.settingsEffective)) as { streaming: { enabled: boolean }; levels: { user: number } };
    expect(effective.streaming.enabled).toBe(false);
    expect(effective.levels.user).toBe(after.version);
    const stale = await fetch(`${mock.base}${DEMO_PATHS.settingsUser}`, put(before.version, next));
    expect(stale.status).toBe(409);
    expect(await refusal(stale)).toMatchObject({ code: "conflict", details: { current_version: after.version } });
  });

  it("lists, creates and forgets conversations, and answers CopilotKit's runtime probe with no agents", async () => {
    const list = (await (await fetch(`${mock.base}${DEMO_PATHS.conversations}`)).json()) as { conversations: { id: string; title?: string; preview?: string; updatedAt?: string }[] };
    expect(list.conversations.length).toBeGreaterThanOrEqual(4);
    for (const item of list.conversations) {
      expect(item.id).toMatch(/^[A-Za-z0-9_.-]+$/);
      expect(typeof item.title).toBe("string");
      expect(Date.parse(item.updatedAt ?? "")).not.toBeNaN();
    }
    const created = (await (await fetch(`${mock.base}${DEMO_PATHS.conversations}`, { method: "POST" })).json()) as { id: string };
    expect((await fetch(`${mock.base}${DEMO_PATHS.conversations}/${created.id}`, { method: "DELETE" })).status).toBe(204);
    const info = (await (await fetch(`${mock.base}${DEMO_PATHS.copilotRuntime}/info`)).json()) as { agents: object };
    expect(info.agents).toEqual({});
    const single = await fetch(`${mock.base}${DEMO_PATHS.copilotRuntime}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ method: "info" }) });
    expect(((await single.json()) as { version: string }).version).toBe("1.76.0");
    expect((await fetch(`${mock.base}/v1/nothing-here`)).status).toBe(404);
  });
});
