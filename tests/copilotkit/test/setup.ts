import { afterAll, afterEach } from "vitest";
import { installNetworkGuard, takeViolations } from "./helpers/network-guard.js";

// Loaded before any test file imports CopilotKit: from here on only loopback exists, and the test that was running when
// something tried another host fails, even when the code that tried swallowed the refusal. The guard covers this Node process
// only (the Go and Python proxies are held to the same rule by their own SDK tests).
installNetworkGuard();

const refused = (): string => takeViolations().map((violation) => `${violation.kind} ${violation.target}`).join(", ");

afterEach(() => {
  const violations = refused();
  if (violations !== "") throw new Error(`network guard: refused ${violations}`);
});

// A fire-and-forget call that outlives the last test (a telemetry event sent while the runtime shuts down) is caught here.
afterAll(() => {
  const violations = refused();
  if (violations !== "") throw new Error(`network guard: refused ${violations} after the last test`);
});
