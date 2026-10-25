package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// GateInput is what the default-enable gate reads of one side (the baseline or the
// candidate): the metrics of one configuration plus the run-level facts that decide
// whether the evidence is admissible.
type GateInput struct {
	Name string
	// RunID is the run the numbers come from. Memory is measured once per run, so a
	// baseline and a candidate with the same RunID cannot tell the layer on from the layer
	// off: their memory reserve is not comparable.
	RunID string
	// Synthetic and ControllerScripted mark evidence that measures the harness, not a
	// system: a synthetic run, or a controller whose latency was scripted.
	Synthetic          bool
	ControllerScripted bool
	Metrics            MetricSet
	// FirstResponseP95 and its sample count; nil when unknown.
	FirstResponseP95 *float64
	FirstResponseN   int
	// MemoryReserveMB is the free memory under the profile's cap with the configuration
	// resident; nil when it was not measured.
	MemoryReserveMB *float64
}

// GateDecision is the gate's answer. Enable is true only when nothing is regressed or
// unknown and at least one of latency, capacity or safety improved.
type GateDecision struct {
	Enable    bool
	Improved  []string
	Regressed []string
	Unknown   []string
}

// Reason is a one-line, human-readable account of the decision.
func (d GateDecision) Reason() string {
	if d.Enable {
		return "enable: improved " + strings.Join(d.Improved, ", ") + "; nothing guarded regressed"
	}
	var parts []string
	if len(d.Regressed) > 0 {
		parts = append(parts, "regressed: "+strings.Join(d.Regressed, ", "))
	}
	if len(d.Unknown) > 0 {
		parts = append(parts, "not comparable or not measured: "+strings.Join(d.Unknown, ", "))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing improved (latency, capacity and safety are all unchanged or worse)")
	}
	return "do not enable: " + strings.Join(parts, "; ")
}

// guarded and safety are the metrics that must not regress. The five the plan names are
// task success, citation correctness, multilingual quality, fail-closed behaviour and
// memory reserve; the four safety metrics are guarded too, because a layer that improves
// latency by weakening a refusal must not be enabled.
type metricPick struct {
	name string
	of   func(MetricSet) Ratio
}

var guardedMetrics = []metricPick{
	{"task_completion", func(m MetricSet) Ratio { return m.TaskCompletion }},
	{"citation_correctness", func(m MetricSet) Ratio { return m.CitationCorrectness }},
	{"multilingual_quality", func(m MetricSet) Ratio { return m.MultilingualQuality }},
	{"fail_closed", func(m MetricSet) Ratio { return m.FailClosed }},
}

var safetyMetrics = []metricPick{
	{"unauthorised_rejection", func(m MetricSet) Ratio { return m.UnauthorisedRejection }},
	{"injection_resistance", func(m MetricSet) Ratio { return m.InjectionResistance }},
	{"untrusted_document_handling", func(m MetricSet) Ratio { return m.UntrustedDocumentHandling }},
	{"out_of_scope_rejection", func(m MetricSet) Ratio { return m.OutOfScopeRejection }},
}

// DefaultEnableGate decides whether the candidate may be enabled by default, against the
// baseline (the same fixtures with the layer off). It enables only if latency, capacity or
// safety improves AND task success, citation correctness, multilingual quality,
// fail-closed behaviour, the safety refusals and memory reserve do not regress. It fails
// closed: unknown is not non-regression, so a guarded metric that is missing, has no
// cases, or has a different denominator on the two sides refuses; so does evidence from a
// synthetic run or a scripted controller. Counts are compared, never float rates.
func DefaultEnableGate(baseline, candidate GateInput) GateDecision {
	var d GateDecision
	if baseline.Synthetic || candidate.Synthetic {
		d.Unknown = append(d.Unknown, "synthetic evidence (the fake scorer measures the harness, not a system)")
	}
	if baseline.ControllerScripted || candidate.ControllerScripted {
		d.Unknown = append(d.Unknown, "scripted controller (its latency is not a measurement)")
	}
	if baseline.Metrics.Cases == 0 || baseline.Metrics.Cases != candidate.Metrics.Cases {
		d.Unknown = append(d.Unknown, "case counts differ or are zero")
	}

	compare := func(p metricPick, improved *[]string, guard bool) {
		b, c := p.of(baseline.Metrics), p.of(candidate.Metrics)
		if b.Total == 0 || c.Total == 0 || b.Total != c.Total {
			d.Unknown = append(d.Unknown, p.name)
			return
		}
		switch {
		case c.Passed < b.Passed:
			if guard {
				d.Regressed = append(d.Regressed, p.name)
			}
		case c.Passed > b.Passed:
			*improved = append(*improved, p.name)
		}
	}
	var improved []string
	for _, p := range guardedMetrics {
		var ignore []string
		compare(p, &ignore, true)
	}
	var safetyImproved []string
	for _, p := range safetyMetrics {
		compare(p, &safetyImproved, true)
	}

	// Memory reserve: more is better; it must be measured on both sides.
	switch {
	case baseline.RunID != "" && baseline.RunID == candidate.RunID:
		d.Unknown = append(d.Unknown, "memory_reserve (baseline and candidate are the same run: measure them in separate runs)")
	case baseline.MemoryReserveMB == nil || candidate.MemoryReserveMB == nil:
		d.Unknown = append(d.Unknown, "memory_reserve")
	case *candidate.MemoryReserveMB < *baseline.MemoryReserveMB:
		d.Regressed = append(d.Regressed, "memory_reserve")
	}

	// Improvements: latency, capacity, safety.
	switch {
	case baseline.FirstResponseP95 == nil || candidate.FirstResponseP95 == nil ||
		baseline.FirstResponseN == 0 || baseline.FirstResponseN != candidate.FirstResponseN:
		d.Unknown = append(d.Unknown, "first_response_p95")
	case *candidate.FirstResponseP95 < *baseline.FirstResponseP95:
		improved = append(improved, "latency")
	case *candidate.FirstResponseP95 > *baseline.FirstResponseP95:
		d.Regressed = append(d.Regressed, "first_response_p95")
	}
	if candidate.Metrics.ControllerCallsAvoided.Passed > baseline.Metrics.ControllerCallsAvoided.Passed ||
		candidate.Metrics.GeneratedToksAvoided > baseline.Metrics.GeneratedToksAvoided {
		improved = append(improved, "capacity")
	}
	if len(safetyImproved) > 0 {
		improved = append(improved, "safety")
	}

	d.Improved = improved
	sort.Strings(d.Improved)
	sort.Strings(d.Regressed)
	sort.Strings(d.Unknown)
	d.Enable = len(d.Regressed) == 0 && len(d.Unknown) == 0 && len(improved) > 0
	return d
}

// gateInputOf extracts one configuration of a result for the gate.
func gateInputOf(r *Result, configID int) (GateInput, error) {
	for _, c := range r.Configurations {
		if c.ID != configID {
			continue
		}
		in := GateInput{
			Name: fmt.Sprintf("configuration %d (%s)", c.ID, c.Name), RunID: r.RunID, Synthetic: r.Synthetic,
			ControllerScripted: r.Controller.Source != ControllerSupplied && c.UsesController,
			Metrics:            c.Metrics, FirstResponseP95: c.Latency.FirstResponseMS.P95, FirstResponseN: c.Latency.FirstResponseMS.N,
			MemoryReserveMB: r.Live.MemoryReserveMB,
		}
		return in, nil
	}
	return GateInput{}, fmt.Errorf("the result holds no configuration %d", configID)
}

func readResult(path string) (*Result, error) {
	schema, err := LoadResultSchema("")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	res, problems := decodeResult(schema, data)
	if len(problems) == 0 {
		problems = VerifyResult(res)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s is not a valid result: %s", path, problems[0])
	}
	return res, nil
}

func gateCommand(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("decbench gate", flag.ContinueOnError)
	fl.SetOutput(stderr)
	base := fl.String("baseline", "", "result file of the baseline run (the layer off)")
	baseCfg := fl.String("baseline-config", "4", "configuration of the baseline (default 4, direct controller routing)")
	cand := fl.String("candidate", "", "result file of the candidate run")
	candCfg := fl.String("candidate-config", "5", "configuration of the candidate (default 5, scorer then controller)")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: decbench gate --baseline <result.json> [--baseline-config 4] --candidate <result.json> [--candidate-config 5]")
		fmt.Fprintln(stderr, "  prints whether the candidate may be enabled by default; exit 0 enable, 1 do not enable")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	bid, err1 := strconv.Atoi(*baseCfg)
	cid, err2 := strconv.Atoi(*candCfg)
	if *base == "" || *cand == "" || err1 != nil || err2 != nil {
		fl.Usage()
		return 2
	}
	load := func(path string, id int) (GateInput, error) {
		r, err := readResult(path)
		if err != nil {
			return GateInput{}, err
		}
		return gateInputOf(r, id)
	}
	b, err := load(*base, bid)
	if err != nil {
		fmt.Fprintf(stderr, "decbench gate: %v\n", err)
		return 1
	}
	c, err := load(*cand, cid)
	if err != nil {
		fmt.Fprintf(stderr, "decbench gate: %v\n", err)
		return 1
	}
	d := DefaultEnableGate(b, c)
	out, _ := json.Marshal(map[string]any{"enable": d.Enable, "improved": d.Improved, "regressed": d.Regressed, "unknown": d.Unknown})
	fmt.Fprintf(stdout, "%s\n%s vs baseline %s\n%s\n", out, c.Name, b.Name, d.Reason())
	if !d.Enable {
		return 1
	}
	return 0
}
