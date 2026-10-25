# Dependency licences

What AlixAI's own code links or installs at run time, and under which licence. Checked by the maintainer on 2026-10-25 for
1.0; not legal advice. Models, the LocalAI runtime and the decision layer are covered in [model-licences.md](model-licences.md)
and [decision-layer-licences.md](decision-layer-licences.md).

**Outcome:** every runtime dependency is under a permissive licence (Apache-2.0, MIT, MIT-0, BSD-3-Clause, ISC, PSF-2.0) or,
for `certifi` in the Python SDK, MPL-2.0 (file-level copyleft: it applies only to certifi's own files, which AlixAI does not
modify). Nothing is GPL, LGPL or AGPL. AlixAI itself is Apache-2.0 ([LICENSE](../../LICENSE), [NOTICE](../../NOTICE)).

## 1. Go server and CLI (`alixai`, `alixai-parser`)

Built statically from the modules below (`go-licenses report ./...`). CI enforces the allow-list Apache-2.0, MIT,
BSD-2-Clause, BSD-3-Clause, ISC, MPL-2.0 on every change. Two modules are checked by hand because the classifier does not
recognise their licence text: `modernc.org/mathutil` (BSD-3-Clause) and `github.com/segmentio/asm` (MIT No Attribution
since v1.2). The Go standard library and toolchain are BSD-3-Clause.

| Module | Licence |
| --- | --- |
| `github.com/aymerick/douceur` | MIT |
| `github.com/coder/websocket` | ISC |
| `github.com/danieljoos/wincred` | MIT |
| `github.com/dustin/go-humanize` | MIT |
| `github.com/getkin/kin-openapi` | MIT |
| `github.com/google/go-cmp` | BSD-3-Clause |
| `github.com/google/jsonschema-go` | MIT |
| `github.com/go-openapi/jsonpointer` | Apache-2.0 |
| `github.com/gorilla/css` | BSD-3-Clause |
| `github.com/mattn/go-isatty` | MIT |
| `github.com/microcosm-cc/bluemonday` | BSD-3-Clause |
| `github.com/modelcontextprotocol/go-sdk` | Apache-2.0 |
| `github.com/ncruces/go-strftime` | MIT |
| `github.com/oasdiff/yaml` | MIT |
| `github.com/oasdiff/yaml3` | MIT |
| `github.com/oklog/ulid` | Apache-2.0 |
| `github.com/remyoudompheng/bigfft` | BSD-3-Clause |
| `github.com/santhosh-tekuri/jsonschema` | Apache-2.0 |
| `github.com/segmentio/asm` | MIT-0 (checked by hand) |
| `github.com/segmentio/encoding` | MIT |
| `github.com/yosida95/uritemplate` | BSD-3-Clause |
| `github.com/yuin/goldmark` | MIT |
| `github.com/zalando/go-keyring` | MIT |
| `golang.org/x/net` | BSD-3-Clause |
| `golang.org/x/oauth2` | BSD-3-Clause |
| `golang.org/x/sync` | BSD-3-Clause |
| `golang.org/x/sys` | BSD-3-Clause |
| `golang.org/x/text` | BSD-3-Clause |
| `golang.org/x/time` | BSD-3-Clause |
| `gopkg.in/yaml.v3` | MIT |
| `modernc.org/libc` | MIT |
| `modernc.org/mathutil` | BSD-3-Clause (checked by hand) |
| `modernc.org/memory` | BSD-3-Clause |
| `modernc.org/sqlite` | BSD-3-Clause |

The release workflow attaches a CycloneDX SBOM to every binary, listing the exact module versions of that build.

## 2. TypeScript SDK and UI kits

`@alixai/sdk`, `@alixai/voice-orb` and the Vue and React kits have **no third-party runtime dependencies**; they depend only on
each other. The host application installs the peer dependencies itself:

| Peer dependency | Used by | Licence |
| --- | --- | --- |
| `vue` | `@alixai/vue` | MIT |
| `react`, `react-dom` | `@alixai/react`, `@alixai/copilotkit-adapter` | MIT |
| `@copilotkit/react-core`, `@copilotkit/react-ui`, `@copilotkit/runtime` | `@alixai/react`, `@alixai/copilotkit-adapter` (optional) | MIT |
| `@ag-ui/client`, `@ag-ui/core` | `@alixai/react`, `@alixai/copilotkit-adapter` | MIT |
| `rxjs` | `@alixai/react`, `@alixai/copilotkit-adapter` | Apache-2.0 |

CopilotKit pulls in `@scarf/scarf`, an install-time analytics package; the workspace refuses its install script
(`pnpm-workspace.yaml`, ADR 0035), and AlixAI adds no telemetry. Development dependencies (Vite, Vitest, TypeScript, Sass,
jsdom, ...) are not shipped.

## 3. Python SDK (`alixai`)

The runtime closure on Python 3.11 and later:

| Package | Licence |
| --- | --- |
| `httpx` | BSD-3-Clause |
| `httpcore` | BSD-3-Clause |
| `h11` | MIT |
| `anyio` | MIT |
| `idna` | BSD-3-Clause |
| `certifi` | MPL-2.0 |
| `pydantic`, `pydantic-core` | MIT |
| `annotated-types` | MIT |
| `typing-extensions` | PSF-2.0 |
| `typing-inspection` | MIT |

`certifi` is a direct dependency with a floor of 2026.10.23, the release that removes the compromised eMudhra intermediate
CAs (CVE-2026-111943), because `httpx` verifies TLS against its bundle.

## 4. Container images

| Image | Role | Licence |
| --- | --- | --- |
| `gcr.io/distroless/static-debian13:nonroot` | runtime base of the `alixai` image | Apache-2.0 (distroless build files); the few Debian packages inside (CA certificates, time-zone data, base files) are under their own free licences, listed in the image's `/usr/share/doc/*/copyright` |
| `golang:1.27.3-alpine` | build stage only, not shipped | BSD-3-Clause (Go), Alpine packages under their own licences |

LocalAI and vLLM images are covered in [model-licences.md](model-licences.md) and
[decision-layer-licences.md](decision-layer-licences.md).

## 5. How to re-check

```sh
go run github.com/google/go-licenses@latest report ./...
pnpm licenses list --prod
python -m pip show -v httpx pydantic certifi
```
