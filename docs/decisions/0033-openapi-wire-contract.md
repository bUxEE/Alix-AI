# 0033 - OpenAPI/HTTP adapter wire contract

- **Status:** accepted
- **Date:** 2026-10-03
- **Decided in task:** M1-11 (`adapters/openapi`)

## Context
The OpenAPI transport reaches an application through an approved subset of its HTTP API. The core design fixes what the model may do (only mapped
operations, deterministic validation, no hidden commit) but not how a capability call becomes an HTTP request or how a reply is judged. The choice has to
fail closed, never retry a commit, and keep credentials and identity out of everything a model or a log can see.

## Options
- **A. Free-form mapping per package.** Each package describes headers, bodies and status handling itself. Flexible; every package becomes its own security review.
- **B. One fixed wire contract, configured only by header names.** The document says where fields go; the adapter checks the request and the reply against it.
- **C. A generated client from the document.** Fewer rules to write; the document decides behaviour (servers, security, redirects), which a package must not.

## Decision
Option B, implemented in `adapters/openapi` over kin-openapi v0.149.0:
- Only operationIds the capabilities map (plus the management operations the signed adapter configuration names) are callable; the document's `servers` and
  `security` are ignored; no external `$ref`.
- Identity is one header, `X-AlixAI-Identity` (unpadded base64url JSON of `adapters.Identity`); the idempotency key is the `Idempotency-Key` header and also a
  declared `idempotency_key` path or query parameter; a Commit's body is the PreparedAction. Header names are configurable, and a field that collides with a
  protocol-owned header is refused.
- A request that violates the document is never sent. A reply is accepted only if its status is described, its content type is JSON, it matches the response
  schema and `adapters.Normalise` accepts it (which also checks the echoed idempotency key).
- No redirects and no hidden retry: every request sets `Close=true`, because net/http would otherwise repeat a request on a dropped connection (5 sends for
  3 calls in the mutation run). A Commit that was sent and has no usable reply is `uncertain_outcome`; one that was never sent is `unavailable` or
  `validation_failed`.
- Credentials come from an opaque `credential_ref` through a `SecretProvider`, injected for the endpoint's own origin only and redacted under every formatting verb;
  with no provider the build fails closed.

## Consequences
- One handshake per call (`Close=true`); acceptable for the call volumes of Lite, and the safe default for commits.
- Packages with an API that needs cookies, object parameters, non-JSON bodies or non-default styles are refused until the contract is extended.
- A package without a `health` operation reports `degraded` and every operator has to accept the audit warning; packages should map one.
- The audit's disposable-tenant operations are reachable only through `Adapter.Manage`; the probes.Provisioner bridge is M1-26.

## Reversal plan
The contract is an implementation detail of one transport. A change adds a version to the adapter configuration, keeps the old behaviour for existing signed
packages, and goes through a new ADR.
