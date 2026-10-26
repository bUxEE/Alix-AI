import type { IncomingMessage, ServerResponse } from "node:http";
import type { Duplex } from "node:stream";
import type { SettingsUserRecord } from "@alixai/sdk";
import { createAguiHandler } from "./agui/handler";
import { createThreadStore } from "./agui/threads";
import { effectiveSettings, initialUserSettings, integrationAudit, providerHealth, ragSources, readyReport, settingsCapabilities, settingsProfiles, USER_SETTINGS_SCHEMA } from "./data/panels";
import { createFileStore } from "./files/store";
import { BodyTooLargeError, isRecord, readJson, sendEmpty, sendError, sendJson } from "./http";
import { createMeMemoryHandler } from "./me-memory";
import { DEMO_PATHS, DEMO_THREAD_ID } from "./paths";
import { createDictationHandler, type DictationMode } from "./voice/dictation";
import { createVoiceServer } from "./voice/server";

// The whole mock as a connect-style middleware and an upgrade handler, so a Vite dev server, a preview server or a plain
// node:http server (the tests) can mount it. It answers only its own paths and passes everything else on.

export interface DemoMockOptions {
  /** A path prefix for every route (for example "/mock"). Default: none, the routes are at the root like the gateway's. */
  basePath?: string;
  /** Multiplies every delay: 1 (default) streams like a small local model, 0 sends everything at once. */
  pace?: number;
  /** The largest upload, in megabytes. Default 8 (the effective `attachments.max_file_mb`). */
  maxFileMb?: number;
  /** How replies are spoken on the voice socket: "system" (default) with the machine's own voice, "silent" for tests. */
  voice?: "system" | "silent";
  /**
   * Dictation HTTP: `available` (default) answers capabilities and scripted transcripts; `disabled` hides the control
   * (`available: false`); `unavailable` answers 503 as a deployment with no STT.
   */
  dictation?: DictationMode;
}

export interface DemoMock {
  middleware: (req: IncomingMessage, res: ServerResponse, next: (error?: unknown) => void) => void;
  /** Takes a WebSocket upgrade for a voice socket; false when the request is not one of the mock's. */
  handleUpgrade: (req: IncomingMessage, socket: Duplex, head: Buffer) => boolean;
  close: () => void;
}

const COPILOTKIT_RUNTIME_INFO = { version: "1.76.0", agents: {}, audioFileTranscriptionEnabled: false };

export const createDemoMock = (options: DemoMockOptions = {}): DemoMock => {
  const base = (options.basePath ?? "").replace(/\/+$/, "");
  const pace = options.pace ?? 1;
  const files = createFileStore({ maxFileMb: options.maxFileMb ?? 8 });
  const threads = createThreadStore(DEMO_THREAD_ID);
  const meMemory = createMeMemoryHandler();
  const agui = createAguiHandler({ pace, threads, files, meMemory });
  const voice = createVoiceServer({ pace, voice: options.voice ?? "system" });
  const dictation = createDictationHandler(options.dictation ?? "available");
  let userSettings: SettingsUserRecord = initialUserSettings();

  const split = (rawUrl: string | undefined): { path: string; query: URLSearchParams } | undefined => {
    const url = new URL(rawUrl ?? "/", "http://demo-mock.invalid");
    if (base !== "" && url.pathname !== base && !url.pathname.startsWith(`${base}/`)) return undefined;
    return { path: url.pathname.slice(base.length) || "/", query: url.searchParams };
  };

  const putUserSettings = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    let body: unknown;
    try {
      body = await readJson(req, 64 * 1024);
    } catch (error) {
      sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the settings document could not be read");
      return;
    }
    if (!isRecord(body) || typeof body["expected_version"] !== "number" || !isRecord(body["settings"])) {
      sendError(res, 400, "validation_failed", "the body is {expected_version, settings}");
      return;
    }
    if (body["expected_version"] !== userSettings.version) {
      sendError(res, 409, "conflict", "the settings changed since they were read", { current_version: userSettings.version });
      return;
    }
    const next = body["settings"] as SettingsUserRecord["settings"];
    if (JSON.stringify(next) !== JSON.stringify(userSettings.settings)) {
      userSettings = { level: "user", version: userSettings.version + 1, schema: USER_SETTINGS_SCHEMA, updated_at: new Date().toISOString(), updated_by: "usr_demo_alex", settings: next };
    }
    sendJson(res, 200, userSettings);
  };

  const route = async (req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams): Promise<boolean> => {
    const method = req.method ?? "GET";
    if (await agui.handle(req, res, path, query)) return true;
    if (await dictation.handle(req, res, path, query)) return true;
    if (await meMemory.handle(req, res, path, query)) return true;
    if (path.startsWith("/v1/files") && (await files.handle(req, res, path))) return true;
    const get = method === "GET";
    if (get && path === DEMO_PATHS.healthz) sendJson(res, 200, { status: "ok" });
    else if (get && path === DEMO_PATHS.readyz) sendJson(res, 200, readyReport());
    else if (get && path === DEMO_PATHS.settingsUser) sendJson(res, 200, userSettings);
    else if (method === "PUT" && path === DEMO_PATHS.settingsUser) await putUserSettings(req, res);
    else if (get && path === DEMO_PATHS.settingsEffective) sendJson(res, 200, effectiveSettings(userSettings));
    else if (get && path === DEMO_PATHS.settingsProfiles) sendJson(res, 200, settingsProfiles(effectiveSettings(userSettings).model_profile));
    else if (get && path === DEMO_PATHS.settingsCapabilities) sendJson(res, 200, settingsCapabilities(effectiveSettings(userSettings)));
    else if (get && path === DEMO_PATHS.integrationAudit) sendJson(res, 200, integrationAudit());
    else if (get && path === DEMO_PATHS.providerHealth) sendJson(res, 200, providerHealth());
    else if (get && path === DEMO_PATHS.ragSources) sendJson(res, 200, ragSources());
    else if (method === "POST" && path === DEMO_PATHS.ragReaudit) sendJson(res, 200, { checked: 5, rebound: 0, queued: 1, removed: 0, failed: 1 });
    else if (get && path === DEMO_PATHS.conversations) sendJson(res, 200, { conversations: threads.list() });
    else if (method === "POST" && path === DEMO_PATHS.conversations) {
      const created = threads.create();
      sendJson(res, 201, { id: created.id, title: created.title, updatedAt: created.updatedAt });
    } else if (method === "DELETE" && path.startsWith(`${DEMO_PATHS.conversations}/`)) {
      const removed = threads.remove(decodeURIComponent(path.slice(DEMO_PATHS.conversations.length + 1)));
      if (removed) sendEmpty(res, 204);
      else sendError(res, 404, "scope_violation", "no such conversation");
    } else if (get && path === DEMO_PATHS.fileList) sendJson(res, 200, { files: files.list() });
    else if (get && path === `${DEMO_PATHS.copilotRuntime}/info`) sendJson(res, 200, COPILOTKIT_RUNTIME_INFO);
    else if (method === "POST" && path === DEMO_PATHS.copilotRuntime) {
      const body = await readJson(req, 64 * 1024).catch(() => undefined);
      if (isRecord(body) && body["method"] === "info") sendJson(res, 200, COPILOTKIT_RUNTIME_INFO);
      else sendError(res, 404, "validation_failed", "the demo runtime answers only {\"method\": \"info\"}: point an HttpAgent at /v1/agui");
    } else return false;
    return true;
  };

  const middleware = (req: IncomingMessage, res: ServerResponse, next: (error?: unknown) => void): void => {
    const target = split(req.url);
    if (target === undefined) {
      next();
      return;
    }
    route(req, res, target.path, target.query)
      .then((handled) => {
        if (handled) return;
        // Under the mock's own prefixes an unknown route is the mock's 404, never the page's HTML fallback.
        if (/^\/(v1|demo)(\/|$)/.test(target.path) || target.path === DEMO_PATHS.healthz || target.path === DEMO_PATHS.readyz) {
          sendError(res, 404, "validation_failed", `the demo mock has no route ${req.method ?? "GET"} ${target.path}`);
          return;
        }
        next();
      })
      .catch((error: unknown) => {
        if (!res.headersSent) sendError(res, 500, "internal", error instanceof Error ? error.message : "the demo mock failed");
        else res.destroy();
      });
  };

  const handleUpgrade = (req: IncomingMessage, socket: Duplex, head: Buffer): boolean => {
    const target = split(req.url);
    return target === undefined ? false : voice.handleUpgrade(req, socket, head, target.path, target.query);
  };

  return { middleware, handleUpgrade, close: voice.close };
};
