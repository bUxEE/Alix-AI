# Verification of AlixAI 1.0

This folder says what was verified for the 1.0 release, how, and by whom, and what was not verified. AlixAI has one
maintainer. Some checks are automated and run on every change; some were done by the maintainer and have not been
reviewed independently; some were only analysed on paper; and some were not done. Each row below says which.

If you can close one of the gaps (real hardware, a long soak, a live provider run, an independent review), the linked page
says what to run and what to send.

## Documents

| Document | What it covers |
| --- | --- |
| [release-1.0.md](release-1.0.md) | The 1.0 release record: automated checks, signing, what was skipped |
| [hardware.md](hardware.md) | Lite, Balanced and ARM64: emulated measurements, theoretical fit, how to test on real hardware; decision-layer hardware gates |
| [soak-analysis.md](soak-analysis.md) | 24-hour and 7-day soaks: what the tests cover, expected behaviour, how to run one |
| [dependency-licences.md](dependency-licences.md) | Licences of every Go module, npm peer and Python package AlixAI links or installs, and of the container base image |
| [model-licences.md](model-licences.md) | Licences and supply chain of the generative and embedding models and the LocalAI runtime |
| [decision-layer-licences.md](decision-layer-licences.md) | Licences and supply chain of CLM-v0.1-8B, the Qwen3-8B encoder, vLLM and the scorers |
| [provider-terms.md](provider-terms.md) | Retention, training use, zero retention, regions and spend controls of each External-mode provider |
| [external-mode-review.md](external-mode-review.md) | Legal, data-residency and security review of External mode |
| [security-self-review.md](security-self-review.md) | The maintainer's security self-review |

## Verification status of 1.0

Statuses: **Done** (automated, repeatable by anyone), **Done (maintainer, not independent)** (done by the maintainer, not
reviewed by a third party), **Analysis only** (reasoned from measurements or documents, not run as specified),
**Not done**.

| Area | Status | What it means for you | Details |
| --- | --- | --- | --- |
| Unit, integration, security, degradation and gate tests (Go; TypeScript and Python SDKs) | Done | Every change runs them in CI; you can run them yourself | [release record](release-1.0.md#2-automated-checks) |
| CI workflows (`.github/workflows`) | Done | Every change runs `go test`, the race detector, `govulncheck`, the Go licence allow-list, the TypeScript and Python suites, and the supply-chain pin checks | [release record](release-1.0.md#2-automated-checks) |
| API and schema compatibility against the frozen v1 contracts | Done | No breaking change; later additions follow the compatibility policy | [release record](release-1.0.md#2-automated-checks) |
| Dependency vulnerabilities and licences | Done | `govulncheck` (Go, on every change) is clean on Go 1.27.3, which fixes the eMudhra CA issue (CVE-2026-111943); `pnpm audit --prod` is clean; the Go licence allow-list passes, with two modules checked by hand | [dependency-licences.md](dependency-licences.md) |
| Reproducible build, SBOM, signed release manifest | Done (maintainer, not independent) | The build was checked reproducible on the candidate; at the tag the release files are signed with a key only the maintainer holds, so check them against the fingerprint in the release record | [release record](release-1.0.md#3-signing) |
| Controller model qualification | Not done | No local controller model passes the quality bar on any profile; 1.0 serves the deterministic path, RAG and the embedder, and a model you add is your own choice | [hardware.md](hardware.md#2-theoretical-fit-from-measured-numbers) |
| Lite profile (x86-64, 8 GB) | Analysis only | Measured under container caps on one desktop, not certified on a real 8 GB machine | [hardware.md](hardware.md) |
| Balanced profile (16 GB) | Analysis only | As Lite | [hardware.md](hardware.md) |
| ARM64 | Not done | Never run; expect to be the first to try it | [hardware.md](hardware.md#3-arm64) |
| 24-hour mixed load and 7-day soak | Analysis only | No long run exists; slow leaks and drift are not ruled out | [soak-analysis.md](soak-analysis.md) |
| Decision-layer hardware gates (CPU, ARM64, CUDA, TensorRT, VPS) | Not done | The decision layer is optional, bounded, non-generative and non-authoritative; CLM-v0.1-8B and every scorer are off on every profile | [hardware.md](hardware.md#4-decision-layer-hardware-gates) |
| Licences of models and runtime | Done (maintainer, not independent) | Not legal advice; one model carries a use policy, several declare their licence only in metadata | [model-licences.md](model-licences.md) |
| Licences and supply chain of the decision layer | Done (maintainer, not independent) | Not legal advice; the CLM head is a pickle file, loaded weights-only by the pinned torch default | [decision-layer-licences.md](decision-layer-licences.md) |
| External provider terms | Done (maintainer, not independent) | A summary of public documentation on one date; read the current terms yourself | [provider-terms.md](provider-terms.md) |
| Provider accounts and live conformance against real endpoints | Not done | External mode passes its suite only against recorded fixtures | [provider-terms.md](provider-terms.md#4-what-was-verified-and-what-was-not) |
| External mode legal, residency and security review | Done (maintainer, not independent) | Not legal advice; External mode is off by default | [external-mode-review.md](external-mode-review.md) |
| Security self-review | Done (maintainer, not independent) | The maintainer's review of the code and design | [security-self-review.md](security-self-review.md) |
| Internal penetration test | Done (maintainer, not independent) | One Medium finding fixed, one Low accepted, nothing Critical or High open | [findings](../security/pentest-internal/findings.md) |
| Independent external security review | Not done | Nobody outside the project has reviewed the security of 1.0 | [release record](release-1.0.md#4-not-done-for-10) |
| Disaster-recovery drill (kill during commit and backup, restore, key rotation) | Done | Automated drill, emulated on one machine with small data | [drill report](../evidence/m6-dr/report.md) |
| Project name and compatibility-label policy | Done (maintainer, not independent) | The name is unregistered and was not cleared | [trademark.md](../governance/trademark.md) |
| Private security contact | Done | Report through GitHub private vulnerability reporting | [SECURITY.md](../../SECURITY.md) |
| Public beta | Not done | Skipped: 1.0 is the first public release | [release record](release-1.0.md) |
