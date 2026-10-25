// Provider client surface of External Provider Mode (provider connections, modes, egress policy and disclosure, budgets, usage, the kill switch and the
// browser OAuth flow). src/index.ts re-exports this file as the `providers` namespace, so everything below is `providers.<name>` to a host.
//
//   const client = providers.createProvidersClient({ baseUrl, getContextToken });
//   const mode = await client.getMode();                       // local is the default everywhere
//   const started = await client.startOAuth("openai", { capabilities: ["chat"], redirect_uri });
//   // ... open started.authorization_url in the user's browser, then relay the redirect:
//   const status = await client.completeOAuth({ redirect_uri, query });
//   await client.submitCredential("acme", { key: providers.providerSecret(key) }); // when `providers.isManualFallback(error)`
//
// External mode is opt-in and OFF by default; a deployment without it answers every method `unavailable`. The SDK never persists, logs or echoes a
// provider credential: a connected provider is represented by a ConnectionStatus (the state and a safe account id). A host tests against
// `providers.createFakeProvidersServer`.

export type {
  BillingNoticeAck,
  BillingNoticeAckInput,
  Budget,
  BudgetID,
  BudgetInput,
  BudgetState,
  Channel,
  ConnectionStatus,
  CredentialInput,
  DataClassification,
  DeploymentMode,
  DisclosureQuery,
  EgressDisclosure,
  EgressEligibleClass,
  EgressLevel,
  EgressMode,
  EgressPolicy,
  EgressPolicyContentEgress,
  EgressPolicyUpdate,
  EgressPolicyView,
  KillSwitch,
  ModeLevel,
  ModelRef,
  OAuthCallbackInput,
  OAuthConsent,
  OAuthScope,
  OAuthStarted,
  OAuthStartInput,
  ProviderBudgets,
  ProviderID,
  ProviderList,
  ProviderMode,
  ProviderSecret,
  RegionCode,
  Role,
  SafeAccount,
  SourceID,
  UsageExport,
  UsageQuery,
} from "./types.js";

export { createProvidersClient, isManualFallback, providerSecret } from "./client.js";
export type { ProvidersClient, ProvidersClientOptions, ProvidersContextTokenProvider, ProvidersContextTokenRequest } from "./client.js";
export { PROVIDERS_OPERATIONS } from "./operations.js";
export type { ProvidersOperationId, ProvidersOperationSpec } from "./operations.js";
export {
  isBillingNoticeAck,
  isBudget,
  isConnectionStatus,
  isEgressDisclosure,
  isEgressPolicy,
  isEgressPolicyView,
  isKillSwitch,
  isOAuthStarted,
  isProviderBudgets,
  isProviderList,
  isProviderMode,
} from "./guards.js";
export { createFakeProvidersServer, defaultProvidersConfig } from "./testing.js";
export type {
  FakeProvidersClaims,
  FakeProvidersServer,
  FakeProvidersServerOptions,
  ProviderSeed,
  ProvidersFixtureConfig,
  RecordedProvidersRequest,
} from "./testing.js";
