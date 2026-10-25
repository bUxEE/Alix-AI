// Package metrics is AlixAI's local metrics: a small registry, the Prometheus
// text exposition format and a loopback-only listener (master plan 3 rule 3:
// "Operational metrics stay local unless the operator explicitly exports them").
//
// # What this package does not do
//
// It never dials. There is no push gateway, no remote-write client, no
// StatsD or OTLP exporter and no version or usage ping: the only way a number
// leaves the process is a scrape that an operator's own collector makes against
// the loopback listener, and exporting beyond the machine is the operator's
// decision and the operator's tooling (TestNoOutboundTelemetry holds the
// package to this at run time and at the source level). It is a tier 2 package
// of docs/architecture/module-map.md and imports base packages only: the
// packages that own the numbers (the job queue, the provider registry, the
// resource governor) expose plain stats and internal/diag turns them into
// Collectors.
//
// # Model
//
// A Collector returns Families (a name, a help text, a type and samples) when
// the registry is gathered. Nothing is stored between scrapes, so there is no
// cardinality to leak: label values come from small closed sets (a priority
// class, a state, a provider role) and never from a tenant, a principal, a
// session or a request. A collector that fails is reported as
// alixai_scrape_collector_up{collector="..."} 0 and never fails the scrape or
// hides the others.
//
// # Where it listens
//
// Listen binds a literal loopback address and nothing else; Serve refuses any
// listener that is not loopback TCP, and every request is checked again
// (loopback peer, loopback local address, loopback Host) before a handler sees
// it, so a reverse proxy, a port forward or a DNS-rebinding page does not turn
// the listener into a public one. The `metrics` configuration section is
// disabled by default.
package metrics
