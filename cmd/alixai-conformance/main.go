// Command alixai-conformance is the integration-package conformance tool of master plan
// 21 (milestone 3): what an application team runs in its own CI to learn whether its
// Application Integration Package and the running application behind it are ready to
// be activated on AlixAI.
//
//	alixai-conformance [flags] PACKAGE_DIR
//
// It runs three stages, in order, and prints one report (JSON, and JUnit XML on request):
//
//	package        the loader's schemas and cross-file checks, and the package's signature
//	               and integrity digests (`alixai package validate`)
//	audit          the full integration audit of master plan 4.5, against the running
//	               application: signatures, documentation, inventory, deterministic
//	               results, scopes, conversation contracts, RAG sources, safe execution,
//	               failures and the runtime smoke test (`alixai audit run -no-activate`)
//	conversation   each example of each capability contract replayed as a scripted turn
//	               through the real dialogue engine, policy gateway and action coordinator
//	               against the running application, with a scripted model provider
//
// The audit and the conversation stage need the application running and reachable at
// the endpoints its manifest names (or the ones -endpoint gives instead), in the test
// support mode its package's fixtures.provisioning describes: they create, use and
// remove disposable tenants, never touching a tenant they did not create.
//
// A package that fails its first stage is not looked at further: the later stages are
// not_run, which fails the run. The tool never activates anything and never installs
// into a real deployment: the package is installed, copied, into a throwaway data
// directory made for the run (the copy is pointed at the endpoints under test and signed
// with a key generated for the run, so the original's signature is checked by the first
// stage and never needs the author's key here).
//
// # What passes
//
// Five statuses are reported, in the report and in the JUnit file: pass, warn, unverified,
// not_run and fail. Fail and not_run always fail the run (a stage that did not run, or a
// package that was not looked at, proves nothing). Warn and unverified do not, and are named
// in every report: the integration audit itself records a section it could not look at as
// unverified and still lets a package activate once its warnings are accepted (the clinic's
// retrieval sections are unverified in this build, because the documents of a source shipped
// inside the package are not available to the audit), and a run that failed for what the
// audit accepts would be of no use in a team's CI. -strict fails both, in the exit status and
// in the JUnit file, which maps unverified to a skip otherwise.
//
// # Exit status
//
//	0  nothing failed (items may have warned or been unverified, without -strict)
//	1  at least one item failed or was not run (with -strict, warned or was unverified)
//	2  the command line was wrong
//
// # Conversation conformance
//
// The examples of a capability (capabilities/*.yaml, examples:) are test data, never
// runtime policy (master plan 9.2): each one drives a scripted turn. The model is a
// scripted provider that proposes exactly what the example says a correct reading of its
// utterance contains; what the core does with the proposal, what the application is
// asked and what the user is told are what is checked (stage_conversation.go).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// Exit statuses.
const (
	exitPass  = 0
	exitFail  = 1
	exitUsage = 2
)

// listFlag is a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

type options struct {
	pkgDir        string
	targets       map[string]string
	trustKeys     []string
	allowTestKeys bool
	jsonOut       string
	junitOut      string
	strict        bool
	coreVersion   string
	workDir       string
	timeout       time.Duration
}

const usageText = `usage: alixai-conformance [flags] PACKAGE_DIR

flags:
  -endpoint ID=URL       point the package's endpoint ID at the running application's URL (a native endpoint over
                         loopback_tcp: its host:port; its application key is read from the environment
                         variable its adapter configuration names) (repeatable;
                         default: the url the manifest names)
  -trust-key KEYID=KEY   trust this publisher key (base64 of the raw 32-byte public key) to establish
                         who signed the package (repeatable)
  -allow-test-keys       accept a published TEST-ONLY key as -trust-key (fixtures only)
  -json FILE             write the JSON report to FILE ('-' is standard output, the default)
  -junit FILE            also write a JUnit XML report to FILE
  -strict                fail the run on warnings and unverified items too
  -core-version V        AlixAI core version the package is checked against
  -work DIR              build the throwaway deployment in the empty directory DIR and keep it, to inspect
                         the signed audit report (default: a temporary directory removed at the end)
  -timeout D             give up after D (default 10m)
`

// run is main with its streams, so tests can drive it.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stdout, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitPass
		}
		fmt.Fprintf(stderr, "alixai-conformance: %v\n", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	rep := conform(ctx, opts)
	rep.finish()

	if err := emit(rep, opts, stdout); err != nil {
		fmt.Fprintf(stderr, "alixai-conformance: %v\n", err)
		return exitFail
	}
	summarise(stderr, rep)
	if rep.Result != "pass" {
		return exitFail
	}
	return exitPass
}

func parseArgs(args []string, stdout, stderr io.Writer) (options, error) {
	opts := options{targets: map[string]string{}, jsonOut: "-", timeout: 10 * time.Minute}
	fs := flag.NewFlagSet("alixai-conformance", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var endpoints, keys listFlag
	fs.Var(&endpoints, "endpoint", "")
	fs.Var(&keys, "trust-key", "")
	fs.BoolVar(&opts.allowTestKeys, "allow-test-keys", false, "")
	fs.StringVar(&opts.jsonOut, "json", "-", "")
	fs.StringVar(&opts.junitOut, "junit", "", "")
	fs.BoolVar(&opts.strict, "strict", false, "")
	fs.StringVar(&opts.coreVersion, "core-version", "", "")
	fs.StringVar(&opts.workDir, "work", "", "")
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usageText)
			return opts, flag.ErrHelp
		}
		return opts, fmt.Errorf("%v\n%s", err, usageText)
	}
	if fs.NArg() != 1 {
		return opts, fmt.Errorf("need exactly one package directory\n%s", usageText)
	}
	opts.pkgDir = fs.Arg(0)
	if opts.timeout <= 0 {
		return opts, errors.New("-timeout must be positive")
	}
	for _, e := range endpoints {
		id, target, ok := strings.Cut(e, "=")
		if !ok || id == "" || target == "" {
			return opts, fmt.Errorf("-endpoint %q is not ID=URL", e)
		}
		if _, dup := opts.targets[id]; dup {
			return opts, fmt.Errorf("-endpoint %s given twice", id)
		}
		opts.targets[id] = target
	}
	opts.trustKeys = keys
	if opts.jsonOut == "-" && opts.junitOut == "-" {
		return opts, errors.New("-json and -junit cannot both be standard output")
	}
	return opts, nil
}

// conform runs the stages and returns the report, not yet rolled up.
func conform(ctx context.Context, opts options) *Report {
	rep := &Report{Strict: opts.strict, Package: PackageInfo{Path: opts.pkgDir}}
	if len(opts.targets) > 0 {
		rep.Targets = opts.targets
	}

	pkgStage, pkgOut := validatePackage(ctx, opts.pkgDir, opts.coreVersion, opts.trustKeys, opts.allowTestKeys)
	rep.Package.ApplicationID, rep.Package.Version = pkgOut.ApplicationID, pkgOut.PackageVersion
	rep.Package.ManifestHash, rep.Package.Authenticity, rep.Package.SignedBy = pkgOut.ManifestHash, pkgOut.Authenticity, pkgOut.SignedBy
	rep.Stages = append(rep.Stages, pkgStage)

	notRun := func(why string) {
		for _, name := range []string{"audit", "conversation"} {
			rep.Stages = append(rep.Stages, Stage{Name: name, Status: StatusNotRun, Message: why,
				Items: []Item{{ID: name + ".not_run", Name: name + " stage", Status: StatusNotRun, Message: why}}})
		}
	}
	if stageFailed(pkgStage) {
		notRun("the package stage failed, so the package is not looked at further")
		return rep
	}

	pkg, err := loadPackage(ctx, opts.pkgDir, opts.coreVersion)
	if err != nil {
		notRun("the package could not be loaded: " + err.Error())
		return rep
	}

	root, cleanup, err := scratchDir(opts.workDir)
	if err != nil {
		notRun("no scratch directory: " + err.Error())
		return rep
	}
	defer cleanup()

	env, err := setUp(ctx, root, pkg, opts)
	if err != nil {
		notRun("the throwaway deployment could not be set up: " + err.Error())
		return rep
	}

	rep.Stages = append(rep.Stages, auditPackage(ctx, env.dep, env.applicationID))
	conv, declared, replayed := replayExamples(ctx, env)
	rep.Stages = append(rep.Stages, conv)
	rep.Summary.ExamplesDeclared, rep.Summary.Examples = declared, replayed
	return rep
}

// scratchDir returns the directory the throwaway deployment is built in and what to do
// when the run is over: a fresh temporary directory that is removed, or the empty
// directory the operator named, which is kept.
func scratchDir(work string) (string, func(), error) {
	if work == "" {
		root, err := os.MkdirTemp("", "alixai-conformance-")
		if err != nil {
			return "", nil, err
		}
		return root, func() { _ = os.RemoveAll(root) }, nil
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		return "", nil, err
	}
	if len(entries) > 0 {
		return "", nil, fmt.Errorf("-work %s is not empty", work)
	}
	abs, err := filepath.Abs(work)
	if err != nil {
		return "", nil, err
	}
	return abs, func() {}, nil
}

// stageFailed reports whether a stage has an item that fails a run (warnings do not stop
// the later stages, even under -strict: they are reported at the end).
func stageFailed(st Stage) bool {
	for _, it := range st.Items {
		if it.Status.failing(false) {
			return true
		}
	}
	return st.Status != "" && st.Status.failing(false)
}

// env is the throwaway deployment of a run, with the package installed in it.
type env struct {
	dep           *deployment
	applicationID string
	// dir is the signed copy of the package that was installed; the conversation stage
	// loads its own view of it.
	dir string
	pkg *apppkg.Package
	// targets are the endpoint URLs under test, by endpoint id (the manifest's, or the
	// ones -endpoint named).
	targets map[string]string
	opts    options
}

// loadPackage loads the package as the audit would see it, for the declared examples and
// the capabilities. A package with a critical finding does not load: the stage before
// has already said why.
func loadPackage(ctx context.Context, dir, coreVersion string) (*apppkg.Package, error) {
	src, err := apppkg.FromPath(dir)
	if err != nil {
		return nil, err
	}
	if coreVersion == "" {
		coreVersion = apppkg.DevCoreVersion
	}
	res, err := apppkg.Load(ctx, src, apppkg.Options{CoreVersion: coreVersion})
	if err != nil {
		return nil, err
	}
	if res.Package == nil {
		return nil, errors.New("the package has critical findings")
	}
	return res.Package, nil
}

// setUp creates the throwaway deployment and installs the copy of the package in it.
func setUp(ctx context.Context, root string, pkg *apppkg.Package, opts options) (*env, error) {
	d, err := newDeployment(ctx, root)
	if err != nil {
		return nil, err
	}
	app := string(pkg.Manifest.Application.ID)
	keyID := ""
	for _, k := range pkg.Manifest.Integrity.PublisherKeyIDs {
		keyID = string(k)
		break
	}
	if keyID == "" {
		return nil, errors.New("the manifest lists no integrity.publisher_key_ids")
	}
	copyDir := root + string(os.PathSeparator) + "package"
	if err := os.MkdirAll(copyDir, 0o700); err != nil {
		return nil, err
	}
	if err := d.prepareCopy(opts.pkgDir, copyDir, app, string(pkg.Manifest.Package.Version), keyID, opts.targets); err != nil {
		return nil, err
	}
	if _, err := d.install(ctx, copyDir, keyID, app); err != nil {
		return nil, err
	}
	targets := map[string]string{}
	for id, ep := range pkg.Manifest.Endpoints {
		targets[id] = endpointURL(ep)
	}
	for id, u := range opts.targets {
		targets[id] = u
	}
	return &env{dep: d, applicationID: app, dir: copyDir, pkg: pkg, targets: targets, opts: opts}, nil
}

// emit writes the report files the options ask for.
func emit(rep *Report, opts options, stdout io.Writer) error {
	write := func(path string, fn func(io.Writer) error) error {
		if path == "-" {
			return fn(stdout)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if err := fn(f); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	if opts.jsonOut != "" {
		if err := write(opts.jsonOut, rep.writeJSON); err != nil {
			return fmt.Errorf("write the JSON report: %w", err)
		}
	}
	if opts.junitOut != "" {
		if err := write(opts.junitOut, rep.writeJUnit); err != nil {
			return fmt.Errorf("write the JUnit report: %w", err)
		}
	}
	return nil
}

// summarise prints the human summary: a line per stage, and each item that did not pass.
func summarise(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "alixai-conformance: %s (%s %s)\n", strings.ToUpper(rep.Result), rep.Package.ApplicationID, rep.Package.Version)
	for _, st := range rep.Stages {
		fmt.Fprintf(w, "  %-13s %-10s %d item(s)\n", st.Name, st.Status, len(st.Items))
		var bad []Item
		for _, it := range st.Items {
			if it.Status != StatusPass {
				bad = append(bad, it)
			}
		}
		sort.SliceStable(bad, func(i, j int) bool { return bad[i].Status.rank() > bad[j].Status.rank() })
		for _, it := range bad {
			fmt.Fprintf(w, "    %-10s %s", it.Status, it.Name)
			if it.Message != "" {
				fmt.Fprintf(w, ": %s", firstLine(it.Message, ""))
			}
			fmt.Fprintln(w)
		}
	}
	s := rep.Summary
	fmt.Fprintf(w, "  %d item(s): %d passed, %d warning(s), %d failed, %d unverified, %d not run; %d of %d example(s) replayed\n",
		s.Items, s.Passed, s.Warnings, s.Failed, s.Unverified, s.NotRun, s.Examples, s.ExamplesDeclared)
}
