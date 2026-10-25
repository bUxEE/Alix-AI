import { readFileSync } from "node:fs";
import net from "node:net";
import { fileURLToPath } from "node:url";
import { COPILOTKIT_PROVIDER_PROPS, COPILOTKIT_TELEMETRY_ENV, telemetryIsOff, withoutCloud } from "@alixai/copilotkit-adapter";
import { afterEach, describe, expect, it } from "vitest";
import { isLoopbackHost, takeViolations, totalSeen } from "./helpers/network-guard.js";
import { run, startGate } from "./helpers/rig.js";
import type { Gate } from "./helpers/rig.js";
import { row } from "./rows.js";

let gate: Gate | undefined;
afterEach(async () => {
  await gate?.close();
  gate = undefined;
});

const read = (path: string): string => readFileSync(fileURLToPath(new URL(`../../../${path}`, import.meta.url)), "utf8");

describe(row("no-cloud"), () => {
  it("the guard refuses CopilotKit's telemetry host and any public address, and records the attempt (it is armed, not decoration)", async () => {
    await expect(fetch("https://telemetry.copilotkit.ai/v1/event", { method: "POST", body: "{}" })).rejects.toThrow();
    await new Promise<void>((resolve) => {
      const socket = net.connect({ host: "192.0.2.1", port: 9 });
      socket.on("error", () => resolve());
    });
    const violations = takeViolations();
    expect(violations.map((violation) => violation.target)).toEqual(expect.arrayContaining(["telemetry.copilotkit.ai", "192.0.2.1"]));
    expect(violations.every((violation) => !violation.allowed)).toBe(true);
    expect(["127.0.0.1", "localhost", "::1", "[::1]"].every(isLoopbackHost)).toBe(true);
    expect(["192.0.2.1", "telemetry.copilotkit.ai", "10.0.0.1", "127.evil.example"].some(isLoopbackHost)).toBe(false);
  });

  it("a whole run through the runtime goes through the guard, and everything it did was loopback", async () => {
    gate = await startGate();
    const seen = totalSeen();
    gate.gateway.queue({ golden: "plain_answer" });
    expect((await run(gate.browser(), "plain_answer")).error).toBeUndefined();
    // The connections of the page, the runtime and the agent were all seen by the guard (the control for 'nothing was refused') ...
    expect(totalSeen()).toBeGreaterThan(seen);
    // ... and none was refused: the afterEach of test/setup.ts fails the test otherwise.
    expect(takeViolations()).toEqual([]);
  });

  it("the switches that keep CopilotKit's own telemetry off are set before the runtime loads, and are the ones the version matrix records", () => {
    const matrix = JSON.parse(read("sdk/copilotkit-adapter/compat/matrix.json")) as { telemetry: { env: Record<string, string> } };
    expect(COPILOTKIT_TELEMETRY_ENV).toEqual(matrix.telemetry.env);
    expect(telemetryIsOff()).toBe(true);
    for (const [name, value] of Object.entries(COPILOTKIT_TELEMETRY_ENV)) expect(process.env[name], name).toBe(value);
  });

  it("the browser side carries no cloud key and no inspector, and the install never runs the analytics script of @scarf/scarf", () => {
    const props = withoutCloud({ runtimeUrl: "/api/copilotkit", publicApiKey: "ck_pub_x", publicLicenseKey: "lic", licenseToken: "tok" });
    expect(props).toEqual({ runtimeUrl: "/api/copilotkit", ...COPILOTKIT_PROVIDER_PROPS });
    expect(Object.values(COPILOTKIT_PROVIDER_PROPS)).toEqual([false, false, false]);
    expect(read("pnpm-workspace.yaml")).toMatch(/'@scarf\/scarf':\s*false/);
  });

  it("OpenTelemetry, which CopilotKit's dependencies carry as an API, has no SDK, provider or exporter registered in the process that served the runs", async () => {
    gate = await startGate();
    gate.gateway.queue({ golden: "plain_answer" });
    await run(gate.browser(), "plain_answer");
    // @opentelemetry/api is inert until an SDK registers a provider under this global; nothing in the chain does.
    const registry = (globalThis as unknown as Record<symbol, Record<string, unknown> | undefined>)[Symbol.for("opentelemetry.js.api.1")];
    expect(Object.keys(registry ?? {}).filter((key) => ["trace", "metrics", "logs"].includes(key))).toEqual([]);
  });
});
