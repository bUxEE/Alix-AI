package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The full result of a run: the .soak file. It uses the field names and the
// rules of the bench envelope (schemas/bench/result.schema.json) for what the
// two share: format_version, run_id, timestamp, duration_ms, git, host,
// profile, emulated, synthetic and container_limits. A run under lite,
// balanced or experimental-4g is emulated, never certified (master plan 7.4):
// the harness applies no cgroup cap and cannot certify a host, so there is no
// way to write emulated: false.
//
// Why a file of its own and not a bench result. The bench schema is closed and
// its body is the TaskProposal benchmark (cases of routing, extraction,
// refusal and injection, each judged for intent and catalogue conformance). A
// soak has no such case: projecting its turns onto them would claim an intent
// and a catalogue check nobody made. The extension is not .json for the reason
// tools/rageval gives: `bench check` validates every *.json under a directory
// against the bench schema. `soak check` validates this file and re-derives
// its verdict from the recorded observations. A soak section in schemas/bench
// is M0-11's and is reported, not edited.
//
// Every key is always written (no omitempty on a number or a list) and none
// contains the word "token" (the log redactor masks keys that do).

const (
	// ResultFormatVersion is result.format_version.
	ResultFormatVersion = 1
	// ResultKind marks a file as a soak result.
	ResultKind = "alixai.soak.result"
	// ResultExt is the extension of a result file.
	ResultExt = ".soak"

	runIDPrefix = "run"
	verdictPass = "pass"
	verdictFail = "fail"
)

// Result is one soak run.
type Result struct {
	FormatVersion   int             `json:"format_version"`
	Kind            string          `json:"kind"`
	RunID           string          `json:"run_id"`
	Timestamp       string          `json:"timestamp"`
	DurationMS      float64         `json:"duration_ms"`
	PlannedMS       int64           `json:"planned_ms"`
	Git             GitInfo         `json:"git"`
	Host            HostInfo        `json:"host"`
	Profile         string          `json:"profile"`
	Emulated        bool            `json:"emulated"`
	Synthetic       bool            `json:"synthetic"`
	ContainerLimits ContainerLimits `json:"container_limits"`
	Decision        DecisionInfo    `json:"decision"`
	Target          TargetInfo      `json:"target"`
	Mix             Mix             `json:"mix"`
	MixSHA256       string          `json:"mix_sha256"`
	SampleMS        int64           `json:"sample_interval_ms"`
	Rules           Rules           `json:"rules"`
	Observations    Observations    `json:"observations"`
	Summary         Summary         `json:"summary"`
	Failures        []Failure       `json:"failures"`
	Verdict         string          `json:"verdict"`
}

// DecisionInfo is the decision-layer axis of the run.
type DecisionInfo struct {
	// Axis is off or on: whether the optional scorer was enabled.
	Axis string `json:"axis"`
	// QualifiedAsserted is true when the operator asserted, for lite, that a
	// qualified sub-1B scorer was configured.
	QualifiedAsserted bool `json:"qualified_asserted"`
}

// newResult assembles the result of a run and derives its verdict.
func newResult(cfg Config, rules Rules, deps Deps, started time.Time, elapsed time.Duration, obs Observations) *Result {
	limits, _ := limitsFor(cfg.Profile)
	sum, fails := Evaluate(rules, obs)
	res := &Result{
		FormatVersion:   ResultFormatVersion,
		Kind:            ResultKind,
		RunID:           deps.RunID,
		Timestamp:       started.UTC().Format(time.RFC3339),
		DurationMS:      round3v(float64(elapsed) / float64(time.Millisecond)),
		PlannedMS:       cfg.Duration.Milliseconds(),
		Git:             deps.Git,
		Host:            deps.Host,
		Profile:         string(cfg.Profile),
		Emulated:        true,
		Synthetic:       cfg.Synthetic,
		ContainerLimits: limits,
		Decision:        DecisionInfo{Axis: cfg.Decision, QualifiedAsserted: cfg.DecisionQualified},
		Target:          cfg.Target,
		Mix:             cfg.Mix,
		MixSHA256:       cfg.MixSHA256,
		SampleMS:        cfg.SampleInterval.Milliseconds(),
		Rules:           rules,
		Observations:    obs,
		Summary:         sum,
		Failures:        fails,
		Verdict:         verdictPass,
	}
	if len(fails) > 0 {
		res.Verdict = verdictFail
	}
	return res
}

// Passed reports whether the run passed.
func (r *Result) Passed() bool { return r.Verdict == verdictPass }

var nonName = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// FileName is the name of the result file: profile, decision axis, time and
// the run id, so results of the same host sort and never collide.
func (r *Result) FileName() string {
	ts := strings.NewReplacer("-", "", ":", "").Replace(r.Timestamp)
	part := func(s string) string { return strings.Trim(nonName.ReplaceAllString(s, "-"), "-") }
	return fmt.Sprintf("soak_%s_decision-%s_%s_%s%s", part(r.Profile), part(r.Decision.Axis), part(ts), part(r.RunID), ResultExt)
}

// WriteResult writes the result into dir (created if need be) and returns the
// path.
func WriteResult(dir string, r *Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.FileName())
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Report is the human summary printed at the end of a run.
func (r *Result) Report() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	f := func(p *float64) string {
		if p == nil {
			return "n/a"
		}
		return fmt.Sprintf("%.0f", *p)
	}
	w("run %s  profile %s (emulated)  decision %s  mix %s%s", r.RunID, r.Profile, r.Decision.Axis, r.Mix.ID, map[bool]string{true: "  SYNTHETIC (fakes)", false: ""}[r.Synthetic])
	w("ran %.1f s of %.1f s planned", r.DurationMS/1000, float64(r.PlannedMS)/1000)
	m := r.Summary.Memory
	w("memory     steady median %s MiB, peak %s MiB of %.0f MiB; worst headroom %s MiB (required %.0f)", f(m.SteadyMedianMiB), f(m.PeakMiB), m.LimitMiB, f(m.MinHeadroomMiB), m.RequiredHeadroomMiB)
	w("swap       measured %v, peak %.0f MiB", r.Summary.Swap.Measured, r.Summary.Swap.PeakMiB)
	st := r.Summary.Stack
	w("stack      protected roles %v; reloads observed %v (delta %d); peak queue pending %d", st.ProtectedRoles, st.ReloadsObserved, st.ReloadsDelta, st.PeakQueuePending)
	t := r.Summary.Turns
	w("turns      %d (%d failed), p50 %s ms, p95 %s ms", t.Count, t.Errors, f(t.P50MS), f(t.P95MS))
	g := r.Summary.Ingests
	w("ingests    %d (%d failed), p50 %s ms, p95 %s ms", g.Count, g.Errors, f(g.P50MS), f(g.P95MS))
	if r.Passed() {
		w("PASS")
		return b.String()
	}
	for _, fl := range r.Failures {
		w("FAIL %s: %s", fl.Code, fl.Detail)
	}
	return b.String()
}
