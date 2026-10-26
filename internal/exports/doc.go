// Package exports turns a turn's verified rich blocks into downloadable files
// (release 1.2.0): CSV and XLSX from tables and chart datasets, HTML and Markdown
// reports of any selected blocks. Bytes are generated deterministically by this
// package from blocks the core already validated; the model never supplies file
// content. Results are stored through internal/files as OriginGenerated and
// downloaded with GET /v1/files/{file_id}/content.
//
// Capability: exports.create in the context token.
package exports
