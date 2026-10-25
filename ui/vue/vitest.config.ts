import { fileURLToPath } from "node:url";
import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vitest/config";

// @alixai/sdk and @alixai/voice-orb resolve to their source so the suite needs no build first (their package.json points at dist/).
// jsdom has no canvas: test/setup.ts makes getContext answer null quietly, and the orb tests hand it fake contexts.
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      "@alixai/sdk": fileURLToPath(new URL("../../sdk/typescript/src/index.ts", import.meta.url)),
      "@alixai/voice-orb": fileURLToPath(new URL("../voice-orb/src/index.ts", import.meta.url)),
    },
  },
  test: {
    include: ["test/**/*.test.ts"],
    environment: "jsdom",
    setupFiles: ["./test/setup.ts"],
    testTimeout: 20000,
  },
});
