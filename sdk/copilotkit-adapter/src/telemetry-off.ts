import { applyTelemetryOff } from "./telemetry.js";

// Imported for its effect, and imported first: the telemetry switches of @copilotkit/runtime are read when that package
// loads. See telemetry.ts.
applyTelemetryOff();
