import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// The workspace packages resolve to their source, so the suite needs no build of sdk/typescript or the adapter first.
const source = (path: string): string => fileURLToPath(new URL(path, import.meta.url));

export default defineConfig({
  resolve: {
    alias: [
      { find: "@alixai/sdk", replacement: source("../../sdk/typescript/src/index.ts") },
      { find: /^@alixai\/voice-orb$/, replacement: source("../voice-orb/src/index.ts") },
      { find: /^@alixai\/copilotkit-adapter\/react$/, replacement: source("../../sdk/copilotkit-adapter/src/react/index.ts") },
      { find: /^@alixai\/copilotkit-adapter$/, replacement: source("../../sdk/copilotkit-adapter/src/index.ts") },
    ],
  },
  test: {
    include: ["test/**/*.test.{ts,tsx}"],
    environment: "node",
    // The network guard is loaded before any CopilotKit module: it refuses every connection that is not loopback.
    setupFiles: ["./test/setup/network-guard.ts", "./test/setup/jsdom.ts"],
    // CopilotKit's v2 entry imports its stylesheet from JavaScript: Vite has to process that file, not Node.
    server: { deps: { inline: [/@copilotkit\/(?!license-verifier)/] } },
    testTimeout: 30000,
    hookTimeout: 30000,
  },
});
