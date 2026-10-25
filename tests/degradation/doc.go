// Package degradation holds one test per row of the reliability table of master plan section 18 (task M5-06).
// The table is a contract: each row's "Required behavior" is asserted literally against the real components
// (the dialogue engine, the action coordinator, the retrieval pipeline, the voice session service, the job queue
// and the resource governor, the LocalAI supervisor, the derived-index recovery), with the failure injected where
// the table says it happens (the fake model and speech providers, the fake host application, the disk probe, the
// supervised runtime). Every test has a healthy control in the same test, so a failure that cannot happen cannot
// pass. TestDegradationMatrixCoversSection18 fails when the plan's table and the tests drift apart.
package degradation
