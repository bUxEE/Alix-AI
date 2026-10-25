// This import comes first on purpose: it sets the telemetry switches of @copilotkit/runtime, which the runtime reads when
// it loads (see telemetry.ts).
import "./telemetry-off.js";
import { CopilotSseRuntime, createCopilotRuntimeHandler } from "@copilotkit/runtime/v2";
import { createCopilotNodeHandler } from "@copilotkit/runtime/v2/node";
import type { CopilotRuntimeFetchHandler, CopilotSseRuntimeOptions } from "@copilotkit/runtime/v2";
import { AlixAIAgent } from "./agent.js";
import type { AlixAIAgentConfig } from "./agent.js";
import { AlixAIRunner } from "./runner.js";

// The CopilotRuntime side of the adapter, for the host's backend: a Next.js App Router route, an Express route or a plain
// Node server. The runtime serves CopilotKit's browser client; AlixAIAgent talks to the AlixAI gateway with a context
// token the host mints. Defaults chosen for a local deployment: no CORS headers (the page and the route share an origin),
// no managed channels, no license or telemetry identity, and the Intelligence runtime is not used.

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
export const createAlixAIRuntime = (options: AlixAIRuntimeOptions): AlixAIRuntime => {
  const { agentId = "default", runtime: runtimeOptions, ...agentConfig } = options;
  const agent = new AlixAIAgent(agentConfig);
  const runner = new AlixAIRunner(agent);
  const runtime = new CopilotSseRuntime({ ...runtimeOptions, agents: { [agentId]: agent }, runner });
  return { runtime, agent, agentId, runner };
};

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
export const createAlixAIFetchHandler = (alixai: AlixAIRuntime, options: AlixAIHandlerOptions = {}): CopilotRuntimeFetchHandler =>
  createCopilotRuntimeHandler({
    runtime: alixai.runtime,
    ...(options.basePath !== undefined && { basePath: options.basePath }),
    cors: false,
    activateChannels: false,
  });

/** The same handler for `node:http` (`http.createServer(createAlixAINodeHandler(alixai, { basePath }))`). */
export const createAlixAINodeHandler = (alixai: AlixAIRuntime, options: AlixAIHandlerOptions = {}): ReturnType<typeof createCopilotNodeHandler> =>
  createCopilotNodeHandler(createAlixAIFetchHandler(alixai, options));

export { AlixAIAgent } from "./agent.js";
export type { AlixAIAgentConfig } from "./agent.js";
export { AlixAIRunner } from "./runner.js";
