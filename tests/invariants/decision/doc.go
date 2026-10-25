// Package decision holds the invariant that the optional decision layer stays local: nothing the scorer path or
// its adapters import, directly or transitively, is a package of external provider mode (ADR 0029, ADR 0030).
package decision
