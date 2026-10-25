// Package extprovidersec is the security suite of External Provider Mode (additive plan section 14, master plan 16.4
// and 22.5, task X-17). External mode lets permitted data leave the deployment for a hosted model provider, so the
// suite attacks that boundary: the provider's key, what the provider is sent and keeps, what the provider says
// back, which provider and model answer, what it costs, and who can stop it. The threats, the controls and the
// gaps are in docs/security/external-provider-threats.md; each test below carries the SEC id it proves.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-114  TestSecExtAPIKeyNeverInClientResponsesOrLogs
//	SEC-115  TestSecExtProviderRetentionDisclosedBeforeDataLeaves
//	SEC-116  TestSecExtCrossTenantRequestMixingImpossible
//	SEC-117  TestSecExtProviderResponseInjectionCannotExpandScope
//	SEC-118  TestSecExtProviderToolCallCannotBypassHostReauthorization
//	SEC-119  TestSecExtOverBroadSourceCannotLeakLocalOnly
//	SEC-120  TestSecExtModelSubstitutionDetected
//	SEC-121  TestSecExtNoHiddenFallback
//	SEC-122  TestSecExtCostExhaustionStopsExternalCalls
//	SEC-123  TestSecExtRegionChangeQuarantinesProvider
//	SEC-124  TestSecExtNetworkMetadataLeavesOnlyToAllowlistedDestination
//	SEC-125  TestSecExtAudioImageFileRetentionDisclosed
//	SEC-126  TestSecExtGlobalKillSwitchStopsAllEgress
//	SEC-127  TestSecExtExternalModeOffByDefaultNeedsEveryLevel
//	SEC-128  TestSecReplyModelMustMatchThePinnedModel
//	SEC-129  TestSecKillSwitchIsReReadAfterReservation
//
// # The world
//
// world_test.go builds the same world as the failure matrix of X-18 (tests/degradation/ext): the clinic reference
// application's signed package behind its stand-in host, the real policy gateway, action coordinator, session store
// and dialogue engine, the real data-egress service over a real store, and the external-mode runtime routing
// between two scripted fake providers (the external one and the local one). Where the matrix scripts the profile
// registry, this suite uses the REAL registry (internal/providerreg) with a scripted credential-status source, and
// the REAL provider budget ledger, because region changes, model drift, stale terms, fallbacks and cost are exactly
// the behaviour under test. The host adapter and the sender record every request with its scope, so "the provider
// could not do X" is read off the host and off the wire, not inferred. Nothing here dials a real provider or uses a
// real key; the only sockets are loopback httptest servers.
//
// # Controls
//
// Every "never" has a positive control in the same world: the same request without the fault is sent, the canary
// that must not leak does reach the place it is meant to reach (the Authorization header of the mock provider), the
// permitted region passes where the changed one is refused. A test that cannot fail proves nothing.
//
// # Known defects
//
// None: the two defects this suite found (SEC-128, SEC-129) are fixed and their tests assert for real.
package extprovidersec
