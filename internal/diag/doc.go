// Package diag is local monitoring and diagnostics (master plan 14.2
// "Operations", 16.2 "Logs redact tokens, secrets and configured sensitive
// fields", 17 "observable failure"): queue status, provider and model status,
// resource usage, the metrics registry that internal/metrics serves on loopback,
// and a diagnostics bundle an operator can attach to a bug report.
//
// # Two audiences, two doors
//
// A host application reaches only what its own token allows, through
// internal/gateway (routes_ops.go): the queue counts of its own application,
// tenant and workspace (jobs.Queue.ScopedStatus), the status of the model
// roles, and the export of its own audit events. Each needs an exact capability
// in the context token (CapabilityQueueRead, CapabilityProvidersRead,
// audit.CapabilityExport), the optional Sources.Authorize hook can add a rule of
// the deployment's own, and nothing deployment-wide (global counts, configuration,
// resource figures, recent errors) is on that door.
//
// The operator reaches the deployment-wide view on the loopback listener
// (OperatorHandler, served by metrics.Serve): Prometheus text at /metrics, and
// the diagnostics bundle at /diagnostics behind the operator credential. A
// token minted by a host application is not that credential.
//
// # The bundle and redaction
//
// Bundle collects versions, the configuration, health, the queue, resource
// usage and the recent warnings and errors. It is built to work when the system
// is not: a source that fails is named in Problems and the rest is still
// reported. Every value passes through one choke point (sanitize) before it
// leaves: a key whose name looks sensitive (token, secret, password,
// authorization, api key, cookie, private key, credential, plus the keys the
// operator configures) is replaced, and so is a secret embedded in text (a
// bearer token, a JWT, URL userinfo, a PEM private key, key=value). The rules are
// the ones internal/log applies to log lines, so a bundle is never less
// redacted than the log it summarises.
//
// The package makes no network connection of its own (TestNoOutboundTelemetry in
// internal/metrics reads its source to prove it): it reads in-process state and
// hands bytes to whoever called it.
package diag
