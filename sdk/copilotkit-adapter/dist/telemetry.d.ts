export declare const COPILOTKIT_TELEMETRY_ENV: Readonly<Record<string, string>>;
/**
 * Sets every telemetry switch in `env` (the process environment by default), overriding a value that says otherwise.
 * Returns the switches it set.
 */
export declare const applyTelemetryOff: (env?: Record<string, string | undefined>) => Record<string, string>;
/** True when the environment keeps the runtime's telemetry off (the two switches the runtime reads). */
export declare const telemetryIsOff: (env?: Record<string, string | undefined>) => boolean;
/** The provider props that turn off what could contact a CopilotKit service from the browser. */
export declare const COPILOTKIT_PROVIDER_PROPS: Readonly<{
    readonly enableInspector: false;
    readonly showDevConsole: false;
    readonly showIntelligenceIndicator: false;
}>;
declare const CLOUD_KEYS: readonly ["publicApiKey", "publicLicenseKey", "licenseToken"];
export type WithoutCloud<T> = Omit<T, (typeof CLOUD_KEYS)[number]>;
/**
 * Props for `<CopilotKit>` or `<CopilotKitProvider>` with the cloud keys removed and the inspector, dev console and
 * Intelligence indicator off. Spread the result: `<CopilotKit {...withoutCloud({ runtimeUrl, threadId })}>`.
 */
export declare const withoutCloud: <T extends object>(props: T) => WithoutCloud<T> & typeof COPILOTKIT_PROVIDER_PROPS;
export {};
//# sourceMappingURL=telemetry.d.ts.map