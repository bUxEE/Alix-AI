package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// maxResultBytes bounds a result file that check will read: a 7-day run at one
// sample a minute and one turn every 20 s is a few megabytes.
const maxResultBytes = 256 << 20

func checkCommand(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("soak check", flag.ContinueOnError)
	fset.SetOutput(stderr)
	rejectSynthetic := fset.Bool("reject-synthetic", false, "fail on results from the harness's own fakes (use for evidence that must come from a real stack)")
	requirePass := fset.Bool("require-pass", false, "also fail on a result whose verdict is fail (a valid file of a failed run is otherwise valid)")
	fset.Usage = func() {
		fmt.Fprintln(stderr, "usage: soak check [-reject-synthetic] [-require-pass] <dir or file>...")
		fmt.Fprintln(stderr, "  validates every *"+ResultExt+" result: its labels, its rules, and that its verdict is")
		fmt.Fprintln(stderr, "  what the recorded observations give; it fails when it finds no result")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fset.NArg() == 0 {
		fset.Usage()
		return 2
	}
	files, err := collectResultFiles(fset.Args())
	if err != nil {
		fmt.Fprintf(stderr, "soak check: %v\n", err)
		return 1
	}
	failed := 0
	for _, f := range files {
		problems := checkFile(f, *rejectSynthetic, *requirePass)
		if len(problems) == 0 {
			fmt.Fprintf(stdout, "PASS  %s\n", filepath.ToSlash(f))
			continue
		}
		failed++
		fmt.Fprintf(stdout, "FAIL  %s\n", filepath.ToSlash(f))
		for _, p := range problems {
			fmt.Fprintf(stdout, "      %s\n", p)
		}
	}
	if failed > 0 {
		fmt.Fprintf(stdout, "soak check: FAIL - %d of %d result file(s) are invalid\n", failed, len(files))
		return 1
	}
	fmt.Fprintf(stdout, "soak check: ok - %d result file(s)\n", len(files))
	return 0
}

// collectResultFiles lists the result files named by args: a file is taken as
// it is, a directory contributes every *.soak under it (hidden entries skipped).
// Finding none is an error, so an empty or mistyped directory cannot pass.
func collectResultFiles(args []string) ([]string, error) {
	var files []string
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			files = append(files, a)
			continue
		}
		err = filepath.WalkDir(a, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			name := d.Name()
			if d.IsDir() {
				if p != a && strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(name, ResultExt) && !strings.HasPrefix(name, ".") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no result files (*%s) found under %s", ResultExt, strings.Join(args, ", "))
	}
	sort.Strings(files)
	return files, nil
}

func checkFile(path string, rejectSynthetic, requirePass bool) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{err.Error()}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxResultBytes+1))
	if err != nil {
		return []string{err.Error()}
	}
	if len(data) > maxResultBytes {
		return []string{fmt.Sprintf("larger than %d bytes", maxResultBytes)}
	}
	var r Result
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return []string{fmt.Sprintf("does not decode: %v", err)}
	}
	if dec.More() {
		return []string{"more than one JSON document"}
	}
	problems := VerifyResult(&r)
	if rejectSynthetic && r.Synthetic {
		problems = append(problems, "synthetic result (the harness's fakes): it measures the harness, not a stack")
	}
	if requirePass && !r.Passed() {
		problems = append(problems, fmt.Sprintf("the run failed: %d failure(s), first %s", len(r.Failures), firstFailure(&r)))
	}
	return problems
}

func firstFailure(r *Result) string {
	if len(r.Failures) == 0 {
		return "(none recorded)"
	}
	return r.Failures[0].Code
}

var hexSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// VerifyResult applies what a decoder cannot: the labels are honest (emulated,
// the limits of the profile, synthetic exactly for the fakes), the rules are
// the plan's, and the summary and the failures are what the recorded
// observations give, so a result edited by hand, or written by a tool with a
// different idea of the rules, does not pass.
func VerifyResult(r *Result) []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, fmt.Sprintf(format, args...)) }

	if r.Kind != ResultKind {
		add("kind is %q, not %q", r.Kind, ResultKind)
	}
	if r.FormatVersion != ResultFormatVersion {
		add("format_version is %d, not %d", r.FormatVersion, ResultFormatVersion)
	}
	if !strings.HasPrefix(r.RunID, runIDPrefix+"_") {
		add("run_id %q is not a run id", r.RunID)
	}
	if _, err := time.Parse(time.RFC3339, r.Timestamp); err != nil {
		add("timestamp %q is not RFC 3339", r.Timestamp)
	}
	if r.DurationMS <= 0 || r.PlannedMS <= 0 {
		add("duration_ms %.0f and planned_ms %d must be positive", r.DurationMS, r.PlannedMS)
	} else if r.Observations.Completed && r.DurationMS < float64(r.PlannedMS) {
		add("the run is marked completed but lasted %.0f ms of the %d ms planned", r.DurationMS, r.PlannedMS)
	}

	p, err := ParseProfile(r.Profile)
	limits, known := limitsFor(p)
	switch {
	case err != nil || !known:
		add("profile %q is not a known profile", r.Profile)
	default:
		if !r.Emulated {
			add("emulated is false but results under profile %s are emulated (never certified)", r.Profile)
		}
		if r.ContainerLimits != limits {
			add("container_limits %+v are not the limits of profile %s (%+v)", r.ContainerLimits, r.Profile, limits)
		}
		if err := checkDecisionAxis(p, r.Decision.Axis, r.Decision.QualifiedAsserted); err != nil {
			add("decision axis: %v", err)
		}
		limit, _ := profileMemMiB(p)
		if r.Rules.Profile != r.Profile || r.Rules.LimitMiB != limit || r.Rules.RequiredHeadroomMiB != RequiredHeadroomMiB(limit) {
			add("rules do not match profile %s: the limit is %.0f MiB and the required headroom max(15%%, 1 GB) = %.1f MiB",
				r.Profile, limit, RequiredHeadroomMiB(limit))
		}
	}
	if r.Rules.Decision != r.Decision.Axis {
		add("rules.decision %q is not the decision axis %q", r.Rules.Decision, r.Decision.Axis)
	}
	if r.Synthetic != (r.Target.Driver == "fake") {
		add("synthetic is %v for driver %q", r.Synthetic, r.Target.Driver)
	}
	if err := r.Mix.Validate(); err != nil {
		add("mix: %v", err)
	}
	if !hexSHA.MatchString(r.MixSHA256) {
		add("mix_sha256 is not a SHA-256")
	}
	if r.Rules.InteractiveEnabled != r.Mix.Interactive.Enabled || r.Rules.IngestionEnabled != r.Mix.Ingestion.Enabled {
		add("the rules judge a different load than the mix puts on the stack")
	}
	if r.Rules.WarmupMS < 0 || r.Rules.WarmupMS >= r.PlannedMS {
		add("warmup_ms %d must be at least 0 and shorter than the planned %d ms", r.Rules.WarmupMS, r.PlannedMS)
	}

	var prev int64
	for i, s := range r.Observations.Samples {
		if s.AtMS < prev {
			add("sample %d is out of order", i)
			break
		}
		prev = s.AtMS
	}

	sum, fails := Evaluate(r.Rules, r.Observations)
	if !sameJSON(sum, r.Summary) {
		add("summary does not match the observations")
	}
	if !sameJSON(fails, r.Failures) {
		add("failures do not match the observations (they give %s)", failureCodes(fails))
	}
	want := verdictPass
	if len(fails) > 0 {
		want = verdictFail
	}
	if r.Verdict != want {
		add("verdict is %q but the observations give %q", r.Verdict, want)
	}
	return ps
}

func failureCodes(fs []Failure) string {
	if len(fs) == 0 {
		return "none"
	}
	codes := make([]string, len(fs))
	for i, f := range fs {
		codes[i] = f.Code
	}
	return strings.Join(codes, ", ")
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}
