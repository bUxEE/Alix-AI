// @alixai/sdk/testing: an in-process fake of the AlixAI public API for the tests of a host
// application. See ./fake-server.ts for exactly what it implements.
export { createFakeServer } from "./fake-server.js";
export type {
  FakeAlixAIServer,
  FakeServerOptions,
  FakeTokenClaims,
  FakeTurn,
  InjectedFailure,
  RecordedRequest,
  TurnHandler,
} from "./fake-server.js";
export { FAKE_HISTORY_SCOPES, FAKE_SETTINGS_SCOPES } from "./context.js";
export type { FakeFilesOptions } from "./fake-files.js";
export type { FakeSettingsOptions } from "./fake-settings.js";
export { askChoice, failTurn, prepareAction, reply, rejectTurn } from "./plans.js";
export type { ChoicePlan, CommitPlan, PrepareInput, TurnPlan } from "./plans.js";
