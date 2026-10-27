# Domain scope, knowledge and web use

Design: [ADR 0047](../decisions/0047-analyst-turns.md). Companion: [analyst-turns.md](analyst-turns.md).

## 1. Declaring the domain

The application package may add an additive `assistant` section:

```yaml
assistant:
  domain:
    description: "Meetings, minutes, companies and Italian company governance."
    in_scope:
      - company meetings and minutes
      - corporate incentives and filings
      - governance and related law/practice
    out_of_scope:
      - general chatbot use
      - unrelated consumer topics
    examples:
      en:
        in_domain: ["Should we apply for incentive XX?"]
        out_of_domain: ["Is a Persian cat big?"]
      it:
        in_domain: ["Dobbiamo candidarci all'incentivo XX?"]
        out_of_domain: ["Il persiano è un gatto grande?"]
    knowledge_allowed: true
    web_search_allowed: true
    preferred_web_hosts: [gov.it, europa.eu]
  abilities:
    loop: true
    compute: true
    knowledge: true
    web_search: true
    web_fetch: true
```

Schema: `schemas/analyst/domain.schema.json`, wired from
`schemas/application-package/manifest.schema.json` (`assistant`).

Without `assistant.domain`, in-domain answers that match no capability stay unavailable
(classifier / knowledge / web remain off for that application).

## 2. Scopegate and domain classification

- **scopegate** (`internal/dialogue/scopegate`) stays a cheap hard refusal for clearly
  unrelated utterances (no word overlap with the package).
- When analyst `loop` is effective and a domain is declared, the strong model also returns a
  typed **domain classification** (`in_domain` | `out_of_domain` with reason).
- `out_of_domain` → polite typed refusal (`out_of_domain`); no search, no read, no essay.
- In-domain questions that match **no** capability may still be answered via the analyst loop
  (reads, compute, knowledge, web as enabled).
- Attempts to steer outside the domain via record text or web pages stay out-of-domain /
  fail closed (required tests).

## 3. Source labels

Every claim span or block may carry a source label (`schemas/analyst/source-span.schema.json`,
optional `source` on rich blocks):

| Label | Meaning | UI treatment (kits) |
| --- | --- | --- |
| `app` | Verified read fact from the host | Verified / from your data |
| `computed` | Core-derived fact with provenance | Calculated |
| `web` | Cited fetched page (`WebCitation`) | Web source + link metadata |
| `knowledge` | Model general knowledge | General information (not verified) |

Verifier rules:

- A number about app data must be `app` or `computed`.
- A `web` number must appear in its cited fetched text.
- `knowledge` must not state figures that look like the tenant’s own data (replaced if it does).

## 4. Web search and fetch

Abilities `web_search` / `web_fetch`, off by default. Query egress category **`web_query`**:
the core rejects queries that embed confidential/restricted values from datasets read in the
turn (`web_query_refused`). Results are untrusted and fenced. No credentials or cookies are
sent. Per-tenant allow/deny hosts; package `preferred_web_hosts` intersect with tenant policy.

## 5. Drafts

Block type `draft` (title, format markdown|plain, body). Quoting app data follows the same
tracing rules. Persisting a draft is a host capability (= action path), never automatic.
