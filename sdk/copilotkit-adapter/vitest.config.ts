import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// @alixai/sdk resolves to its source here so the suite needs no build of sdk/typescript first (its package.json
// points at dist/). The published adapter resolves it through node_modules like any other dependency.
export default defineConfig({
  resolve: {
    alias: {
      "@alixai/sdk": fileURLToPath(new URL("../typescript/src/index.ts", import.meta.url)),
    },
  },
  test: {
    include: ["test/**/*.test.{ts,tsx}"],
    environment: "node",
    // The network guard is loaded before any CopilotKit module: it refuses every connection that is not loopback.
    setupFiles: ["./test/setup/network-guard.ts", "./test/setup/jsdom.ts"],
    // CopilotKit's v2 entry imports its stylesheet from JavaScript: Vite has to process that file, not Node.
    // (Not the license verifier: it references a source map it does not ship, and Node can load it as it is.)
    server: { deps: { inline: [/@copilotkit\/(?!license-verifier)/] } },
    testTimeout: 30000,
    hookTimeout: 30000,
  },
});
