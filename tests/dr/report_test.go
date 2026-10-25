package dr

import (
	"bytes"
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"text/template"
	"time"
)

//go:embed report.md.tmpl
var reportTemplate string

// scenarioOrder is every scenario the report covers, in the order it lists them. The report is
// written only when the run observed every one of them: a partial run (go test -run) must not
// overwrite the evidence with a report that leaves a scenario out.
var scenarioOrder = []string{
	"kill during commit, before the application applies",
	"kill during commit, after the application applies",
	"corrupt index",
	"restore after a crash",
	"restore over a destroyed database",
	"restore into an empty data directory",
	"kill during backup",
	"key rotation",
	"monitoring",
}

func checkComplete() error {
	seen := map[string]bool{}
	for _, o := range evidence.Observations {
		seen[o.Scenario] = true
	}
	var missing []string
	for _, s := range scenarioOrder {
		if !seen[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the run did not observe %s: the evidence is only written by a run of the whole drill", strings.Join(missing, "; "))
	}
	return nil
}

type timingRow struct {
	Name   string
	Runs   int
	Min    string
	Median string
	Max    string
	Detail string
}

type scenarioRow struct {
	Name         string
	Observations []string
}

type envRow struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func ms(v int64) string {
	switch {
	case v >= 10000:
		return fmt.Sprintf("%.1f s", float64(v)/1000)
	case v >= 1000:
		return fmt.Sprintf("%.2f s", float64(v)/1000)
	default:
		return fmt.Sprintf("%d ms", v)
	}
}

// renderReport renders the report from what the run recorded. The caller holds evidence.mu.
func renderReport(now time.Time) ([]byte, error) {
	byName := map[string][]timing{}
	var order []string
	for _, t := range evidence.Timings {
		if _, ok := byName[t.Name]; !ok {
			order = append(order, t.Name)
		}
		byName[t.Name] = append(byName[t.Name], t)
	}
	var rows []timingRow
	for _, name := range order {
		ts := byName[name]
		vals := make([]int64, len(ts))
		for i, t := range ts {
			vals[i] = t.Millis
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		rows = append(rows, timingRow{
			Name: name, Runs: len(ts), Min: ms(vals[0]), Median: ms(vals[len(vals)/2]), Max: ms(vals[len(vals)-1]), Detail: ts[0].Detail,
		})
	}
	var scenarios []scenarioRow
	for _, s := range scenarioOrder {
		row := scenarioRow{Name: s}
		for _, o := range evidence.Observations {
			if o.Scenario == s {
				row.Observations = append(row.Observations, o.Text)
			}
		}
		scenarios = append(scenarios, row)
	}
	env := append([]envRow(nil), evidence.Env...)

	tmpl, err := template.New("report").Parse(reportTemplate)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = tmpl.Execute(&out, map[string]any{
		"Date": now.Format("2006-01-02"), "Env": env, "Timings": rows, "Scenarios": scenarios,
	})
	return out.Bytes(), err
}
