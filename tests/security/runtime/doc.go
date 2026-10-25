// Package runtimesec is the runtime-compatibility security suite of master plan 16.3 and 16.4 b15: a LocalAI
// or backend upgrade must not leave AlixAI serving from a runtime it was not qualified against.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-072  TestSecIncompatibleBackendRefusedAndRolledBack
//
// # Startup (gap G4, first half)
//
// The LocalAI adapter is configured with the qualified set and pointed at a loopback runtime whose /version
// can change while the test runs. An unqualified release (newer, older, another major, a source build, no
// version, a version that is not a version, an unreadable /version) makes the adapter report unavailable, the
// provider registry mark the controller unusable, /readyz answer 503, and the model route refuse and never
// call the runtime. A runtime upgraded behind the service's back is caught at the next probe, and once it is
// put back to the qualified release it serves again. Control: the qualified release is healthy, usable and served.
//
// # Upgrade and rollback (gap G4, second half)
//
// The real staged upgrade (internal/upgrade) runs on a data directory with a release manifest written and
// signed with the production release package, and a runtime that records what it is asked. An unqualified runtime
// or backend set (version, backend version, unknown backend, another runtime, none, the old set) is refused
// before anything changes (no switch, the database unmigrated). A backend that is not the qualified one after
// the switch, and a failed health probe, restore the database and the previous runtime set, which the release it
// belongs to qualifies and the new release does not. An operator rollback after a good upgrade returns to the
// same set. Control: a qualified upgrade is applied.
//
// # What is not covered
//
// The runtime's backend versions are not observable through the adapter's allowed endpoints (they are pinned by
// digest in the deployment), and upgrade.FileRuntime.Observe reports the recorded set, not a probe of the running
// process; the backend half of the check is therefore tested on the upgrade's Runtime seam, not on a live LocalAI.
package runtimesec
