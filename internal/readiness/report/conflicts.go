package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// rdyConflictRequirement is the requirement an API disagreement is filed under: the control
// surface the audit compares (master plan 4.5 check 7) is the thing that cannot be trusted when
// two descriptions of it disagree.
const rdyConflictRequirement readiness.RequirementID = "req.surface.control_surface"

const (
	rdyConflictFixEN = "Make the API descriptions agree on this operation, or remove the outdated one."
	rdyConflictFixIT = "Allineare le descrizioni delle API per questa operazione, oppure rimuovere quella obsoleta."
)

// RdyConflicts finds the disagreements between two API descriptions of the same project: one
// operation id that two documents declare with a different method or path. It is deterministic
// and works from the scan's own inventory, so every conflict cites a line in each document. Each
// result is a contradicted rule finding; it still goes through the verifier like any other, and
// the evaluator applies it only when it confirms.
func RdyConflicts(res *scan.ScanResult) []readiness.Finding {
	if res == nil {
		return nil
	}
	sha := map[string]string{}
	for _, f := range res.Files {
		sha[f.Path] = f.SHA256
	}
	type decl struct {
		doc  string
		op   scan.ScanOperation
		repr string
	}
	byID := map[string][]decl{}
	for _, doc := range res.Inventory.OpenAPI {
		for _, op := range doc.Operations {
			if op.OperationID == "" || op.Line < 1 || sha[doc.Path] == "" {
				continue
			}
			byID[op.OperationID] = append(byID[op.OperationID], decl{doc: doc.Path, op: op, repr: op.Method + " " + op.Path})
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []readiness.Finding
	for _, id := range ids {
		ds := byID[id]
		sort.SliceStable(ds, func(i, j int) bool { return ds[i].doc < ds[j].doc })
		first := ds[0]
		for _, other := range ds[1:] {
			if other.doc == first.doc || other.repr == first.repr {
				continue
			}
			ev := []readiness.FindingEvidence{
				{Path: readiness.RelativePath(first.doc), LineStart: int64(first.op.Line), LineEnd: int64(first.op.Line), FileSHA256: readiness.SHA256(sha[first.doc])},
				{Path: readiness.RelativePath(other.doc), LineStart: int64(other.op.Line), LineEnd: int64(other.op.Line), FileSHA256: readiness.SHA256(sha[other.doc])},
			}
			// The names come from project files: each part is bounded so the summary always fits
			// the schema's text limit, whatever the project holds.
			sum := scan.ScanRedact(fmt.Sprintf("Two API descriptions disagree about the operation %s: %s in one, %s in the other.",
				rdyBound(id, 80), rdyBound(first.repr, 160), rdyBound(other.repr, 160)))
			t := readiness.Text(sum)
			itFix := readiness.Text(rdyConflictFixIT)
			var key strings.Builder
			for _, e := range ev {
				fmt.Fprintf(&key, "%s:%d:%d:%s\n", e.Path, e.LineStart, e.LineEnd, e.FileSHA256)
			}
			h := sha256.Sum256([]byte(string(rdyConflictRequirement) + "\n" + key.String() + "conflict\n"))
			out = append(out, readiness.Finding{
				SchemaVersion: 1,
				FindingID:     readiness.FindingID("fnd_" + hex.EncodeToString(h[:])[:16]),
				RequirementID: rdyConflictRequirement,
				Status:        readiness.StatusContradicted,
				Source:        readiness.SourceDeterministic,
				Evidence:      ev,
				Summary:       &t,
				Remediation:   readiness.RemediationText{En: rdyConflictFixEN, It: &itFix},
			})
		}
	}
	return out
}

// rdyBound shortens untrusted text to at most max runes and removes control characters.
func rdyBound(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		r = append(r[:max], '.', '.', '.')
	}
	for i, c := range r {
		if c < 0x20 || c == 0x7f {
			r[i] = ' '
		}
	}
	return string(r)
}
