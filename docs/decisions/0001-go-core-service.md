# 0001 — Go for the core service

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
The core is a long-running local service that must stay small enough for the Lite profile (8 GB
certified, 4 GB aspirational) while sharing the machine with a model runtime. It must run as one
native binary on Linux, Windows and macOS, handle concurrent sessions, background jobs and
streaming, and be easy for an operator to install and supervise. No always-on extra services are
acceptable in the minimum profile (rule 12). The build runs on a dev box with no C compiler.

## Options
- **Go.** Single static binary, goroutines for sessions, jobs and streams, `net/http` and
  `log/slog` in the standard library, simple cross-compilation with `CGO_ENABLED=0`. Memory-safe,
  small standard-library attack surface, `govulncheck` available. Toolchain licence: BSD-3-Clause.
  Idle footprint of `alixai serve` is not measured yet (see Decision).
- **Node.js / TypeScript (Koa).** The owner's default stack for web services and the language of
  the SDK. Needs a Node runtime on the host or a bundled one, a larger baseline footprint, and an
  external store for anything beyond in-process state (the default MongoDB is not embeddable).
  Rejected for the core because it forces extra resident components into the minimum profile.
- **Rust.** Smallest possible footprint and strong safety. Slower to build and onboard for a
  surface that is mostly HTTP, JSON Schema and adapters. Not rejected on merit; rejected on
  delivery cost, with no measured need for the extra headroom.
- **Python (FastAPI).** Fast to write, but needs an interpreter and dependency environment on the
  host, uses more memory per process and distributes poorly as a single binary. Kept for the
  Python SDK and one reference app only.
- **JVM (Kotlin/Java).** Mature, but a heavier resident runtime and slower start; works against the
  4 GB direction.

## Decision
The core service and CLI are written in Go, as one binary, pure Go (`CGO_ENABLED=0`, see
[0009](0009-pure-go-sqlite-driver.md)), using the standard library for HTTP and logging and no web
framework. TypeScript is used for the SDK and reference apps; Python for the Python SDK and one
reference app. This deliberately deviates from the personal default stack (Node/Koa/MongoDB), and
that deviation is recorded here.

Measurement owed: idle RSS and cold-start time of `alixai serve` with store and gateway, recorded
in the M0-15 resource budget (`docs/operations/resource-budgets.md`). If the idle footprint alone
threatens the 4 GB direction, this ADR is revisited rather than worked around.

## Consequences
- Easier: one artifact to ship, sign and supervise; cheap concurrency; uniform cross-compilation.
- Easier: SDKs and reference apps talk to a stable HTTP API and JSON Schemas, so the core language
  is invisible to integrators.
- Harder: no cgo means every dependency must be pure Go (SQLite driver, parsers); `-race` runs in
  Linux CI only because it needs cgo.
- Forbidden: a second resident runtime (Node, Python, JVM) in the core process model.

## Reversal plan
The public boundary is the HTTP API, the JSON Schemas under `schemas/` and the generated types,
not Go packages (`internal/` is private to the module). A rewrite of the core in another language
keeps the SDKs, schemas, conformance suite and deployment layout. Persistent data is a SQLite file
([0002](0002-sqlite-wal-core-store.md)), readable from any language. Cost is a full
re-implementation of `internal/*`; no data migration is needed.
