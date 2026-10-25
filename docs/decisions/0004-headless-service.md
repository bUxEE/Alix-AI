# 0004 — Headless service, no AlixAI UI

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
AlixAI is integrated into other applications that already own identity, tenancy and user
experience. Shipping its own end-user or admin UI would duplicate that, widen the attack surface
(sessions, CSRF, XSS, a second login) and add resident code to the minimum profile.

## Options
- **Headless: HTTP API, SDKs, CLI.** Host applications render the conversation. Operators use the
  CLI and the operations API. Official UI kits are optional later packages.
- **Bundled chat UI.** Fast demo, but it becomes a second product with its own authentication and
  pushes host applications toward our UX.
- **Bundled admin dashboard.** Useful to operators, but a privileged web surface that must be
  hardened and kept loopback-only; the CLI plus API covers the same operations with less surface.

## Decision
AlixAI ships no end-user application, dashboard or product UI. The integrated application owns the
user experience and consumes AlixAI through the API or SDK. LocalAI's own GUI is not exposed in
production. Official UI kits are deferred to M7 and ship as separate optional packages over the
public API; they never gain private endpoints.

## Consequences
- Easier: small attack surface, no UI dependencies in the core, one public contract (API and
  schemas) for every client.
- Harder: first-run and operator experience rely on the CLI and docs; reference apps carry the
  demonstration load.
- Forbidden before M7: any dashboard, admin UI or end-user UI in this repository, including a
  temporary one. Dev-only endpoints are API endpoints, off by default.

## Reversal plan
Adding a UI later means a new package consuming the public API plus a superseding ADR. Nothing in
the core changes because the core never assumed a UI.
