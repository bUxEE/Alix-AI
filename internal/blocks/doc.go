// Package blocks validates and sanitises rich blocks before they leave the core
// (master plan 14.1: "Charts, diagrams and rich blocks are data, not executable
// HTML or JavaScript").
//
// A Validator checks, in this order:
//
//  1. the published JSON Schema (schemas/conversation/rich-block.schema.json,
//     embedded; TestEmbeddedSchemasMatchPublished keeps the copy honest), so an
//     unknown member, a wrong type or a missing member is refused before
//     anything is decoded;
//  2. the checks only the core can make: sizes (a block as a whole, a table's
//     rows, columns and cells, a chart's points, a diagram's lines), unique table
//     keys, a rectangular table, clean text in every string (no control,
//     bidirectional or zero-width character, no markup, URL, template or formula
//     in a title, label or value), a diagram from a closed set of kinds with no
//     callback, link, directive or front matter, and a chart whose every encoded
//     field exists, with no field no channel reads;
//  3. for a chart, that every number it shows is an authoritative fact (FactSet,
//     built from the DialogueDirective's and the receipt's facts, compared as
//     exact rationals): a chart is never a way to state a number the application
//     did not state. ChartMarks and ChartDigest derive what a renderer must draw
//     from the declared data and encoding alone, so a chart is reproducible.
//
// Markdown is not stripped, it is rewritten. goldmark parses the text; only an
// allowlisted set of nodes (paragraphs, headings, emphasis, code, lists, quotes,
// rules, and links with an allowed scheme) is written again, as canonical
// Markdown, with every character that could start a construct escaped. Raw HTML
// and images are dropped, so a renderer that reads the output differently from
// goldmark still finds nothing active in it. The result is rendered and checked
// a second time by bluemonday, which must find nothing to remove; if it does,
// the text is returned as escaped plain text. Sanitising the output again
// returns it unchanged.
//
// Every refusal is a contracts.Error with code schema_invalid whose cause lists
// typed violations (Violations, HasRule): a rule name and a JSON location, never
// text of the block, which is untrusted.
//
// HOSTS.md in this directory is the note for host applications: which renderers
// to enable and the content-security policy to serve them under.
//
// # Analyst blocks (ADR 0047)
//
// Additive block types kpi, timeline, record_card, provenance and draft, and
// chart marks scatter / heatmap / combo_bar_line with optional bar_layout,
// pie_style, y2 and size channels, validate against the live rich-block schema.
// Emitting them to a client that did not announce support is the stream layer's
// job (internal/stream.MapEvent): this package only validates and sanitises.
package blocks
