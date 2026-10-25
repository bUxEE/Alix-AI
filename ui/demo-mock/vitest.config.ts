import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// The workspace packages resolve to their source, so the suite needs no build of sdk/typescript or the adapter first. The kits'
// own parsers are imported from their source too (ui/vue/src/agui), so the mock is checked by the code that reads it in the apps.
const source = (path: string): string => fileURLToPath(new URL(path, import.meta.url));

export default defineConfig({
  resolve: {
    alias: [
      { find: "@alixai/sdk", replacement: source("../../sdk/typescript/src/index.ts") },
      { find: /^@alixai\/copilotkit-adapter$/, replacement: source("../../sdk/copilotkit-adapter/src/index.ts") },
    ],
  },
  test: {
    include: ["test/**/*.test.ts"],
    environment: "node",
    testTimeout: 30000,
    hookTimeout: 30000,
  },
});
