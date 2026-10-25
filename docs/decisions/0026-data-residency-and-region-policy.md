# 0026 — Provider region and data-residency policy by jurisdiction

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01 (the mechanism); the jurisdiction rules are the owner's and are not decided here; finalised by X-04 (code) and X-H03 (rules)

## Context

The open decision asks for a provider-region and data-residency policy by jurisdiction. Two different things are
hiding in that sentence: a **mechanism** (profiles state where a provider processes, policy states where a tenant
allows data to go, the gate compares them) and **jurisdiction rules** (which regions are acceptable for which
tenant, which depends on law and contract). The mechanism is engineering and is decided here. The rules are the
owner's and need legal review, so no default list of allowed jurisdictions ships.

### Facts, with sources (fetched 2026-10-03; summarised by a fetch tool, not verified by AlixAI; these are provider statements, not legal advice)

| Provider | Statement |
| --- | --- |
| OpenAI | Data residency is configured per project; regions listed: United States, Europe (EEA and Switzerland), Australia, Canada, Japan, India, Singapore, South Korea, United Kingdom, United Arab Emirates; "the services will perform inference for your Customer Content in the selected region" where regional processing is supported; most non-US regions need approval for abuse-monitoring controls; not every endpoint supports every region (one live endpoint supports only the US and Europe); a 10 % uplift applies to data-residency endpoints for eligible models released on or after 2026-03-05 |
| Anthropic | Two independent settings: `inference_geo` per request or workspace default (`global`, the default, may run in any geography; `us` runs only in US infrastructure; reported back in `usage.inference_geo`) and a workspace geo for data at rest (only `us` exists, fixed at creation); US-only inference is 1.1 times the standard price for the newer models; on Amazon Bedrock and Google Cloud the region follows the endpoint or inference profile |
| Google Gemini API | Data "may be stored transiently or cached in any country in which Google or its agents maintain facilities"; users in the EEA, Switzerland and the UK may use paid services only |
| Meta | no hosted API to qualify (ADR-0021) |

## Options

| Option | Security | Cost | Verdict |
| --- | --- | --- | --- |
| A. AlixAI ships a per-jurisdiction allow list | claims legal judgement AlixAI cannot make | low | rejected |
| B. Profiles record region attributes, egress policy carries `allowed_regions` at each level, the gate compares, unknown is denied | the operator's own rule, enforced deterministically, most restrictive level wins | needs accurate profiles | **default** |
| C. Ignore regions, rely on provider terms | a region change is invisible | none | rejected: region change is a listed threat ([external-provider threats](../security/external-provider-threats.md)) |

## Decision

1. **A profile records `region`:** `residency` (`pinned`, `selectable`, `global` or `unknown`),
   `processing_regions` (region codes, or `global`) and `compliance` attestations. The operator supplies it from
   the provider's statements (authority `host`) and it is reviewed with the terms (ADR-0028).
2. **Policy carries `allowed_regions`** at deployment, application, tenant and workspace level; the most
   restrictive level wins (X-04). `allowed_regions` must list every region the provider may process in. A profile
   whose residency is `unknown` or `global`, or whose region list holds `global`, satisfies the policy only if
   `global` is listed explicitly; the literal `unknown` is not a region code and can never be allowed. **An
   unknown or unlisted region is denied** (`TestUnknownRegionDenied`, planned in X-04).
3. **A provider default that is wider than the policy needs a profile that narrows it.** Anthropic's
   `inference_geo` default of `global` is exactly such a case: for a tenant that allows only US processing, the
   profile must declare the pinned US setting and the adapter must send it. Each adapter task researches the
   concrete mechanism (a request parameter, a project setting, a regional endpoint) and records it with a date;
   an adapter that cannot pin a region declares `global` or `unknown`.
4. **A region change is an event.** `providerreg.Observe` quarantines a profile when the provider reports a
   different processing-region set (`region_changed`); a quarantined profile serves nothing. The call that lifts a
   `region_changed` quarantine is an open gap (ADR-0028, known gaps 3 and 4), to be settled with X-H03.
5. **Disclosure.** The egress disclosure lists the regions (schema `egress-disclosure`); the host can show them
   before data leaves.
6. **No default jurisdictions.** A deployment whose policy has no `allowed_regions` permits no external provider.
   Which regions are acceptable per tenant class is decided by the owner in X-H03 and recorded in
   `docs/evidence/x-legal-review.md`; until then this ADR is provisional.

## Consequences

Easier: residency is a property the gate checks, not a clause someone must remember. Harder: profile accuracy now
matters (a wrong `processing_regions` is a wrong gate), and provider region options change over time. Forbidden:
inferring a region from an endpoint hostname, treating `global` as acceptable by default, a hidden provider switch
to satisfy a region rule (a fallback must itself pass the same region check).

## What confirms or overturns this (owed)

- X-04: the region subset test and the unknown-region denial, with cross-tenant fixtures.
- X-H03: per-provider, per-region table of allowed jurisdictions by tenant class; review of provider terms and
  legal-policy exposure; sign-off or a list of changes.
- X-10 and X-19: the concrete region-pinning mechanism per adapter, dated.

## Reversal plan

The region fields are already in the schemas; the comparison lives in `internal/dataegress`. Dropping the
residency requirement is a policy change (permit `global` explicitly) with no data migration. A stricter rule
(per-source regions) would be a new schema version.
