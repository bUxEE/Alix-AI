# Example application packages

Fixtures for the package loader (`internal/apppkg`, task M1-04) and for people who want to see what a
package looks like. The format is specified in `docs/integration/application-package.md`.

| Directory | What it is |
| --- | --- |
| `valid-minimal` | A complete, signed, valid package for the synthetic `demo-library` application: two capabilities (a read-only search and a reservation that is prepared, confirmed and committed), conversation contracts in English and Italian, documentation, policy, terminology, one RAG source and a test suite. |
| `egress-local-only`, `egress-allow-excerpt` | `valid-minimal` plus egress declarations (external provider mode): a restricted source that stays `local_only` with a `local_only`, confidential field, and a public source that allows excerpts. Both stay valid; the loader never lets them leave anything by itself. |
| `broken-restricted-excerpt` | A restricted source that declares `egress_mode: allow_excerpt`: critical `egress.restricted_source_exposed`. |
| `broken-*` | `valid-minimal` with exactly one defect each: unsigned, changed after signing, a missing documentation facet, a missing conversation contract, a capability with no adapter mapping, a RAG source with no scope, missing safe-execution probes and an incompatible core version. |

```bash
go run ./cmd/alixai package validate examples/packages/valid-minimal      # exit 0
go run ./cmd/alixai package validate examples/packages/broken-tampered    # exit 1, findings as JSON
```

Everything here is synthetic. The packages are signed with the published TEST-ONLY publisher key in
`tools/devkeys`: anyone can sign with it, no real trust store accepts it, and `package validate`
only trusts it with `-allow-test-keys`.

The signature covers every byte, so after editing `valid-minimal` sign it again and regenerate the
broken ones, which `go test ./internal/apppkg` checks:

```bash
go run ./cmd/alixai package sign -app demo-library -version 1.0.0 \
  -key alixai-test-only-publisher=tools/devkeys/test-only-publisher.key.pem \
  -signed-at 2026-10-01T00:00:00Z -expires-at 2036-10-01T00:00:00Z examples/packages/valid-minimal
go test ./internal/apppkg -run 'TestBrokenFixtures|TestEgressFixtures' -update-fixtures
```
