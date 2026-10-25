import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import type { IncomingMessage, Server } from "node:http";

import Router from "@koa/router";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import Koa from "koa";

import { APPLICATION_ID, roleCapabilities, type Role } from "./access.ts";
import { createOffsetClock, systemClock, type Clock, type OffsetClock } from "./clock.ts";
import {
  createTestIssuer,
  MAX_LIFETIME_SECONDS,
  mintContextToken,
  TEST_ONLY_KEY_ID,
  TokenRefused,
  type TokenIssuer,
} from "./context-token.ts";
import { openDatabase, type Database } from "./db.ts";
import { createClinicService, type ClinicService } from "./domain/service.ts";
import { MAX_BODY_BYTES } from "./domain/rules.ts";
import { isRefusal, refuse } from "./errors.ts";
import { APPLICATION_VERSION, inspect, parseAspects, type RuntimeInfo } from "./inspect.ts";
import { createInstrumentation, type FaultMode, type Instrumentation } from "./instrumentation.ts";
import { createMcpServer, type McpContext } from "./mcp.ts";
import { rejectedResult } from "./results.ts";
import { seedDemoTenant } from "./demo.ts";
import { databaseTotals } from "./testsupport.ts";

// The application: SQLite, the domain service, the MCP server and the HTTP
// surface, composed. It listens on the loopback interface only.

export type AppOptions = {
  // ":memory:" (the default) or a file path. A file is created owner-only.
  dbPath?: string;
  host?: string;
  // 0 asks the system for a free port.
  port?: number;
  clock?: Clock;
  // The shared credential AlixAI presents (Authorization: Bearer). Absent means
  // no credential is required, which is acceptable only because the socket is
  // loopback-only.
  mcpBearer?: string;
  // Mounts disposable-tenant provisioning, counters, fault injection and the
  // context-token endpoint. Off unless asked: the TEST-ONLY key is never
  // loaded otherwise.
  testSupport?: boolean;
  tokenKeyFile?: string;
  tokenIssuer?: string;
  tokenAudience?: string;
  // Creates a synthetic demonstration tenant at start.
  demoSeed?: boolean;
};

export type ClinicApp = {
  service: ClinicService;
  db: Database;
  clock: Clock;
  instrumentation: Instrumentation;
  runtime: () => RuntimeInfo;
  issuer: TokenIssuer | undefined;
  listen: () => Promise<{ host: string; port: number; url: string }>;
  close: () => Promise<void>;
};

const LOOPBACK_HOSTS = new Set(["127.0.0.1", "::1", "localhost", "[::1]"]);

const digest = (value: string): Buffer => createHash("sha256").update(value).digest();

const bearerMatches = (header: string | undefined, expected: string): boolean => {
  const match = /^Bearer (.+)$/u.exec(header ?? "");
  return match !== null && timingSafeEqual(digest(match[1] ?? ""), digest(expected));
};

const hostName = (hostHeader: string): string => {
  if (hostHeader.startsWith("[")) {
    return hostHeader.slice(0, hostHeader.indexOf("]") + 1);
  }
  return hostHeader.split(":")[0] ?? "";
};

const isLoopbackOrigin = (origin: string): boolean => {
  try {
    return LOOPBACK_HOSTS.has(new URL(origin).hostname) || LOOPBACK_HOSTS.has(`[${new URL(origin).hostname}]`);
  } catch {
    return false;
  }
};

const readJson = async (request: IncomingMessage): Promise<unknown> => {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as string);
    size += buffer.length;
    if (size > MAX_BODY_BYTES) {
      throw Object.assign(new Error("the request body is too large"), { status: 413, code: "resource_exhausted" });
    }
    chunks.push(buffer);
  }
  if (size === 0) {
    return undefined;
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8")) as unknown;
  } catch {
    throw Object.assign(new Error("the request body is not valid JSON"), { status: 400, code: "validation_failed" });
  }
};

const RFC_ULID_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

// A session identifier shaped like the ones the core mints (ses_ and 26
// Crockford base32 characters), so it is accepted wherever a typed ID is.
const newSessionId = (): string => {
  const bytes = randomBytes(26);
  let out = "";
  bytes.forEach((byte, index) => {
    const alphabet = index === 0 ? RFC_ULID_ALPHABET.slice(0, 8) : RFC_ULID_ALPHABET;
    out += alphabet[byte % alphabet.length];
  });
  return `ses_${out}`;
};

type Failure = { status: number; code: string; message: string };

const failureOf = (error: unknown): Failure => {
  if (typeof error === "object" && error !== null && "status" in error && "code" in error) {
    const { status, code, message } = error as { status: number; code: string; message: string };
    return { status, code, message };
  }
  if (isRefusal(error)) {
    return { status: 400, code: error.refusal.code, message: error.refusal.message };
  }
  if (error instanceof TokenRefused) {
    return { status: 400, code: "validation_failed", message: error.message };
  }
  return { status: 500, code: "internal", message: "the clinic application could not complete the request" };
};

export const createApp = (options: AppOptions = {}): ClinicApp => {
  const host = options.host ?? "127.0.0.1";
  if (!LOOPBACK_HOSTS.has(host)) {
    throw new Error("the clinic application listens on a loopback address only");
  }
  const testSupport = options.testSupport ?? false;
  const clock: Clock = options.clock ?? (testSupport ? createOffsetClock() : systemClock);
  const databasePath = options.dbPath ?? ":memory:";
  const db = openDatabase(databasePath);
  const instrumentation = createInstrumentation();
  const service = createClinicService({ db, clock });

  const issuer = testSupport
    ? createTestIssuer({
        allowTestOnlyKey: true,
        ...(options.tokenKeyFile === undefined ? {} : { keyFile: options.tokenKeyFile }),
        issuer: options.tokenIssuer ?? "https://clinic-booking.example.test",
        audience: options.tokenAudience ?? "alixai",
        applicationId: APPLICATION_ID,
        clock,
      })
    : undefined;

  const runtimeInfo: RuntimeInfo = {
    host,
    port: options.port ?? 0,
    databasePath,
    testSupport,
    credentialRequired: options.mcpBearer !== undefined,
    tokenKeyId: issuer?.keyId ?? null,
  };
  const runtime = (): RuntimeInfo => ({ ...runtimeInfo });
  const mcpContext: McpContext = { service, instrumentation, testSupport, runtime };

  if (options.demoSeed === true) {
    seedDemoTenant({ db, clock });
  }

  const koa = new Koa();
  koa.silent = true;

  // Errors never carry stack traces or SQL: a typed code and a short message.
  koa.use(async (ctx, next) => {
    try {
      await next();
    } catch (error) {
      const failure = failureOf(error);
      if (failure.status >= 500) {
        process.stderr.write(`${JSON.stringify({ level: "error", event: "http_error", path: ctx.path })}\n`);
      }
      ctx.status = failure.status;
      ctx.body = { error: { code: failure.code, message: failure.message } };
    }
  });

  // DNS-rebinding guard: the Host must be a loopback name and any Origin a
  // loopback origin.
  koa.use(async (ctx, next) => {
    const origin = ctx.get("origin");
    if (!LOOPBACK_HOSTS.has(hostName(ctx.get("host"))) || (origin !== "" && !isLoopbackOrigin(origin))) {
      ctx.status = 403;
      ctx.body = { error: { code: "scope_violation", message: "requests must address the loopback interface" } };
      return;
    }
    await next();
  });

  koa.use(async (ctx, next) => {
    if (ctx.path !== "/healthz" && options.mcpBearer !== undefined && !bearerMatches(ctx.get("authorization"), options.mcpBearer)) {
      ctx.status = 401;
      ctx.set("WWW-Authenticate", "Bearer");
      ctx.body = { error: { code: "unauthenticated", message: "a valid credential is required" } };
      return;
    }
    await next();
  });

  const router = new Router();

  router.get("/healthz", (ctx) => {
    ctx.body = { status: "ok", application_version: APPLICATION_VERSION };
  });

  // MCP over streamable HTTP, stateless: a server and a transport per request,
  // JSON responses. There is no session to hijack and nothing to expire.
  router.post("/mcp", async (ctx) => {
    const body = await readJson(ctx.req);
    const server = createMcpServer(mcpContext);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    ctx.res.on("close", () => {
      void transport.close();
      void server.close();
    });
    await server.connect(transport);
    ctx.respond = false;
    await transport.handleRequest(ctx.req, ctx.res, body);
  });
  for (const method of ["get", "delete"] as const) {
    router[method]("/mcp", (ctx) => {
      ctx.status = 405;
      ctx.set("Allow", "POST");
      ctx.body = { error: { code: "validation_failed", message: "this endpoint accepts POST only" } };
    });
  }

  // The documented management API for the runtime and system audit.
  const inspection = (ctx: Koa.ParameterizedContext, aspects: unknown): void => {
    try {
      ctx.body = inspect(db, runtime(), parseAspects(aspects));
    } catch (error) {
      if (isRefusal(error)) {
        ctx.status = 400;
        ctx.body = rejectedResult(error.refusal.code, error.refusal.message);
        return;
      }
      throw error;
    }
  };
  router.get("/inspect", (ctx) => {
    inspection(ctx, undefined);
  });
  router.post("/inspect", async (ctx) => {
    const body = await readJson(ctx.req);
    inspection(ctx, typeof body === "object" && body !== null ? (body as { aspects?: unknown }).aspects : undefined);
  });

  if (testSupport && issuer !== undefined) {
    // The host's token exchange. A real host runs it behind its own login; the
    // fixture has no UI, so test support stands in for the authenticated user.
    router.post("/host/context-token", async (ctx) => {
      const body = (await readJson(ctx.req)) as Record<string, unknown> | undefined;
      const input = body ?? {};
      const { tenant_id: tenantId, workspace_id: workspaceId, principal_id: principalId } = input;
      if (typeof tenantId !== "string" || typeof workspaceId !== "string" || typeof principalId !== "string") {
        throw refuse("missing_fields", "tenant_id, workspace_id and principal_id are required");
      }
      const principal = db
        .prepare("SELECT role FROM principals WHERE tenant_id = ? AND site_id = ? AND id = ?")
        .get(tenantId, workspaceId, principalId) as { role: Role } | undefined;
      if (principal === undefined) {
        throw refuse("scope_violation", "the principal is not known in this tenant and workspace");
      }
      const scopes = Array.isArray(input.capability_scopes)
        ? (input.capability_scopes as string[])
        : roleCapabilities(principal.role);
      const minted = mintContextToken(issuer, {
        principalId,
        tenantId,
        workspaceId,
        roles: [principal.role],
        capabilityScopes: scopes,
        sessionId: typeof input.session_id === "string" ? input.session_id : newSessionId(),
        ...(typeof input.lifetime_seconds === "number" ? { lifetimeSeconds: input.lifetime_seconds } : {}),
      });
      ctx.body = {
        token: minted.token,
        jti: minted.jti,
        issued_at: minted.issuedAt,
        expires_at: minted.expiresAt,
        key_id: issuer.keyId,
        issuer: issuer.issuer,
        audience: issuer.audience,
        max_lifetime_seconds: MAX_LIFETIME_SECONDS,
      };
    });

    router.get("/test/stats", (ctx) => {
      ctx.body = {
        calls: instrumentation.calls(),
        totals: databaseTotals(db),
        faults: instrumentation.pendingFaults(),
        clock_offset_seconds: (clock as Partial<OffsetClock>).offsetSeconds?.() ?? 0,
      };
    });
    router.post("/test/reset-counters", (ctx) => {
      instrumentation.reset();
      ctx.body = { status: "ok" };
    });
    router.post("/test/faults", async (ctx) => {
      const body = (await readJson(ctx.req)) as { tool?: unknown; mode?: unknown; times?: unknown } | undefined;
      const modes: FaultMode[] = ["unavailable", "error_after_commit"];
      if (
        typeof body?.tool !== "string" ||
        !modes.includes(body.mode as FaultMode) ||
        (body.times !== undefined && (!Number.isInteger(body.times) || (body.times as number) < 1))
      ) {
        throw refuse("validation_failed", "a fault needs a tool, a mode (unavailable or error_after_commit) and optionally times");
      }
      instrumentation.injectFault(body.tool, body.mode as FaultMode, body.times as number | undefined);
      ctx.body = { status: "ok", faults: instrumentation.pendingFaults() };
    });
    router.delete("/test/faults", (ctx) => {
      instrumentation.clearFaults();
      ctx.body = { status: "ok" };
    });
    router.post("/test/clock", async (ctx) => {
      const body = (await readJson(ctx.req)) as { advance_seconds?: unknown } | undefined;
      const advance = body?.advance_seconds;
      const movable = clock as Partial<OffsetClock>;
      if (typeof advance !== "number" || !Number.isInteger(advance) || advance < 0 || movable.advanceSeconds === undefined) {
        throw refuse("validation_failed", "advance_seconds must be a whole number of seconds, zero or more");
      }
      movable.advanceSeconds(advance);
      ctx.body = { status: "ok", clock_offset_seconds: movable.offsetSeconds?.() ?? 0 };
    });
  }

  koa.use(router.routes());
  koa.use(router.allowedMethods());

  let server: Server | undefined;

  return {
    service,
    db,
    clock,
    instrumentation,
    runtime,
    issuer,
    listen: () =>
      new Promise((resolve, reject) => {
        const listening = koa.listen(options.port ?? 0, host);
        server = listening;
        listening.once("error", reject);
        listening.once("listening", () => {
          const address = listening.address();
          if (address === null || typeof address === "string") {
            reject(new Error("the server has no TCP address"));
            return;
          }
          runtimeInfo.port = address.port;
          const urlHost = host.includes(":") ? `[${host}]` : host;
          resolve({ host, port: address.port, url: `http://${urlHost}:${address.port}` });
        });
      }),
    close: () =>
      new Promise((resolve) => {
        const finish = (): void => {
          try {
            db.close();
          } catch {
            // Already closed.
          }
          resolve();
        };
        if (server === undefined) {
          finish();
          return;
        }
        server.close(finish);
        server.closeAllConnections();
      }),
  };
};

export { TEST_ONLY_KEY_ID };
