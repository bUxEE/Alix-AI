// @alixai/copilotkit-adapter: the optional CopilotKit and AG-UI integration of AlixAI (master plan 15.6, ADR 0035).
// This entry has no React and no CopilotKit import; it needs @ag-ui/client and rxjs. The CopilotRuntime helpers are in
// `@alixai/copilotkit-adapter/runtime`, the React helpers in `@alixai/copilotkit-adapter/react`.
export { AlixAIAgent, CONFIRM_TOOL_CALL_PREFIX, CONFIRM_TOOL_NAME, answerOf, isApprovalPayload, requestBodyFor } from "./agent.js";
export { createMemorySecretStore } from "./secrets.js";
export { COPILOTKIT_PROVIDER_PROPS, COPILOTKIT_TELEMETRY_ENV, applyTelemetryOff, telemetryIsOff, withoutCloud } from "./telemetry.js";
export * from "./activities.js";
//# sourceMappingURL=index.js.map