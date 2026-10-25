package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/apps"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func init() {
	Register(Command{
		Name:    "package validate",
		Summary: "load an application package (directory or .tar.gz), run every check and print the findings as JSON",
		Run:     runPackageValidate,
	})
	// The security-domain rule (ADR 0016 item 5) is enforced by the store; these two wrap the commands
	// that write to it so that a refusal is typed and recorded. They run after cmd_audit.go and
	// cmd_install.go registered theirs (init functions run in file-name order, as transport_mcp_provision.go
	// relies on too); a missing command is a programming error found when the binary starts.
	domainWrapCommand(domainCommandInstall, domainAfterInstall)
	domainWrapCommand(domainCommandActivate, domainAfterActivation)
}

const validateUsage = "alixai package validate [-core-version V] [-trust-key KEYID=PUBLICKEY ...] [-allow-test-keys] PACKAGE"

// validateReport is what "package validate" prints.
type validateReport struct {
	OK             bool                `json:"ok"`
	ApplicationID  string              `json:"application_id,omitempty"`
	PackageVersion string              `json:"package_version,omitempty"`
	ManifestHash   string              `json:"manifest_hash,omitempty"`
	Authenticity   apppkg.Authenticity `json:"authenticity"`
	SignedBy       string              `json:"signed_by,omitempty"`
	CoreVersion    string              `json:"core_version"`
	Files          int                 `json:"files"`
	Bytes          int64               `json:"bytes"`
	Critical       int                 `json:"critical"`
	Warnings       int                 `json:"warnings"`
	Info           int                 `json:"info"`
	Findings       []contracts.Finding `json:"findings"`
}

// trustKeyFlags collects the repeatable -trust-key KEYID=PUBLICKEY flag.
type trustKeyFlags []string

func (k *trustKeyFlags) String() string     { return strings.Join(*k, ",") }
func (k *trustKeyFlags) Set(v string) error { *k = append(*k, v); return nil }

// anchorKeys is a trust anchor made of keys named on the command line: a
// stand-in for the deployment's trust store at authoring time. A key listed
// here is trusted for whichever application the package declares, because the
// operator named it for this very package; nothing is persisted.
type anchorKeys map[string]ed25519.PublicKey

// LookupKey implements trust.KeyLookup.
func (a anchorKeys) LookupKey(_ context.Context, applicationID, keyID string) (trust.Key, error) {
	pub, ok := a[keyID]
	if !ok {
		return trust.Key{}, fmt.Errorf("%w: %q", trust.ErrUnknownKey, keyID)
	}
	return trust.Key{ApplicationID: applicationID, KeyID: keyID, PublicKey: pub, Status: trust.KeyActive}, nil
}

// parseAnchors reads -trust-key values. A published TEST-ONLY key is refused
// unless allowTest is set, as the trust store refuses it.
func parseAnchors(specs []string, allowTest bool) (anchorKeys, error) {
	out := anchorKeys{}
	for _, spec := range specs {
		id, enc, ok := strings.Cut(spec, "=")
		if !ok || id == "" || enc == "" {
			return nil, Usagef("-trust-key %q is not KEYID=PUBLICKEY (base64 of the raw 32-byte public key)", spec)
		}
		pub, err := trust.DecodePublicKey(enc)
		if err != nil {
			return nil, Usagef("-trust-key %q: %v", id, err)
		}
		if trust.IsTestOnlyKey(pub) && !allowTest {
			return nil, Usagef("-trust-key %q is a published TEST-ONLY key that anyone can sign with; pass -allow-test-keys to use it in a test", id)
		}
		if _, dup := out[id]; dup {
			return nil, Usagef("-trust-key %q is given twice", id)
		}
		out[id] = pub
	}
	return out, nil
}

// resolveCoreVersion is the core version packages are checked against: the
// -core-version flag, else the running binary's version, else the development
// version for an unstamped build.
func resolveCoreVersion(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return apppkg.CoreVersionOrDev(version.Get().Version)
}

// runPackageValidate loads the package, applies the schemas of every file and
// the cross-file checks of docs/integration/application-package.md section 14,
// prints the report as JSON on stdout and fails (exit 1) when any finding is
// critical. It writes nothing and installs nothing.
//
// Without a trust anchor the signature's digests are still checked against the
// package content, but who signed it is not established, which is reported as
// the warning signature.not_checked; -trust-key establishes it for this run.
func runPackageValidate(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("package validate")
	core := fs.String("core-version", "", "AlixAI core version to check the package's compatibility range against (default: this binary's version; a development build uses "+apppkg.DevCoreVersion+")")
	var keys trustKeyFlags
	fs.Var(&keys, "trust-key", "KEYID=PUBLICKEY: trust this publisher key (base64 of the raw 32-byte public key) for this run, to establish who signed the package; repeatable")
	allowTest := fs.Bool("allow-test-keys", false, "accept a published TEST-ONLY key as -trust-key (tests and fixtures only)")
	if err := env.ParseFlags(fs, validateUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one package directory or .tar.gz (usage: %s)", validateUsage)
	}
	anchors, err := parseAnchors(keys, *allowTest)
	if err != nil {
		return err
	}
	src, err := apppkg.FromPath(fs.Arg(0))
	if err != nil {
		return err
	}
	opts := apppkg.Options{CoreVersion: resolveCoreVersion(*core)}
	if len(anchors) > 0 {
		opts.Verifier = trust.NewVerifier(anchors, clock.Real{})
	}
	res, err := apppkg.Load(ctx, src, opts)
	if err != nil {
		return err
	}

	crit, warn, info := contracts.CountSeverity(res.Findings)
	rep := validateReport{
		OK:             res.OK(),
		ApplicationID:  res.ApplicationID,
		PackageVersion: res.PackageVersion,
		ManifestHash:   res.ManifestHash,
		Authenticity:   res.Authenticity,
		CoreVersion:    opts.CoreVersion,
		Files:          res.Files,
		Bytes:          res.Bytes,
		Critical:       crit,
		Warnings:       warn,
		Info:           info,
		Findings:       res.Findings,
	}
	if res.Signer != nil {
		rep.SignedBy = res.Signer.KeyID
	}
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return err
	}
	if crit > 0 {
		return fmt.Errorf("package has %d critical finding(s)", crit)
	}
	return nil
}

// ---- the security-domain rule (ADR 0016, item 5) ------------------------------------------------
//
// One process serves one security domain: internal/apps owns the rule and the store enforces it with
// two triggers on the package table, inside the very transaction that installs or activates a
// package, so no command, present or future, can get past it. A trigger can raise only a literal
// abort, which the installer and the audit runner (both outside this file) report as an unavailable
// store error. What follows gives that refusal its type and its audit event for the two commands that
// can hit it, without editing them: each is wrapped by name, runs unchanged, and only when it failed
// with a refusal of the rule does the wrapper read the command line again, find the package, and
// record package.domain_refused on the refused application's chain (apps.Guard.Admit). A wrapper that
// cannot record the event still returns the typed refusal, and says that the event is missing.

const (
	domainCommandInstall  = "package install"
	domainCommandActivate = "audit run"
)

// domainRefusalHandler is given the failure of a wrapped command that is a refusal of the rule: the
// command's own arguments, the error it returned and the typed form of the refusal. It returns what
// the wrapped command returns.
type domainRefusalHandler func(ctx context.Context, env *Env, args []string, cause error, refusal *contracts.Error) error

// domainWrapCommand replaces the command registered under name by one that runs it and hands a
// refusal of the security-domain rule to handle. Any other result, an error that is not a refusal
// included, passes through untouched.
func domainWrapCommand(name string, handle domainRefusalHandler) {
	Default.mu.Lock()
	defer Default.mu.Unlock()
	c, ok := Default.cmds[name]
	if !ok {
		panic(fmt.Sprintf("cli: the security-domain rule wraps %q, which is not registered (is its file sorted after cmd_package.go?)", name))
	}
	inner := c.Run
	c.Run = func(ctx context.Context, env *Env, args []string) error {
		err := inner(ctx, env, args)
		if err == nil {
			return nil
		}
		refusal, ok := apps.AsRefusal(err)
		if !ok {
			return err
		}
		return handle(ctx, env, args, err, refusal)
	}
	Default.cmds[name] = c
}

// domainAfterInstall records the refusal of `package install`: the package is read again from the
// command line, since the installer refused it before anything about it was stored.
func domainAfterInstall(ctx context.Context, env *Env, args []string, _ error, refusal *contracts.Error) error {
	fs := env.FlagSet(domainCommandInstall)
	path := env.ConfigFlag(fs)
	core := fs.String("core-version", "", "")
	operator := fs.String("operator", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return domainUnrecorded(refusal, errors.New("the command line could not be read again to find the package"))
	}
	src, err := apppkg.FromPath(fs.Arg(0))
	if err != nil {
		return domainUnrecorded(refusal, err)
	}
	res, err := apppkg.Load(ctx, src, apppkg.Options{CoreVersion: resolveCoreVersion(*core)})
	if err != nil || res.Package == nil {
		return domainUnrecorded(refusal, errors.New("the package could not be read again"))
	}
	cand := apps.Candidate{
		ApplicationID: res.ApplicationID, SecurityDomain: res.Package.SecurityDomain(),
		ManifestHash: res.ManifestHash, Phase: apps.PhaseInstall,
	}
	return domainRecord(ctx, env, *path, *operator, refusal, func(*stack) (apps.Candidate, error) { return cand, nil })
}

// domainAfterActivation records the refusal of `audit run`: the package that stayed in quarantine is
// the one the command audited, found as the command found it.
func domainAfterActivation(ctx context.Context, env *Env, args []string, _ error, refusal *contracts.Error) error {
	fs := env.FlagSet(domainCommandActivate)
	path := env.ConfigFlag(fs)
	pkgHash := fs.String("package", "", "")
	fs.Bool("no-activate", false, "")
	fs.String("signing-key", "", "")
	operator := fs.String("operator", "", "")
	core := fs.String("core-version", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return domainUnrecorded(refusal, errors.New("the command line could not be read again to find the application"))
	}
	app := fs.Arg(0)
	return domainRecord(ctx, env, *path, *operator, refusal, func(s *stack) (apps.Candidate, error) {
		inst, err := s.installer(*core)
		if err != nil {
			return apps.Candidate{}, err
		}
		rec, err := auditPackageToAudit(ctx, inst, app, *pkgHash)
		if err != nil {
			return apps.Candidate{}, err
		}
		return apps.Candidate{
			ApplicationID: rec.ApplicationID, SecurityDomain: rec.SecurityDomain,
			ManifestHash: rec.ManifestHash, Phase: apps.PhaseActivation,
		}, nil
	})
}

// domainRecord opens the deployment's store, applies the rule to the candidate the way the triggers
// did, and records the refusal. It returns the refusal with the details of both applications; when
// the store no longer shows a conflict (the other application went out of service in between) the
// trigger's refusal stands, since that is what happened to the command.
func domainRecord(ctx context.Context, env *Env, configPath, operator string, refusal *contracts.Error, candidate func(*stack) (apps.Candidate, error)) error {
	s, err := openStack(ctx, env, configPath, false)
	if err != nil {
		return domainUnrecorded(refusal, err)
	}
	defer s.close()
	cand, err := candidate(s)
	if err != nil {
		return domainUnrecorded(refusal, err)
	}
	guard, err := apps.NewGuard(s.db, s.log)
	if err != nil {
		return domainUnrecorded(refusal, err)
	}
	if err := guard.Admit(ctx, cand, operator); err != nil {
		return err
	}
	return refusal
}

// domainUnrecorded is the refusal together with the reason its audit event is missing.
func domainUnrecorded(refusal *contracts.Error, why error) error {
	return errors.Join(refusal, fmt.Errorf("the refusal could not be recorded in the audit log: %w", why))
}
