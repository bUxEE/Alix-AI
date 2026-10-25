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
)

// resultSchemaFile is the decision-result schema, relative to the schemas directory.
const resultSchemaFile = "bench/decision-result.schema.json"

// maxResultBytes bounds a result file that check will read.
const maxResultBytes = 64 << 20

// FindRepoRoot walks up from the working directory to the directory holding go.mod.
func FindRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if st, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !st.IsDir() {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("no go.mod above the working directory")
		}
	}
}

// LoadResultSchema compiles the decision-result schema. It is read from the repository's
// schemas tree at run time (a copy would drift) and is self-contained: no reference is
// followed anywhere, and no network is touched.
func LoadResultSchema(schemasDir string) (*jsonschema.Schema, error) {
	if schemasDir == "" {
		root, err := FindRepoRoot()
		if err != nil {
			return nil, fmt.Errorf("cannot find the schemas directory: %w (pass -schemas)", err)
		}
		schemasDir = filepath.Join(root, "schemas")
	}
	p := filepath.Join(schemasDir, filepath.FromSlash(resultSchemaFile))
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	obj, ok := doc.(map[string]any)
	id, _ := obj["$id"].(string)
	if !ok || id == "" {
		return nil, fmt.Errorf("%s declares no $id", p)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(noRefLoader{})
	if err := c.AddResource(id, doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	s, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("%s does not compile: %w", p, err)
	}
	return s, nil
}

// noRefLoader refuses every external reference: the schema is self-contained.
type noRefLoader struct{}

func (noRefLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("the decision-result schema is self-contained; refusing to load %q", url)
}

func checkCommand(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("decbench check", flag.ContinueOnError)
	fl.SetOutput(stderr)
	rejectSynthetic := fl.Bool("reject-synthetic", false, "fail on results from the fake scorer (evidence that must come from a real scorer)")
	schemas := fl.String("schemas", "", "schemas directory (default: schemas/ beside go.mod)")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: decbench check [-reject-synthetic] [-schemas dir] <dir or file>...")
		fmt.Fprintln(stderr, "  validates every *.json result against schemas/bench/decision-result.schema.json and")
		fmt.Fprintln(stderr, "  re-derives its aggregates from the raw cases; it fails when it finds no result")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fl.NArg() == 0 {
		fl.Usage()
		return 2
	}
	schema, err := LoadResultSchema(*schemas)
	if err != nil {
		fmt.Fprintf(stderr, "decbench check: %v\n", err)
		return 1
	}
	files, err := collectResultFiles(fl.Args())
	if err != nil {
		fmt.Fprintf(stderr, "decbench check: %v\n", err)
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
		fmt.Fprintf(stdout, "decbench check: FAIL - %d of %d result file(s) are invalid\n", failed, len(files))
		return 1
	}
	fmt.Fprintf(stdout, "decbench check: ok - %d result file(s)\n", len(files))
	return 0
}

// collectResultFiles lists the result files named by args: a file is taken as it is, a
// directory contributes every *.json under it (hidden entries skipped). Finding none is an
// error, so an empty or mistyped directory cannot pass.
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
		problems = append(problems, "synthetic result (fake scorer): it measures the harness, not a model")
	}
	return problems
}

// decodeResult validates data against the schema and decodes it strictly.
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

// validateResultBytes is the full check of one result document.
func validateResultBytes(schema *jsonschema.Schema, data []byte) []string {
	res, problems := decodeResult(schema, data)
	if len(problems) > 0 {
		return problems
	}
	return VerifyResult(res)
}

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
		s := err.Error()
		if len(s) > 300 {
			s = s[:300]
		}
		return s
	}
	return strings.Join(lines, "; ")
}

func specByID(id int) (ConfigSpec, bool) {
	for _, c := range Configs {
		if c.ID == id {
			return c, true
		}
	}
	return ConfigSpec{}, false
}

// VerifyResult applies what the JSON Schema cannot express: the profile label is honest,
// every configuration is one of the five and ran on the identical fixtures, the call
// counts agree with the configuration's definition, and every aggregate in the file equals
// what its raw cases say.
func VerifyResult(r *Result) []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, fmt.Sprintf(format, args...)) }

	if limits, ok := LimitsFor(r.Profile); !ok {
		add("profile %q is not a known profile", r.Profile)
	} else {
		if !r.Emulated {
			add("emulated is false but results under profile %s are emulated (never certified)", r.Profile)
		}
		if r.ContainerLimits != limits {
			add("container_limits %+v are not the limits of profile %s (%+v)", r.ContainerLimits, r.Profile, limits)
		}
	}
	if r.Synthetic != (r.Scorer.Kind == ScorerFake) {
		add("synthetic is %v for scorer %s", r.Synthetic, r.Scorer.Kind)
	}
	if (r.Scorer.ArtifactSHA256 != nil) != (r.Scorer.ArtifactHashSource == HashComputed) {
		add("scorer.artifact_sha256 and artifact_hash_source disagree")
	}
	if (r.Controller.Source == ControllerSupplied) != (r.Controller.LatencyMS != nil) {
		add("controller.latency_ms is set exactly when the controller source is supplied")
	}
	if r.Config.PercentileMethod != PercentileMethod || r.Config.CalibrationBins != CalibrationBins {
		add("config uses percentile method %q with %d bins; this tool defines %q with %d", r.Config.PercentileMethod, r.Config.CalibrationBins, PercentileMethod, CalibrationBins)
	}

	want := liveOf(RunOptions{PeakRSSMB: r.Live.PeakRSSMB, ResidentModelMB: r.Live.ResidentModelMB}, r.ContainerLimits)
	if !reflect.DeepEqual(r.Live.MemoryReserveMB, want.MemoryReserveMB) {
		add("live.memory_reserve_mb is not the profile cap minus peak_rss_mb and resident_model_mb")
	}

	if len(r.Configurations) == 0 {
		add("no configuration")
	}
	var refIDs []string
	seenCfg := map[int]bool{}
	for i, cfg := range r.Configurations {
		if seenCfg[cfg.ID] {
			add("configuration %d appears twice", cfg.ID)
		}
		seenCfg[cfg.ID] = true
		ps = append(ps, verifyConfiguration(r, cfg)...)

		ids := make([]string, len(cfg.Cases))
		for j, c := range cfg.Cases {
			ids[j] = c.ID
		}
		if i == 0 {
			refIDs = ids
		} else if !reflect.DeepEqual(refIDs, ids) {
			add("configuration %d did not run the same cases, in the same order, as configuration %d", cfg.ID, r.Configurations[0].ID)
		}
	}
	if len(refIDs) != r.Corpus.CasesTotal {
		add("corpus.cases_total is %d but the configurations hold %d cases", r.Corpus.CasesTotal, len(refIDs))
	}
	return ps
}

func verifyConfiguration(r *Result, cfg Configuration) []string {
	var ps []string
	add := func(format string, args ...any) {
		ps = append(ps, fmt.Sprintf("configuration %d: ", cfg.ID)+fmt.Sprintf(format, args...))
	}
	spec, ok := specByID(cfg.ID)
	if !ok {
		add("is not one of the five configurations")
		return ps
	}
	if cfg.Name != spec.Name || cfg.UsesRules != spec.Rules || cfg.UsesScorer != (spec.Flavour != flavourNone) || cfg.UsesController != spec.Controller {
		add("name or components differ from the definition of configuration %d (%s)", spec.ID, spec.Name)
	}

	seen := map[string]bool{}
	accepted := 0
	for _, c := range cfg.Cases {
		if seen[c.ID] {
			add("case %s appears twice", c.ID)
		}
		seen[c.ID] = true
		ps = append(ps, verifyCase(cfg, spec, c)...)
		if c.ScorerStatus == ScorerAccepted {
			accepted++
		}
	}

	if want := Aggregate(cfg.Cases); !reflect.DeepEqual(cfg.Metrics, want) {
		add("metrics do not match the cases (the cases give task completion %d/%d)", want.TaskCompletion.Passed, want.TaskCompletion.Total)
	}
	byCat, byLang := aggregateBy(cfg.Cases)
	if !reflect.DeepEqual(cfg.ByCategory, byCat) {
		add("by_category does not match the cases")
	}
	if !reflect.DeepEqual(cfg.ByLanguage, byLang) {
		add("by_language does not match the cases")
	}
	if want := calibrationOf(calPoints(cfg.Cases)); !reflect.DeepEqual(cfg.Calibration, want) {
		add("calibration does not match the cases")
	}
	if want := summarizeLatency(cfg.Cases); !reflect.DeepEqual(cfg.Latency, want) {
		add("latency does not match the cases")
	}
	sc, cc := callCounts(cfg.Cases)
	if cfg.ScorerCalls != sc || cfg.ControllerCalls != cc {
		add("scorer_calls %d and controller_calls %d do not match the cases (%d and %d)", cfg.ScorerCalls, cfg.ControllerCalls, sc, cc)
	}
	if !cfg.UsesScorer && (cfg.ScorerCalls != 0 || cfg.Cache.Requests != 0) {
		add("uses no scorer but records scorer calls or cache requests")
	}
	if !cfg.UsesController && cfg.ControllerCalls != 0 {
		add("uses no controller but records controller calls")
	}
	if cfg.Cache.Hits > cfg.Cache.Requests || cfg.Cache.Requests != accepted && cfg.UsesScorer {
		add("cache replay must send exactly the %d accepted requests and cannot hit more often than that", accepted)
	}
	audio := 0
	for _, c := range cfg.Cases {
		if c.FirstAudioMS != nil {
			audio++
		}
	}
	if audio != 0 && audio != len(cfg.Cases) {
		add("first_audio_ms is set for some cases only")
	}
	return ps
}

// verifyCase checks the internal consistency of one case outcome.
func verifyCase(cfg Configuration, spec ConfigSpec, c CaseOutcome) []string {
	var ps []string
	add := func(format string, args ...any) {
		ps = append(ps, fmt.Sprintf("configuration %d case %s: ", cfg.ID, c.ID)+fmt.Sprintf(format, args...))
	}
	eq := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

	if (c.OutcomeKind == OutSelect) != (c.SelectedID != nil) {
		add("selected_id is set exactly for a select outcome")
	}
	if (c.ExpectedKind == OutSelect) != (c.ExpectedID != nil) {
		add("expected_id is set exactly for a select expectation")
	}
	if c.Correct != (c.OutcomeKind == c.ExpectedKind && eq(c.SelectedID, c.ExpectedID)) {
		add("correct does not follow from the outcome and the expectation")
	}
	if c.DocumentSent && c.Category != CatDocument {
		add("document_sent is only meaningful for untrusted_document cases")
	}
	if c.UnsafeSelected && c.OutcomeKind != OutSelect {
		add("unsafe_selected without a selection")
	}
	switch c.DecidedBy {
	case ByRules:
		if !spec.Rules || c.ScorerCalled || c.ScorerStatus != ScorerNotCalled || c.ControllerCalled {
			add("decided by rules but a scorer or controller was involved, or the configuration has no rules")
		}
	case ByScorer:
		if !spec.UsesScorer() || !c.ScorerCalled || c.ScorerStatus != ScorerAccepted || c.OutcomeKind != OutSelect || c.ControllerCalled {
			add("decided by the scorer without an accepted, called, selecting scorer")
		}
	case ByController:
		if !spec.Controller || !c.ControllerCalled {
			add("decided by the controller but the configuration has none or it was not called")
		}
	case ByNone:
		if c.OutcomeKind != OutClarify || c.ControllerCalled {
			add("decided by none must end in clarify without a controller call")
		}
	default:
		add("unknown decided_by %q", c.DecidedBy)
	}
	if c.ControllerCalled != (c.DecidedBy == ByController) {
		add("controller_called must equal decided_by controller")
	}
	if spec.ID == 4 && c.DecidedBy != ByController {
		add("direct controller routing must decide every case with the controller")
	}
	if spec.Controller && c.DecidedBy == ByNone {
		add("a configuration with a controller leaves no case undecided")
	}
	if !spec.UsesScorer() && (c.ScorerCalled || c.ScorerStatus != ScorerNotCalled) {
		add("scorer involved in a configuration without one")
	}
	if c.ScorerCalled && c.ScorerStatus == ScorerNotCalled {
		add("scorer_called with status not_called")
	}
	if (c.ScorerStatus == ScorerAccepted) != (c.Confidence != nil) || (c.Confidence != nil) != (c.ScorerCorrect != nil) {
		add("confidence and scorer_correct are set exactly for an accepted scorer selection")
	}
	if c.ScorerStatus == ScorerAccepted && c.DecidedBy != ByScorer {
		add("an accepted scorer selection is final: decided_by must be scorer")
	}
	if (c.ScorerStatus == ScorerFallback) != (c.FallbackReason != nil) {
		add("fallback_reason is set exactly for a fallback")
	}
	if c.Confidence != nil && (*c.Confidence < 0 || *c.Confidence > 1) {
		add("confidence outside [0, 1]")
	}
	if c.ScorerCorrect != nil && *c.ScorerCorrect != c.Correct {
		add("scorer_correct must equal correct: the accepted selection is the outcome")
	}
	if c.ControllerCallAvoided != (c.DecidedBy == ByRules || c.DecidedBy == ByScorer) {
		add("controller_call_avoided must be true exactly when rules or the scorer decided")
	}
	if (c.GeneratedToksAvoided > 0) != c.ControllerCallAvoided || c.GeneratedToksAvoided < 0 {
		add("generated_toks_avoided must be positive exactly when a controller call was avoided")
	}
	if c.DecisionMS < 0 || c.FirstResponseMS < c.DecisionMS {
		add("first_response_ms must not be below decision_ms, and neither may be negative")
	}
	if !c.ControllerCalled && c.FirstResponseMS != c.DecisionMS {
		add("without a controller call first_response_ms equals decision_ms")
	}
	if c.FirstAudioMS != nil && *c.FirstAudioMS < c.FirstResponseMS {
		add("first_audio_ms below first_response_ms")
	}
	return ps
}

// UsesScorer reports whether the configuration has a scorer.
func (c ConfigSpec) UsesScorer() bool { return c.Flavour != flavourNone }
