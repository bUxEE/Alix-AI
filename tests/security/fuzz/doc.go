// Package fuzzsec holds the fuzz targets of the security suite: hostile input to
// the public surface of AlixAI, driven through exported APIs from outside the
// packages that own the code (the fuzz targets that sit inside those packages
// stay where they are; these exercise the same code the way a caller does, with
// independent oracles).
//
// # Targets and the backlog (docs/security/test-backlog.md)
//
//	SEC-016  FuzzParseDocument      the in-process document parsers (txt, md, html, csv, pdf, docx)
//	SEC-024  FuzzFTSQueryBuilder    user text into a real FTS5 index with two tenants
//	SEC-097  FuzzSchemaValidate     hostile JSON bodies against the public JSON Schemas
//	SEC-097  FuzzStreamFraming      SSE and WebSocket framing, emitter and reader
//	SEC-097  FuzzMarkdownSanitize   the Markdown sanitiser of the rich blocks
//
// # How they run
//
// Under a plain `go test` each target runs its seed corpus: the published
// schema examples, classic XSS payloads, the pathological FTS5 queries, small
// valid documents built with internal/rag/parse/parsetest and zip-slip and bomb
// shaped archives. That takes seconds. For the agreed time, one target per
// invocation (Go allows a single -fuzz target):
//
//	go test -run '^$' -fuzz '^FuzzSchemaValidate$' -fuzztime 5s ./tests/security/fuzz/
//
// A crash is written by Go under testdata/fuzz/<Target>/ as a seed file; it is
// the regression seed and stays until the production fix lands.
//
// # What each one proves, and its controls
//
// Every "never happens" has a control that shows the check can fail; the controls
// run before the fuzzing, inside the target, so a plain `go test` runs them too.
//
//   - FuzzSchemaValidate: a body accepted by a production path that validates
//     (blocks.Validator.JSON, adapters.Normalise, models.ParseEntry) is accepted
//     by the raw JSON Schema (santhosh-tekuri/jsonschema/v6 compiled from schemas/,
//     the way tools/schemacheck does), and what the path returns is valid and
//     stable. The generated types are shape-only by design and are held to a
//     round-trip property only. Controls: the published invalid examples are
//     refused, and the generated decoders alone accept some of them.
//   - FuzzStreamFraming: the SSE body the hub emits is, line by line, a retry hint
//     and frames of exactly id / event / one data line, with no CR and no newline
//     injected through an event payload; the same events reach a WebSocket client
//     byte for byte and the production client (pkg/sdk-go) reads them back; the
//     client's reader, on arbitrary bytes, delivers only events a standard
//     conforming reader finds in them; a client message to the server-to-client
//     WebSocket closes it; the resume cursor accepts only digits in range.
//     Controls: the line checker flags an injected field, a CR and an unfinished
//     event.
//   - FuzzMarkdownSanitize: sanitised Markdown is inert as Markdown and as HTML
//     (CommonMark with raw HTML on and off, and GFM, read as a browser would,
//     with attribute values decoded, URLs stripped of tabs and folded for case and
//     full-width letters): no active element, handler, style expression or URL
//     scheme, no raw HTML, no image, no hidden character, and sanitising twice
//     changes nothing. Controls: the detectors flag planted payloads and pass
//     clean text.
//   - FuzzParseDocument: no parser panic, valid output under the host's own check,
//     the output and expansion caps, stable warning codes, no archive entry name
//     in an error or a warning, and hidden-text flags that hold under hostile
//     visible text. Controls: the same marker in plain text is visible and not
//     hidden, and the hostile archives are refused.
//   - FuzzFTSQueryBuilder: FTS5 syntax in user text never acts as syntax (the
//     same words without punctuation return the same rows), never makes FTS5
//     reject the expression, and never returns a row of another scope. Controls: a
//     deliberately unquoted stand-in on the same table shows errors, column
//     filters, NEAR, prefix, NOT and AND changing a result, and the other tenant's
//     rows when there is no scope predicate.
//
// The companion table test TestSecFTSQuerySyntaxNeutralised (SEC-024) is in
// tests/security/rag, where the real ingestion and query pipeline are.
package fuzzsec
