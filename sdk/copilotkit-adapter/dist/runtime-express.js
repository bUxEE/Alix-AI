import "./telemetry-off.js";
import { createCopilotEndpointExpress } from "@copilotkit/runtime/v2/express";
/**
 * An Express router for the runtime:
 *
 * ```ts
 * app.use(createAlixAIExpressRouter(createAlixAIRuntime({ baseUrl, getContextToken }), { basePath: "/copilotkit" }));
 * ```
 *
 * CORS is off by default (the runtime's own default is `*`): the page and the route share an origin, and a different origin is
 * the host's decision, with its own `cors` option.
 */
export const createAlixAIExpressRouter = (alixai, options) => createCopilotEndpointExpress({ runtime: alixai.runtime, basePath: options.basePath, cors: options.cors ?? false, activateChannels: false });
//# sourceMappingURL=runtime-express.js.map