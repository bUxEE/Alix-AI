import { afterEach, describe, expect, it } from "vitest";
import { AlixAIRunner, createAlixAIFetchHandler, createAlixAIRuntime } from "../src/runtime.js";
import { createMemorySecretStore } from "../src/secrets.js";
import { mintTokens } from "./helpers/agent-rig.js";
import { CONFIRMATION_SECRET, SECRET_HEADER, startFakeGateway } from "./helpers/fake-gateway.js";
import type { FakeGateway } from "./helpers/fake-gateway.js";
import { golden } from "./helpers/golden.js";
import type { GoldenName } from "./helpers/golden.js";
import type { AlixAIContextTokenRequest } from "../src/agent.js";

// The real @copilotkit/runtime (v2 handler, the one a Next.js route serves) in front of AlixAIAgent in front of the fake
// gateway. The "browser" here is a Request to the handler: everything the handler returns is what a browser would get.

let gateway: FakeGateway | undefined;
afterEach(async () => {
  await gateway?.close();
  gateway = undefined;
});

const BASE = "http://127.0.0.1/api/copilotkit";

const setup = async (): Promise<{ handler: ReturnType<typeof createAlixAIFetchHandler>; tokenRequests: AlixAIContextTokenRequest[]; gateway: FakeGateway }> => {
  gateway = await startFakeGateway({ rewriteIds: true });
  const tokenRequests: AlixAIContextTokenRequest[] = [];
  const alixai = createAlixAIRuntime({
    baseUrl: gateway.url,
    getContextToken: mintTokens(tokenRequests),
    secrets: createMemorySecretStore(),
    sleep: () => Promise.resolve(),
  });
  return { handler: createAlixAIFetchHandler(alixai, { basePath: "/api/copilotkit" }), tokenRequests, gateway };
};

const runBody = (name: GoldenName, extra: Record<string, unknown> = {}): string => {
  const body = golden(name).meta.request?.body as Record<string, unknown>;
  return JSON.stringify({ ...body, ...extra });
};

/** What a browser gets for one run: the status, every header, and the body text. */
const browserRun = async (
  handler: ReturnType<typeof createAlixAIFetchHandler>,
  body: string,
  headers: Record<string, string> = {},
): Promise<{ status: number; headers: Record<string, string>; text: string; events: Array<Record<string, any>> }> => {
  const response = await handler(
    new Request(`${BASE}/agent/default/run`, { method: "POST", headers: { "Content-Type": "application/json", Accept: "text/event-stream", ...headers }, body }),
  );
  const text = await response.text();
  const events = text
    .split("\n\n")
    .flatMap((frame) => frame.split("\n").filter((line) => line.startsWith("data:")).map((line) => JSON.parse(line.slice(5).trim()) as Record<string, any>));
  const seen: Record<string, string> = {};
  response.headers.forEach((value, name) => {
    seen[name] = value;
  });
  return { status: response.status, headers: seen, text, events };
};

describe("AlixAIAgent behind the real CopilotRuntime handler", () => {
  it("advertises the agent and serves a golden run to the browser", async () => {
    const { handler } = await setup();
    const info = await handler(new Request(`${BASE}/info`));
    expect(info.status).toBe(200);
    expect(JSON.stringify(await info.json())).toContain('"default"');

    gateway?.queue({ golden: "plain_answer" });
    const run = await browserRun(handler, runBody("plain_answer"));
    expect(run.status).toBe(200);
    const types = run.events.map((event) => event["type"]);
    expect(types[0]).toBe("RUN_STARTED");
    expect(types).toContain("TEXT_MESSAGE_CONTENT");
    expect(types[types.length - 1]).toBe("RUN_FINISHED");
  });

  it("the confirmation secret never reaches the browser: not in a header, not in any body, not in the approval round trip", async () => {
    const { handler, tokenRequests } = await setup();
    const prepared = await browserRun(handler, runBody("prepare_interrupt"));
    const finished = prepared.events[prepared.events.length - 1];
    expect(finished?.["outcome"]?.["type"]).toBe("interrupt");
    expect(JSON.stringify(prepared)).not.toContain(CONFIRMATION_SECRET);
    expect(Object.keys(prepared.headers).some((name) => name.toLowerCase().includes("confirmation"))).toBe(false);

    // The browser approves; it holds no secret and sends none (a forged one is not forwarded).
    const approval = await browserRun(handler, runBody("resume_approve"), { "X-AlixAI-Confirmation-Token": "cfm_forged_by_the_browser" });
    expect(approval.status).toBe(200);
    expect(approval.events[approval.events.length - 1]?.["outcome"]?.["type"]).toBe("success");
    expect(JSON.stringify(approval)).not.toContain(CONFIRMATION_SECRET);
    expect(JSON.stringify(approval)).not.toContain("cfm_forged");

    const toGateway = gateway?.requests.filter((request) => request.method === "POST") ?? [];
    expect(toGateway).toHaveLength(2);
    expect(toGateway[0]?.headers[SECRET_HEADER]).toBeUndefined();
    expect(toGateway[1]?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
    // The host's callback saw who is asking and minted a fresh token for each call.
    expect(tokenRequests.map((request) => request.resume)).toEqual([false, true]);
  });

  it("a decline through the runtime sends no secret and ends with the cancelled outcome", async () => {
    const { handler } = await setup();
    await browserRun(handler, runBody("prepare_interrupt"));
    const decline = await browserRun(handler, runBody("resume_reject"));
    expect(decline.events[decline.events.length - 1]?.["outcome"]?.["type"]).toBe("cancelled");
    expect(gateway?.requests[1]?.headers[SECRET_HEADER]).toBeUndefined();
  });

  it("an approval for a preparation this server never prepared is refused before anything reaches the gateway", async () => {
    const { handler } = await setup();
    const run = await browserRun(handler, runBody("resume_approve"));
    expect(JSON.stringify(run.events) + run.text).toMatch(/confirmation|RUN_ERROR|error/i);
    expect(gateway?.requests).toHaveLength(0);
  });

  it("sets the telemetry switches before the runtime loads, and the runtime sends nothing to any other host", async () => {
    await setup();
    expect(process.env["COPILOTKIT_TELEMETRY_DISABLED"]).toBe("true");
    expect(process.env["DO_NOT_TRACK"]).toBe("1");
    expect(process.env["SCARF_ANALYTICS"]).toBe("false");
    // The network guard (setupFiles) fails this test, and every other, if anything above tried a non-loopback host.
  });
});

describe("AlixAIRunner behind the real CopilotRuntime handler", () => {
  const connectBody = (threadId: string): string =>
    JSON.stringify({ threadId, runId: "run_page_connect", messages: [], state: {}, tools: [], context: [], forwardedProps: {} });

  const browserConnect = async (
    handler: ReturnType<typeof createAlixAIFetchHandler>,
    threadId: string,
    headers: Record<string, string> = {},
  ): Promise<{ status: number; text: string; types: string[] }> => {
    const response = await handler(
      new Request(`${BASE}/agent/default/connect`, { method: "POST", headers: { "Content-Type": "application/json", Accept: "text/event-stream", ...headers }, body: connectBody(threadId) }),
    );
    const text = await response.text();
    const types = [...text.matchAll(/"type":"([A-Z_]+)"/g)].map((match) => match[1] as string);
    return { status: response.status, text, types };
  };

  it("builds the runtime with its own runner, whatever runner the options carry, and that runner has no local thread endpoints", async () => {
    gateway = await startFakeGateway({ rewriteIds: true });
    const foreign = { run: () => { throw new Error("a runner of the host's was used"); }, connect: () => { throw new Error("a runner of the host's was used"); }, isRunning: () => Promise.resolve(false), stop: () => Promise.resolve(false), ɵsupportsLocalThreadEndpoints: true };
    const alixai = createAlixAIRuntime({
      baseUrl: gateway.url,
      getContextToken: mintTokens([]),
      runtime: { runner: foreign } as unknown as NonNullable<Parameters<typeof createAlixAIRuntime>[0]["runtime"]>,
    });
    expect(alixai.runtime.runner).toBe(alixai.runner);
    expect(alixai.runner).toBeInstanceOf(AlixAIRunner);
    expect((alixai.runner as { ɵsupportsLocalThreadEndpoints?: boolean }).ɵsupportsLocalThreadEndpoints).not.toBe(true);
  });

  it("answers the browser's connect from the gateway through the token callback, as a connect, with the forwarded headers", async () => {
    const s = await setup();
    s.gateway.queue({ golden: "plain_answer" });
    expect((await browserRun(s.handler, runBody("plain_answer", { threadId: "ses_run_01" }), { "x-user": "alice" })).status).toBe(200);
    const asked = s.tokenRequests.length;

    const joined = await browserConnect(s.handler, "ses_run_01", { "x-user": "alice" });
    expect(joined.types).toEqual(["RUN_STARTED", "MESSAGES_SNAPSHOT", "STATE_DELTA", "RUN_FINISHED"]);
    expect(joined.text).toContain("The item is named gadget.");
    expect(s.tokenRequests.slice(asked)).toMatchObject([{ operation: "agui.connect", sessionId: "ses_run_01", resume: false, forwardedHeaders: { "x-user": "alice" } }]);
    expect(s.gateway.connects).toHaveLength(1);
  });

  it("refuses a connect the callback refuses: one RUN_ERROR, the gateway never asked, nothing of the thread in the answer", async () => {
    gateway = await startFakeGateway({ rewriteIds: true });
    const alixai = createAlixAIRuntime({
      baseUrl: gateway.url,
      getContextToken: ({ forwardedHeaders }) => {
        if (forwardedHeaders["x-user"] !== "alice") throw new Error("secret detail of the host's check");
        return "ses_run_02.1";
      },
      sleep: () => Promise.resolve(),
    });
    const handler = createAlixAIFetchHandler(alixai, { basePath: "/api/copilotkit" });
    const refused = await browserConnect(handler, "ses_run_02", { "x-user": "mallory" });
    expect(refused.types).toEqual(["RUN_STARTED", "RUN_ERROR"]);
    expect(refused.text).toContain('"code":"unauthenticated"');
    expect(refused.text).not.toContain("secret detail");
    expect(gateway.connects).toEqual([]);
    expect(gateway.requests).toEqual([]);
  });

  it("serves no in-memory thread data: the thread routes of the runtime answer an error", async () => {
    const s = await setup();
    s.gateway.queue({ golden: "plain_answer" });
    await browserRun(s.handler, runBody("plain_answer", { threadId: "ses_run_03" }));
    for (const path of ["/threads", "/threads/ses_run_03/events", "/threads/ses_run_03/messages", "/threads/ses_run_03/state"]) {
      const response = await s.handler(new Request(`${BASE}${path}`));
      const body = await response.text();
      expect(response.status, path).toBeGreaterThanOrEqual(400);
      expect(body, path).not.toContain("gadget");
      expect(body, path).not.toContain("ses_run_03");
    }
  });
});
