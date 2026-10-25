// Package httpapi is the source provider for application-managed document APIs
// and scoped HTTP record APIs (master plan 12.1, 12.2, 16.1, 16.2). It
// implements source.Provider for the source types application_document_api and
// http_api, whose root is an "api" root of the rag-source declaration: an
// absolute http(s) URL (or a manifest endpoint a resolver maps to one) plus an
// optional base_path.
//
// The provider is generic. It does not know any application; it expects the
// small, read-only HTTP contract below, which an application exposes next to
// its other APIs. Everything it sends and everything it accepts is bounded by
// the declaration, and an API that does not keep the contract fails closed.
//
// # The API contract
//
// Below, ROOT is the declared url followed by base_path (no trailing slash; the
// bare origin gives the root path "/"), and KEY is an item key: a clean,
// slash-separated relative path ("policies/refund.md", "records/4711") that
// the include and exclude globs of the declaration are matched against. Each
// path segment of a key is percent-encoded when it is put in a URL.
//
//	GET  ROOT?<params>[&cursor=<c>]   list one page of items
//	GET  ROOT/KEY?<params>            the item's bytes
//	HEAD ROOT/KEY?<params>            the item's metadata, without a body
//
// <params> always holds the same values on every request the provider makes
// (list, fetch, metadata and the reachability probe): see "Scope" below.
//
// List response, 200 with a JSON body (testdata/list-page.schema.json):
//
//	{
//	  "items": [
//	    {"key": "policies/refund.md", "version": "7", "size": 1280,
//	     "modified_at": "2026-02-01T09:30:00Z"},
//	    {"key": "policies/old.md", "version": "3", "deleted": true}
//	  ],
//	  "next_cursor": "opaque-cursor-or-null"
//	}
//
// The members are:
//
//   - items (required): the page; an empty array is a valid page.
//   - key (required): the item key, as above.
//   - version (required): the item's current opaque version, 1 to 256
//     characters, no whitespace. It names the same state the item response's
//     ETag names. A version of the form "sha256:" and 64 lower-case hex digits
//     is a content address and is verified against the bytes that were read.
//   - size (optional): the item's exact length in bytes. A listed size above
//     limits.max_file_bytes skips the item without fetching it.
//   - modified_at (optional): when the item last changed, RFC 3339.
//   - deleted (optional, default false): the record is a deletion marker for
//     key. The item is gone; the listing never has to be fetched for it.
//   - next_cursor: the opaque cursor of the next page; null, absent or "" on
//     the last page. The provider builds every URL itself and sends the cursor
//     back verbatim, URL-encoded, as the "cursor" parameter; it never follows a
//     URL the server supplies.
//
// The pages of one listing must be a consistent traversal of one ordering:
// keyset cursors ("after this key") or a snapshot, not an offset into a list
// that can change under it. With offset paging an item inserted during a scan
// shifts another item over a page boundary, the listing misses it, and it is
// tombstoned and added again by the next scan. Keys are at most 4096
// characters; a longer one is a malformed page.
//
// Item response, 200 with the item's bytes. ETag (optional) is the same value
// as the listed version, with or without quotes and a weak "W/" marker;
// Content-Length, when present, is the exact length; Last-Modified is the
// modification time of a HEAD answer. The provider sends "Accept-Encoding:
// identity" and refuses a response that is encoded. The bytes are never taken
// on the word of Content-Type: the type is sniffed from the content, and the
// declared Content-Type (like a file name) only chooses among types that bytes
// cannot tell apart, such as JSON and plain text. Stat and Open report the
// ItemInfo size from Content-Length, and -1 when the API states none; the
// sizes of a Version and of a scan are always exact.
//
// Statuses. 200 is success. 404 and 410 on an item mean the item does not
// exist; they are trusted as "gone" only after the list endpoint has been seen
// to answer, so a wrong base_path cannot delete a source. 401 and 403 are
// permission_denied (the credential or scope was refused), 429 is rate_limited,
// 5xx and every transport failure are unavailable, a redirect is refused (the
// provider never follows one, so a credential cannot be carried to another
// place) and any other status is schema_invalid. None of them is ever read as a
// deletion.
//
// # Scope: what is always sent
//
// The verified scope and the declaration's fixed parameters are query
// parameters on every request, in this order of precedence and without a way to
// turn them off:
//
//   - application_id: the application of the verified scope;
//   - tenant_id and workspace_id: the tenant and workspace of the verified
//     scope. A level that the declaration binds as "shared" is not sent: the
//     source is not specific to one tenant or workspace there, and naming the
//     caller would make a shared source depend on who triggered the sync;
//   - every entry of query_scope.fixed_parameters (a dataset or collection
//     name, say), with the declared value;
//   - limit (the page size) on a list request, and cursor on every page after
//     the first.
//
// The application is expected to answer for that tenant and workspace only and
// to re-authorise the scope itself. AlixAI additionally verifies the scope
// against the registration (source.Definition.CheckScope) before any request is
// made, and every list and item URL is built by the provider from the declared
// root, so a key, a cursor or a server response cannot add, remove or override
// a scope parameter.
//
// query_scope.allowed_parameters is the declaration's whitelist of query
// parameter names AlixAI may send, and the provider honours it as written: when
// a query_scope is declared, "limit", "cursor", "application_id" and the
// tenant and workspace names that are sent must all be listed there, or the
// definition is refused (validation_failed) before any request. A fixed
// parameter may not reuse one of those names. Without a query_scope only the
// root and the include rules restrict the source. The parameter names are
// configurable (Config.Params) for an application that spells them differently.
//
// # Credentials and egress
//
// A declared credential_ref is an opaque "cred:" ID. It is resolved by a
// SecretProvider for every request and put in one request header (Config
// AuthHeader and AuthScheme; "Authorization: Bearer <secret>" by default),
// only for the origin of the declared root, never in a URL, a log record, an
// error or the result. A source that declares a credential that cannot be
// resolved sends nothing. Secret values print as a placeholder in every
// formatting verb.
//
// Every connection goes through internal/egress. The source must reference at
// least one egress entry (an empty list means the source makes no network
// connection, and every call is refused as permission_denied before any
// dialing); the Dialer comes from Config.Egress for the source's application,
// whose allowlist matches exact host and port, allows plain http to loopback
// only, never reaches link-local or metadata addresses and re-resolves and
// re-checks the address on every dial. A refusal is a permission_denied error
// that egress.IsDenied recognises. Transport errors pass through
// egress.StripURL before they are wrapped or logged, because net/http puts the
// full request URL, query string included, in its errors.
//
// # Items, versions and deletions
//
// Discover lists the whole source (every page, in a bounded number of pages and
// entries) and then fetches only the items that need it. An item is accepted as
// unchanged without being fetched when the record's type is still accepted and
// either the listed version is a content address equal to the previous
// Record's hash, or the listing gives its size and modified_at and both equal
// what the Record holds and the Record is older than the racy window relative
// to modified_at. Otherwise the item is fetched once, sniffed, and read
// through a SHA-256 under max_file_bytes. Version.Hash is always the content
// address of exactly the bytes that were read, so it equals the hash of what
// Open later delivers (the ingestion pipeline re-hashes it); the API's own
// version is a consistency check and not the hash: a fetched ETag, or a content
// address given as version, that does not match the listed version means the
// item changed during the scan, and the item is a Failure for this scan
// (it keeps its previous record). A listed size that differs from the bytes
// read is the same Failure. source.Record has no slot for an opaque
// version, so an API whose versions are not content addresses and that lists
// neither size nor modified_at is fetched in full on every scan.
//
// An item is "gone" only on a complete listing: every page was read without
// error, then a previous key that is absent from it, or that a deletion marker
// names, becomes a tombstone (reason deleted). A listing that fails on any
// page, an unreachable or refusing API, a malformed page or a reached limit
// returns an error and a ChangeSet with no Added, Modified, Deleted or Next, so
// nothing is applied and nothing is deleted. DetectDeletions asks the API
// whether it answers (one list request, before and after), then HEADs only the
// previous keys.
//
// A complete listing is believed, whatever it holds. An API that answers 200 with
// an empty (or mis-scoped, or truncated-but-well-formed) listing makes every
// previous item look deleted, exactly as an emptied folder does for the folder
// provider. The provider reports what it sees; a guard against mass deletions
// (refusing to apply a scan that deletes most of a large source until an
// operator confirms it) belongs to the caller, which sees the ChangeSet before
// it acts on Deleted.
//
// # Limits
//
// A list page larger than Config.MaxPageBytes, a body that does not end where
// Content-Length says, an item whose Content-Length or body exceeds
// limits.max_file_bytes (it is skipped as too_large, and tombstoned if it was
// indexed before), more than limits.max_files items or limits.max_total_bytes
// bytes (resource_exhausted), more pages than Config.MaxPages and a cursor that
// repeats are all refused; nothing is truncated. A listing may hold at most
// 2*max_files+10000 entries and about 1 KiB of keys and versions per entry it
// may hold, so entries with huge keys cannot add up to unbounded memory. Every
// request has a timeout.
package httpapi
