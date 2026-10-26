// Where the mock answers, relative to the dev server's origin (plus the plugin's `basePath`, empty by default). The `/v1/...` routes
// are the public API's own paths and shapes (schemas/api); the `/demo/...` routes are the mock's additions for things the public
// API leaves to the host (a conversation list, a file list) or that only the demo needs (a CopilotKit runtime stub).

export const DEMO_PATHS = Object.freeze({
  /** AG-UI over SSE: `POST` a RunAgentInput (the Vue kit's `createAguiClient({ endpoint })`, the React kit's `HttpAgent({ url })`). */
  agui: "/v1/agui",
  aguiCapabilities: "/v1/agui/capabilities",
  /** `POST` multipart (`file`, optional `treatment`); `GET|DELETE /v1/files/{id}`, `/content`, `/preview`. */
  files: "/v1/files",
  settingsUser: "/v1/settings/user",
  settingsEffective: "/v1/settings/effective",
  settingsProfiles: "/v1/settings/profiles",
  settingsCapabilities: "/v1/settings/capabilities",
  integrationAudit: "/v1/settings/integration-audit",
  providerHealth: "/v1/settings/providers",
  ragSources: "/v1/rag/sources",
  ragReaudit: "/v1/rag/sources/reaudit",
  healthz: "/healthz",
  readyz: "/readyz",
  /** Mock only: the conversations to switch between (`GET`), a new one (`POST`), forget one (`DELETE /demo/conversations/{id}`). */
  conversations: "/demo/conversations",
  /** Mock only: every file the mock holds, as FileRefs. */
  fileList: "/demo/files",
  /** Mock only: the `runtimeUrl` for CopilotKit. It answers `/info` with no agents, so the page's self-managed HttpAgent is used. */
  copilotRuntime: "/demo/copilotkit",
  /** Dictation capabilities: `GET /v1/dictations/capabilities`. */
  dictationCapabilities: "/v1/dictations/capabilities",
} as const);

/** The voice WebSocket of a session: `GET /v1/sessions/{session_id}/voice` with `Upgrade: websocket`. */
export const voicePath = (sessionId: string): string => `/v1/sessions/${encodeURIComponent(sessionId)}/voice`;

/** One-shot dictation: `POST /v1/sessions/{session_id}/dictations` with raw `audio/wav`. */
export const dictationPath = (sessionId: string): string => `/v1/sessions/${encodeURIComponent(sessionId)}/dictations`;

/** The thread the demo opens first: it has a short history, so the panel shows something on its first connect. */
export const DEMO_THREAD_ID = "ses_demo_welcome";
