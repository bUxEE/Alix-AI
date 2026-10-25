// Keeping CopilotKit's own telemetry off (ADR 0035: a local AlixAI deployment needs no CopilotKit cloud and sends no
// telemetry). The names below were read from the pinned releases, not from memory; compat/matrix.json records which
// versions they were verified against and test/telemetry.test.ts fails on any non-loopback connection.
//
//   COPILOTKIT_TELEMETRY_DISABLED=true|1   @copilotkit/runtime, v2/runtime/telemetry/telemetry-client and
//                                          lib/telemetry-disclosure (the runtime's `oss.runtime.*` events go to
//                                          telemetry.copilotkit.ai unless this or DO_NOT_TRACK is set)
//   DO_NOT_TRACK=true|1                    the same runtime check, and @scarf/scarf
//   SCARF_ANALYTICS=false, SCARF_NO_ANALYTICS=true   @scarf/scarf, the install-time analytics package that
//                                          @copilotkit/react-core, react-ui and runtime depend on
//
// The runtime reads the first two ONCE, when its telemetry module is loaded, so they must be set before
// `@copilotkit/runtime` is imported: `@alixai/copilotkit-adapter/runtime` imports `./telemetry-off` first for that reason. A
// host that imports `@copilotkit/runtime` itself before the adapter sets them in its process environment (the container,
// the systemd unit or `.env`) instead.
//
// The browser side: CopilotKit's provider takes no telemetry switch of its own; what contacts CopilotKit services is a
// public API key, a public license key or a license token, and the inspector in development builds. `withoutCloud`
// removes the keys and turns the inspector, the dev console and the Intelligence indicator off.

export const COPILOTKIT_TELEMETRY_ENV: Readonly<Record<string, string>> = Object.freeze({
  COPILOTKIT_TELEMETRY_DISABLED: "true",
  DO_NOT_TRACK: "1",
  SCARF_ANALYTICS: "false",
  SCARF_NO_ANALYTICS: "true",
});

/**
 * Sets every telemetry switch in `env` (the process environment by default), overriding a value that says otherwise.
 * Returns the switches it set.
 */
export const applyTelemetryOff = (env: Record<string, string | undefined> = process.env): Record<string, string> => {
  const applied: Record<string, string> = {};
  for (const [name, value] of Object.entries(COPILOTKIT_TELEMETRY_ENV)) {
    env[name] = value;
    applied[name] = value;
  }
  return applied;
};

/** True when the environment keeps the runtime's telemetry off (the two switches the runtime reads). */
export const telemetryIsOff = (env: Record<string, string | undefined> = process.env): boolean =>
  env["COPILOTKIT_TELEMETRY_DISABLED"] === "true" ||
  env["COPILOTKIT_TELEMETRY_DISABLED"] === "1" ||
  env["DO_NOT_TRACK"] === "true" ||
  env["DO_NOT_TRACK"] === "1";

/** The provider props that turn off what could contact a CopilotKit service from the browser. */
export const COPILOTKIT_PROVIDER_PROPS = Object.freeze({
  enableInspector: false,
  showDevConsole: false,
  showIntelligenceIndicator: false,
} as const);

const CLOUD_KEYS = ["publicApiKey", "publicLicenseKey", "licenseToken"] as const;

export type WithoutCloud<T> = Omit<T, (typeof CLOUD_KEYS)[number]>;

/**
 * Props for `<CopilotKit>` or `<CopilotKitProvider>` with the cloud keys removed and the inspector, dev console and
 * Intelligence indicator off. Spread the result: `<CopilotKit {...withoutCloud({ runtimeUrl, threadId })}>`.
 */
export const withoutCloud = <T extends object>(props: T): WithoutCloud<T> & typeof COPILOTKIT_PROVIDER_PROPS => {
  const copy = { ...props } as Record<string, unknown>;
  for (const key of CLOUD_KEYS) delete copy[key];
  return { ...copy, ...COPILOTKIT_PROVIDER_PROPS } as WithoutCloud<T> & typeof COPILOTKIT_PROVIDER_PROPS;
};
