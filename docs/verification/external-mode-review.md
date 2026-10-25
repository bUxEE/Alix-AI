# External mode: legal, data-residency and security review

Status: **maintainer review, not legal advice.** Done by the project maintainer for 1.0, not by a lawyer, a data-protection
officer or an independent security reviewer. It describes what the software does and where the risks are. Whether a given
deployment may send a given kind of data to a given provider is a decision for the operator and their own advisers.

**External mode is off by default.** With `external_providers.enabled` false (the default), no external adapter is built
into the running service, no connection is opened, and every `/v1/providers` and `/v1/oauth` route answers `unavailable`.
A binary built with `-tags noexternal` contains no external code at all. Everything below applies only after an operator
turns it on.

Back to the [verification index](README.md). Provider terms are summarised in [provider-terms.md](provider-terms.md); the
operator guide is [Operating External and Hybrid mode](../operations/external-mode.md); the threat model is
[External provider mode: threat-model addendum](../security/external-provider-threats.md).

## 1. Roles

- **The operator** runs AlixAI, opens the provider account, holds the API key, chooses the policy and pays the bill. For
  personal data, the operator (or the host application's owner) is the party that decides to send it to a provider.
- **The provider** receives and processes what is sent, under its own terms: its retention, training and region rules
  apply, not AlixAI's ([provider-terms.md](provider-terms.md)).
- **AlixAI** is software the operator runs. It sends nothing to the AlixAI project, has no telemetry, and has no account
  with any provider. It enforces the operator's policy deterministically before anything leaves.
- **The model never decides.** An external model is an interchangeable text backend: it never executes a tool, owns a
  permission or commits anything, and its output is validated like any untrusted input.

## 2. What leaves the machine, and under which settings

Nothing leaves unless **all** of these hold for the request: the mode at the deployment, application and tenant levels
allows it (the most restrictive level wins, an unset level counts as Local, the kill switch forces Local); a provider profile
is qualified (active credential, acknowledged billing notice, a policy review less than 30 days old, a passing conformance
report for the exact model version); the egress policy allows the provider, the model, the data class and the region at
every level; a budget applies and has room; and the destination is on the network allowlist, which is a separate gate.

| Data | Leaves when | Never leaves |
| --- | --- | --- |
| User message, conversation history, verified facts | External mode routes the role to a provider and the egress policy allows the data class | if the class is `restricted` or unknown |
| Hybrid mode wording requests | only a verified dialogue directive with facts of the class named in `hybrid_fact_class` | anything, while `hybrid_fact_class` is empty (the default) |
| RAG content | per source: `allow_metadata` sends titles and ids, `allow_excerpt` sends bounded authorised excerpts | sources set to `local_only` (the default for a source with no policy) or `deny`; a package can only restrict, never grant |
| Uploaded files (`files` setting) | only when every level (deployment, application, tenant, workspace) enables it, up to the lowest `max_class`; above `public` needs the `dataegress.approve` permission | **off by default** |
| Raw audio for an external recogniser (`audio` setting) | same rules as `files`, plus a session that states the audio's class; reserved on the budget by its length | **off by default**; audio with no class never leaves. No adapter ships an external speech client in 1.0 |
| Credentials, secrets, filesystem paths, database handles, raw application database content, MCP URLs | never | always |

Every egress decision, allowed or denied, is an audit record without content. Before a request, a host can call
`GET /v1/providers/{provider}/disclosure` to show the user the provider, model, data categories, the recorded retention and
training terms (`unknown` stays `unknown`), an upper-bound cost and the budget state.

**Redaction.** Before text leaves, deterministic rules remove secret-shaped strings (API key and token patterns, credential
reference ids), filesystem paths, database connection strings and MCP URLs, and replace them with placeholders. This is a
safety net under the typed data categories, not a substitute. **It does not detect personal data**: names, e-mail addresses,
health or financial details in a user's message or an excerpt are sent if their class is allowed. Classification of sources
and fields, not redaction, is what keeps personal data local.

## 3. Region and data residency

### What AlixAI enforces ([ADR 0026](../decisions/0026-data-residency-and-region-policy.md))

- Each provider profile records `residency` (`pinned`, `selectable`, `global` or `unknown`) and `processing_regions`. The
  operator supplies them from the provider's statements.
- The egress policy carries `allowed_regions` at the deployment, application, tenant and workspace levels; the most
  restrictive level wins.
- **An unknown or unlisted region is denied.** A profile whose residency is `global` or whose regions include `global` passes
  only if `global` is listed explicitly. `unknown` can never be allowed.
- **No default jurisdictions.** A deployment whose policy has no `allowed_regions` permits no external provider.
- A provider reporting a different processing region quarantines the profile (`region_changed`) until the operator releases
  it naming the regions they accept.

### What each adapter can actually pin (1.0)

No adapter sends a region parameter. What an operator can do:

| Provider | Region options offered by the provider | What AlixAI can pin | Honest profile setting |
| --- | --- | --- | --- |
| OpenAI | regional domains (for example `eu.api.openai.com`), project-level residency; non-US regions need provider approval | the origin, through `openai_base_url` (a bare origin is accepted and added to the network allowlist). **Not tested against a real regional endpoint** | `pinned` with that region only after the operator has the provider's approval and has checked the endpoint; otherwise `global` |
| Anthropic | `inference_geo` `global` (default) or `us`; storage US only | nothing: the adapter does not send `inference_geo` | `global` |
| Google Gemini API | none documented; data "may be stored transiently or cached in any country" where Google has facilities | nothing | `global` |
| Meta Model API | none documented | nothing | `global` |
| Third-party Llama host | the host's | the origin, through `meta_base_url` | whatever the host contractually commits to |

### Guidance per tenant class

AlixAI does not decide which jurisdictions are acceptable; that is each operator's legal question. Given the table above,
the maintainer's guidance for the common cases is:

| Tenant class | `allowed_regions` | What it means with 1.0 |
| --- | --- | --- |
| Must stay on premises (regulated, air-gapped, health, legal) | none | Local mode only. External mode is not needed for any feature |
| Personal data that must stay in the EU or EEA | an EU region code only | no shipped adapter can prove EU processing, so no profile qualifies; requests are denied. OpenAI's EU endpoint is the only candidate, once approved by OpenAI and checked by the operator |
| US-only processing | `us` | Anthropic would need `inference_geo: "us"`, which the adapter does not send, so it is denied; OpenAI's US region is the candidate, under the same conditions as above |
| No residency requirement, public or internal data only | `global` | all four providers can qualify; keep `allowed_classes` to `public` and `internal` and sources at `local_only` unless deliberately opened |

Cross-border transfer rules (for example GDPR chapter V) are not evaluated by AlixAI. If personal data is in scope, the
operator needs the provider's data processing terms and a transfer basis of their own.

## 4. Budgets and billing

- Every call needs an applicable budget: **a call with no budget is refused** (`no_budget`). There is no unlimited budget.
- Budgets are integer micro-units per day and per month (UTC), for a provider, tenant, workspace or principal, on the
  interactive or background channel. A call is reserved at an **upper-bound estimate** before it is sent and settled with the
  provider's reported usage. A used unit with no price makes the request unpriceable, so it is not sent.
- An exhausted budget stops external calls with `resource_exhausted` and **never switches to another provider**.
- The kill switch (a deployment operator) stops all external processing at once and survives a restart.
- The provider's invoice is the authority. AlixAI's estimate can differ from it, and prices change: re-review them with the
  terms. Where the provider has no hard cap (Meta), AlixAI's budget is the only hard stop; where the provider's own cap can be
  overshot (OpenAI), keep AlixAI's budget below it.
- A consumer chat subscription is not API access; the operator must acknowledge a billing notice saying so before a profile
  can be qualified.

## 5. Credentials

- A provider key lives only in the credential service: the operating system's secret store, or a JSON file with every value
  sealed by AES-256-GCM (fresh nonce per write, the slot and binding as authenticated data). Read-only references to an
  environment variable (`ALIXAI_SECRET_` prefix) or a mounted file are supported for headless installs.
- The host application, the browser, prompts, logs, URLs and errors never see the key: after submission only a masked
  fingerprint and a connection status exist. A manual key is validated against its own provider with one minimal request
  before it is stored.
- Every use, allowed or denied, is audited without the value. An invalid, expired or revoked credential fails closed and
  never selects another credential or provider.
- **As wired by `alixai serve` in 1.0**, the encrypted file's master key is a key file (`master.key`, owner-only) in the same
  `<data_dir>/external/secrets/` directory as the sealed file. Whoever can read the data directory can therefore decrypt the
  keys. Protect the data directory and its backups accordingly; prefer environment or mounted-file references, or a
  provider-side key with a low spend limit.

## 6. Security controls and their tests

The external-mode security suite (`go test ./tests/security/extprovider/...`) and the External-mode acceptance gates
(`tests/gates/x`) cover: providers cannot reach an application adapter without local validation; credentials never reach
clients or models; local-only sources are blocked deterministically; every external response goes through action, fact and
citation verification; cost, quota and failure state are visible; network loss leaves deterministic work and local search
usable; removing every provider returns to a fully local deployment; the `noexternal` build has no external code; unknown
regions are denied; budget, kill switch, tenant isolation, injection in provider output and retention. These run against
fakes and recorded fixtures. **None has run against a real provider endpoint.**

## 7. Residual risks

1. **Provider-side retention.** Abuse-monitoring logs (typically up to 30 days, longer for flagged content) cannot be turned
   off by AlixAI. Zero retention needs a contract with the provider.
2. **Gemini Unpaid Services** may use data for training and human review. AlixAI cannot tell whether a key's project has
   billing enabled.
3. **Region pinning is unverified** against real endpoints, and three of four providers offer no pinning AlixAI can use. A
   wrong `processing_regions` entry on a profile is a wrong gate: the operator's review is the control.
4. **Terms are recorded by the operator**, not fetched or verified by AlixAI. A profile goes stale after at most 30 days and
   is quarantined; a changed terms hash quarantines it too, but only if the operator records the new hash.
5. **Redaction does not catch personal data** (section 2).
6. **Master key next to the sealed credentials** (section 5).
7. **Cost estimates are upper bounds from an operator-maintained price table**; a provider price change between reviews makes
   them wrong in either direction.
8. **Live behaviour is untested**: OAuth consent, streaming edge cases, provider error shapes and rate limits were exercised
   only against recorded fixtures.
9. **Prompt injection through provider output** is contained by validation (a provider never gains authority), but a provider
   can still return misleading text that passes as a well-formed reply in Hybrid wording; the fact verifier and template
   fallback limit, not eliminate, that.

## 8. What would change this review

A live conformance run per provider with a real account, a region-pinning parameter in the Anthropic adapter, a key-derivation
or OS-store master key in the default composition, and a provider terms change. Each would be recorded in a later release.
