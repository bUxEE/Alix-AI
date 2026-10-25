import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import { alixDemoMock } from "../../demo-mock/src/plugin";

// The demo of @alixai/react: `pnpm dev` in ui/react. The page and the scripted mock backend (ui/demo-mock) share one origin, so
// the kit talks to `/v1/agui`, `/v1/files`, the settings routes and the voice socket exactly as it would to a host's gateway.
// The workspace packages resolve to their source (as in vitest.config.ts): nothing has to be built first. The React plugin
// compiles the TSX with the automatic runtime and hot-swaps edited components (Fast Refresh).

const source = (path: string): string => fileURLToPath(new URL(path, import.meta.url));

export default defineConfig({
  root: source("."),
  clearScreen: false,
  plugins: [react(), alixDemoMock()],
  resolve: {
    alias: [
      { find: "@alixai/sdk", replacement: source("../../../sdk/typescript/src/index.ts") },
      { find: /^@alixai\/voice-orb$/, replacement: source("../../voice-orb/src/index.ts") },
      { find: /^@alixai\/copilotkit-adapter\/react$/, replacement: source("../../../sdk/copilotkit-adapter/src/react/index.ts") },
      { find: /^@alixai\/copilotkit-adapter$/, replacement: source("../../../sdk/copilotkit-adapter/src/index.ts") },
      // The demo imports the kit by its package name (from source) and the mock's browser helpers.
      { find: /^@alixai\/react\/copilotkit$/, replacement: source("../src/copilotkit.ts") },
      { find: /^@alixai\/react$/, replacement: source("../src/index.ts") },
      { find: /^@alixai\/demo-mock$/, replacement: source("../../demo-mock/src/index.ts") },
    ],
  },
  server: { host: "127.0.0.1", port: 5174 },
  preview: { host: "127.0.0.1", port: 5174 },
  build: { outDir: source("../dist-demo"), emptyOutDir: true, chunkSizeWarningLimit: 4096 },
});
