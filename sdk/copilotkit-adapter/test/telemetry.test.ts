import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { createServer } from "node:http";
import { describe, expect, it, vi } from "vitest";
import {
  COPILOTKIT_PROVIDER_PROPS,
  COPILOTKIT_TELEMETRY_ENV,
  applyTelemetryOff,
  telemetryIsOff,
  withoutCloud,
} from "../src/telemetry.js";
import { attempts, isLoopbackHost, takeViolations } from "./helpers/network-guard.js";

// ADR 0035: no CopilotKit cloud, no telemetry. Three kinds of evidence: the switches are the ones the pinned releases read,
// the adapter sets them, and the guard that fails every test on a non-loopback connection really blocks one.

const require = createRequire(import.meta.url);
const packageDir = (name: string): string => require.resolve(`${name}/package.json`).replace(/package\.json$/, "");

describe("the switches are the ones the pinned releases read", () => {
  it("@copilotkit/runtime reads COPILOTKIT_TELEMETRY_DISABLED and DO_NOT_TRACK in its telemetry client", () => {
    const dir = packageDir("@copilotkit/runtime");
    const source = readFileSync(`${dir}dist/v2/runtime/telemetry/telemetry-client.mjs`, "utf8");
    expect(source).toContain("COPILOTKIT_TELEMETRY_DISABLED");
    expect(source).toContain("DO_NOT_TRACK");
    // The singleton is built when the module loads: the switches have to be set before the runtime is imported.
    expect(source).toMatch(/const telemetry = new TelemetryClient\(\)/);
  });

  it("@scarf/scarf, which CopilotKit depends on, reads SCARF_ANALYTICS, SCARF_NO_ANALYTICS and DO_NOT_TRACK", () => {
    const scarf = readFileSync(`${packageDir("@copilotkit/runtime").replace(/@copilotkit[\\/]runtime[\\/]$/, "")}@scarf/scarf/report.js`, "utf8");
    expect(scarf).toContain("SCARF_ANALYTICS");
    expect(scarf).toContain("SCARF_NO_ANALYTICS");
    expect(scarf).toContain("DO_NOT_TRACK");
  });

  it("the adapter sets exactly those, over a value that says otherwise, and reports them", () => {
    const env: Record<string, string | undefined> = { COPILOTKIT_TELEMETRY_DISABLED: "false", DO_NOT_TRACK: "0" };
    expect(telemetryIsOff(env)).toBe(false);
    const applied = applyTelemetryOff(env);
    expect(applied).toEqual(COPILOTKIT_TELEMETRY_ENV);
    expect(telemetryIsOff(env)).toBe(true);
    expect(env["SCARF_ANALYTICS"]).toBe("false");
  });

  it("importing the runtime entry turns them on in the process environment before @copilotkit/runtime loads", async () => {
    delete process.env["COPILOTKIT_TELEMETRY_DISABLED"];
    vi.resetModules();
    await import("../src/telemetry-off.js");
    expect(process.env["COPILOTKIT_TELEMETRY_DISABLED"]).toBe("true");
  });

  it("the browser props carry no cloud key and switch the inspector, dev console and Intelligence indicator off", () => {
    const props = withoutCloud({ runtimeUrl: "/api/copilotkit", publicApiKey: "ck_pub_x", publicLicenseKey: "lic", licenseToken: "tok", threadId: "ses_1" });
    expect(props).toEqual({ runtimeUrl: "/api/copilotkit", threadId: "ses_1", ...COPILOTKIT_PROVIDER_PROPS });
    expect(props).not.toHaveProperty("publicApiKey");
    expect(COPILOTKIT_PROVIDER_PROPS).toEqual({ enableInspector: false, showDevConsole: false, showIntelligenceIndicator: false });
  });

  it("pnpm is told never to run the install script of @scarf/scarf", () => {
    const workspace = readFileSync(`${process.cwd()}/../../pnpm-workspace.yaml`, "utf8");
    expect(workspace).toMatch(/allowBuilds:\s*\n\s*'@scarf\/scarf': false/);
  });
});

describe("the network guard", () => {
  it("recognises loopback and nothing else", () => {
    for (const host of ["127.0.0.1", "127.9.9.9", "localhost", "::1", "[::1]", undefined]) expect(isLoopbackHost(host)).toBe(true);
    for (const host of ["example.com", "telemetry.copilotkit.ai", "192.0.2.1", "10.0.0.1", "0.0.0.0"]) expect(isLoopbackHost(host)).toBe(false);
  });

  it("refuses fetch to another host (so a telemetry call would fail the suite, not pass silently)", async () => {
    await expect(fetch("https://telemetry.copilotkit.ai/ingest", { method: "POST", body: "{}" })).rejects.toThrow();
    const violations = takeViolations();
    expect(violations.length).toBeGreaterThan(0);
    expect(violations.every((violation) => violation.target.endsWith("copilotkit.ai"))).toBe(true);
  });

  it("refuses a raw connection to a non-loopback address", async () => {
    const net = await import("node:net");
    await new Promise<void>((resolve) => {
      const socket = net.connect({ host: "192.0.2.1", port: 443 });
      socket.on("error", () => resolve());
      socket.on("connect", () => resolve());
    });
    expect(takeViolations().map((violation) => violation.target)).toContain("192.0.2.1");
  });

  it("lets loopback through", async () => {
    const server = createServer((_request, response) => response.end("ok"));
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const { port } = server.address() as { port: number };
    const response = await fetch(`http://127.0.0.1:${port}/`);
    expect(await response.text()).toBe("ok");
    server.close();
    expect(attempts().some((attempt) => attempt.target === "127.0.0.1" && attempt.allowed)).toBe(true);
    expect(takeViolations()).toEqual([]);
  });
});
