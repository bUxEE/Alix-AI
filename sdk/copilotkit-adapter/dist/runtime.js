// This import comes first on purpose: it sets the telemetry switches of @copilotkit/runtime, which the runtime reads when
// it loads (see telemetry.ts).
import "./telemetry-off.js";
import { CopilotSseRuntime, createCopilotRuntimeHandler } from "@copilotkit/runtime/v2";
import { createCopilotNodeHandler } from "@copilotkit/runtime/v2/node";
import { AlixAIAgent } from "./agent.js";
import { AlixAIRunner } from "./runner.js";
/**
 * Builds the CopilotRuntime with one AlixAIAgent and the AlixAIRunner. The runtime clones the agent per request; the clones
 * share its secret store. Authenticate the whole route in front of the handler as well: the runtime's `info` and `stop` routes
 * never call the token callback (docs/integration/copilotkit-compatibility.md).
 */
export const createAlixAIRuntime = (options) => {
    const { agentId = "default", runtime: runtimeOptions, ...agentConfig } = options;
    const agent = new AlixAIAgent(agentConfig);
    const runner = new AlixAIRunner(agent);
    const runtime = new CopilotSseRuntime({ ...runtimeOptions, agents: { [agentId]: agent }, runner });
    return { runtime, agent, agentId, runner };
};
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
export const createAlixAIFetchHandler = (alixai, options = {}) => createCopilotRuntimeHandler({
    runtime: alixai.runtime,
    ...(options.basePath !== undefined && { basePath: options.basePath }),
    cors: false,
    activateChannels: false,
});
/** The same handler for `node:http` (`http.createServer(createAlixAINodeHandler(alixai, { basePath }))`). */
export const createAlixAINodeHandler = (alixai, options = {}) => createCopilotNodeHandler(createAlixAIFetchHandler(alixai, options));
export { AlixAIAgent } from "./agent.js";
export { AlixAIRunner } from "./runner.js";
//# sourceMappingURL=runtime.js.map