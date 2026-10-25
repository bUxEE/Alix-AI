// Package external holds the build-boundary invariants of external provider mode (ADR 0022) and of integration
// readiness (ADR 0037): a binary built with the noexternal tag holds no external-mode package and still holds the
// readiness commands, only the tagged composition-root files reach the external packages, and readiness imports
// nothing that signs, installs, activates or audits.
package external
