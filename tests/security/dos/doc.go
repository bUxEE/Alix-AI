// Package dossec is the denial-of-service security suite of master plan 17 and 23: one tenant's usage must
// not take another tenant's capacity.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-077  TestSecOneTenantCannotStarveAnother
//
// # One tenant cannot starve another (SEC-077)
//
// The real per-tenant quotas (internal/quotas, ADR 0018) run on a fake clock, so the result does not depend
// on timing. In each of the four dimensions a tenant is driven to its limit and refused with a typed code
// (rate_limited for the request bucket, resource_exhausted for sessions, daily ingest and storage), never
// silently; a second tenant, in the same application and under another application with the same tenant id,
// is then admitted at its full allowance, and its recorded usage is untouched by the first tenant's flood.
// Controls: the flood does exhaust the first tenant (otherwise the test proves nothing), and a limit is the
// tenant's own: a tenant override raises only that tenant.
//
// # On the serve path
//
// The wired subtest also drives storage and ingestion through real multipart uploads (M9-01): the files service
// charges them in the transaction that stores the file, a tenant that fills its storage is refused with a typed
// 507 while another tenant of the application keeps its own, the commit path is still reached with storage full,
// and a source upload that this build's RAG seam refuses leaves nothing charged (the counters are read from the
// store after serve stopped), so repeating it cannot use up the tenant's storage or day.
package dossec
