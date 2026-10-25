import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// The gate imports the adapter and the SDK the way a host does, by package name, and resolves them to their sources so
// that no build has to run first (the adapter's own suite does the same for @alixai/sdk; both packages point at dist/
// in their package.json).
const source = (path: string): string => fileURLToPath(new URL(`../../${path}`, import.meta.url));

export default defineConfig({
  resolve: {
    alias: [
      { find: "@alixai/sdk", replacement: source("sdk/typescript/src/index.ts") },
      { find: /^@alixai\/copilotkit-adapter\/runtime$/, replacement: source("sdk/copilotkit-adapter/src/runtime.ts") },
      { find: /^@alixai\/copilotkit-adapter\/react$/, replacement: source("sdk/copilotkit-adapter/src/react/index.ts") },
      { find: /^@alixai\/copilotkit-adapter$/, replacement: source("sdk/copilotkit-adapter/src/index.ts") },
    ],
  },
  test: {
    include: ["test/**/*.test.{ts,tsx}"],
    environment: "node",
    // The network guard is loaded before any CopilotKit module: every connection that is not loopback fails the test.
    setupFiles: ["./test/setup.ts"],
    // CopilotKit's v2 entry imports its stylesheet from JavaScript: Vite has to process that file, not Node.
    // (Not the license verifier: it references a source map it does not ship, and Node can load it as it is.)
    server: { deps: { inline: [/@copilotkit\/(?!license-verifier)/] } },
    testTimeout: 30000,
    hookTimeout: 30000,
  },
});
