import { readdirSync, readFileSync, statSync } from "node:fs";
import type { AddressInfo } from "node:net";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020.js";
import addFormatsImport from "ajv-formats";
import { createServer } from "vite";
import { createAguiClient, type RunResult } from "../../vue/src/agui/client";
import { applyEvent, createChatState } from "../../vue/src/agui/reducer";
import type { AguiEvent, ChatItem, ChatState, RunAgentInput } from "../../vue/src/agui/types";
import { alixDemoMock, type DemoMockOptions } from "../src/plugin";

// The suite runs the mock the way the demo apps do: a real Vite dev server with the plugin, on a loopback port. The kits' own
// clients and parsers read what it sends, and the repository's schemas check every document, so the mock cannot drift from the
// formats the kits and the gateway use.

export const REPO_ROOT = fileURLToPath(new URL("../../../", import.meta.url));

export interface RunningMock {
  readonly base: string;
  readonly close: () => Promise<void>;
}

export const startMock = async (options: DemoMockOptions = { pace: 0 }): Promise<RunningMock> => {
  options = { voice: "silent", ...options };
  const server = await createServer({
    configFile: false,
    root: fileURLToPath(new URL("..", import.meta.url)),
    logLevel: "silent",
    appType: "custom",
    server: { host: "127.0.0.1", port: 0, strictPort: false, watch: null },
    optimizeDeps: { noDiscovery: true, include: [] },
    plugins: [alixDemoMock(options)],
  });
  await server.listen();
  const address = server.httpServer?.address() as AddressInfo;
  return { base: `http://127.0.0.1:${address.port}`, close: () => server.close() };
};

// ---- schemas ----

const addFormats = ((addFormatsImport as unknown as { default?: unknown }).default ?? addFormatsImport) as (ajv: Ajv2020) => void;

const ajv = (() => {
  const instance = new Ajv2020({ strict: false, allErrors: true });
  addFormats(instance);
  const root = join(REPO_ROOT, "schemas");
  for (const area of readdirSync(root)) {
    const dir = join(root, area);
    if (area === "api" || !statSync(dir).isDirectory()) continue;
    for (const name of readdirSync(dir)) {
      if (name.endsWith(".schema.json")) instance.addSchema(JSON.parse(readFileSync(join(dir, name), "utf8")) as object);
    }
  }
  return instance;
})();

/** Throws with the validator's errors unless `value` is valid against the schema with this `$id`. */
export const expectSchema = (id: string, value: unknown): void => {
  const validate = ajv.getSchema(id);
  if (validate === undefined) throw new Error(`schema ${id} is not loaded`);
  if (!validate(value)) throw new Error(`${id}: ${JSON.stringify(validate.errors?.slice(0, 5), null, 1)}\n${JSON.stringify(value).slice(0, 600)}`);
};

export const SCHEMA = Object.freeze({
  richBlock: "urn:alixai:schema:conversation:rich-block:v1",
  citation: "urn:alixai:schema:conversation:citation:v1",
  directive: "urn:alixai:schema:conversation:dialogue-directive:v1",
  fileRef: "urn:alixai:schema:conversation:file-ref:v1",
  receipt: "urn:alixai:schema:actions:action-receipt:v1",
  error: "urn:alixai:schema:common:error:v1",
  serverEvent: "urn:alixai:schema:voice:server-event:v1",
});

// ---- the Vue kit's client and reducer ----

export interface VueRun {
  readonly events: AguiEvent[];
  readonly cursors: (string | undefined)[];
  readonly state: ChatState;
  readonly result: RunResult;
  readonly reconnects: number;
}

let sequence = 0;
export const uid = (prefix: string): string => {
  sequence += 1;
  return `${prefix}_${Date.now().toString(36)}${sequence}`;
};

/** One run through the Vue kit's own AG-UI client (`createAguiClient`), folded by its own reducer. */
export const runVue = async (base: string, input: Partial<RunAgentInput> & { threadId: string }, state: ChatState = createChatState()): Promise<VueRun> => {
  const client = createAguiClient({ endpoint: `${base}/v1/agui`, sleep: async () => undefined });
  const events: AguiEvent[] = [];
  const cursors: (string | undefined)[] = [];
  let reconnects = 0;
  const result = await client.run(
    { runId: uid("run"), messages: [], ...input },
    {
      onEvent: (event, cursor) => {
        events.push(event);
        cursors.push(cursor);
        applyEvent(state, event);
      },
      onReconnecting: () => {
        reconnects += 1;
      },
    },
  );
  return { events, cursors, state, result, reconnects };
};

export const userMessage = (text: string, files: readonly { fileId: string; kind: "image" | "document" }[] = []): RunAgentInput["messages"][number] => ({
  id: uid("msg"),
  role: "user",
  content: files.length === 0 ? text : [{ type: "text", text }, ...files.map((file) => ({ type: file.kind, source: { type: "file" as const, value: file.fileId } }))],
});

/** The timeline as kinds: `message`, `block:chart`, `citations`, `confirmation` ... */
export const kindsOf = (items: readonly ChatItem[]): string[] => items.map((item) => (item.kind === "block" ? `block:${item.blockType}` : item.kind));
