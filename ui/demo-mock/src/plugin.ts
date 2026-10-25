import type { IncomingMessage, Server as HttpServer } from "node:http";
import type { Http2SecureServer } from "node:http2";
import type { Duplex } from "node:stream";
import type { Plugin } from "vite";
import { createDemoMock, type DemoMock, type DemoMockOptions } from "./server";

// @alixai/demo-mock, Node side: the Vite plugin. In vite.config.ts of a demo app:
//
//   import { alixDemoMock } from "../demo-mock/src/plugin";
//   export default defineConfig({ plugins: [vue(), alixDemoMock()] });
//
// It mounts the mock's routes in front of Vite's own middlewares (so `/v1/...` never falls through to the page) and takes the
// WebSocket upgrades of the voice sockets on the dev server; every other upgrade, Vite's HMR among them, is left alone.

export { createDemoMock } from "./server";
export type { DemoMock, DemoMockOptions } from "./server";
export * from "./index";

type UpgradeServer = HttpServer | Http2SecureServer;

const attach = (mock: DemoMock, middlewares: { use: (fn: DemoMock["middleware"]) => unknown }, httpServer: UpgradeServer | null): void => {
  middlewares.use(mock.middleware);
  httpServer?.on("upgrade", (req: IncomingMessage, socket: Duplex, head: Buffer) => {
    mock.handleUpgrade(req, socket, head);
  });
  httpServer?.on("close", () => mock.close());
};

/** The demo backend as a Vite plugin: the dev server (and `vite preview`) answer the AlixAI routes the kits call. */
export const alixDemoMock = (options: DemoMockOptions = {}): Plugin => ({
  name: "alixai-demo-mock",
  configureServer: (server) => {
    attach(createDemoMock(options), server.middlewares, server.httpServer);
  },
  configurePreviewServer: (server) => {
    attach(createDemoMock(options), server.middlewares, server.httpServer);
  },
});
