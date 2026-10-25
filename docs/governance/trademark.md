# Project name and compatibility-label policy

Status: **in force from 1.0.** Decision record: [ADR 0019](../decisions/0019-trademark-compatibility-label.md). Nothing here
is legal advice.

This policy says how others may use the name **AlixAI** and its logo (`docs/assets/brand/`), and when a third-party build
may call itself compatible. It is separate from the software licence.

## 1. The licence is not narrowed

The code, schemas, SDKs and tools are licensed under the [Apache License 2.0](../../LICENSE). You may use, modify and
redistribute them under that licence whatever this page says. Section 6 of the licence grants no right to use the
project's name or logo beyond describing where the work comes from; this page is what governs that use.

## 2. What is and is not registered

The name is **not a registered trademark**, and **no trademark clearance search has been done**. A preliminary web search
found other products using similar names, including in software; no legal conclusion was drawn from it. If the name turns
out to conflict with someone else's rights, the project may have to rename, and this policy would follow the new name.

## 3. The rules

1. **Facts are free.** "Built on AlixAI", "works with AlixAI", "a fork of AlixAI 1.0", "integrates with AlixAI" need no
   permission, as long as they do not suggest endorsement by the project.
2. **Unmodified redistribution may use the name and logo as published.** That means the project's own release artifacts,
   or a rebuild that reproduces their hashes. Changing configuration within the documented configuration surface is not
   a modification.
3. **Modified distributions use their own name.** A distribution that changes code, schemas, policy or safety defaults
   must not use "AlixAI" as its product, package, image or domain name, and must say it is based on AlixAI and modified.
4. **"AlixAI Compatible <version>"** may be claimed for one specific build only when its publisher:
   - passed, for that exact build, the conformance suites for the role it claims (an integration package or application:
     `alixai-conformance`; a runtime distribution or SDK: the conformance, security and degradation suites in the
     repository that apply to it);
   - left the safety invariants unchanged: prepare before commit, scope isolation, fail closed, deterministic authority,
     mandatory provenance, no mandatory telemetry;
   - published the machine-readable report together with the exact models, quantisations, backend and configuration used.

   The claim names the AlixAI version, lapses with the next minor release, and can be withdrawn publicly if it turns out to
   be wrong. In 1.0 the label is for software you distribute; a hosted service may state facts (rule 1) but does not use
   the label.
5. **"Certified" is reserved for the project.** It is used only for hardware-profile results the project produced or
   witnessed on real hardware. Results from container memory caps are called **emulated**. No third party uses
   "certified" for anything AlixAI-related, and nobody uses "official" for a build the project did not publish.
6. **A label is not an audit.** A compatibility claim never replaces an operator's own review of what they deploy.

## 4. Asking, claiming and reporting

- **Permission** for a use these rules do not cover: open an issue in the repository
  (<https://github.com/bUxEE/Alix-AI/issues>) with "Name use" in the title.
- **A compatibility claim**: open an issue with "Compatibility claim: <product> <version>" in the title and attach or link
  the report. The maintainer checks that the report covers the claimed role and build, and records the outcome in the
  issue. A withdrawn claim is recorded in the same place.
- **Misuse** of the name or logo: open an issue with "Name misuse" in the title. Do not use the private vulnerability
  channel for this; it is for security reports ([SECURITY.md](../../SECURITY.md)).

## 5. Changes

The policy can be tightened or loosened with public notice in the release notes. A claim already accepted keeps the terms
it was accepted under until it lapses.
