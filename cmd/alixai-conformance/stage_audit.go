package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// auditOutput is what `alixai audit run` prints, as far as the run reads it.
type auditOutput struct {
	Outcome string `json:"outcome"`
	Summary struct {
		Critical   int `json:"critical"`
		Warnings   int `json:"warnings"`
		Unverified int `json:"unverified"`
	} `json:"summary"`
	Targets  []string `json:"targets"`
	Sections []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"sections"`
	Findings []struct {
		Key         string `json:"key"`
		Severity    string `json:"severity"`
		CheckID     string `json:"check_id"`
		Capability  string `json:"capability"`
		Message     string `json:"message"`
		Remediation string `json:"remediation"`
	} `json:"findings"`
	Transport struct {
		Status string `json:"status"`
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	} `json:"transport"`
}

// auditPackage is the second stage: the full integration audit of master plan 4.5,
// `alixai audit run -no-activate`, against the running application. It is the audit
// that gates activation in production, run in the throwaway deployment, so nothing is
// activated and the report is signed with a key made for this run. A section the audit
// could not look at is unverified, and fails here like a failed one.
func auditPackage(ctx context.Context, d *deployment, applicationID string) Stage {
	start := time.Now()
	st := Stage{Name: "audit"}
	res := d.run(ctx, "audit", "run", "-no-activate", "-operator", conformanceOperator, applicationID)
	var out auditOutput
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		st.Status = StatusFail
		st.Message = "the integration audit did not produce a report: " + firstNonEmpty(res.Stderr, res.Stdout, err.Error())
		st.Items = []Item{{ID: "audit.run", Name: "integration audit runs", Status: StatusFail, Message: st.Message}}
		st.DurationMS = time.Since(start).Milliseconds()
		return st
	}

	for _, sec := range out.Sections {
		it := Item{ID: fmt.Sprintf("audit.section.%d", sec.Number), Name: fmt.Sprintf("%d. %s", sec.Number, sec.Title)}
		switch Status(sec.Status) {
		case StatusPass:
			it.Status = StatusPass
		case StatusFail:
			it.Status = StatusFail
		case StatusUnverified:
			it.Status = StatusUnverified
			it.Message = "the audit could not look at this: unverified is not a pass"
		case StatusNotRun:
			it.Status = StatusNotRun
			it.Message = "nothing implements this section in this run: not a pass"
		default:
			it.Status = StatusFail
			it.Message = fmt.Sprintf("the audit reported an unknown section status %q", sec.Status)
		}
		st.Items = append(st.Items, it)
	}
	if len(out.Sections) == 0 {
		st.Items = append(st.Items, Item{ID: "audit.sections", Name: "audit sections", Status: StatusFail, Message: "the audit report lists no section"})
	}

	for _, f := range out.Findings {
		var status Status
		switch contracts.Severity(f.Severity) {
		case contracts.SeverityCritical:
			status = StatusFail
		case contracts.SeverityWarning:
			status = StatusWarn
		default:
			continue
		}
		st.Items = append(st.Items, findingItem("audit", contracts.Finding{
			CheckID: f.CheckID, Severity: contracts.Severity(f.Severity), Capability: f.Capability, Message: f.Message, Remediation: f.Remediation,
		}, status))
	}

	outcome := Item{ID: "audit.outcome", Name: "audit outcome", Details: map[string]any{
		"outcome": out.Outcome, "critical": out.Summary.Critical, "warnings": out.Summary.Warnings,
		"unverified": out.Summary.Unverified, "targets": out.Targets, "transport": out.Transport.Status,
	}}
	switch out.Outcome {
	case "clear":
		outcome.Status = StatusPass
	case "needs_acceptance":
		outcome.Status = StatusWarn
		outcome.Message = "the audit found no critical problem; its warnings need an operator's acceptance before the package can be activated"
	default:
		outcome.Status = StatusFail
		outcome.Message = fmt.Sprintf("the audit blocks the package: %d critical finding(s)", out.Summary.Critical)
	}
	if out.Transport.Status != "in_use" {
		// no adapter: the application and runtime targets were not looked at
		outcome.Status = StatusFail
		outcome.Message = "the audit reached no application adapter: " + firstNonEmpty(out.Transport.Detail, out.Transport.Reason, "none registered for the package's endpoints")
	}
	st.Items = append([]Item{outcome}, st.Items...)
	st.DurationMS = time.Since(start).Milliseconds()
	return st
}
