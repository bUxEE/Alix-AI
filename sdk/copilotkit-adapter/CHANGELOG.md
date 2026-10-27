# Changelog: @alixai/copilotkit-adapter

All notable changes to this package. Versions follow semantic versioning with the rules of
[compatibility.md](../../docs/policies/compatibility.md).

## 1.5.0 — 2026-10-27

### Added

- Analyst rich blocks (ADR 0047): parsers and default React activity renderers for `kpi`, `timeline`, `record_card`, `provenance`, `draft`.
- Forwards `forwardedProps.alixai.clientCapabilities` on AG-UI runs so the gateway can map emit for 1.4-safe clients.

## 1.4.1 — 2026-10-27

### Fixed

- Version aligns with core/SDK 1.4.1. No adapter API change beyond dependency bumps.
