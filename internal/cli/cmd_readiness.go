package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/readiness/assist"
	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/report"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	rdystore "github.com/buxee/alix-ai/internal/readiness/store"
	corestore "github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// Integration readiness discovery from the command line (master plan 9.5, ADR 0037; milestone M8).
//
//	alixai readiness scan   DIR [-app ID] [-lang en|it] [-format md|json|both] [-no-model] [-model ID] [-out FILE] [-scaffold OUTDIR]
//	alixai readiness status [DIR] [-app ID] [-lang en|it] [-format md|json] [-diff] [-no-model] [-model ID]
//	alixai readiness list   [DIR] [-app ID] [-format md|json]
//	alixai readiness show   [DIR] [-app ID] [-report ID] [-lang en|it] [-format md|json]
//
// These work offline against the data directory, like `alixai audit` and `alixai providers`: no HTTP route, no OpenAPI
// or SDK change, and no running server needed. The scan is read-only over the directory the operator names, bounded,
// and never reads a secret-looking file. A report is advisory: it certifies nothing, signs nothing and activates
// nothing, and a complete one is not an audit pass (the audit still decides). The first line of every rendering says so.
//
// Where a report is kept. Reports are stored in the data directory under a key: -app ID when the operator names an
// application (the key the audit command looks up to link the latest report), otherwise a key derived from the
// directory. The same key is used by scan, status, list and show, so name the same one.
//
// What writes where. The scan reads the named directory and writes only to the data directory (the report) and, when
// -out is given, to that one file. Nothing is written into the scanned directory.
//
// Exit status. A completed scan exits 0 whatever the findings are; a non-zero status means the scan itself failed (a
// refused directory, an unusable store, a file that cannot be written).
//
// The model leg. Local by default: see readiness_assist.go. With no model configured, with -no-model, or when the
// model is unavailable, the deterministic leg runs alone and the output says so.

const (
	rdyCLIScanUsage   = "alixai readiness scan DIR [-config FILE] [-app ID] [-lang en|it] [-format md|json|both] [-no-model] [-model ID] [-out FILE] [-scaffold OUTDIR]"
	rdyCLIStatusUsage = "alixai readiness status [DIR] [-config FILE] [-app ID] [-lang en|it] [-format md|json] [-diff] [-no-model] [-model ID]"
	rdyCLIListUsage   = "alixai readiness list [DIR] [-config FILE] [-app ID] [-format md|json]"
	rdyCLIShowUsage   = "alixai readiness show [DIR] [-config FILE] [-app ID] [-report ID] [-lang en|it] [-format md|json]"

	// rdyCLIMaxNote bounds a model or store message echoed on stderr.
	rdyCLIMaxNote = 240
)

func init() {
	Register(Command{
		Name:    "readiness scan",
		Summary: "scan a project directory, report what AlixAI integration needs and what is missing (advisory, not an audit)",
		Run:     rdyCLIRunScan,
	})
	Register(Command{
		Name:    "readiness status",
		Summary: "re-evaluate the readiness checklist against the latest report and the current files, and show what changed",
		Run:     rdyCLIRunStatus,
	})
	Register(Command{
		Name:    "readiness list",
		Summary: "list the stored readiness reports with their input hashes",
		Run:     rdyCLIRunList,
	})
	Register(Command{
		Name:    "readiness show",
		Summary: "print a stored readiness report (the latest, or one by id)",
		Run:     rdyCLIRunShow,
	})
}

// rdyCLIParse parses flags that may come before or after the positional arguments (`scan DIR -app X` and
// `scan -app X DIR` are the same), and returns the positionals.
func rdyCLIParse(env *Env, fs *flag.FlagSet, usage string, args []string, maxPos int) ([]string, error) {
	var pos []string
	rest := args
	for {
		if err := env.ParseFlags(fs, usage, rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		if len(pos) > maxPos {
			return nil, Usagef("too many arguments (usage: %s)", usage)
		}
		rest = fs.Args()[1:]
	}
}

// rdyCLIFlags are the flags the commands share.
type rdyCLIFlags struct {
	config, app, lang, format, out, model, report string
	noModel, diff                                 bool
}

func rdyCLICommon(env *Env, fs *flag.FlagSet, f *rdyCLIFlags, formats string) {
	def, _ := env.LookupEnv("ALIXAI_CONFIG")
	fs.StringVar(&f.config, "config", def, "path to the YAML config file (default $ALIXAI_CONFIG; empty: built-in defaults)")
	fs.StringVar(&f.app, "app", "", "application id the report is stored under (default: a key derived from the directory)")
	fs.StringVar(&f.lang, "lang", "en", "report language: en or it")
	fs.StringVar(&f.format, "format", "md", "output format: "+formats)
}

func rdyCLIModelFlags(fs *flag.FlagSet, f *rdyCLIFlags) {
	fs.BoolVar(&f.noModel, "no-model", false, "run the deterministic scan alone, without the model leg")
	fs.StringVar(&f.model, "model", "", "registry id of the model to use (one of inference.models; default: the configured controller)")
}

func (f *rdyCLIFlags) check(formats ...string) (report.RdyLang, error) {
	lang := report.RdyLang(f.lang)
	if !lang.Valid() {
		return "", Usagef("-lang %q: want en or it", f.lang)
	}
	ok := false
	for _, v := range formats {
		ok = ok || f.format == v
	}
	if !ok {
		return "", Usagef("-format %q: want %s", f.format, strings.Join(formats, ", "))
	}
	if f.app != "" {
		if _, err := rdystore.RdyAppKey(f.app); err != nil {
			return "", Usagef("-app %q is not a plain application identifier", f.app)
		}
	}
	if f.model != "" && !modelIDRE.MatchString(f.model) {
		return "", Usagef("-model %q is not a registry entry id", f.model)
	}
	return lang, nil
}

// key is the store key: the application, else the directory.
func (f *rdyCLIFlags) key(dir string) (rdystore.RdyKey, error) {
	if f.app != "" {
		k, err := rdystore.RdyAppKey(f.app)
		if err != nil {
			return "", Usagef("-app %q is not a plain application identifier", f.app)
		}
		return k, nil
	}
	if dir == "" {
		return "", Usagef("name the directory or -app: reports are stored under one of them")
	}
	k, err := rdystore.RdyProjectKey(dir)
	if err != nil {
		return "", Usagef("%v", err)
	}
	return k, nil
}

// rdyCLIDeps is what a command works with.
type rdyCLIDeps struct {
	db  *corestore.DB
	st  *rdystore.RdyStore
	clk clock.Clock
}

func (d *rdyCLIDeps) close() {
	if d != nil && d.db != nil {
		_ = d.db.Close()
	}
}

// rdyCLIOpen loads the configuration and opens the store with its migrations. With create false and no database in
// the data directory it returns nil and no error: there is nothing stored, and a read-only command creates nothing.
func rdyCLIOpen(ctx context.Context, env *Env, configPath string, create bool) (*rdyCLIDeps, error) {
	if err := env.Config.LoadFrom(configPath, env.LookupEnv); err != nil {
		return nil, err
	}
	opts := corestore.FromConfig()
	opts.Path = filepath.Join(env.Config.Core().DataDir.String(), corestore.DefaultFileName)
	if !create {
		if _, err := os.Stat(opts.Path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("the database at %s cannot be read: %w", opts.Path, err)
		}
	}
	db, err := corestore.Open(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("open the store: %w", err)
	}
	clk := clock.Clock(clock.Real{})
	return &rdyCLIDeps{db: db, st: rdystore.RdyOpen(db, clk), clk: clk}, nil
}

// rdyCLINote bounds and cleans a message that goes to stderr: one line, no control characters, no secret-looking value.
func rdyCLINote(s string) string {
	s = scan.ScanRedact(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > rdyCLIMaxNote {
		out = out[:rdyCLIMaxNote]
		for len(out) > 0 && out[len(out)-1] >= 0x80 {
			out = out[:len(out)-1]
		}
		out += "..."
	}
	return out
}

func rdyCLIScope(app string) contracts.Scope {
	if app == "" {
		app = "readiness-project"
	}
	return contracts.Scope{
		DeploymentID: "alixai-operator-cli", ApplicationID: app, TenantID: "operator", WorkspaceID: "operator-cli",
		PrincipalID: "operator-cli", Roles: []string{"operator"}, CapabilityScopes: []string{"readiness.scan"}, SessionID: "ses_readiness_cli",
	}
}

// rdyCLIAbsDir resolves the directory the operator named; it must exist and be a directory.
func rdyCLIAbsDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" || strings.ContainsRune(dir, 0) {
		return "", Usagef("the directory is not a usable path")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", Usagef("the directory cannot be resolved: %v", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("the directory cannot be read: %w", err)
	}
	if !fi.IsDir() {
		return "", Usagef("%s is not a directory", abs)
	}
	return abs, nil
}

// rdyCLICheckOut refuses an -out that cannot be written before any work is done.
func rdyCLICheckOut(out string) error {
	if out == "" {
		return nil
	}
	if strings.ContainsRune(out, 0) {
		return Usagef("-out is not a usable path")
	}
	if fi, err := os.Stat(out); err == nil && fi.IsDir() {
		return Usagef("-out %s is a directory; name a file", out)
	}
	if fi, err := os.Stat(filepath.Dir(out)); err != nil || !fi.IsDir() {
		return Usagef("-out %s: its directory does not exist", out)
	}
	return nil
}

// rdyCLIRun is the result of one scan-and-store run.
type rdyCLIRun struct {
	Doc     readiness.Report // as stored
	Dropped int              // proposals the verifier did not turn into findings
	Saved   rdystore.RdySaved
	// Cause is why the model leg did not run, when it did not.
	Cause error
	// Disabled is set by -no-model.
	Disabled bool
}

// rdyCLIBuild turns a scan into a report: the assist leg when wanted, then report.RdyBuild, the only path to a report.
func rdyCLIBuild(ctx context.Context, env *Env, d *rdyCLIDeps, setup *rdyCLIAssistSetup, app, dir string, res *scan.ScanResult) (*report.RdyReport, *assist.AssistOutcome, error) {
	in := report.RdyBuildInput{
		Root: dir, ScannedRoot: dir, Scan: res,
		ReportID: report.RdyNewReportID(ids.NewGenerator(d.clk)), GeneratedAt: d.clk.Now(),
	}
	var out *assist.AssistOutcome
	if setup != nil {
		var err error
		out, err = assist.AssistRun(ctx, rdyCLIScope(app), dir, res, setup.Config)
		if err != nil {
			return nil, nil, err
		}
		out.Apply(&in)
	}
	rep, err := report.RdyBuild(in)
	return rep, out, err
}

// rdyCLIModelSetup builds the model setup for a run, or nil when -no-model.
func rdyCLIModelSetup(ctx context.Context, env *Env, f *rdyCLIFlags) (*rdyCLIAssistSetup, error) {
	if f.noModel {
		return nil, nil
	}
	s, err := rdyCLIModelSource(ctx, env, f.app, f.model)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// rdyCLIScanStore scans dir, runs the model leg, stores the report under key and returns what was stored.
func rdyCLIScanStore(ctx context.Context, env *Env, d *rdyCLIDeps, key rdystore.RdyKey, f *rdyCLIFlags, dir string, res *scan.ScanResult, setup *rdyCLIAssistSetup) (*rdyCLIRun, error) {
	rep, out, err := rdyCLIBuild(ctx, env, d, setup, f.app, dir, res)
	if err != nil {
		return nil, err
	}
	saved, err := d.st.Save(ctx, key, rep.Doc)
	if err != nil {
		return nil, err
	}
	doc, ok, err := d.st.Get(ctx, key, saved.ReportID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, contracts.New(contracts.ErrInternal, "the report that was just stored cannot be read back")
	}
	run := &rdyCLIRun{Doc: doc, Dropped: len(rep.Dropped), Saved: saved, Disabled: setup == nil}
	if out != nil {
		run.Cause = out.Cause
	}
	return run, nil
}

// rdyCLIModelNotes tells the operator, on stderr, what the model leg did when it did not run as asked.
func rdyCLIModelNotes(env *Env, run *rdyCLIRun) {
	switch {
	case run.Disabled:
		fmt.Fprintln(env.Stderr, "alixai: note: the model leg is disabled by -no-model; this report comes from the deterministic scan alone")
	case run.Doc.ModelLeg == nil || run.Doc.ModelLeg.Status == readiness.ReportModelLegStatusRan:
	default:
		msg := "the model leg did not run (" + string(run.Doc.ModelLeg.Status) + "); this report comes from the deterministic scan alone"
		if run.Cause != nil {
			msg += ": " + string(contracts.CodeOf(run.Cause)) + ": " + rdyCLINote(run.Cause.Error())
		}
		fmt.Fprintln(env.Stderr, "alixai: note: "+msg)
	}
}

func rdyCLICatalog() (*catalog.Catalog, error) { return catalog.CatalogLoad() }

func rdyCLIJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// rdyCLIWriteOut writes the one file the operator named, owner-only.
func rdyCLIWriteOut(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("the report is stored, but -out %s could not be written: %w", path, err)
	}
	return nil
}

// rdyCLIEmitReport prints a stored report in the format asked for. For "both" the markdown goes to stdout and the JSON to
// out when it is given (after the markdown on stdout when it is not). out receives a copy of what stdout would
// carry for md and json.
func rdyCLIEmitReport(env *Env, doc readiness.Report, dropped int, lang report.RdyLang, format, out string) error {
	cat, err := rdyCLICatalog()
	if err != nil {
		return err
	}
	var md string
	if format != "json" {
		if md, err = report.RdyRenderMarkdownWith(doc, cat, lang, report.RdyRenderOptions{DroppedProposals: dropped}); err != nil {
			return err
		}
	}
	var js []byte
	if format != "md" {
		if js, err = rdyCLIJSON(doc); err != nil {
			return err
		}
	}
	switch format {
	case "md":
		fmt.Fprint(env.Stdout, md)
		if out != "" {
			return rdyCLIWriteOut(out, []byte(md))
		}
	case "json":
		_, _ = env.Stdout.Write(js)
		if out != "" {
			return rdyCLIWriteOut(out, js)
		}
	default:
		fmt.Fprint(env.Stdout, md)
		if out != "" {
			return rdyCLIWriteOut(out, js)
		}
		fmt.Fprint(env.Stdout, "\n---\n\n")
		_, _ = env.Stdout.Write(js)
	}
	return nil
}

func rdyCLIRunScan(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("readiness scan")
	var f rdyCLIFlags
	rdyCLICommon(env, fs, &f, "md, json or both")
	rdyCLIModelFlags(fs, &f)
	fs.StringVar(&f.out, "out", "", "also write the report to this one file (owner-only); with -format both it receives the JSON")
	pos, err := rdyCLIParse(env, fs, rdyCLIScanUsage, args, 1)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Usagef("need exactly one directory (usage: %s)", rdyCLIScanUsage)
	}
	lang, err := f.check("md", "json", "both")
	if err != nil {
		return err
	}
	if err := rdyCLICheckOut(f.out); err != nil {
		return err
	}
	dir, err := rdyCLIAbsDir(pos[0])
	if err != nil {
		return err
	}
	key, err := f.key(dir)
	if err != nil {
		return err
	}
	d, err := rdyCLIOpen(ctx, env, f.config, true)
	if err != nil {
		return err
	}
	defer d.close()
	setup, err := rdyCLIModelSetup(ctx, env, &f)
	if err != nil {
		return err
	}
	res, err := scan.ScanDir(dir, scan.ScanOptions{})
	if err != nil {
		return fmt.Errorf("the scan failed: %w", err)
	}
	run, err := rdyCLIScanStore(ctx, env, d, key, &f, dir, res, setup)
	if err != nil {
		return fmt.Errorf("the scan failed: %w", err)
	}
	rdyCLIModelNotes(env, run)
	fmt.Fprintf(env.Stderr, "alixai: note: stored report %s under %s\n", run.Doc.ReportID, key)
	return rdyCLIEmitReport(env, run.Doc, run.Dropped, lang, f.format, f.out)
}

func rdyCLIRunList(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("readiness list")
	var f rdyCLIFlags
	rdyCLICommon(env, fs, &f, "md or json")
	pos, err := rdyCLIParse(env, fs, rdyCLIListUsage, args, 1)
	if err != nil {
		return err
	}
	if _, err := f.check("md", "json"); err != nil {
		return err
	}
	dir := ""
	if len(pos) == 1 {
		if dir, err = rdyCLIAbsDir(pos[0]); err != nil {
			return err
		}
	}
	key, err := f.key(dir)
	if err != nil {
		return err
	}
	d, err := rdyCLIOpen(ctx, env, f.config, false)
	if err != nil {
		return err
	}
	defer d.close()
	type entry struct {
		ReportID         string            `json:"report_id"`
		PreviousReportID string            `json:"previous_report_id,omitempty"`
		GeneratedAt      time.Time         `json:"generated_at"`
		Mode             string            `json:"mode"`
		InputsSHA256     string            `json:"inputs_sha256"`
		CatalogSHA256    string            `json:"catalog_sha256"`
		Summary          readiness.Summary `json:"summary"`
	}
	list := []entry{}
	if d != nil {
		hist, err := d.st.History(ctx, key, 0)
		if err != nil {
			return err
		}
		for _, h := range hist {
			e := entry{
				ReportID: string(h.ReportID), GeneratedAt: h.GeneratedAt, Mode: string(h.Mode),
				InputsSHA256: h.InputsSHA256, CatalogSHA256: h.CatalogSHA256, Summary: h.Summary,
			}
			if h.PreviousReportID != nil {
				e.PreviousReportID = string(*h.PreviousReportID)
			}
			list = append(list, e)
		}
	}
	if f.format == "json" {
		b, err := rdyCLIJSON(map[string]any{"key": string(key), "reports": list})
		if err != nil {
			return err
		}
		_, err = env.Stdout.Write(b)
		return err
	}
	fmt.Fprintf(env.Stdout, "%s\n\n", report.RdyAdvisoryLine(report.RdyLangEN))
	if len(list) == 0 {
		fmt.Fprintf(env.Stdout, "No readiness report is stored under %s. Create one with: alixai readiness scan DIR\n", key)
		return nil
	}
	fmt.Fprintf(env.Stdout, "Readiness reports under %s, newest first:\n\n", key)
	for _, e := range list {
		na := ""
		if e.Summary.NotApplicable != nil {
			na = fmt.Sprintf(", not applicable %d", *e.Summary.NotApplicable)
		}
		fmt.Fprintf(env.Stdout, "- %s  %s  %s  inputs %s  met %d, partial %d, missing %d, unverified %d, contradicted %d%s\n",
			e.ReportID, e.GeneratedAt.UTC().Format(time.RFC3339), e.Mode, e.InputsSHA256,
			e.Summary.Found, e.Summary.Partial, e.Summary.Missing, e.Summary.Unverified, e.Summary.Contradicted, na)
	}
	return nil
}

func rdyCLIRunShow(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("readiness show")
	var f rdyCLIFlags
	rdyCLICommon(env, fs, &f, "md or json")
	fs.StringVar(&f.report, "report", "", "id of the report to show (default: the latest)")
	pos, err := rdyCLIParse(env, fs, rdyCLIShowUsage, args, 1)
	if err != nil {
		return err
	}
	lang, err := f.check("md", "json")
	if err != nil {
		return err
	}
	dir := ""
	if len(pos) == 1 {
		if dir, err = rdyCLIAbsDir(pos[0]); err != nil {
			return err
		}
	}
	key, err := f.key(dir)
	if err != nil {
		return err
	}
	d, err := rdyCLIOpen(ctx, env, f.config, false)
	if err != nil {
		return err
	}
	defer d.close()
	if d == nil {
		return fmt.Errorf("no readiness report is stored under %s; create one with: alixai readiness scan DIR", key)
	}
	var (
		doc readiness.Report
		ok  bool
	)
	if f.report != "" {
		doc, ok, err = d.st.Get(ctx, key, common.TypedID(f.report))
	} else {
		doc, ok, err = d.st.Latest(ctx, key)
	}
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such readiness report under %s; see: alixai readiness list", key)
	}
	return rdyCLIEmitReport(env, doc, 0, lang, f.format, "")
}

// rdyCLIAuditLinkOut is the readiness line of the audit command's output. It is information for the operator: the audit
// itself is unchanged and independent of it.
type rdyCLIAuditLinkOut struct {
	// Status is "linked" (a stored report exists), "none" (there is none) or "unavailable" (the store could not be read).
	Status       string             `json:"status"`
	ReportID     string             `json:"report_id,omitempty"`
	GeneratedAt  string             `json:"generated_at,omitempty"`
	Mode         string             `json:"mode,omitempty"`
	InputsSHA256 string             `json:"inputs_sha256,omitempty"`
	Summary      *readiness.Summary `json:"summary,omitempty"`
	Note         string             `json:"note"`
}

const rdyCLIAuditAdvisory = "A readiness report is advisory: it is not an audit pass, and this audit does not depend on it."

// rdyCLIAuditLink finds the latest readiness report stored for the application (scan -app APP) and describes it. It
// never fails the audit: any problem reading the store is "unavailable".
func rdyCLIAuditLink(ctx context.Context, db *corestore.DB, app string) *rdyCLIAuditLinkOut {
	key, err := rdystore.RdyAppKey(app)
	if err != nil {
		return &rdyCLIAuditLinkOut{Status: "unavailable", Note: "the application id is not a readiness key. " + rdyCLIAuditAdvisory}
	}
	doc, ok, err := rdystore.RdyOpen(db, clock.Real{}).Latest(ctx, key)
	switch {
	case err != nil:
		return &rdyCLIAuditLinkOut{Status: "unavailable", Note: "the readiness store could not be read. " + rdyCLIAuditAdvisory}
	case !ok:
		return &rdyCLIAuditLinkOut{Status: "none", Note: "no readiness report is stored for this application; alixai readiness scan DIR -app " + app + " creates one. " + rdyCLIAuditAdvisory}
	}
	sum := doc.Summary
	return &rdyCLIAuditLinkOut{
		Status: "linked", ReportID: string(doc.ReportID), GeneratedAt: string(doc.GeneratedAt), Mode: string(doc.Mode),
		InputsSHA256: string(doc.InputsSHA256), Summary: &sum,
		Note: "the latest readiness report for this application; show it with: alixai readiness show -app " + app + ". " + rdyCLIAuditAdvisory,
	}
}

// rdyCLIStatusText is the wording of `readiness status` in one language. The report package owns the wording of a
// report; a status view has its own.
type rdyCLIStatusText struct {
	title, report, generated, evaluated, mode, modeDet, modeModel, rescanned, unchanged, summary, requirements string
	was, feeds, none, diffTitle, noPrev, newlyMet, newlyMissing, otherChanges, added, removed                  string
	inputsChanged, catalogChanged, unchangedCount, model, olderRules                                           string
	status                                                                                                     map[readiness.Status]string
	level                                                                                                      map[readiness.Level]string
}

var rdyCLIStatusEN = rdyCLIStatusText{
	title: "Integration readiness status", report: "Report", generated: "Generated", evaluated: "Evaluated", mode: "Mode",
	modeDet: "No model ran: the report comes from the deterministic scan alone.", modeModel: "A model proposed findings and the verifier checked each one.",
	rescanned: "The files or the catalog changed since the previous report, so the project was scanned again and a new report was stored.",
	unchanged: "The files and the catalog are unchanged since this report was stored.",
	summary:   "Summary", requirements: "Requirements", was: "was", feeds: "feeds the audit check", none: "None.",
	diffTitle: "Changes since the previous report", noPrev: "There is no previous report to compare with.",
	newlyMet: "Newly met", newlyMissing: "Newly missing", otherChanges: "Other changes", added: "New in the checklist", removed: "No longer in the checklist",
	inputsChanged: "The inputs (files, catalog or scan options) differ from the previous report.", catalogChanged: "The catalog differs from the previous report.",
	unchangedCount: "Requirements with the same status", model: "Model",
	olderRules: "The stored report was made by an older version of the readiness rules, so the project was evaluated again with the current rules and a new report was stored.",
	status: map[readiness.Status]string{
		readiness.StatusFound: "Met", readiness.StatusPartial: "Partial", readiness.StatusMissing: "Missing",
		readiness.StatusUnverified: "Unverified", readiness.StatusContradicted: "Contradicted",
		readiness.StatusNotApplicable: "Not applicable",
	},
	level: map[readiness.Level]string{readiness.LevelMandatory: "mandatory", readiness.LevelRecommended: "recommended"},
}

var rdyCLIStatusIT = rdyCLIStatusText{
	title: "Stato di readiness dell'integrazione", report: "Rapporto", generated: "Generato", evaluated: "Valutato", mode: "Modalita'",
	modeDet:   "Nessun modello e' stato eseguito: il rapporto deriva solo dalla scansione deterministica.",
	modeModel: "Un modello ha proposto dei riscontri e il verificatore ha controllato ciascuno.",
	rescanned: "I file o il catalogo sono cambiati dal rapporto precedente: il progetto e' stato analizzato di nuovo ed e' stato salvato un nuovo rapporto.",
	unchanged: "I file e il catalogo sono invariati da quando il rapporto e' stato salvato.",
	summary:   "Riepilogo", requirements: "Requisiti", was: "prima", feeds: "alimenta il controllo di audit", none: "Nessuno.",
	diffTitle: "Cambiamenti dal rapporto precedente", noPrev: "Non c'e' un rapporto precedente con cui confrontare.",
	newlyMet: "Ora soddisfatti", newlyMissing: "Ora mancanti", otherChanges: "Altri cambiamenti", added: "Nuovi nella checklist", removed: "Non piu' nella checklist",
	inputsChanged: "Gli input (file, catalogo od opzioni di scansione) differiscono dal rapporto precedente.", catalogChanged: "Il catalogo differisce dal rapporto precedente.",
	unchangedCount: "Requisiti con lo stesso stato", model: "Modello",
	olderRules: "Il rapporto salvato era stato prodotto da una versione precedente delle regole di readiness: il progetto e' stato valutato di nuovo con le regole attuali ed e' stato salvato un nuovo rapporto.",
	status: map[readiness.Status]string{
		readiness.StatusFound: "Soddisfatto", readiness.StatusPartial: "Parziale", readiness.StatusMissing: "Mancante",
		readiness.StatusUnverified: "Non verificato", readiness.StatusContradicted: "Contraddittorio",
		readiness.StatusNotApplicable: "Non applicabile",
	},
	level: map[readiness.Level]string{readiness.LevelMandatory: "obbligatorio", readiness.LevelRecommended: "raccomandato"},
}

// rdyCLIStatusOrder is the order the requirements are listed in: what needs attention first. Not applicable is last:
// the requirements of a transport the project does not use, because another one is met.
var rdyCLIStatusOrder = []readiness.Status{
	readiness.StatusContradicted, readiness.StatusMissing, readiness.StatusUnverified, readiness.StatusPartial, readiness.StatusFound,
	readiness.StatusNotApplicable,
}

// rdyCLITitle is the title of a requirement in the language, or its id when this build's catalog does not know it.
func rdyCLITitle(cat *catalog.Catalog, id readiness.RequirementID, lang report.RdyLang) string {
	if r, ok := cat.Get(id); ok {
		if lang == report.RdyLangIT {
			return string(r.Title.It)
		}
		return string(r.Title.En)
	}
	return string(id)
}

func rdyCLIChangeLine(cat *catalog.Catalog, c rdystore.RdyChange, tx rdyCLIStatusText, lang report.RdyLang) string {
	return fmt.Sprintf("- `%s` %s: %s -> %s\n", c.RequirementID, rdyCLINote(rdyCLITitle(cat, c.RequirementID, lang)), tx.status[c.From], tx.status[c.To])
}

// rdyCLIRenderStatus renders a status view. diff is nil unless -diff was given and there is a previous report to compare
// with.
func rdyCLIRenderStatus(view readiness.StatusView, cur readiness.Report, cat *catalog.Catalog, lang report.RdyLang, rescanned, olderRules bool, diff *rdystore.RdyDiff) string {
	tx := rdyCLIStatusEN
	if lang == report.RdyLangIT {
		tx = rdyCLIStatusIT
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("%s\n\n# %s\n\n", report.RdyAdvisoryLine(lang), tx.title)
	w("- %s: `%s`\n- %s: `%s`\n- %s: `%s`\n", tx.report, view.ReportID, tx.generated, view.GeneratedAt, tx.evaluated, view.EvaluatedAt)
	if view.Mode == readiness.StatusViewModeDeterministicAndModel {
		w("- %s: %s\n", tx.mode, tx.modeModel)
		if cur.Model != nil {
			q := "unqualified, advisory"
			if cur.Model.Qualification == readiness.ReportModelQualificationQualified {
				q = "qualified, advisory"
			}
			w("- %s: `%s` (%s), %s\n", tx.model, cur.Model.ModelID, cur.Model.Runtime, q)
		}
	} else {
		w("- %s: %s\n", tx.mode, tx.modeDet)
	}
	switch {
	case rescanned && olderRules:
		w("- %s\n", tx.olderRules)
	case rescanned:
		w("- %s\n", tx.rescanned)
	default:
		w("- %s\n", tx.unchanged)
	}
	w("\n## %s\n\n", tx.summary)
	counts := map[readiness.Status]int64{
		readiness.StatusFound: view.Summary.Found, readiness.StatusPartial: view.Summary.Partial, readiness.StatusMissing: view.Summary.Missing,
		readiness.StatusUnverified: view.Summary.Unverified, readiness.StatusContradicted: view.Summary.Contradicted,
	}
	if n := view.Summary.NotApplicable; n != nil {
		counts[readiness.StatusNotApplicable] = *n
	}
	for _, st := range rdyCLIStatusOrder {
		if st == readiness.StatusNotApplicable && counts[st] == 0 {
			continue
		}
		w("- %s: %d\n", tx.status[st], counts[st])
	}
	w("\n## %s\n", tx.requirements)
	for _, st := range rdyCLIStatusOrder {
		var lines []string
		for _, it := range view.Items {
			if it.Status != st {
				continue
			}
			line := fmt.Sprintf("- `%s` %s (%s), %s `%s`", it.RequirementID, rdyCLINote(rdyCLITitle(cat, it.RequirementID, lang)), tx.level[it.Level], tx.feeds, it.CheckID)
			if it.Changed && it.PreviousStatus != nil {
				line += fmt.Sprintf(" [%s: %s]", tx.was, tx.status[*it.PreviousStatus])
			}
			lines = append(lines, line)
		}
		if st == readiness.StatusNotApplicable && len(lines) == 0 {
			continue
		}
		w("\n### %s (%d)\n\n", tx.status[st], len(lines))
		if len(lines) == 0 {
			w("%s\n", tx.none)
			continue
		}
		w("%s\n", strings.Join(lines, "\n"))
	}
	if diff != nil {
		w("\n## %s\n\n", tx.diffTitle)
		w("- `%s` -> `%s`\n", diff.FromID, diff.ToID)
		if diff.InputsChanged {
			w("- %s\n", tx.inputsChanged)
		}
		if diff.CatalogChanged {
			w("- %s\n", tx.catalogChanged)
		}
		w("- %s: %d\n", tx.unchangedCount, diff.Unchanged)
		for _, sec := range []struct {
			name  string
			items []rdystore.RdyChange
		}{{tx.newlyMet, diff.NewlyMet}, {tx.newlyMissing, diff.NewlyMissing}, {tx.otherChanges, diff.Changed}} {
			w("\n### %s (%d)\n\n", sec.name, len(sec.items))
			if len(sec.items) == 0 {
				w("%s\n", tx.none)
			}
			for _, c := range sec.items {
				w("%s", rdyCLIChangeLine(cat, c, tx, lang))
			}
		}
		for _, sec := range []struct {
			name string
			ids  []readiness.RequirementID
		}{{tx.added, diff.Added}, {tx.removed, diff.Removed}} {
			if len(sec.ids) == 0 {
				continue
			}
			w("\n### %s (%d)\n\n", sec.name, len(sec.ids))
			for _, id := range sec.ids {
				w("- `%s` %s\n", id, rdyCLINote(rdyCLITitle(cat, id, lang)))
			}
		}
	}
	return b.String()
}

func rdyCLIRunStatus(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("readiness status")
	var f rdyCLIFlags
	rdyCLICommon(env, fs, &f, "md or json")
	rdyCLIModelFlags(fs, &f)
	fs.BoolVar(&f.diff, "diff", false, "also list the requirements that changed since the previous report")
	pos, err := rdyCLIParse(env, fs, rdyCLIStatusUsage, args, 1)
	if err != nil {
		return err
	}
	lang, err := f.check("md", "json")
	if err != nil {
		return err
	}
	dir := ""
	if len(pos) == 1 {
		if dir, err = rdyCLIAbsDir(pos[0]); err != nil {
			return err
		}
	}
	key, err := f.key(dir)
	if err != nil {
		return err
	}
	d, err := rdyCLIOpen(ctx, env, f.config, false)
	if err != nil {
		return err
	}
	defer d.close()
	none := fmt.Errorf("no readiness report is stored under %s; create one with: alixai readiness scan DIR", key)
	if d == nil {
		return none
	}
	latest, ok, err := d.st.Latest(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return none
	}
	if dir == "" {
		// The report records the absolute path it scanned.
		if dir, err = rdyCLIAbsDir(latest.ScannedRoot); err != nil {
			return fmt.Errorf("the directory the latest report scanned (%s) is not available; name the directory: %w", rdyCLINote(latest.ScannedRoot), err)
		}
	}

	// Re-evaluate against the current tree. A deterministic report is built from a fresh scan only to compare its inputs
	// hash with the stored one; the model leg runs, and a new report is stored, only when the inputs differ.
	res, err := scan.ScanDir(dir, scan.ScanOptions{})
	if err != nil {
		return fmt.Errorf("the scan failed: %w", err)
	}
	probe, _, err := rdyCLIBuild(ctx, env, d, nil, f.app, dir, res)
	if err != nil {
		return fmt.Errorf("the scan failed: %w", err)
	}
	// The rules version is part of the inputs hash, so a report made by older rules is stale even when the files are not.
	rescanned := probe.Doc.InputsSHA256 != latest.InputsSHA256
	olderRules := report.RdyReportEvaluatorVersion(latest) < report.RdyEvaluatorVersion
	if rescanned {
		setup, err := rdyCLIModelSetup(ctx, env, &f)
		if err != nil {
			return err
		}
		run, err := rdyCLIScanStore(ctx, env, d, key, &f, dir, res, setup)
		if err != nil {
			return fmt.Errorf("the scan failed: %w", err)
		}
		rdyCLIModelNotes(env, run)
		if olderRules {
			fmt.Fprintf(env.Stderr, "alixai: note: report %s was made by older readiness rules (version %d, now %d); evaluated again with the current rules and stored report %s\n",
				latest.ReportID, report.RdyReportEvaluatorVersion(latest), report.RdyEvaluatorVersion, run.Doc.ReportID)
		} else {
			fmt.Fprintf(env.Stderr, "alixai: note: the files or the catalog changed since report %s; scanned again and stored report %s\n", latest.ReportID, run.Doc.ReportID)
		}
	}
	view, ok, err := d.st.StatusView(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return none
	}
	cur, ok, err := d.st.Get(ctx, key, view.ReportID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the latest report cannot be read back")
	}
	var diff *rdystore.RdyDiff
	if f.diff && view.PreviousReportID != nil {
		prev, ok, err := d.st.Get(ctx, key, *view.PreviousReportID)
		if err != nil {
			return err
		}
		if ok {
			dd := rdystore.RdyDiffReports(prev, cur)
			diff = &dd
		}
	}
	if f.format == "json" {
		out := map[string]any{"rescanned": rescanned, "older_rules": rescanned && olderRules, "status": view}
		if f.diff {
			out["diff"] = rdyCLIDiffJSON(diff)
		}
		b, err := rdyCLIJSON(out)
		if err != nil {
			return err
		}
		_, err = env.Stdout.Write(b)
		return err
	}
	cat, err := rdyCLICatalog()
	if err != nil {
		return err
	}
	fmt.Fprint(env.Stdout, rdyCLIRenderStatus(view, cur, cat, lang, rescanned, rescanned && olderRules, diff))
	if f.diff && diff == nil {
		tx := rdyCLIStatusEN
		if lang == report.RdyLangIT {
			tx = rdyCLIStatusIT
		}
		fmt.Fprintf(env.Stdout, "\n## %s\n\n%s\n", tx.diffTitle, tx.noPrev)
	}
	return nil
}

type rdyCLIChangeJSON struct {
	RequirementID string `json:"requirement_id"`
	CheckID       string `json:"check_id"`
	Level         string `json:"level"`
	From          string `json:"from"`
	To            string `json:"to"`
}

// rdyCLIDiffJSON is the diff as the JSON output carries it; nil when there is no previous report.
func rdyCLIDiffJSON(d *rdystore.RdyDiff) any {
	if d == nil {
		return nil
	}
	conv := func(cs []rdystore.RdyChange) []rdyCLIChangeJSON {
		out := []rdyCLIChangeJSON{}
		for _, c := range cs {
			out = append(out, rdyCLIChangeJSON{string(c.RequirementID), string(c.CheckID), string(c.Level), string(c.From), string(c.To)})
		}
		return out
	}
	ids := func(in []readiness.RequirementID) []string {
		out := []string{}
		for _, id := range in {
			out = append(out, string(id))
		}
		return out
	}
	return map[string]any{
		"from_report_id": d.FromID, "to_report_id": d.ToID, "inputs_changed": d.InputsChanged, "catalog_changed": d.CatalogChanged,
		"newly_met": conv(d.NewlyMet), "newly_missing": conv(d.NewlyMissing), "changed": conv(d.Changed),
		"added": ids(d.Added), "removed": ids(d.Removed), "unchanged": d.Unchanged,
	}
}
