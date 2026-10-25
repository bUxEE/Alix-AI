import type { voice } from "@alixai/sdk";
import { DEMO_PATHS } from "./paths";

// @alixai/demo-mock, browser side: the prompt registry, the paths and a few helpers a demo page uses to talk to the mock. Nothing
// here imports Node. The Vite plugin itself is in `./plugin` (vite.config.ts imports that one).

export { demoPrompt, demoPrompts, detectLocale, matchScenario } from "./prompts";
export type { DemoLocale, DemoPrompt, DemoScenarioId, DemoText, ScenarioMatch } from "./prompts";
export { DEMO_PATHS, DEMO_THREAD_ID, voicePath } from "./paths";

/** The context token the demo hands to the SDK's voice client. The mock does not check tokens; a real host mints one per connection. */
export const demoContextToken = (): string => "demo-context-token";

/** The voice socket factory for a browser. A browser WebSocket cannot send `Authorization`; the mock does not ask for it. */
export const demoVoiceSocket: voice.VoiceSocketFactory = (url) => new WebSocket(url) as unknown as voice.VoiceSocketLike;

/** The options of `voice.createVoiceSession` for the mock, apart from the mode. */
export const demoVoiceOptions = (sessionId: string, options: { base?: string; language?: string } = {}): voice.VoiceOptions => ({
  baseUrl: options.base ?? globalThis.location.origin,
  sessionId,
  getContextToken: demoContextToken,
  webSocket: demoVoiceSocket,
  ...(options.language !== undefined && { language: options.language }),
});

const errorMessage = async (response: Response): Promise<string> => {
  try {
    const body = (await response.json()) as { message?: unknown };
    if (typeof body.message === "string") return body.message;
  } catch {
    // not JSON
  }
  return `the mock answered ${response.status}`;
};

/** The kits' `uploader`: posts one file to `POST /v1/files` and returns its id. Rejects with the mock's message on a refusal. */
export const demoUploader =
  (base = ""): ((file: File, signal: AbortSignal) => Promise<{ fileId: string }>) =>
  async (file, signal) => {
    const form = new FormData();
    form.append("file", file, file.name);
    const response = await fetch(`${base}${DEMO_PATHS.files}`, { method: "POST", body: form, signal });
    if (!response.ok) throw new Error(await errorMessage(response));
    const ref = (await response.json()) as { file_id: string };
    return { fileId: ref.file_id };
  };

export type DemoPreview =
  | { readonly kind: "image"; readonly url: string }
  | { readonly kind: "text"; readonly text: string; readonly truncated: boolean }
  | { readonly kind: "none"; readonly reason: string };

/**
 * Reads a file's safe preview (`GET /v1/files/{id}/preview`): an image becomes a `blob:` URL (the only kind of address the kits'
 * file previews accept besides https), text comes back as text. Revoke the URL when the preview goes away.
 */
export const fetchDemoPreview = async (fileId: string, base = "", signal?: AbortSignal): Promise<DemoPreview> => {
  const response = await fetch(`${base}${DEMO_PATHS.files}/${encodeURIComponent(fileId)}/preview`, signal === undefined ? {} : { signal });
  if (!response.ok) return { kind: "none", reason: await errorMessage(response) };
  const type = response.headers.get("Content-Type") ?? "";
  if (type.startsWith("image/")) return { kind: "image", url: URL.createObjectURL(await response.blob()) };
  return { kind: "text", text: await response.text(), truncated: response.headers.get("X-Alixai-Preview-Truncated") === "true" };
};

/** `GET` one of the mock's JSON routes (`DEMO_PATHS.settingsEffective`, `DEMO_PATHS.ragSources`, ...). */
export const fetchDemoJson = async <T>(path: string, base = "", init: RequestInit = {}): Promise<T> => {
  const response = await fetch(`${base}${path}`, init);
  if (!response.ok) throw new Error(await errorMessage(response));
  return (await response.json()) as T;
};
