// Package decisionsec is the security suite of the optional decision layer (master plan 8.5 and 16.4,
// docs/architecture/decision-layer.md section 9, ADR 0029). The layer is optional, bounded, non-generative and
// non-authoritative: a scorer proposes one of a bounded set of candidates that deterministic code already
// authorised, and everything it says is re-checked. The threats, the controls and the gaps are in
// docs/security/decision-layer-threats.md; each test below carries the SEC id it proves.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-098  TestSecDecisionManifestSignatureAndRevisionHashVerified
//	SEC-099  TestSecScorerManifestSignatureIsVerified
//	SEC-100  TestSecDecisionNoCrossTenantCandidateLeakage
//	SEC-101  TestSecDecisionOversizedCandidateSetRejected
//	SEC-102  TestSecDecisionInjectionInUserTextCannotChangeAuthority
//	SEC-103  TestSecDecisionInjectionInDocumentOrRAGTextNeverReachesScorer
//	SEC-104  TestSecDecisionSelectedIDAlwaysReresolved
//	SEC-105  TestSecDecisionScorerPackageCannotImportDBFilesystemNetworkOrCommit
//	SEC-106  TestSecDecisionFailClosedOnTimeoutCrashMismatchAndLowConfidence
//	SEC-107  TestSecDecisionScorerCannotLowerRiskOrBypassConfirmation
//	SEC-108  TestSecDecisionScorerCannotDiscloseASourceOrAuthorizeAUser
//	SEC-109  TestSecDecisionAuditEventsForInvocationAcceptanceFallbackAndFinalAction
//	SEC-110  TestSecDecisionUpdateRollbackRestoresPreviousRevision
//	SEC-111  TestSecDecisionCLMCheckpointIsPickleGuardedByPinsAndReadOnlyMounts (automated part; the loader review is human)
//	SEC-113  TestSecPanickingScorerLeavesATerminalAuditEvent
//	SEC-112  licence and supply-chain review of CLM, Qwen3-8B, vLLM and the quantisations: human, D-H01
//
// # Two rigs
//
// The engine rig (rig_test.go) is the guard of internal/decision with a scripted fake scorer that records every
// request it receives, a catalogue resolver that knows which tenant owns which candidate, and a real audit log
// (SQLite, hash chained) behind a tee that keeps the events. The dialogue world (world_test.go) is the real
// dialogue engine of the conversation tests: the clinic reference application's signed package behind its
// stand-in host, the real policy gateway, action coordinator and session store, a scripted controller and the
// decision layer of internal/dialogue/decision in front of it. The host counts every Prepare and Commit that
// reaches it, so "the scorer authorised nothing" is a count, not a comment.
//
// # Controls
//
// Every "never" has a control that shows the thing could have happened: the same request without the fault is
// accepted, the canary that must not reach the scorer does reach the controller or the sources, the import guard
// is run over planted sources that import a database, write a file and reach a commit package, and so on. The
// task that added this suite also ran mutations (a check removed from the production code, the suite re-run,
// the code restored) and recorded which test caught each one in the threat document.
//
// # Known defects
//
// None are open. G11 (SEC-099, the manifest signature) and G12 (SEC-113, a panicking scorer) were fixed by D-16 and their tests run
// for real; a test added later that records a defect calls secutil.Known and is listed as a DEFECT by the coverage tool.
package decisionsec
