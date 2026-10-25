<!--
Thank you for contributing. Read CONTRIBUTING.md first.
Security vulnerabilities do NOT go in pull requests: see SECURITY.md.
Contributions are licensed under Apache-2.0 (inbound = outbound). No DCO or CLA is required.
-->

## What and why

<!-- What does this change, and why? Link the issue it closes (Closes #123). -->

## How it was tested

<!-- Commands run and what they showed. For security-relevant behaviour, name the test that
     exercises the denial path. -->

## Contracts and decisions

- [ ] No public contract changes, or: I list them below and note whether the change is breaking
      (`pkg/contracts`, `schemas/`, OpenAPI, SDKs, CLI or configuration surface).
- [ ] No ADR needed, or: the ADR is included (`docs/decisions/NNNN-title.md`).

## Resource impact

<!-- CPU, RAM, storage, latency and concurrency effect, and the effect on the 4 GB profile.
     "None" is a valid answer for a docs-only or trivial change; say why. -->

- [ ] Efficiency review recorded (`docs/efficiency/<id>.md`), or not needed because this adds no
      feature, model, provider, integration or behaviour.

## Checklist

- [ ] CI is green: `gofmt`, `go vet`, `go test` (with `CGO_ENABLED=0`), `govulncheck`, licence check.
- [ ] Tests added or updated; hermetic by default (anything needing network, Docker, LocalAI or
      models sits behind the `live` build tag).
- [ ] Docs updated where behaviour, configuration or operations changed.
- [ ] No telemetry, no call to an AlixAI-operated service, no paid or hosted dependency.
- [ ] No secrets, tokens, real personal data or model weights in code, fixtures or logs.
- [ ] Authority, scope and failure paths preserved: no model-owned permission or write, no default
      scope, and every failure is an error, never a success-looking result.
- [ ] New dependencies are pure Go, pinned, and use an allowed licence (Apache-2.0, MIT, BSD-2-Clause,
      BSD-3-Clause, ISC, MPL-2.0).
- [ ] I have the right to contribute this work under Apache-2.0.
