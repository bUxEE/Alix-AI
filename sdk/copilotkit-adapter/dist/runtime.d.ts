import "./telemetry-off.js";
import { CopilotSseRuntime } from "@copilotkit/runtime/v2";
import { createCopilotNodeHandler } from "@copilotkit/runtime/v2/node";
import type { CopilotRuntimeFetchHandler, CopilotSseRuntimeOptions } from "@copilotkit/runtime/v2";
import { AlixAIAgent } from "./agent.js";
import type { AlixAIAgentConfig } from "./agent.js";
import { AlixAIRunner } from "./runner.js";
export interface AlixAIRuntimeOptions extends AlixAIAgentConfig {
    /** The agent id the browser names (`<CopilotKit agent="...">`). Default `default`. */
    agentId?: string;
    /**
     * Further CopilotRuntime options. `agents`, `runner`, `licenseToken` and `telemetryId` are the adapter's to set and are not
     * accepted: the runner is AlixAIRunner, so `connect` goes through the agent (and the host's token callback and the gateway)
     * and CopilotRuntime's in-memory thread routes are off. A runner of the host's own would silently bring them back.
     */
    runtime?: Omit<CopilotSseRuntimeOptions, "agents" | "runner" | "licenseToken" | "telemetryId" | "intelligence" | "channels" | "generateThreadNames">;
}
export interface AlixAIRuntime {
    readonly runtime: CopilotSseRuntime;
    readonly agent: AlixAIAgent;
    readonly agentId: string;
    readonly runner: AlixAIRunner;
}
/**
 * Builds the CopilotRuntime with one AlixAIAgent and the AlixAIRunner. The runtime clones the agent per request; the clones
 * share its secret store. Authenticate the whole route in front of the handler as well: the runtime's `info` and `stop` routes
 * never call the token callback (docs/integration/copilotkit-compatibility.md).
 */
export declare const createAlixAIRuntime: (options: AlixAIRuntimeOptions) => AlixAIRuntime;
export interface AlixAIHandlerOptions {
    /** The path the route is mounted at, for example `/api/copilotkit`. Omit to match the runtime's routes as a suffix. */
    basePath?: string;
}
/**
 * A `(Request) => Promise<Response>` handler for a Next.js App Router route:
 *
 * ```ts
 * // app/api/copilotkit/[[...path]]/route.ts
 * const handler = createAlixAIFetchHandler(createAlixAIRuntime({ baseUrl, getContextToken }), { basePath: "/api/copilotkit" });
 * export const GET = handler;
 * export const POST = handler;
 * ```
 */
export declare const createAlixAIFetchHandler: (alixai: AlixAIRuntime, options?: AlixAIHandlerOptions) => CopilotRuntimeFetchHandler;
/** The same handler for `node:http` (`http.createServer(createAlixAINodeHandler(alixai, { basePath }))`). */
export declare const createAlixAINodeHandler: (alixai: AlixAIRuntime, options?: AlixAIHandlerOptions) => ReturnType<typeof createCopilotNodeHandler>;
export { AlixAIAgent } from "./agent.js";
export type { AlixAIAgentConfig } from "./agent.js";
export { AlixAIRunner } from "./runner.js";
//# sourceMappingURL=runtime.d.ts.map