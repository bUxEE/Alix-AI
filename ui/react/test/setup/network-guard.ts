import { afterEach } from "vitest";
import { installNetworkGuard, resetAttempts, takeViolations } from "../helpers/network-guard.js";

// Loaded before any test file imports CopilotKit: from here on only loopback connections exist, and a test during which
// something tried another host fails even when the code that tried swallowed the refusal.
installNetworkGuard();

afterEach(() => {
  const violations = takeViolations();
  resetAttempts();
  if (violations.length > 0) {
    throw new Error(`network guard: refused ${violations.map((v) => `${v.kind} ${v.target}`).join(", ")}`);
  }
});
