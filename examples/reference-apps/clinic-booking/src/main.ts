import { createApp } from "./app.ts";

// Starts the application from the environment:
//
//   CLINIC_PORT          port (default 8787; 0 picks a free one)
//   CLINIC_HOST          loopback address (default 127.0.0.1; anything else is refused)
//   CLINIC_DB_PATH       SQLite file (default in memory)
//   CLINIC_MCP_BEARER    shared credential AlixAI must present (default none)
//   CLINIC_TEST_SUPPORT  1 mounts disposable tenants, counters, faults and the
//                        context-token endpoint (uses the TEST-ONLY dev key)
//   CLINIC_CTX_KEY_FILE  path of the dev key (default: found under tools/devkeys)
//   CLINIC_DEMO_SEED     1 creates the synthetic demonstration tenant

const env = process.env;

const app = createApp({
  port: env.CLINIC_PORT === undefined ? 8787 : Number(env.CLINIC_PORT),
  ...(env.CLINIC_HOST === undefined ? {} : { host: env.CLINIC_HOST }),
  ...(env.CLINIC_DB_PATH === undefined ? {} : { dbPath: env.CLINIC_DB_PATH }),
  ...(env.CLINIC_MCP_BEARER === undefined || env.CLINIC_MCP_BEARER === "" ? {} : { mcpBearer: env.CLINIC_MCP_BEARER }),
  ...(env.CLINIC_CTX_KEY_FILE === undefined ? {} : { tokenKeyFile: env.CLINIC_CTX_KEY_FILE }),
  testSupport: env.CLINIC_TEST_SUPPORT === "1",
  demoSeed: env.CLINIC_DEMO_SEED === "1",
});

const { host, port, url } = await app.listen();
// One line on stdout, so a parent process that asked for port 0 can learn it.
process.stdout.write(`${JSON.stringify({ event: "listening", host, port, url, mcp: `${url}/mcp` })}\n`);

const stop = (): void => {
  void app.close().then(() => process.exit(0));
};
process.on("SIGINT", stop);
process.on("SIGTERM", stop);
