# Contributing to AlixAI

Thank you for helping. AlixAI is an open-source, strictly self-hosted AI service that an
application embeds for local text and voice conversation, scoped document RAG and safe
natural-language control. This guide covers how to propose a change, what a good change looks like
and what you are agreeing to when you submit one.

By taking part you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md). To report a
vulnerability, **do not open an issue**: follow [SECURITY.md](SECURITY.md). How decisions are made
and who can merge is described in [GOVERNANCE.md](GOVERNANCE.md).

## Licence of contributions

AlixAI is licensed under the [Apache License 2.0](LICENSE). Contributions use the same terms,
**inbound = outbound**:

- Unless you explicitly state otherwise, anything you intentionally submit for inclusion in the
  project is licensed under Apache-2.0, as section 5 of the licence provides. There is nothing
  further to sign.
- There is **no Contributor License Agreement and no DCO sign-off requirement.** A
  `Signed-off-by:` line is accepted but is never required or checked.
- Submit only work you have the right to contribute under Apache-2.0. Do not paste code, schemas,
  prompts, model outputs, test data or documentation whose licence you cannot confirm is compatible.
  Material copied from another project must keep its licence notices and must be permissively
  licensed (see [Dependencies](#dependencies)).
- You keep the copyright in your contribution. The project's notices say "The AlixAI Authors".

The project name and logo are **not** licensed by Apache-2.0. See the
[trademark placeholder](docs/governance/trademark-placeholder.md).

## Ground rules

Every change is held to the project's non-negotiable rules. The ones that come up most in review:

1. **Self-hosted, no telemetry.** Nothing may call an AlixAI-operated service, phone home or require
   a paid or hosted dependency. Metrics stay local unless the operator exports them.
2. **Deterministic authority.** The model never owns permissions, business facts, validation,
   conflict resolution or commits. Natural language never becomes a database write, shell command
   or external side effect directly: it becomes a typed, validated, prepared action.
3. **Fail closed.** Missing permission, uncertain identity, invalid schema, stale prepared action or
   unavailable source means no execution. Never map a failure to a success-looking result.
4. **Tenant isolation.** Every request carries an application, tenant, workspace and principal scope.
   No scope is an error, never a default.
5. **Least capability.** A model gets only the tools, documents and fields the current authorised
   task needs.
6. **Efficiency is a feature.** Evaluate the smallest deterministic method first, larger models only
   when the measured gain justifies the cost, and record the CPU, RAM, storage and latency impact
   (see [Efficiency review](#efficiency-review)). The 4 GB profile must stay possible.
7. **Headless.** AlixAI ships no dashboard or end-user UI before the deferred UI kits.

## Before you start

- **Search the tracker** for an existing issue or discussion.
- **Small fixes** (typos, docs, an obvious bug with a test) can go straight to a pull request.
- **Anything larger** (new capability, new dependency, public contract change, new provider or
  model, security-relevant behaviour) needs an issue first so the approach can be agreed. A material
  change also needs an ADR (below). Opening a pull request without discussion risks it being declined.
- Check the [verification status page](docs/verification/README.md) and the open issues to see
  whether the area you want to change is already being worked on.

## Development setup

Requirements: Git and the Go version in [`go.mod`](go.mod). Node.js with pnpm is needed only for the
TypeScript SDK and reference apps, and Python 3.11 or newer only for the Python SDK. There is no
`make`; developer tools run as `go run ./tools/<name>` or `node tools/<name>/...`.

AlixAI is **pure Go**. Build and test with cgo disabled, exactly as CI does:

```bash
export CGO_ENABLED=0
go build ./...
test -z "$(gofmt -l $(go list -f '{{.Dir}}' ./...))"
go vet ./...
go test ./...
```

The race detector needs cgo and a C compiler, so it runs in Linux CI only:
`CGO_ENABLED=1 go test -race ./...`. Tests that need Docker, LocalAI, models or the network sit
behind the `live` build tag (`//go:build live`) and are never part of the default run.

CI also runs, and your change must pass:

```bash
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go run github.com/google/go-licenses@latest check ./... \
  --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0
```

The repository checks must also stay green:

```bash
go run ./tools/schemagen --check   # generated Go/TypeScript types match the schemas
go run ./tools/compat check        # the frozen 1.0 contracts are unchanged
go run ./tools/doclint docs        # documentation consistency rules
pnpm install --frozen-lockfile && pnpm -r test
cd sdk/python && python -m pytest
```

Line endings are LF (see `.gitattributes`).

## Making a change

1. **Fork** the repository on GitHub, clone your fork, and add the upstream remote:

   ```bash
   git clone https://github.com/<you>/Alix-AI.git
   cd Alix-AI
   git remote add upstream https://github.com/bUxEE/Alix-AI.git
   ```

   Create a **branch** from an up-to-date `main` (`git fetch upstream && git switch -c my-change upstream/main`)
   and keep the change focused: one concern per pull request.
2. **Follow the conventions** of the code around you: repository layout, Go
   style (`gofmt`, `go vet` clean, stdlib `net/http` and `log/slog`, `ctx` and scope on every
   request-scoped function, injected clock and ID source, typed error codes wrapped with `%w`),
   per-module migrations, one file per CLI subcommand and per HTTP route area, and schemas as the
   source of truth.
3. **Write tests.** Stdlib `testing`, table-driven, hermetic by default, `go-cmp` for diffs. A bug
   fix comes with a test that fails without it. Tests for security-relevant behaviour must exercise
   the denial path, not only the allowed one.
4. **Schemas are the contract.** JSON Schema 2020-12 under `schemas/<area>/`, IDs of the form
   `urn:alixai:schema:<area>:<name>:v1`. Go and TypeScript types are generated from them and the
   generated output is committed. Do not hand-edit generated files.
5. **Docs.** Update the relevant file under `docs/` when behaviour, configuration or operations
   change.
6. **Commit** with a short imperative subject line and a body that says why. Keep history readable;
   squash fix-up commits before review.
7. **Push** the branch to your fork and **open a pull request** against `bUxEE/Alix-AI:main`. Fill in the template. Describe what changed and why, how you tested
   it, and any resource impact.

### Architecture decision records

A change that is hard to reverse or that shapes other work needs an ADR in
[`docs/decisions/`](docs/decisions/), written from [`TEMPLATE.md`](docs/decisions/TEMPLATE.md) as
`NNNN-title.md`. This includes new or changed: public contracts and schemas, persistence or queue
design, security and authority model, runtime or model providers, dependencies with licence or
footprint implications, resource profiles, and anything that touches the non-negotiable rules.
The ADR is part of the pull request that makes the change, and [GOVERNANCE.md](GOVERNANCE.md)
describes how it is accepted.

### Efficiency review

Every new feature, model, provider, integration or requested behaviour needs an efficiency review
before it counts as done: copy [`docs/efficiency/TEMPLATE.md`](docs/efficiency/TEMPLATE.md) to
`docs/efficiency/<id>.md` and fill every section. Start with the no-model option, then the smallest
candidate, and record its measured RAM, latency and storage cost and what it does to the 4 GB profile.

### Dependencies

Add a dependency only when it earns its place against the standard library and against writing a few
lines yourself. Each new Go module must:

- carry one of the allowed licences: Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC or MPL-2.0
  (CI enforces this); any other licence, including GPL, LGPL and AGPL, fails the check unless an
  ADR changes the allow-list;
- be pure Go (no cgo) unless an ADR says otherwise;
- be pinned in `go.mod` and `go.sum`, with no network access or runtime download at start-up;
- pass `govulncheck`, and be justified in the pull request by what it saves and what it costs in
  binary size and memory.

The core must never require a closed component. GitHub Actions are pinned by full commit SHA with a
version comment; CI fails on an unpinned reference.

### Public contracts and versioning

Public contracts (`pkg/contracts`, `schemas/`, the OpenAPI spec, the SDKs and the CLI and
configuration surface) follow semantic versioning. A breaking change to a published contract needs
a new schema or API version, an ADR and a migration note; additive changes do not. Say in your
pull request which contracts you touch.

## Review

A maintainer reviews every pull request. Expect questions about authority boundaries, failure
paths, tenant scope and resource cost before style. A pull request is ready to merge when CI is
green, review comments are resolved, tests and docs are included and any ADR or efficiency review
it needs is present. Maintainers may ask you to split a change or may decline one that does not fit
the project's direction, and will say why.

## Reporting bugs and requesting features

Use the issue forms. Include the version or commit, the deployment profile, and what you expected
versus what happened. For a feature request, describe the problem before the solution and the
resource cost you expect. Never include secrets, tokens or private data in an issue.

Security vulnerabilities go through [SECURITY.md](SECURITY.md), never the public tracker.
