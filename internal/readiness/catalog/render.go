package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// CatalogRenderMarkdown renders the catalog for humans, in English and Italian, as the text
// of docs/integration/readiness-requirements.md. The output is deterministic: a test compares
// it with the committed file, so the document cannot drift from the data.
func CatalogRenderMarkdown(c *Catalog) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# Integration readiness requirements\n\n")
	w("<!-- Generated from internal/readiness/catalog/requirements.json by CatalogRenderMarkdown. Do not edit by hand: change the catalog, then run `READINESS_WRITE_DOCS=1 go test ./internal/readiness/catalog -run TestCatalogDocIsCurrent`. -->\n\n")
	w("Source of truth: [ADR 0037](../decisions/0037-integration-readiness-discovery.md) and the catalog data this page is generated from. ")
	w("Where this page and the ADR disagree, the ADR wins and this page is the bug.\n\n")
	w("This is the checklist `alixai readiness scan <dir>` walks through on a project directory the operator names. It is **advisory**: ")
	w("a readiness report never certifies, never signs, never activates and never edits a package, and a complete report is not an audit pass. ")
	w("The integration audit ([application packages](application-package.md)) still decides activation. ")
	w("Each requirement below names the audit check it feeds, so a covered checklist points at what the audit will then verify.\n\n")
	w("Catalog version %d, %d requirements. Every title, description, rule and remediation exists in English and Italian.\n\n", CatalogVersion, len(c.Requirements))

	w("## How a requirement is covered\n\n")
	w("- **Statuses.** `found` (covered, with evidence the verifier confirmed), `partial`, `missing`, `unverified` (could not be determined, or the evidence did not confirm; never counted as covered) and `contradicted` (two pieces of evidence disagree).\n")
	w("- **Sources.** `deterministic` (a rule found it), `model_verified` (a model proposed it and the verifier re-read and confirmed its evidence) and `model_unverified` (a model proposed it and the evidence did not confirm). Only the first two can cover a requirement.\n")
	w("- **Evidence.** Every finding cites a path relative to the scanned root, a line span and the file hash. A finding that does not confirm is `unverified`.\n")
	w("- **Levels.** `mandatory`: the audit check it feeds cannot pass without it. `recommended`: the audit reports a warning without it.\n\n")

	w("## Overview\n\n")
	w("| Requirement | Category | Level | Audit check | Covered by |\n")
	w("| --- | --- | --- | --- | --- |\n")
	for _, r := range catalogSorted(c) {
		by := make([]string, len(r.SatisfiedBy))
		for i, s := range r.SatisfiedBy {
			by[i] = "`" + string(s) + "`"
		}
		w("| `%s` | %s | %s | `%s` | %s |\n", r.ID, r.Category, r.Level, r.CheckID, strings.Join(by, ", "))
	}
	w("\n")

	w("## Requirements by audit check\n\n")
	for _, id := range c.CheckIDs() {
		w("### `%s`\n\n", id)
		for _, r := range c.ByCheck(id) {
			w("#### `%s`\n\n", r.ID)
			w("- Category: `%s`. Level: `%s`. Covered by: %s.\n\n", r.Category, r.Level, catalogJoinBy(r.SatisfiedBy))
			w("**English.** %s. %s\n\n", catalogTrimDot(string(r.Title.En)), r.Description.En)
			w("- Rule: %s\n", r.HowSatisfied.En)
			w("- If missing: %s\n\n", r.Remediation.En)
			w("**Italiano.** %s. %s\n\n", catalogTrimDot(string(r.Title.It)), r.Description.It)
			w("- Regola: %s\n", r.HowSatisfied.It)
			w("- Se manca: %s\n\n", r.Remediation.It)
		}
	}
	return b.String()
}

func catalogSorted(c *Catalog) []readiness.Requirement {
	out := append([]readiness.Requirement(nil), c.Requirements...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func catalogJoinBy(by []readiness.SatisfiedBy) string {
	parts := make([]string, len(by))
	for i, s := range by {
		parts[i] = "`" + string(s) + "`"
	}
	return strings.Join(parts, ", ")
}

func catalogTrimDot(s string) string { return strings.TrimRight(s, ". ") }
