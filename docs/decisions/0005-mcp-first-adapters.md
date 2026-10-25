# 0005 — MCP-first application adapters, policy stays in AlixAI

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
AlixAI controls a host application through typed capabilities. The host exposes those operations
over some transport. Integrators already have, or are building, MCP servers and OpenAPI
descriptions; asking each to adopt a bespoke protocol would make adoption expensive. But a
transport is not a security model: MCP tool lists and OpenAPI operations say what can be called,
not what this principal may do now.

## Options
- **MCP first, OpenAPI/HTTP and native adapters supported, event adapter for invalidation.**
  Broad compatibility and typed tool discovery. Risk: MCP servers can add or change tools, and a
  model handed a whole server can reach everything on it.
- **OpenAPI/HTTP only.** Simple and widely available, but loses tool discovery and ties
  capabilities to REST shapes. Still supported as a first-class adapter.
- **Native SDK bridge only.** Tightest integration and lowest latency, but requires a bridge per
  language and framework. Still supported where local IPC is needed.
- **Give the model direct access to the host's MCP server or API.** Fewest moving parts and
  rejected outright: it bypasses policy, scoping and prepare/confirm/commit (rules 5, 6, 7).

## Decision
MCP is the preferred transport for capability discovery and typed tool exchange. OpenAPI/HTTP and
native adapters are supported, and an event adapter receives application events to invalidate
state; events never trigger a commit. All of them implement one adapter contract in
`internal/adapters` and are mapped to AlixAI capability contracts in the application package.

MCP and OpenAPI are transports and schemas, not the policy layer. AlixAI filters and wraps the
individual tools an application package approves; it never gives a model unrestricted access to a
whole MCP server. Each connector has an explicit egress allowlist. There is no arbitrary MCP
registration, agent hub or shell tool in production. Tool lists are inventoried and pinned at
activation; a schema change after activation is a re-audit trigger, not a silent update.

## Consequences
- Easier: most integrators can reuse an existing MCP server or OpenAPI document.
- Easier: policy, scoping, risk floors and the action protocol ([0006](0006-prepare-confirm-commit.md))
  are identical whichever transport carries the call.
- Harder: every adapter must pass the shared conformance suite; MCP tool and schema drift needs
  detection (§16.4 lists malicious MCP servers and schema changes).
- Forbidden: exposing an unfiltered MCP server to a model; registering MCP servers at runtime
  outside an application package; any adapter reaching a destination that is not allowlisted.

## Reversal plan
Adapters sit behind `internal/adapters`; concrete transports live in `adapters/{mcp,openapi,native}`
and are wired only in composition roots. Demoting MCP from first to equal means changing
documentation and the default in new application packages, not code. Removing an adapter requires
its applications to migrate to another transport; capability contracts do not change.
