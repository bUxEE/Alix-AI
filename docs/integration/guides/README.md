# Integration guides

Status: written in task M6-05.

These guides are for the people who put an application on AlixAI: the operator who installs and audits it, and the developer of the host application that calls it.

| Guide | Read it when |
| --- | --- |
| [Readiness discovery](readiness.md) | You are about to integrate a project and want to know what it already provides and what is missing, before any package is signed. Advisory; it is not an audit pass. |
| [Getting started](getting-started.md) | You want to go from nothing to an audited, active application. The worked example is the clinic-booking reference application. |
| [Light integration (external AI, no local LLM)](light-integration.md) | You want a signed help / FAQ chat assistant in light External mode. Starts from `examples/packages/valid-help-assistant`. |
| [CopilotKit and AG-UI](copilotkit.md) | You want a chat UI in front of AlixAI from a Next.js, Express, Go or Python host. |

Reference pages these guides point to: [application package format](../application-package.md), [conversation objects](../conversation-objects.md), [AG-UI mapping](../agui.md),
[CopilotKit compatibility and version matrix](../copilotkit-compatibility.md), [External provider mode](../external-mode.md), [light External deployments](../../operations/guides/light-external.md), [operator commands](../../operations/guides/operator-commands.md).

## What each SDK has

The three SDKs speak the same `/v1` operations for the core path, and they differ beyond it. Nothing is claimed that a test does not show; "no" means the module does not exist in that SDK today. Every
code sample in the SDK READMEs is copied from a test of that SDK.

| Feature | TypeScript (`@alixai/sdk`, `sdk/typescript`) | Python (`alixai`, `sdk/python`) | Go (`pkg/sdk-go`) |
| --- | --- | --- | --- |
| Sessions, streamed and JSON turns, confirmations, actions, applications, capabilities, health | yes | yes (sync and async) | yes |
| Single-use context token callback, no caching, no retries | yes | yes | yes |
| Resume a turn's events from a cursor | yes, opt-in (`resume`) | **no** | yes, on by default (`MaxResumes`) |
| WebSocket transport for the event stream | yes, through a host-supplied factory | **no** | yes, per call (`WithWebSocket`) |
| File ids on a turn | yes | yes | yes |
| Upload, download, preview of scoped files; message composer with attachments | yes | **no** | **no** |
| Conversation helper (stop, regenerate, edit-and-resubmit, switching) | yes | **no** | **no** |
| History, privacy and settings | yes | **no** | **no** |
| RAG: sources, search, grounded answers, citations | yes (`rag`) | **no** | yes (`RAG()`) |
| Voice | yes (`voice`, M4-07). **Full-duplex voice is not claimed** | **no** | **no** |
| Providers, modes, budgets (External provider mode; opt-in, off by default) | yes (`providers`, X-14) | yes (`client.providers`) | yes (`Providers()`) |
| Decision status (read-only, advisory; the layer is optional, bounded, non-generative and non-authoritative) | yes (`decision`, D-10) | yes (`get_decision_status`) | yes (`Decision()`) |
| AG-UI and CopilotKit | not in the SDK: the optional `@alixai/copilotkit-adapter` (`sdk/copilotkit-adapter`) | yes (`alixai.agui`, an ASGI proxy) | yes (`pkg/sdk-go/agui`, a `net/http` proxy) |
| Fake server for host tests | `@alixai/sdk/testing` | `alixai.testing` | `sdktest` |
| Runtime dependencies | none | `httpx`, `pydantic` | standard library and one WebSocket library |

Not in any SDK: the operator-only decision configuration routes. None of the SDKs depends on CopilotKit; none sends telemetry.

## Status and promises

- The v1 public contracts are frozen (`schemas/_frozen/v1`, checked by `tools/compat`); the SDKs speak them.
- **1.1.0 is released.** The packages and changelogs read `1.1.0` ([TypeScript](../../../sdk/typescript/CHANGELOG.md), [Python](../../../sdk/python/CHANGELOG.md), and the changelog section of the [Go README](../../../pkg/sdk-go/README.md)).
- **Support windows and notice periods** are in [compatibility](../../policies/compatibility.md) and [migration and support](../../policies/migration-and-support.md); these guides repeat none of them and commit to nothing.
- An SDK is tested against the core of the same repository commit.
- External providers are opt-in and off by default; the Lite profile is uncertified; the figures elsewhere in the repository are labelled emulated until measured on the target host.
