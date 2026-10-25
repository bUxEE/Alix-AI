# 0013 — Application context token: PASETO, JWT profile or mutually authenticated exchange

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-16 (provisional default and profile); finalised in M1-06

## Context
§10.1: the host application authenticates the human or service principal, then exchanges that
identity for a short-lived, audience-bound AlixAI context token (or uses mutually authenticated
local IPC). AlixAI validates issuer, audience, tenant, workspace, principal, expiry, nonce and
the permitted capability scope, and the model never sees bearer credentials. §16.4 requires
tests for forged tokens and replayed confirmations.

What decides the format, in order:

1. **A verifier that cannot be confused:** exactly one signature algorithm, no algorithm
   negotiation, no key material taken from the token, explicit token typing.
2. **Host-side minting is cheap in every host stack**, not only ours. The three first-party SDKs
   (TypeScript, Go, Python; M3 tasks) provide a mint helper. The TypeScript SDK has zero runtime
   dependencies, so its helper uses Node's built-in `crypto`; Go uses the standard library;
   Python's standard library has no Ed25519, so the Python helper needs `cryptography` as at
   least an optional extra. Hosts without our SDK use whatever JOSE library their stack has.
3. **Existing tooling can read it:** API gateways, service meshes, identity providers and
   observability already understand JWT.
4. Size, and a clean seam for the mutually authenticated local IPC that M3-14 delivers.

## Facts (checked 2026-10-02)

**PASETO.** v4.public is Ed25519 over a pre-authentication encoding; it removes algorithm choice
by design. The IETF draft (`draft-paragon-paseto-rfc-01`) expired on 2022-11-25 and was never
published as an RFC, so there is no standards-track document to cite or audit against. The
community specification is at v1.0.2 (2022-06-10). Implementations are maintained, none is
archived: `panva/paseto` (JS, v4.0.1, 2026-09-04), `aidantwoods/go-paseto` (Go, v1.6.0,
2025-12-27), `dajiaji/pyseto` (Python, v1.10.0, 2026-07-19), `auth70/paseto-ts` (TS, last push
2026-08-25). No gateway or identity provider the project depends on reads PASETO.

**JWT / JWS compact** is standards-track (RFC 7515, 7519), with a Best Current Practice for
exactly this problem (RFC 8725, BCP 225): the verifier must be given a fixed algorithm set, must
validate `aud` and `iss`, should use explicit `typ`, must not trust `kid`, `jku`, `x5u` input,
and should use mutually exclusive validation rules per JWT kind.

**RFC 9864** (Standards Track, October 2025) deprecates the polymorphic JOSE `alg` value `EdDSA`
(RFC 8037) in favour of the fully specified `Ed25519` and `Ed448`. It gives no guidance on
accepting both during a transition. Library support for the new name is uneven:

| Library | Version (date) | `Ed25519` alg name | `EdDSA` alg name |
| --- | --- | --- | --- |
| `golang-jwt/jwt` v5 (Go) | v5.3.1 (2026-01-28); `main` unchanged | **no** (`Alg()` returns `"EdDSA"`; no upstream issue or PR found) | yes |
| `go-jose/go-jose` v4 (Go) | v4.1.5 (2026-09-03) | **no** | yes |
| `lestrrat-go/jwx` v4 (Go) | v4.5.0 (2026-09-08) | yes | yes (flagged deprecated) |
| `PyJWT` (Python) | 2.15.1 (2026-09-28) | **no** (issue #1190 and PR #1199 open, unmerged) | yes |
| `joserfc` (Python) | 1.7.5 (2026-08-29) | yes | yes (flagged "deprecated via RFC 9864") |
| `jose` (JS) | 6.2.12 (2026-09-05) | yes (since 5.10.0, 2025-02-17) | yes |
| Node `crypto`, Go `crypto/ed25519`, Python `cryptography` | stdlib / long-standing | n/a: they sign bytes, the header is written by the host | n/a |

So the two most-used Go and Python JWT libraries cannot emit the new name yet, while every
library can emit the old one.

**Mutually authenticated exchange.** mTLS (optionally token-bound per RFC 8705) gives strong
service identity but needs certificate issuance and TLS termination control on both ends, which
many hosts behind load balancers do not have, and it authenticates the *connection*, not the
per-request principal, tenant and workspace the context must carry.

## Options

| | A/B. Strict JWT profile, one algorithm | C. PASETO v4.public | D. mTLS / mutual session exchange |
| --- | --- | --- | --- |
| Standards status | RFC 7515/7519/8037, BCP 225 (RFC 8725) | none: draft expired 2022, community spec only | RFC 8446 + RFC 8705 for token binding |
| Library maturity | every stack has several; many reviewed | one maintained library per stack, small user bases | TLS stacks mature; per-request identity not carried |
| Algorithm-confusion risk | present in the format, removed by a verifier that accepts one fixed `alg`, `typ` and `kid` set; ours is ~100 lines on `crypto/ed25519` plus fuzzing | none by design (version string fixes the algorithm) | none (no token) |
| Size for our claims | **633 bytes** measured | about 586 bytes computed for the same claims (no JOSE header, `kid` in the footer): 7% smaller, which does not matter against an 8 KiB cap | handshake only; per-request identity needs another channel |
| TS / Go / Python minting | Node `crypto`, Go stdlib, Python `cryptography` (+ any JOSE lib) | needs the PASETO library or hand-rolled PAE in each SDK; the zero-dependency TS SDK would hand-roll it | TLS client certificates in each |
| Readable by gateways, IdPs, debuggers | yes | no | n/a |
| Fits remote hosts behind load balancers | yes | yes | often not |
| Per-request principal, tenant, workspace | yes (claims) | yes (claims) | no |

E. **HMAC-signed tokens** (shared secret): rejected. AlixAI would hold a secret able to forge
tokens for the host, and a single leak breaks every application's trust.

## Decision
**A strict JWT profile with exactly one accepted algorithm, verified by an in-tree stdlib
verifier (`internal/identity`)**, with PASETO v4.public documented as the considered alternative
and mutually authenticated local IPC (option D) delivered by M3-14 as a transport seam.

### The algorithm identifier: `EdDSA`
ADR-0013 was provisional on `Ed25519` (RFC 9864) with an explicit trigger: *switch to `EdDSA` if
library support has not arrived by M1-06.* It has not (table above: `golang-jwt` and PyJWT,
the dominant Go and Python libraries, cannot emit `Ed25519`; PyJWT's fix is an open, unmerged
PR). The single accepted value is therefore **`EdDSA`**, restricted to Ed25519 keys.

- RFC 9864 deprecates `EdDSA` because the name does not say which curve. That ambiguity does not
  exist here: the verifier only ever uses `crypto/ed25519` with a 32-byte registered public key
  and a 64-byte signature, so there is no curve for a token to choose.
- The verifier accepts **exactly one** `alg` string, the constant `identity.Algorithm`. It never
  accepts both spellings and has no algorithm agility. A test refuses the other spelling,
  computed from the constant, so the day the constant changes the test still means the same.
- **Overturn trigger.** Switch the constant to `Ed25519` when `golang-jwt` and PyJWT have both
  released support for it. The switch is one constant, the three SDK mint helpers and the
  conformance vectors; because a verifier cannot accept both names, it is a coordinated cutover
  (hosts mint with the new name, wait out one maximum token lifetime, deploy the new verifier).

### Token profile (normative; implemented by `internal/identity`)
JWS compact serialisation `base64url(header).base64url(payload).base64url(signature)`: the
alphabet is URL-safe, **no padding**, one canonical encoding per segment, signature = Ed25519
over the ASCII bytes `header.payload` as sent, exactly 64 bytes. At most **8 KiB** in total,
checked before anything is decoded.

**Header: exactly `alg`, `typ`, `kid`; nothing else.**
- `alg` = `EdDSA` (above). `none`, every other value, any other type, a missing or duplicate
  `alg` is refused.
- `typ` = `alixai-context+jwt` (RFC 8725 §3.11). A token of another kind signed by the same key
  is never a context token.
- `kid`: 1 to 64 characters of `[A-Za-z0-9._-]`. It selects among keys registered **by the
  operator**; nothing in a token supplies or points at key material. `jku`, `jwk`, `x5u`, `x5c`,
  `crit`, `zip`, `cty` and every other member are refused, as are duplicate members.

**Claims: all of these, and nothing else.** An unknown claim is refused (fail closed; there is
no "extra" channel through a signed token).

| Claim | Meaning | Rule |
| --- | --- | --- |
| `iss` | issuer | must equal the issuer registered for the signing key |
| `aud` | audience | string (or an array of exactly one string) equal to the deployment's audience (default: its deployment ID, configurable). Multi-audience tokens are refused: the replay cache is per deployment, so a token valid for two audiences could be replayed against the other |
| `sub` | principal | scope level `principal` |
| `application_id` | application | scope level `application`; must equal the application of the signing key |
| `tenant_id`, `workspace_id` | tenant, workspace | scope levels |
| `roles` | principal's roles | non-empty array, at most 64 |
| `capability_scopes` | capability IDs the token permits | non-empty array, at most 256; exact match, no wildcards |
| `session_id` | the session the context is bound to | scope level `session` |
| `iat`, `nbf`, `exp` | NumericDates | plain non-negative integers of at most 12 digits (no floats, signs, exponents; arithmetic cannot overflow) |
| `jti` | single-use nonce | 16 to 128 characters of `[A-Za-z0-9._-]` |

There is **no `deployment_id` claim**: the deployment level of the `Scope` is filled from
deployment configuration (`identity.deployment_id`), and a token that carries one is refused.
Context levels that are absent or empty are not "bad tokens": they flow into
`contracts.Scope.Validate()`, so any missing level is `scope_missing` (naming the levels) and any
wildcard, padded, control-character or over-128-character value is `scope_violation`. A claim of
the wrong JSON type (`null`, a number where a string belongs) is a malformed token. Duplicate
roles and capability scopes are collapsed.

**Time.** Fixed skew allowance **30 s** on `nbf`, `iat` and `exp`. The validity window
(`exp` minus the earlier of `iat` and `nbf`) may not exceed the maximum lifetime: a hard
ceiling of **15 minutes**, configurable lower (`identity.max_token_lifetime`), never higher.
`iat` or `nbf` more than the skew in the future is `not_yet_valid` (this is what stops a
future-dated `iat` from stretching the window). A token is expired from `exp` plus the skew.
Hosts should mint with a lifetime of minutes, not the ceiling (the recommended SDK default is 5 minutes, which `identitytest` also uses).

**Verification order (cheap first, signature before payload).** size and structure, header
profile, key lookup, retirement check, signature, claim parsing, issuer, audience, application,
times, `Scope` construction and validation, and only then the nonce is consumed. A token refused
for any reason burns no nonce, and unauthenticated input never reaches the claim parser or the
database.

**Replay.** `jti` is **single use**: one token authenticates exactly one request or one
connection open (an SSE or WebSocket open). The default is final, not a placeholder: allowing a
token to authorise several requests turns it into a bearer credential for its whole lifetime.
Nonces are recorded in SQLite (`identity_replay`, scope columns in the primary key) until
`exp` plus the skew, survive restarts, and are swept about once a minute. A replay cache that
cannot answer refuses the token (`unavailable`, retryable) and never accepts it.

**Keys.** Ed25519, registered per application (`IssuerKey{KeyID, ApplicationID, Issuer,
Public, NotAfter}`), several active at once for rotation, optional `NotAfter` to bound the
overlap, immediate revocation. No shared secrets. Key storage sits behind the small
`identity.KeyResolver` interface: an in-memory `KeyRegistry` now, the trust store
(`internal/trust`, M1-05) later. A key cannot speak for another application, and a signature by a
key under another key's `kid` fails.

**Errors.** `unauthenticated` (everything that makes the token untrustworthy), `expired`,
`replayed`, `scope_missing`, `scope_violation`, `unavailable` (fail closed). Each carries a closed
`reason` in `Details` (`bad_signature`, `wrong_audience`, `lifetime_too_long`, ...) and a generic
message; no response, error text or log line ever contains a token or any segment of one (the `jti` nonce, which is not a credential, is logged at debug level for correlation). HTTP: 401 for
`unauthenticated`, `expired`, `replayed`, `scope_missing`; 403 for `scope_violation`; 503 when the
replay cache or key store cannot answer. The credential is read only from
`Authorization: Bearer`, never from a query string, cookie or body, and is removed from the
request before it reaches a handler.

### Measurements (dev box, i7-10700K, Windows, pure Go, `CGO_ENABLED=0`; not a certified result)
- **Token size:** 633 bytes for a typical context (one role, two capability scopes); the 8 KiB
  cap leaves room for the 64-role and 256-scope bounds.
- **Verification:** 120 µs and 131 allocations (6.4 KB) per token for signature plus claims;
  **370 µs** including the SQLite nonce write on the single writer connection, about 2,700
  verified tokens per second per process, far above the Lite profile's request rate. Minting
  (host side) is 41 µs.
- **Replay-cache growth:** rows = request rate × (token lifetime + 30 s); about **144 bytes per
  row** on disk. At 100 requests/s that is 33,000 rows (4.8 MB) with 5-minute tokens and 93,000
  rows (13.4 MB) at the 15-minute ceiling; entries are removed as they expire, so the size is
  bounded by what authenticated hosts actually mint. Negligible for the 4 GB profile.
- **Fuzzing:** `FuzzTokenVerify` (arbitrary bytes; accepts only the one valid token; never a
  panic) and `FuzzSignedPayload` (arbitrary signed header and payload JSON; accepted scopes are
  valid, bound to the key's application and the configured deployment, within the lifetime
  ceiling) ran 30 s and 40 s locally with no finding. M5-08 runs
  `-fuzz=FuzzTokenVerify ./internal/identity/`.

### Seam for mutually authenticated local IPC (M3-14)
`identity.Authenticator` (`Authenticate(ctx, credential) (*Verified, error)`) is the one shape
the HTTP middleware and every other consumer sees. The token `Verifier` implements it; the local
IPC transport implements it from peer authentication, building its `Scope` through
`identity.NewScope(deploymentID, ContextClaims)`, the same constructor the token path uses. So
both transports produce the same validated `Scope` and nothing downstream knows which was used.

## Consequences
- Easier: one format, one algorithm, readable by existing gateways and debuggers, with a published
  best-practice document to test against; no JOSE library in core; hosts on any stack can mint
  with a three-line recipe (`header.payload` signed with Ed25519 by the platform crypto library).
- Harder: we own a small verifier and its tests (the fuzz targets and the negative-test list
  are the safeguard). Hosts using a library that cannot set `typ`/`kid` as specified sign the
  header themselves; the profile is written to make that easy.
- **Every request or stream open needs a fresh token** (single-use `jti`). The SDKs (M3) must
  take a token *provider* callback invoked per call, not a single token string, and the host's
  backend must be cheap to ask (a local signature, 41 µs). A browser client therefore gets
  tokens from its host backend per request. If that proves too costly in practice, a
  session-scoped credential would be a new ADR, not a flag on this one.
- **Session IDs come from the token** (`session_id`, a required scope level). M1-16's session
  creation must accept the ID the token carries, and every later token for the session must
  carry the same tenant, workspace, principal and session ID: `identity.CheckContinuity` is the
  check, and any difference is `scope_violation`. Roles and capability scopes may change between
  tokens (a refresh).
- Forbidden: shared-secret signing, any second accepted algorithm or spelling, trusting
  header-supplied keys or URLs, accepting a token from a query string, cookie or body, a
  default tenant or workspace, a deployment level read from a token, and using a token to carry
  credentials the model could see.

## Implementation notes (M1-22: issuer keys and key provisioning)
- Issuer keys are PUBLIC keys only, stored in a SQLite table `identity_issuer_keys` (module "identity", separate from the trust store's
  publisher keys), per application; revoked rows are kept and a revoked key id or material is never served again. The resolver reads the
  table on every token, so a CLI revoke applies to a running `serve` immediately.
- A key id is unique across the whole DEPLOYMENT (the resolver receives only the kid before the token is parsed; the application binding is
  the verifier's existing check) — two hosts cannot both use "key-1".
- `alixai identity add-issuer-key` reads the manifest's context-exchange block from the package FILE (not an installed package: installing
  needs a token, which needs a key) and refuses a key id or issuer the manifest does not declare. Published TEST-ONLY keys are refused
  without an explicit developer flag; the TEST-ONLY list is duplicated in identity (it may not import trust) and a test pins the two lists equal.
- Audit checkpoint signing key: `alixai keys init` creates `<data_dir>/keys/audit-checkpoint.key` (0600, dir 0700) and registers its public
  half in the trust store under `alixai.platform`; `serve` refuses to start with a configured key that is missing, loose, not Ed25519 PKCS#8,
  unregistered, revoked or mismatched, and runs unsigned (with a log line) when nothing is configured.

## Reversal plan
Format and verifier are confined to `internal/identity` behind `Authenticator`; adapters and the
policy gateway only ever see `contracts.Scope`. Changing the identifier (to `Ed25519`) or the
format (to PASETO) is a change to the verifier and its constants, the SDK mint helpers and the
conformance vectors; nothing downstream moves. Because tokens are short-lived, cutover is a
key-registry change plus waiting out one maximum lifetime, with no data migration. The replay
table holds only nonces and can be dropped as an incident step (at worst a token replayed inside
its remaining lifetime is accepted once more).
Reconsider PASETO only if the PASETO specification becomes standards-track *and* a host-side
adoption need appears.

## Sources (checked 2026-10-02)
- JWT best current practice: <https://datatracker.ietf.org/doc/html/rfc8725>
- Fully-specified JOSE algorithms: <https://www.rfc-editor.org/rfc/rfc9864.html>; joserfc: <https://jose.authlib.org/en/rfc/9864/>
- JWS with EdDSA: <https://www.rfc-editor.org/rfc/rfc8037.html>
- PASETO draft status (rev 01, expired 2022-11-25): <https://datatracker.ietf.org/doc/draft-paragon-paseto-rfc/>; specification and implementations: <https://paseto.io/>
- golang-jwt (`ed25519.go` on `main`: `Alg()` returns `"EdDSA"`): <https://github.com/golang-jwt/jwt>, <https://pkg.go.dev/github.com/golang-jwt/jwt/v5>
- PyJWT: issue <https://github.com/jpadilla/pyjwt/issues/1190>, PR <https://github.com/jpadilla/pyjwt/pull/1199>
- `jose` (JS): changelog entry "support fully specified Ed25519 algorithm identifier" in 5.10.0 and `src/lib/jws_algorithms.ts` on `main`: <https://github.com/panva/jose>
- `lestrrat-go/jwx` v4: <https://github.com/lestrrat-go/jwx/discussions/2040>; go-jose: <https://github.com/go-jose/go-jose>
- PASETO libraries: <https://github.com/panva/paseto>, <https://github.com/aidantwoods/go-paseto>, <https://github.com/auth70/paseto-ts>, <https://github.com/dajiaji/pyseto>
