# External providers: data, retention and billing terms

Status: **summary of the providers' public documentation, checked 2026-10-25.** Maintainer review, not legal advice.
**No provider account was created, and the conformance suite has NOT been run against any real provider endpoint.** What
passes is the offline conformance suite against recorded fixtures. Provider terms change without notice: read the
provider's current pages before you enable External mode, and record your own review on the provider profile (see
[provider terms policy](../policies/provider-terms.md)).

Back to the [verification index](README.md). The legal, residency and security review of External mode itself is
[external-mode-review.md](external-mode-review.md).

## 1. Which providers External mode supports

External mode is optional and **off by default**. The default binary builds four adapters; a binary built with
`-tags noexternal` has none.

| Provider id | Service | Endpoint the adapter calls | Credential |
| --- | --- | --- | --- |
| `openai` | OpenAI API, Responses API | `https://api.openai.com` (configurable origin) | API key |
| `anthropic` | Anthropic API (Messages) | `https://api.anthropic.com` | API key |
| `gemini` | Google Gemini API (AI Studio, `generativelanguage.googleapis.com`), not Vertex AI | `https://generativelanguage.googleapis.com/v1beta` | API key, or an OAuth token (no OAuth registration ships in 1.0, so a key in practice) |
| `meta` | Meta Model API (`https://api.meta.ai/v1`, Muse Spark models), or an operator-named third-party host of Llama models with an OpenAI-compatible API | as configured | API key |

## 2. Terms per provider

How to read this: each fact links the page it came from. Facts marked **(s)** were read only through a fetch tool's summary,
so the wording may be paraphrased; check them on the page. Quotes are verbatim from the page text.

### OpenAI API

Terms: [Services Agreement](https://openai.com/policies/services-agreement/) (updated 2025-12-01, effective 2026-01-01);
[data controls guide](https://developers.openai.com/api/docs/guides/your-data). The enterprise privacy page
(`https://openai.com/enterprise-privacy/`) answered HTTP 403 and **was not read**.

| Topic | What the provider says |
| --- | --- |
| Retention | "By default, abuse monitoring logs are generated for all API feature usage and retained for up to 30 days, unless longer retention is required by law". The Responses API keeps application state for 30 days by default or when `store` is `true`. AlixAI sends `store: false`; that does not change abuse-monitoring retention |
| Training on API data | "data sent to the OpenAI API is not used to train or improve OpenAI models (unless you explicitly opt in to share data with us)." Services Agreement 4.2: OpenAI will not use Customer Content to develop or improve the Services unless the customer explicitly agrees |
| Zero retention | Zero Data Retention and Modified Abuse Monitoring are "subject to prior approval by OpenAI", requested through sales, per organisation or project. Some endpoints keep state even under ZDR. Image and file inputs may still be scanned and kept for review **(s)** |
| Regions | data residency by project or by regional domain (US, Europe, Australia, Canada, Japan, India, Singapore, South Korea, UK, UAE) **(s)**; only some regions process in-region, and non-US regions need ZDR or Modified Abuse Monitoring approval **(s)**. Regional endpoints carry a 10% price uplift for models released on or after 2026-03-05 |
| Billing and spend controls | prepaid credits (Services Agreement 6.5); usage tiers with monthly limits set by OpenAI; spend alerts ("Spend alerts do not enforce a cap") and an optional **hard monthly limit** per organisation or project, which "is not instantaneous, so recorded spend can slightly exceed the configured amount" ([spend limits](https://developers.openai.com/api/docs/guides/spend-limits), [rate limits](https://developers.openai.com/api/docs/guides/rate-limits)) |

### Anthropic API

Terms: [Commercial Terms](https://www.anthropic.com/legal/commercial-terms) (effective 2025-06-17);
[API and data retention](https://platform.claude.com/docs/en/manage-claude/api-and-data-retention);
[data residency](https://platform.claude.com/docs/en/manage-claude/data-residency);
[rate limits](https://platform.claude.com/docs/en/api/rate-limits).

| Topic | What the provider says |
| --- | --- |
| Retention | inputs and outputs are deleted on the backend within 30 days **(s)** ([privacy article](https://privacy.claude.com/en/articles/7996866-how-long-do-you-store-my-organization-s-data)); content flagged under the usage policy up to 2 years, safety classification scores up to 7 years **(s)**. Batch API 29 days; Files API until deleted. Some named models "require 30-day data retention" |
| Training on API data | Commercial Terms: "Anthropic may not train models on Customer Content from Services." Retention docs: "Retained data is never used for model training without your express permission." |
| Zero retention | "To request ZDR for your organization, contact the Anthropic sales team." Covers the Messages and token counting APIs for eligible features; excludes, among others, the Console, the Batch and Files APIs and code execution. Flagged content may still be kept up to 2 years |
| Regions | `inference_geo` is `"global"` (default) or `"us"`, on recent models only; US-only inference "is priced at 1.1x the standard rate". Workspace storage geo: "`"us"` is the only available workspace geo." **No EU option is listed** |
| Billing and spend controls | prepaid credits (Commercial Terms H.1); a monthly spend cap per usage tier, which is a **hard cap** (usage pauses until the next month); an organisation or workspace may set a lower limit |

### Google Gemini API

Terms: [Gemini API Additional Terms](https://ai.google.dev/gemini-api/terms) (last updated 2026-04-28);
[zero data retention](https://ai.google.dev/gemini-api/docs/zdr); [billing](https://ai.google.dev/gemini-api/docs/billing).

| Topic | What the provider says |
| --- | --- |
| Paid vs unpaid | The API is a Paid Service "only when accessing the API through a Cloud Project associated with an active billing account." For **Unpaid Services**, Google uses submitted content "to provide, improve, and develop Google products and services and machine learning technologies", "human reviewers may read, annotate, and process your API input and output", and "Do not submit sensitive, confidential, or personal information to the Unpaid Services." |
| Retention (paid) | "Google logs prompts and responses for a limited period of time, solely for detecting and preventing violations of the Prohibited Use Policy." No number of days is given. Data "may be stored transiently or cached in any country in which Google or its agents maintain facilities." |
| Training on API data (paid) | "Google doesn't use your prompts ... or responses to improve our products"; processing under Google's data processing terms |
| EEA, Switzerland, UK | "You may use only Paid Services when making API Clients available to users in the European Economic Area, Switzerland, or the United Kingdom." |
| Zero retention | no request process on the page; achieved by not using features that store data (grounding with Search or Maps stores 30 days and cannot be disabled; stateful APIs, files and explicit caching store until deleted or expired). Google's own advice: "If your workload requires guaranteed zero data retention or enterprise data processing agreements, use Vertex AI." AlixAI has no Vertex AI adapter |
| Regions | an availability list of countries; **no data-residency option** for the Gemini API was found in Google's documentation |
| Billing and spend controls | prepaid or postpaid billing; tiers with monthly caps at billing-account level, at which service pauses **(s)**; per-project spend caps are labelled experimental and may be exceeded by about 10 minutes of usage **(s)** |

### Meta Model API

Terms: [Terms of Service](https://dev.meta.ai/legal/terms-of-service) (last updated 2026-10-09);
[data commitments](https://dev.meta.ai/legal/commitments);
[geographic use policy](https://dev.meta.ai/legal/geographic-use-policy);
[acceptable use policy](https://dev.meta.ai/legal/acceptable-use-policy);
[pricing and rate limits](https://dev.meta.ai/docs/pricing-rate-limits).

| Topic | What the provider says |
| --- | --- |
| Retention | Terms section 4: Meta retains content and usage data "as needed" to provide the services, for legal compliance, in connection with its rights to use content, when flagged, and "for security, safety, abuse, and policy review and improvements". **No default period is stated.** Content can be viewed and deleted in the account |
| Training on API data | Standard mode (the default): Meta "will not use Content submitted in a request to the Standard Services to train Meta Models". **Contributor mode** (opt-in, discounted, separate `-contributor` model ids): Meta may use the content to train its models, and "You must not submit sensitive, confidential, or personal information to the Contributor Services." |
| Zero retention | "available for qualified Meta Model API accounts", enabled by Meta on request; "ZDR support may vary by model"; flagged requests kept up to 2 years ([ZDR article](https://dev.meta.ai/help/policies-and-privacy/zero-data-retention)) |
| Regions | available in most countries where Meta AI and billing are; EU customers contract with Meta Platforms Ireland; **no data-residency option** is documented |
| Billing and spend controls | pay as you go by card. **No hard cap**: "Meta Model API doesn't currently offer a spending limit that places a hard cap on usage"; only an email alert, and "You're still responsible for paying any usage costs that exceed the spend limit alert amount" ([spend limit alert](https://dev.meta.ai/help/billing/spend-limit-alert)) |

**Third-party Llama hosts.** Meta states Llama models are not run through the Model API
([Llama and Model API](https://dev.meta.ai/help/about-model-api/llama-and-model-api)). For an operator-named
OpenAI-compatible host, every term above (retention, training, residency, billing) is the host's, and the Llama model
licence and acceptable use policy apply to the model. Neither was reviewed here because there is no default host.

## 3. What this means in practice

- **Gemini Unpaid Services train on your data and allow human review.** AlixAI cannot tell from a key whether its project
  has billing enabled. Use only a key from a Cloud project with active billing, and never for users in the EEA, Switzerland
  or the UK otherwise.
- **Meta has no hard spend cap.** AlixAI's budget (a call with no budget is refused, an exhausted budget stops calls) is the
  only hard stop on that account. Do not route Meta's `-contributor` model ids unless you accept training use.
- **OpenAI's hard monthly limit can be overshot slightly.** Keep AlixAI's budget below it.
- **Regions are narrow.** Anthropic offers only global or US inference and US storage; Gemini API and Meta document no
  residency option; OpenAI's regional options need approval. See [external-mode-review.md](external-mode-review.md).
- **Zero retention is never self-service** at OpenAI, Anthropic or Meta; at Google it means avoiding features, and Google
  points to Vertex AI for a guarantee.

## 4. What was verified, and what was not

| Item | State |
| --- | --- |
| Provider documentation | read on 2026-10-25, as above |
| Provider accounts | **none created** |
| Live conformance against real endpoints | **not run**, for any provider. Live mode needs a `-tags live` build, a flag and an environment variable together, and spends real money |
| Offline conformance | **passes** (2026-10-25): `go run ./tools/providerconf run --provider openai` and `--provider anthropic` against recorded fixtures on a loopback server (all 30 checks passed or not applicable, reports valid), and the Gemini and Meta adapters' conformance tests against their recorded fixtures (`go test ./adapters/external/...`) |
| OAuth consent with a real provider | not run |
| Region pinning against a real endpoint | not run; see the review for what each adapter can pin |

The profile records shipped with the Anthropic, Gemini and Meta adapters record retention, training use and region as `unknown`, and an operator registers an OpenAI profile from their own file. The egress policy denies an
unknown region, so a profile has to be reviewed and completed by the operator before any request can leave.
