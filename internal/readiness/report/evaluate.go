// Package report evaluates verified readiness findings against the requirement catalog, builds the
// deterministic readiness report (master plan 9.5, ADR 0037) and renders it as markdown in English
// and Italian.
//
// The evaluator is the second half of "the model only proposes": it accepts only findings the
// verifier produced (verify.RdyVerified cannot be built anywhere else), takes each requirement's
// status from the rule's finding, and lets a verified model finding raise a requirement only from
// missing or unverified, only where the catalog says model_verified may satisfy it. A model
// finding that did not confirm is listed and never counts as covered, and no model finding can
// lower or replace a status a rule decided.
//
// The report is advisory. It certifies nothing, signs nothing and activates nothing: its first
// line says so, and the integration audit (master plan 4.5) still decides activation.
//
// Every package-level identifier here starts with rdy or Rdy.
package report

import (
	"errors"
	"fmt"
	"slices"

	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/verify"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// RdyEvalInput is everything the evaluator accepts, and all of it comes from the verifier.
type RdyEvalInput struct {
	// Rule holds the scanner's findings, one per requirement.
	Rule []verify.RdyVerified
	// Conflicts holds the cross-source disagreements found in the scan, each a contradicted
	// rule finding that the verifier has re-read.
	Conflicts []verify.RdyVerified
	// Model holds the model's findings, verified or not.
	Model []verify.RdyVerified
}

// RdyEvaluation is the outcome: the checklist, its counts and every finding in report order.
type RdyEvaluation struct {
	Checklist []readiness.ReportChecklistItem
	Summary   readiness.Summary
	Findings  []readiness.Finding
}

// RdyEvaluator turns verified findings into a status per requirement. Its three rules are fields
// only so a test can remove one and prove the tests notice; production code never replaces them.
type RdyEvaluator struct {
	cat *catalog.Catalog
	// grant is the status a model finding may raise a requirement to, or "" when it grants none.
	grant func(v verify.RdyVerified) readiness.Status
	// canLift says whether a requirement whose rule status is st may be raised by a model.
	canLift func(st readiness.Status) bool
	// liftable says whether the catalog lets a model_verified finding satisfy the requirement.
	liftable func(req readiness.Requirement) bool
}

// RdyNewEvaluator returns the evaluator for a catalog.
func RdyNewEvaluator(cat *catalog.Catalog) (*RdyEvaluator, error) {
	if cat == nil {
		var err error
		if cat, err = catalog.CatalogLoad(); err != nil {
			return nil, fmt.Errorf("readiness report: %w", err)
		}
	}
	return &RdyEvaluator{cat: cat, grant: rdyGrant, canLift: rdyCanLift, liftable: rdyLiftable}, nil
}

// rdyGrant: a model finding grants a status only when the verifier confirmed it, it is
// model_verified (never model_unverified) and its verified status says covered. The status read
// is the verified one, never the one the model claimed.
func rdyGrant(v verify.RdyVerified) readiness.Status {
	f := v.Finding()
	if v.FromModel() && v.Confirmed() && f.Source == readiness.SourceModelVerified &&
		(f.Status == readiness.StatusFound || f.Status == readiness.StatusPartial) {
		return f.Status
	}
	return ""
}

// rdyCanLift: a rule that found nothing, or could not tell, leaves room for a model finding. A
// status the rule decided (found, partial, contradicted) is final.
func rdyCanLift(st readiness.Status) bool {
	return st == readiness.StatusMissing || st == readiness.StatusUnverified
}

func rdyLiftable(req readiness.Requirement) bool {
	return slices.Contains(req.SatisfiedBy, readiness.SatisfiedByModelVerified)
}

// Evaluate computes the checklist, in catalog order.
func (e *RdyEvaluator) Evaluate(in RdyEvalInput) (*RdyEvaluation, error) {
	for _, v := range in.Rule {
		if v.FromModel() {
			return nil, errors.New("readiness report: a model finding was given as a rule finding")
		}
	}
	for _, v := range in.Conflicts {
		if v.FromModel() {
			return nil, errors.New("readiness report: a model finding was given as a conflict")
		}
	}
	for _, v := range in.Model {
		if !v.FromModel() {
			return nil, errors.New("readiness report: a rule finding was given as a model finding")
		}
	}
	out := &RdyEvaluation{Checklist: make([]readiness.ReportChecklistItem, 0, len(e.cat.Requirements))}
	for _, req := range e.cat.Requirements {
		var rules, conflicts, models []verify.RdyVerified
		for _, v := range in.Rule {
			if v.Finding().RequirementID == req.ID {
				rules = append(rules, v)
			}
		}
		for _, v := range in.Conflicts {
			if v.Finding().RequirementID == req.ID {
				conflicts = append(conflicts, v)
			}
		}
		for _, v := range in.Model {
			if v.Finding().RequirementID == req.ID {
				models = append(models, v)
			}
		}
		if len(rules) > 1 {
			return nil, fmt.Errorf("readiness report: %d rule findings for %s, want at most one", len(rules), req.ID)
		}

		status := readiness.StatusUnverified // no rule finding: it could not be determined
		var base *verify.RdyVerified
		if len(rules) == 1 {
			base = &rules[0]
			status = base.Finding().Status
		}
		contradicted := false
		for _, c := range conflicts {
			if c.Confirmed() && c.Finding().Status == readiness.StatusContradicted {
				contradicted = true
			}
		}
		switch {
		case contradicted:
			status = readiness.StatusContradicted
		case base != nil && base.Confirmed() && e.canLift(status) && e.liftable(req):
			best := status
			for _, m := range models {
				switch e.grant(m) {
				case readiness.StatusFound:
					best = readiness.StatusFound
				case readiness.StatusPartial:
					if best != readiness.StatusFound {
						best = readiness.StatusPartial
					}
				}
			}
			status = best
		}

		item := readiness.ReportChecklistItem{
			RequirementID: req.ID, Category: req.Category, CheckID: req.CheckID, Level: req.Level, Status: status,
			FindingIDs: []readiness.FindingID{},
		}
		for _, group := range [][]verify.RdyVerified{rules, conflicts, models} {
			for _, v := range group {
				f := v.Finding()
				out.Findings = append(out.Findings, f)
				item.FindingIDs = append(item.FindingIDs, f.FindingID)
			}
		}
		out.Checklist = append(out.Checklist, item)
	}
	rdyMarkOtherTransports(out.Checklist)
	out.Summary = rdySummarize(out.Checklist)
	return out, nil
}

// rdyMarkOtherTransports applies the one rule for alternatives: MCP, OpenAPI and a native adapter are alternative
// transports, so once a requirement of one of them is found, a requirement of another that is missing is not
// applicable. Only missing changes; partial, unverified and contradicted stay what they are, and with no transport
// met every transport requirement stays missing.
func rdyMarkOtherTransports(items []readiness.ReportChecklistItem) {
	used := map[readiness.Category]bool{}
	for _, it := range items {
		if it.Status == readiness.StatusFound && catalog.CatalogIsTransport(it.Category) {
			used[it.Category] = true
		}
	}
	for i := range items {
		if items[i].Status == readiness.StatusMissing && catalog.CatalogOtherTransport(items[i].Category, used) {
			items[i].Status = readiness.StatusNotApplicable
		}
	}
}

// rdySummarize counts a checklist by status. The not-applicable count is absent when it is zero.
func rdySummarize(items []readiness.ReportChecklistItem) readiness.Summary {
	var s readiness.Summary
	var na int64
	for _, it := range items {
		switch it.Status {
		case readiness.StatusFound:
			s.Found++
		case readiness.StatusPartial:
			s.Partial++
		case readiness.StatusMissing:
			s.Missing++
		case readiness.StatusUnverified:
			s.Unverified++
		case readiness.StatusContradicted:
			s.Contradicted++
		case readiness.StatusNotApplicable:
			na++
		}
	}
	if na > 0 {
		s.NotApplicable = &na
	}
	return s
}
