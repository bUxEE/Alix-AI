package loop

import (
	"encoding/json"
	"strings"

	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// MaxEgressClass is the most sensitive class that may leave for an external model
// at directive_class. Restricted fields never leave; confidential may depending
// on the guard — the loop always strips restricted before PromptGuard sees them.
const MaxEgressClass = an.DataClassConfidential

func classRank(c an.DataClass) int {
	switch c {
	case an.DataClassPublic:
		return 0
	case an.DataClassInternal:
		return 1
	case an.DataClassConfidential:
		return 2
	}
	return 3
}

// FilterDatasetForEgress returns a projection safe to send to an external model:
// fields above max (and always restricted) are removed.
func FilterDatasetForEgress(ds an.Dataset, max an.DataClass) an.Dataset {
	keep := map[string]bool{}
	var fields []an.DatasetDatasetField
	for _, f := range ds.Fields {
		if classRank(f.DataClass) > classRank(max) {
			continue
		}
		if f.DataClass == an.DataClassRestricted {
			continue
		}
		keep[string(f.Name)] = true
		fields = append(fields, f)
	}
	rows := make([]an.DatasetRow, 0, len(ds.Rows))
	for _, row := range ds.Rows {
		nr := an.DatasetRow{}
		for k, v := range row {
			if keep[k] {
				nr[k] = v
			}
		}
		rows = append(rows, nr)
	}
	out := ds
	out.Fields = fields
	out.Rows = rows
	out.RecordCount = int64(len(rows))
	return out
}

// SummariseDatasets builds a bounded, egress-filtered summary for the step prompt.
func SummariseDatasets(datasets map[an.DatasetID]an.Dataset, maxClass an.DataClass, maxToks int) string {
	if len(datasets) == 0 {
		return "(no datasets yet)"
	}
	var b strings.Builder
	est := 0
	for id, ds := range datasets {
		safe := FilterDatasetForEgress(ds, maxClass)
		line := summariseOne(id, safe)
		cost := (len(line) + 2) / 3
		if maxToks > 0 && est+cost > maxToks {
			b.WriteString("…(datasets truncated to token budget)\n")
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		est += cost
	}
	return b.String()
}

func summariseOne(id an.DatasetID, ds an.Dataset) string {
	type rowView struct {
		Fields []string         `json:"fields"`
		Sample []map[string]any `json:"sample"`
		Count  int64            `json:"record_count"`
		Prov   string           `json:"provenance_kind"`
	}
	names := make([]string, 0, len(ds.Fields))
	classes := make([]string, 0, len(ds.Fields))
	for _, f := range ds.Fields {
		names = append(names, string(f.Name)+"("+string(f.DataClass)+")")
		classes = append(classes, string(f.DataClass))
	}
	_ = classes
	sampleN := 3
	if len(ds.Rows) < sampleN {
		sampleN = len(ds.Rows)
	}
	sample := make([]map[string]any, 0, sampleN)
	for i := 0; i < sampleN; i++ {
		sample = append(sample, map[string]any(ds.Rows[i]))
	}
	v := rowView{Fields: names, Sample: sample, Count: ds.RecordCount, Prov: string(ds.Provenance.Kind)}
	raw, _ := json.Marshal(v)
	return "dataset " + string(id) + ": " + string(raw)
}

// QueryLeaksRestricted reports whether query embeds a confidential/restricted
// cell value from the turn's datasets (web_query egress).
func QueryLeaksRestricted(query string, datasets map[an.DatasetID]an.Dataset) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return false
	}
	for _, ds := range datasets {
		restricted := map[string]bool{}
		for _, f := range ds.Fields {
			if f.DataClass == an.DataClassRestricted || f.DataClass == an.DataClassConfidential {
				restricted[string(f.Name)] = true
			}
		}
		for _, row := range ds.Rows {
			for name, cell := range row {
				if !restricted[name] {
					continue
				}
				s := cellString(cell)
				if s == "" {
					continue
				}
				if strings.Contains(q, strings.ToLower(s)) {
					return true
				}
			}
		}
	}
	return false
}

func cellString(v an.DatasetCell) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strings.TrimRight(strings.TrimRight(jsonNumber(x), "0"), ".")
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
