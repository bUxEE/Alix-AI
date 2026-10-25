import { fileURLToPath } from "node:url";
import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vite";
import { alixDemoMock } from "../demo-mock/src/plugin";

// The demo of the kit (demo/), served by `pnpm dev` with the shared mock backend (ui/demo-mock) on the same origin. Nothing here
// is part of the published package. The kit and the SDK resolve to their source, as a host's bundler would see them. The test
// suite has its own vitest.config.ts.
const at = (path: string): string => fileURLToPath(new URL(path, import.meta.url));

export default defineConfig({
  root: at("./demo"),
  plugins: [vue(), alixDemoMock()],
  resolve: {
    alias: [
      { find: /^@alixai\/vue\/styles$/, replacement: at("./src/styles/index.scss") },
      { find: /^@alixai\/vue$/, replacement: at("./src/index.ts") },
      { find: /^@alixai\/sdk$/, replacement: at("../../sdk/typescript/src/index.ts") },
      { find: /^@alixai\/voice-orb$/, replacement: at("../voice-orb/src/index.ts") },
    ],
  },
  server: { port: 5173 },
  build: { outDir: at("./demo-dist"), emptyOutDir: true },
});
