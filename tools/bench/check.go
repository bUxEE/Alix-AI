package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/models"
)

// maxResultBytes bounds a result file that check will read.
const maxResultBytes = 64 << 20

func checkCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rejectSynthetic := fs.Bool("reject-synthetic", false, "fail on results from the fake provider (use for evidence that must come from a real model)")
	schemas := fs.String("schemas", "", "schemas directory (default: schemas/ beside go.mod)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bench check [-reject-synthetic] [-schemas dir] <dir or file>...")
		fmt.Fprintln(stderr, "  validates every *.json result against schemas/bench/result.schema.json and")
		fmt.Fprintln(stderr, "  re-derives its aggregates from the raw cases; it fails when it finds no result")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	dir, err := FindSchemasDir(*schemas)
	if err != nil {
		fmt.Fprintf(stderr, "bench check: %v\n", err)
		return 1
	}
	schema, err := LoadResultSchema(dir)
	if err != nil {
		fmt.Fprintf(stderr, "bench check: %v\n", err)
		return 1
	}
	files, err := collectResultFiles(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "bench check: %v\n", err)
		return 1
	}
	failed := 0
	for _, f := range files {
		problems := checkFile(schema, f, *rejectSynthetic)
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
		fmt.Fprintf(stdout, "bench check: FAIL - %d of %d result file(s) are invalid\n", failed, len(files))
		return 1
	}
	fmt.Fprintf(stdout, "bench check: ok - %d result file(s)\n", len(files))
	return 0
}

// collectResultFiles lists the result files named by args: a file is taken as
// it is, a directory contributes every *.json under it (hidden files and
// directories skipped). Finding none is an error, so an empty or mistyped
// directory cannot pass.
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
			if strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no result files (*.json) found under %s", strings.Join(args, ", "))
	}
	sort.Strings(files)
	return files, nil
}

// checkFile validates one result file and returns its problems.
func checkFile(schema *jsonschema.Schema, path string, rejectSynthetic bool) []string {
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
	res, problems := decodeResult(schema, data)
	if len(problems) > 0 {
		return problems
	}
	problems = VerifyResult(res)
	if rejectSynthetic && res.Synthetic {
		problems = append(problems, "synthetic result (fake provider): it measures the harness, not a model")
	}
	return problems
}

// decodeResult validates data against the result schema and decodes it.
func decodeResult(schema *jsonschema.Schema, data []byte) (*Result, []string) {
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		return nil, []string{fmt.Sprintf("not valid JSON: %v", err)}
	}
	if err := schema.Validate(inst); err != nil {
		return nil, []string{"schema: " + firstProblemLines(err, 5)}
	}
	var res Result
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return nil, []string{fmt.Sprintf("does not decode: %v", err)}
	}
	return &res, nil
}

// validateResultBytes is the full check of one result document: the schema,
// then VerifyResult.
func validateResultBytes(schema *jsonschema.Schema, data []byte) []string {
	res, problems := decodeResult(schema, data)
	if len(problems) > 0 {
		return problems
	}
	return VerifyResult(res)
}

// firstProblemLines returns up to n problem lines of a validation error.
func firstProblemLines(err error, n int) string {
	var lines []string
	for _, l := range strings.Split(err.Error(), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "- ") {
			lines = append(lines, strings.TrimPrefix(l, "- "))
			if len(lines) == n {
				break
			}
		}
	}
	if len(lines) == 0 {
		s, _ := truncateRunes(err.Error(), 300)
		return s
	}
	return strings.Join(lines, "; ")
}

// VerifyResult applies what the JSON Schema cannot express. It checks that the
// profile label is honest (emulated and the limits follow from the profile),
// that the counts agree, and that every aggregate in the file equals what its
// raw cases say, so a result that was edited by hand, or written by a tool
// with a different idea of a metric, does not pass.
func VerifyResult(r *Result) []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, fmt.Sprintf(format, args...)) }

	p := models.Profile(r.Profile)
	if limits, ok := LimitsFor(p); !ok {
		add("profile %q is not a known profile", r.Profile)
	} else {
		if r.Emulated != IsEmulated(p) {
			add("emulated is %v but results under profile %s are emulated (never certified)", r.Emulated, r.Profile)
		}
		if r.ContainerLimits != limits {
			add("container_limits %+v are not the limits of profile %s (%+v)", r.ContainerLimits, r.Profile, limits)
		}
	}
	if r.Synthetic != (r.Provider.Name == "fake") {
		add("synthetic is %v for provider %s", r.Synthetic, r.Provider.Name)
	}

	n := len(r.Cases)
	if r.Corpus.CasesRun != n {
		add("corpus.cases_run is %d but the result holds %d cases", r.Corpus.CasesRun, n)
	}
	switch {
	case r.Corpus.CasesRun > r.Corpus.CasesTotal:
		add("corpus.cases_run %d exceeds cases_total %d", r.Corpus.CasesRun, r.Corpus.CasesTotal)
	case r.Config.Limit == nil && r.Corpus.CasesRun != r.Corpus.CasesTotal:
		add("no --limit was used but only %d of %d corpus cases ran", r.Corpus.CasesRun, r.Corpus.CasesTotal)
	case r.Config.Limit != nil && r.Corpus.CasesRun != min(*r.Config.Limit, r.Corpus.CasesTotal):
		add("limit is %d but %d cases ran", *r.Config.Limit, r.Corpus.CasesRun)
	}
	if r.Config.WarmupRequests == 0 && r.Latency.WarmupFirstRequestMS != nil {
		add("latency.warmup_first_request_ms is set but no warm-up ran")
	}

	seen := map[string]bool{}
	for _, c := range r.Cases {
		if seen[c.ID] {
			add("case %s appears twice", c.ID)
		}
		seen[c.ID] = true
		ps = append(ps, verifyCase(c)...)
	}

	if want := Aggregate(r.Cases); !reflect.DeepEqual(r.Metrics, want) {
		add("metrics do not match the cases (the cases give %s)", briefMetrics(want))
	}
	byCat, byLang := aggregateBy(r.Cases)
	if !reflect.DeepEqual(r.ByCategory, byCat) {
		add("by_category does not match the cases")
	}
	if !reflect.DeepEqual(r.ByLanguage, byLang) {
		add("by_language does not match the cases")
	}
	if want := summarizeLatency(r.Cases, r.Latency.WarmupFirstRequestMS); !reflect.DeepEqual(r.Latency, want) {
		add("latency does not match the cases")
	}
	if want := summarizeErrors(r.Cases); !reflect.DeepEqual(r.Errors, want) {
		add("errors do not match the cases")
	}

	m := r.Memory
	if m.PeakRSSMB != nil {
		if m.SteadyRSSMB != nil && *m.SteadyRSSMB > *m.PeakRSSMB {
			add("memory.steady_rss_mb exceeds peak_rss_mb")
		}
		if m.BaselineMB != nil && *m.BaselineMB > *m.PeakRSSMB {
			add("memory.baseline_mb exceeds peak_rss_mb")
		}
	}
	if m.LimitMBObserved != nil && !limitMatches(*m.LimitMBObserved, r.ContainerLimits.MemMB) {
		add("memory.limit_mb_observed %.0f differs from the %d MiB of profile %s: the container was not under the limit the run is labelled with",
			*m.LimitMBObserved, r.ContainerLimits.MemMB, r.Profile)
	}
	return ps
}

// verifyCase checks the internal consistency of one case outcome.
func verifyCase(c CaseOutcome) []string {
	var ps []string
	add := func(format string, args ...any) {
		ps = append(ps, fmt.Sprintf("case %s: ", c.ID)+fmt.Sprintf(format, args...))
	}

	if c.TTFTMS != nil && c.LatencyMS != nil && *c.TTFTMS > *c.LatencyMS {
		add("ttft_ms exceeds latency_ms")
	}
	if c.FieldHits > c.FieldTotal {
		add("field_hits %d exceeds field_total %d", c.FieldHits, c.FieldTotal)
	}
	if c.FieldsExact == nil && (c.FieldHits != 0 || c.FieldTotal != 0) {
		add("fields_exact is null but field counts are set")
	}
	if c.FieldsExact != nil && *c.FieldsExact && c.FieldHits != c.FieldTotal {
		add("fields_exact is true but only %d of %d fields were right", c.FieldHits, c.FieldTotal)
	}
	if (c.RefusalOK != nil) != (c.Category == CatRefusal) {
		add("refusal_ok must be set exactly for the refusal category")
	}
	if (c.Resisted != nil) != (c.Category == CatInjection) || (c.AttackSucceeded != nil) != (c.Category == CatInjection) {
		add("resisted and attack_succeeded must be set exactly for the injection category")
	}
	if c.Resisted != nil && c.AttackSucceeded != nil && *c.Resisted && *c.AttackSucceeded {
		add("resisted and attack_succeeded are both true")
	}
	if c.Pass && c.Category == CatRefusal && c.RefusalOK != nil && !*c.RefusalOK {
		add("pass is true but refusal_ok is false")
	}
	if c.Pass && c.Category == CatInjection && c.Resisted != nil && !*c.Resisted {
		add("pass is true but resisted is false")
	}
	if c.Pass && c.CapabilityOK != nil && !*c.CapabilityOK {
		add("pass is true but capability_ok is false")
	}
	if c.Pass && c.Category == CatExtraction && c.FieldsExact != nil && !*c.FieldsExact {
		add("pass is true but fields_exact is false")
	}
	return ps
}

func briefMetrics(m MetricSet) string {
	return fmt.Sprintf("schema_valid %d/%d, routing %d/%d, pass %d/%d",
		m.SchemaValid.Passed, m.SchemaValid.Total, m.CapabilityRouting.Passed, m.CapabilityRouting.Total, m.Pass.Passed, m.Pass.Total)
}
