package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func init() {
	Register(Command{
		Name:    "identity add-issuer-key",
		Summary: "register the public key that verifies a host application's context tokens, checked against its package manifest",
		Run:     runIdentityAddIssuerKey,
	})
	Register(Command{
		Name:    "identity list",
		Summary: "list the issuer keys of one application or of all of them",
		Run:     runIdentityList,
	})
	Register(Command{
		Name:    "identity revoke-issuer-key",
		Summary: "revoke an issuer key: tokens it signed are refused from the next request",
		Run:     runIdentityRevokeIssuerKey,
	})
}

// issuerKeyAudit is the data of the two issuer key audit events. It holds the
// identity of the key and its fingerprint, never key bytes.
type issuerKeyAudit struct {
	KeyID         string `json:"key_id"`
	ApplicationID string `json:"application_id"`
	Issuer        string `json:"issuer"`
	Fingerprint   string `json:"fingerprint"`
	DevKey        bool   `json:"dev_key"`
	Label         string `json:"label,omitempty"`
	NotAfter      string `json:"not_after,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// The audit events of the issuer key lifecycle (SEC-037): on the application
// chain of the application the key speaks for, written in the same transaction
// as the change.
var (
	eventIssuerKeyAdded = audit.MustRegister[issuerKeyAudit](audit.TypeSpec{
		Name:        "identity.issuer_key_added",
		Description: "an issuer key that verifies an application's context tokens was registered",
		Levels:      audit.LevelApplication,
		Required:    []string{"key_id", "fingerprint"},
	})
	eventIssuerKeyRevoked = audit.MustRegister[issuerKeyAudit](audit.TypeSpec{
		Name:        "identity.issuer_key_revoked",
		Description: "an issuer key was revoked: the tokens it signed are refused",
		Levels:      audit.LevelApplication,
		Required:    []string{"key_id", "fingerprint"},
	})
)

// issuerKeyStore builds the store operator commands use, with the audit record
// of every change written inside the change's own transaction: a key is neither
// added nor revoked unless its record was.
func issuerKeyStore(db *store.DB, clk clock.Clock) (*identity.IssuerKeyStore, error) {
	alg, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		return nil, err
	}
	return identity.NewIssuerKeyStore(db, clk, identity.IssuerKeyStoreOptions{
		Record: func(ctx context.Context, tx *store.Tx, ev identity.IssuerKeyEvent) error {
			loc, err := audit.ForApplication(ev.Key.ApplicationID, "")
			if err != nil {
				return fmt.Errorf("the change cannot be recorded in the audit log: %w", err)
			}
			data := issuerKeyAudit{
				KeyID: ev.Key.KeyID, ApplicationID: ev.Key.ApplicationID, Issuer: ev.Key.Issuer, Fingerprint: ev.Key.Fingerprint,
				DevKey: ev.Key.DevKey, Label: ev.Key.Label, Reason: ev.Reason,
			}
			if !ev.Key.NotAfter.IsZero() {
				data.NotAfter = ev.Key.NotAfter.UTC().Format(time.RFC3339)
			}
			typ := eventIssuerKeyAdded
			if ev.Action == identity.IssuerKeyActionRevoked {
				typ = eventIssuerKeyRevoked
			}
			_, err = typ.AppendTx(ctx, alg, tx, loc, data)
			return err
		},
	}), nil
}

// readContextExchange loads the application package at path (a directory or a
// .tar.gz) and returns the application it is for and the context_exchange block
// of its manifest. The package must load with no critical finding. Its
// signature is checked against the files, not against a trust anchor: the
// package is only the operator's statement of which key ids and issuer the
// application declares; who may publish it is decided where it is installed.
func readContextExchange(ctx context.Context, path string) (application string, ce identity.ContextExchange, err error) {
	src, err := apppkg.FromPath(path)
	if err != nil {
		return "", ce, err
	}
	res, err := apppkg.Load(ctx, src, apppkg.Options{CoreVersion: resolveCoreVersion("")})
	if err != nil {
		return "", ce, err
	}
	if !res.OK() {
		var first []string
		for _, f := range res.Findings {
			if f.Severity == contracts.SeverityCritical && len(first) < 3 {
				first = append(first, f.CheckID+": "+f.Message)
			}
		}
		return "", ce, fmt.Errorf("the package has critical findings (run `alixai package validate`): %s", strings.Join(first, "; "))
	}
	m := res.Package.Manifest.ContextExchange
	if m.Mode != ap.ManifestContextExchangeModeContextToken {
		return "", ce, fmt.Errorf("the package declares context_exchange.mode %q: only %q uses issuer keys", m.Mode, ap.ManifestContextExchangeModeContextToken)
	}
	ce = identity.ContextExchange{Issuer: m.Issuer, Audience: m.Audience}
	for _, id := range m.KeyIDs {
		ce.KeyIDs = append(ce.KeyIDs, string(id))
	}
	return string(res.Package.Manifest.Application.ID), ce, nil
}

// readIssuerPublicKey reads the public key operators supply as base64, in the
// flag or in a file. It accepts nothing else, and never echoes what it read: a
// private key (a PEM block, or a 64-byte Ed25519 key) is refused outright, so
// that one pasted by mistake does not land in a shell history, a log or a
// database.
func readIssuerPublicKey(inline, file string) (ed25519.PublicKey, error) {
	if (inline == "") == (file == "") {
		return nil, Usagef("give exactly one of -public-key and -public-key-file")
	}
	raw := inline
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, Usagef("-public-key-file cannot be read")
		}
		if len(b) > 4096 {
			return nil, Usagef("-public-key-file is too large to be a public key")
		}
		raw = string(b)
	}
	if strings.Contains(raw, "-----BEGIN") {
		return nil, Usagef("the key is PEM text: only the raw 32-byte PUBLIC key in base64 is accepted here, and a private key is never accepted")
	}
	pub, err := trust.DecodePublicKey(raw)
	if err != nil {
		return nil, Usagef("the public key must be the raw 32-byte Ed25519 PUBLIC key in standard base64 (a private key is never accepted)")
	}
	return pub, nil
}

const addIssuerKeyUsage = "alixai identity add-issuer-key [-config file.yaml] -package DIR|FILE.tar.gz -key-id ID (-public-key BASE64 | -public-key-file FILE) [-app ID] [-issuer ISS] [-label TEXT] [-not-after TIME] [-allow-test-key]"

// runIdentityAddIssuerKey registers an issuer key: the Ed25519 PUBLIC key a
// host application's context tokens are verified with. The key id and issuer
// must be the ones the package's manifest declares in context_exchange, so what
// the operator provisions cannot differ from what the package says; the
// package is named here, not looked up in the installed packages, because
// registering a package needs a token and a token needs a key.
func runIdentityAddIssuerKey(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("identity add-issuer-key")
	cfgPath := env.ConfigFlag(fs)
	pkg := fs.String("package", "", "the application package (directory or .tar.gz) whose context_exchange block declares the key (required)")
	keyID := fs.String("key-id", "", "the key id the host puts in the kid of its tokens; it must be listed in the package's context_exchange.key_ids (required)")
	pubInline := fs.String("public-key", "", "the raw 32-byte Ed25519 public key in standard base64")
	pubFile := fs.String("public-key-file", "", "a file holding the public key in standard base64 (one line, as `package keygen` writes ID.pub)")
	app := fs.String("app", "", "the application ID; default and must equal the package's")
	issuer := fs.String("issuer", "", "the iss value of the tokens; default and must equal the package's context_exchange.issuer")
	label := fs.String("label", "", "free text for the operator")
	notAfter := fs.String("not-after", "", "UTC time (RFC 3339, ending in Z) from which the key verifies nothing; bounds a rotation overlap")
	allowTest := fs.Bool("allow-test-key", false, "developer flag: accept a published TEST-ONLY key. Anyone can sign tokens with it; never use it in production")
	if err := env.ParseFlags(fs, addIssuerKeyUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if *pkg == "" || *keyID == "" {
		return Usagef("-package and -key-id are required (usage: %s)", addIssuerKeyUsage)
	}
	pub, err := readIssuerPublicKey(*pubInline, *pubFile)
	if err != nil {
		return err
	}
	var until time.Time
	if *notAfter != "" {
		t, perr := time.Parse(time.RFC3339, *notAfter)
		if perr != nil || !strings.HasSuffix(*notAfter, "Z") {
			return Usagef("-not-after %q is not an RFC 3339 UTC time ending in Z", *notAfter)
		}
		until = t.UTC()
	}

	declaredApp, declared, err := readContextExchange(ctx, *pkg)
	if err != nil {
		return fmt.Errorf("identity add-issuer-key: %w", err)
	}
	if *app != "" && *app != declaredApp {
		return Usagef("-app %q is not the application of the package (%q)", *app, declaredApp)
	}
	iss := *issuer
	if iss == "" {
		iss = declared.Issuer
	}

	db, _, err := operatorStore(ctx, env, *cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	keys, err := issuerKeyStore(db, clock.Real{})
	if err != nil {
		return err
	}
	stored, err := keys.Add(ctx, identity.AddIssuerKey{
		Key:      identity.IssuerKey{KeyID: *keyID, ApplicationID: declaredApp, Issuer: iss, Public: pub, NotAfter: until},
		Label:    *label,
		Declared: declared, AllowTestOnly: *allowTest,
	})
	if err != nil {
		return fmt.Errorf("identity add-issuer-key: %w", err)
	}

	idc := identity.CurrentConfig()
	required := idc.Audience
	if required == "" {
		required = idc.DeploymentID
	}
	if note := declared.AudienceMismatch(required); note != "" {
		fmt.Fprintf(env.Stderr, "alixai: warning: %s\n", note)
	}
	if idc.DeploymentID == "" {
		fmt.Fprintln(env.Stderr, "alixai: warning: identity.deployment_id is not configured: the /v1 API refuses every request until it is")
	}
	if stored.DevKey {
		fmt.Fprintf(env.Stderr, "alixai: warning: %s is a published TEST-ONLY key: anyone can sign tokens for %s with it. Never use it in production\n", stored.KeyID, stored.ApplicationID)
	}
	_, err = fmt.Fprintf(env.Stdout, "issuer key %s registered for application %s\nissuer:      %s\nfingerprint: %s\n",
		stored.KeyID, stored.ApplicationID, stored.Issuer, stored.Fingerprint)
	return err
}

const identityListUsage = "alixai identity list [-config file.yaml] [-app ID] [-json]"

// runIdentityList prints the issuer keys with their state. It prints public
// information only (ids, issuer, fingerprint, state); a key's bytes are not
// shown.
func runIdentityList(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("identity list")
	cfgPath := env.ConfigFlag(fs)
	app := fs.String("app", "", "list only this application's keys")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if err := env.ParseFlags(fs, identityListUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	db, _, err := operatorStore(ctx, env, *cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	keys, err := issuerKeyStore(db, clock.Real{})
	if err != nil {
		return err
	}
	list, err := keys.List(ctx, *app)
	if err != nil {
		return fmt.Errorf("identity list: %w", err)
	}
	type row struct {
		ApplicationID string `json:"application_id"`
		KeyID         string `json:"key_id"`
		Status        string `json:"status"`
		Issuer        string `json:"issuer"`
		Fingerprint   string `json:"fingerprint"`
		DevKey        bool   `json:"dev_key"`
		RegisteredAt  string `json:"registered_at"`
		NotAfter      string `json:"not_after,omitempty"`
		RevokedAt     string `json:"revoked_at,omitempty"`
		Label         string `json:"label,omitempty"`
	}
	rows := make([]row, 0, len(list))
	for _, k := range list {
		r := row{ApplicationID: k.ApplicationID, KeyID: k.KeyID, Status: string(k.Status), Issuer: k.Issuer, Fingerprint: k.Fingerprint,
			DevKey: k.DevKey, RegisteredAt: k.RegisteredAt.UTC().Format(time.RFC3339), Label: k.Label}
		if !k.NotAfter.IsZero() {
			r.NotAfter = k.NotAfter.UTC().Format(time.RFC3339)
		}
		if !k.RevokedAt.IsZero() {
			r.RevokedAt = k.RevokedAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, r)
	}
	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, err = fmt.Fprintln(env.Stdout, "no issuer keys: the /v1 API accepts no token until `alixai identity add-issuer-key` adds one")
		return err
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APPLICATION\tKEY ID\tSTATUS\tISSUER\tFINGERPRINT\tNOT AFTER\tNOTE")
	for _, r := range rows {
		note := r.Label
		if r.DevKey {
			note = strings.TrimSpace("TEST-ONLY " + note)
		}
		fp := r.Fingerprint
		if len(fp) > 16 {
			fp = fp[:16]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ApplicationID, r.KeyID, r.Status, r.Issuer, fp, r.NotAfter, note)
	}
	return tw.Flush()
}

const revokeUsage = "alixai identity revoke-issuer-key [-config file.yaml] -app ID -key-id ID [-reason TEXT]"

// runIdentityRevokeIssuerKey revokes an issuer key, permanently. A running
// `alixai serve` reads the table on every token, so the next request signed by
// the key is refused without a restart.
func runIdentityRevokeIssuerKey(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("identity revoke-issuer-key")
	cfgPath := env.ConfigFlag(fs)
	app := fs.String("app", "", "the application the key speaks for (required: a key is revoked as that application's)")
	keyID := fs.String("key-id", "", "the key id to revoke (required)")
	reason := fs.String("reason", "", "why: recorded in the audit log")
	if err := env.ParseFlags(fs, revokeUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if *app == "" || *keyID == "" {
		return Usagef("-app and -key-id are required (usage: %s)", revokeUsage)
	}
	db, _, err := operatorStore(ctx, env, *cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	keys, err := issuerKeyStore(db, clock.Real{})
	if err != nil {
		return err
	}
	key, changed, err := keys.Revoke(ctx, *app, *keyID, *reason)
	if err != nil {
		if errors.Is(err, identity.ErrKeyNotFound) {
			return fmt.Errorf("identity revoke-issuer-key: application %s has no issuer key %q (see `alixai identity list -app %s`)", *app, *keyID, *app)
		}
		return fmt.Errorf("identity revoke-issuer-key: %w", err)
	}
	if !changed {
		_, err = fmt.Fprintf(env.Stdout, "issuer key %s of application %s was already revoked\n", key.KeyID, key.ApplicationID)
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "issuer key %s of application %s revoked; tokens it signed are refused from the next request\n", key.KeyID, key.ApplicationID)
	return err
}
