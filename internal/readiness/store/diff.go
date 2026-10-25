package store

import (
	"time"

	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// RdyChange is one requirement whose status differs between two reports.
type RdyChange struct {
	RequirementID readiness.RequirementID
	CheckID       readiness.CheckID
	Level         readiness.Level
	From          readiness.Status
	To            readiness.Status
}

// RdyDiff is the difference between two reports, in the order of the later report's checklist.
type RdyDiff struct {
	FromID common.TypedID
	ToID   common.TypedID
	// InputsChanged is true when the files, the catalog or the scan options hash differently.
	InputsChanged  bool
	CatalogChanged bool
	// NewlyMet are requirements that are met now and were not before.
	NewlyMet []RdyChange
	// NewlyMissing are requirements that are missing now and were not before.
	NewlyMissing []RdyChange
	// Changed are the other status changes: partial, unverified or contradicted now.
	Changed []RdyChange
	// Added and Removed are requirements only one of the reports has (the catalog changed).
	Added   []readiness.RequirementID
	Removed []readiness.RequirementID
	// Unchanged counts requirements with the same status in both.
	Unchanged int
}

// Empty reports that nothing differs.
func (d RdyDiff) Empty() bool {
	return !d.InputsChanged && !d.CatalogChanged && len(d.NewlyMet) == 0 && len(d.NewlyMissing) == 0 &&
		len(d.Changed) == 0 && len(d.Added) == 0 && len(d.Removed) == 0
}

// RdyDiffReports compares two reports. It reads only their checklists and hashes, so it is the
// same for a stored report and a fresh one.
func RdyDiffReports(from, to readiness.Report) RdyDiff {
	d := RdyDiff{
		FromID: from.ReportID, ToID: to.ReportID,
		InputsChanged:  from.InputsSHA256 != to.InputsSHA256,
		CatalogChanged: from.CatalogSHA256 != to.CatalogSHA256,
	}
	before := map[readiness.RequirementID]readiness.Status{}
	for _, it := range from.Checklist {
		before[it.RequirementID] = it.Status
	}
	inTo := map[readiness.RequirementID]bool{}
	for _, it := range to.Checklist {
		inTo[it.RequirementID] = true
		old, ok := before[it.RequirementID]
		if !ok {
			d.Added = append(d.Added, it.RequirementID)
			continue
		}
		if old == it.Status {
			d.Unchanged++
			continue
		}
		c := RdyChange{RequirementID: it.RequirementID, CheckID: it.CheckID, Level: it.Level, From: old, To: it.Status}
		switch it.Status {
		case readiness.StatusFound:
			d.NewlyMet = append(d.NewlyMet, c)
		case readiness.StatusMissing:
			d.NewlyMissing = append(d.NewlyMissing, c)
		default:
			d.Changed = append(d.Changed, c)
		}
	}
	for _, it := range from.Checklist {
		if !inTo[it.RequirementID] {
			d.Removed = append(d.Removed, it.RequirementID)
		}
	}
	return d
}

// RdyStatusView builds the status of a report, with each requirement's previous status and whether
// it changed when there is an earlier report. evaluatedAt is when the view was made.
func RdyStatusView(latest readiness.Report, previous *readiness.Report, evaluatedAt time.Time) readiness.StatusView {
	v := readiness.StatusView{
		SchemaVersion: 1,
		Advisory:      true,
		ReportID:      latest.ReportID,
		GeneratedAt:   latest.GeneratedAt,
		EvaluatedAt:   common.Timestamp(evaluatedAt.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")),
		Mode:          readiness.StatusViewMode(latest.Mode),
		Summary:       latest.Summary,
		Items:         make([]readiness.StatusViewItem, 0, len(latest.Checklist)),
	}
	before := map[readiness.RequirementID]readiness.Status{}
	if previous != nil {
		id := previous.ReportID
		v.PreviousReportID = &id
		v.InputsChanged = previous.InputsSHA256 != latest.InputsSHA256 || previous.CatalogSHA256 != latest.CatalogSHA256
		for _, it := range previous.Checklist {
			before[it.RequirementID] = it.Status
		}
	}
	for _, it := range latest.Checklist {
		item := readiness.StatusViewItem{RequirementID: it.RequirementID, Category: it.Category, CheckID: it.CheckID, Level: it.Level, Status: it.Status}
		if old, ok := before[it.RequirementID]; ok {
			o := old
			item.PreviousStatus = &o
			item.Changed = old != it.Status
		}
		v.Items = append(v.Items, item)
	}
	return v
}
