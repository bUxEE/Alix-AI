package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// failBackend wraps a backend and fails chosen calls with an error that quotes the canary, as an
// operating system or a library might.
type failBackend struct {
	Backend
	get, put bool
}

func (f failBackend) Get(ctx context.Context, s Slot) (Secret, error) {
	if f.get {
		return "", errors.New("read failed for value " + tCanary)
	}
	return f.Backend.Get(ctx, s)
}

func (f failBackend) Put(ctx context.Context, s Slot, v Secret) error {
	if f.put {
		return errors.New("write failed for value " + tCanary)
	}
	return f.Backend.Put(ctx, s, v)
}

// SEC-082, SEC-083: no secret in any error text, log line, metric label or JSON serialisation. Every
// public type that can carry or reach a key is exercised with a canary; the logs, the database, the
// encrypted file and the audit trail are scanned for it afterwards.
func TestSecretNeverInLogsErrorsOrJSON(t *testing.T) {
	canaries := []string{tKey, tKey2, tCanary}
	g := newRig(t)
	var texts []string // everything a program could print, log or serialise
	add := func(label string, v any) {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			texts = append(texts, label+" "+verb+": "+fmt.Sprintf(verb, v))
		}
		if raw, err := json.Marshal(v); err == nil {
			texts = append(texts, label+" json: "+string(raw))
		}
		if tm, ok := v.(interface{ MarshalText() ([]byte, error) }); ok {
			if b, err := tm.MarshalText(); err == nil {
				texts = append(texts, label+" text: "+string(b))
			}
		}
		texts = append(texts, label+" slog: "+slogText(label, v))
	}

	// the value types, built with the canary
	secret := Secret(tCanary)
	exp := tStart.Add(time.Hour)
	add("Secret", secret)
	add("*Secret", &secret)
	add("[]Secret", []Secret{secret, Secret(tKey)})
	add("map", map[string]Secret{"k": secret})
	add("ValidationRequest", ValidationRequest{ProviderID: "fake", Key: Secret(tKey), Scopes: []string{"s"}})
	add("ValidationResult", ValidationResult{Accepted: true})
	add("AddAPIKeyRequest", AddAPIKeyRequest{ProviderID: "fake", Label: "l", Key: Secret(tKey), ExpiresAt: &exp})
	add("OAuthGrant", OAuthGrant{ProviderID: "fake", Scopes: []string{"s"}, Access: Secret(tKey), Refresh: Secret(tKey2), ExpiresAt: &exp})
	add("Rotation", Rotation{Key: Secret(tKey), Refresh: Secret(tKey2)})
	add("ExternalRefRequest", ExternalRefRequest{ProviderID: "fake", Backend: BackendEnv, Locator: "ALIXAI_SECRET_X"})
	add("Flag", Flag{Status: providers.CredentialRefStatusInvalid})
	add("Use", Use{Provider: "fake", Purpose: "inference"})
	add("Slot", Slot{Ref: "cred_A", Binding: "b"})
	add("ResolverOptions", ResolverOptions{Provider: "fake"})

	// the service, its backends and key sources, after real use with the canary
	mem := NewMemoryOSStore()
	_ = mem.Set(OSStoreService, "u", tKey)
	osb := NewOSBackend(mem)
	_ = osb.Put(bg, Slot{Ref: "cred_Z"}, Secret(tKey2))
	envb := NewEnvBackend("")
	t.Setenv("ALIXAI_SECRET_CANARY", tKey)
	fileb, _ := NewFileBackend(g.dir)
	add("MemoryOSStore", mem)
	add("OSBackend", osb)
	add("OSStoreKeySource", OSStoreKeySource{Store: mem})
	add("KeyFileSource", KeyFileSource{Path: filepath.Join(g.keyDir, "master.key")})
	add("EnvBackend", envb)
	add("FileBackend", fileb)

	ref := g.addKey(tKey)
	oauth, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Scopes: []string{"s"}, Access: Secret(tKey2), Refresh: Secret(tCanary)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.resolve(ref.RefID); err != nil {
		t.Fatal(err)
	}
	r, _ := g.svc.Resolver(g.scope, ResolverOptions{Provider: "fake"})
	got, err := r.Resolve(bg, string(ref.RefID))
	if err != nil || got.Reveal() != tKey {
		t.Fatalf("Resolve: %v", err)
	}
	add("Resolved Secret", got)
	add("Service", g.svc)
	add("EncryptedFile", g.enc)
	add("Resolver", r)
	add("SecretProvider", AsSecretProvider[Secret](r))
	add("SecretProvider[openapi.Secret]", AsSecretProvider[openapi.Secret](r))
	add("CredentialRef", ref)
	add("CredentialRef oauth", oauth)
	if list, err := g.svc.List(bg, g.scope, ""); err == nil {
		add("List", list)
	}

	// openapi.Secret from the adapter shape redacts too
	if v, err := AsSecretProvider[openapi.Secret](r).Secret(bg, string(ref.RefID)); err != nil || v.Reveal() != tKey {
		t.Fatalf("adapter-shaped provider: %v", err)
	} else {
		add("openapi.Secret", v)
	}

	// errors, from every failure path, with causes that quote the canary
	collect := func(label string, err error) {
		if err == nil {
			t.Errorf("%s: no error", label)
			return
		}
		add("error "+label, err)
		texts = append(texts, "error text "+label+": "+err.Error())
		var ce *contracts.Error
		if errors.As(err, &ce) {
			if raw, jerr := json.Marshal(ce); jerr == nil {
				texts = append(texts, "error json "+label+": "+string(raw))
			}
			texts = append(texts, fmt.Sprintf("error details %s: %v", label, ce.Details))
		}
	}
	g.val.err = errors.New("validator exploded on " + tCanary)
	_, err = g.svc.AddAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tCanary)})
	collect("validator error", err)
	g.val.err = nil

	badGet := newRig(t, func(c *Config) { c.Backends = []Backend{failBackend{Backend: c.Backends[0], get: true}} })
	br := badGet.addKey(tKey)
	_, err = badGet.resolve(br.RefID)
	collect("backend read error", err)

	badPut := newRig(t, func(c *Config) { c.Backends = []Backend{failBackend{Backend: c.Backends[0], put: true}} })
	_, err = badPut.svc.AddAPIKey(bg, badPut.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey)})
	collect("backend write error", err)

	mem2 := NewMemoryOSStore()
	g2 := newRig(t, func(c *Config) { c.Backends = []Backend{NewOSBackend(mem2)}; c.Default = BackendOSStore })
	r2 := g2.addKey(tKey)
	mem2.Fail = errors.New("keychain: " + tCanary)
	_, err = g2.resolve(r2.RefID)
	collect("os store error", err)
	_, err = g2.svc.Rotate(bg, g2.scope, r2.RefID, Rotation{Key: Secret(tCanary)})
	collect("os store rotate error", err)

	_, err = g.svc.Resolve(bg, g.scope, tKey, g.use()) // a key where a reference belongs
	collect("a key used as a reference", err)
	_, err = g.svc.Resolve(bg, scopeOf(tApp, tTenant2), string(ref.RefID), g.use())
	collect("cross tenant", err)
	_, err = g.svc.AddAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tCanary + "\x00")})
	collect("a key with a control character", err)
	_, err = g.svc.Rotate(bg, g.scope, oauth.RefID, Rotation{Key: ""})
	collect("an empty rotation", err)
	if err := json.Unmarshal([]byte(`"`+tCanary+`"`), &secret); err == nil {
		t.Error("a Secret was read from JSON")
	} else {
		collect("unmarshal", err)
	}
	// a tampered encrypted file
	badEnc, _ := NewEncryptedFile(filepath.Join(g.dir, FileName), KeyFileSource{Path: filepath.Join(t.TempDir(), "k", "master.key")})
	_, err = badEnc.Get(bg, Slot{Ref: string(ref.RefID), Binding: bindingOf(ref.Binding)})
	collect("wrong master key", err)

	for _, txt := range texts {
		for _, c := range canaries {
			if strings.Contains(txt, c) {
				t.Errorf("a canary is in %q", clip(txt))
			}
		}
	}

	// the log lines the service wrote, the database files, the encrypted file and the audit trail
	notContains(t, "the service log", g.logbuf.Bytes(), canaries...)
	notContains(t, "the database files", allBytes(t, filepath.Dir(g.db.Path())), canaries...)
	notContains(t, "the encrypted file", allBytes(t, g.dir), canaries...)
	rows, err := g.db.QueryContext(bg, "SELECT data FROM audit_events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		notContains(t, "an audit event", []byte(d), canaries...)
		n++
	}
	if n < 3 {
		t.Fatalf("only %d audit events: the scan proves nothing", n)
	}
	// the audit events name the reference, not "[redacted]" in its place
	if use := g.auditRows(EventUse.Name(), tApp, tTenant); len(use) == 0 || use[0]["ref"] != string(ref.RefID) {
		t.Fatalf("the audit trail does not name the reference: %v", use)
	}
}

func clip(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

func slogText(label string, v any) string {
	var sb strings.Builder
	l := slog.New(slog.NewJSONHandler(&sb, nil))
	l.Info("value", slog.Any("v", v), "label", label)
	t := slog.New(slog.NewTextHandler(&sb, nil))
	t.Info("value", slog.Any("v", v))
	return sb.String()
}

// The Secret type itself: no verb, no encoder, no logger shows the value.
func TestSecretRedactsEverywhere(t *testing.T) {
	s := Secret(tKey)
	if s.Reveal() != tKey || Secret("").Empty() != true || s.Empty() {
		t.Fatal("Reveal/Empty")
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
		if out := fmt.Sprintf(verb, s); strings.Contains(out, tKey) || strings.Contains(strings.ToLower(out), strings.ToLower(tKey)) {
			t.Errorf("%s printed the value", verb)
		}
	}
	if raw, _ := json.Marshal(struct{ K Secret }{s}); strings.Contains(string(raw), tKey) {
		t.Error("JSON")
	}
	if got := slogText("x", s); strings.Contains(got, tKey) || !strings.Contains(got, "[redacted]") {
		t.Errorf("slog: %s", got)
	}
}

func TestResolverResolvesOpaqueReferenceOnly(t *testing.T) {
	g := newRig(t, func(c *Config) { c.Backends = append(c.Backends, NewEnvBackend("")) })
	t.Setenv("ALIXAI_SECRET_FAKE", tKey2)
	ref := g.addKey(tKey)
	r, err := g.svc.Resolver(g.scope, ResolverOptions{Provider: "fake", Purpose: "rag.embed"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := r.Resolve(bg, string(ref.RefID)); err != nil || v.Reveal() != tKey {
		t.Fatalf("Resolve: %v", err)
	}
	// anything that is not an opaque reference of this scope is refused, whatever it looks like
	for name, in := range map[string]string{
		"a raw key":            tKey,
		"an env reference":     "env:ALIXAI_SECRET_FAKE",
		"the variable's name":  "ALIXAI_SECRET_FAKE",
		"a file reference":     "file:/run/secrets/fake",
		"a path":               "../../etc/passwd",
		"empty":                "",
		"a reference prefix":   "cred_",
		"a lower case ULID":    strings.ToLower(string(ref.RefID)),
		"a reference + suffix": string(ref.RefID) + "/refresh",
		"padding":              " " + string(ref.RefID),
	} {
		v, err := r.Resolve(bg, in)
		if err == nil || v != "" {
			t.Errorf("%s: resolved", name)
			continue
		}
		mustCode(t, err, contracts.ErrScopeViolation, ReasonNotBound)
	}
	// bound to a provider: the same reference for another provider is refused, and a resolver of another tenant
	other, _ := g.svc.Resolver(g.scope, ResolverOptions{Provider: "someone"})
	if _, err := other.Resolve(bg, string(ref.RefID)); ReasonOf(err) != ReasonProviderMismatch {
		t.Fatalf("other provider: %v", err)
	}
	b, _ := g.svc.Resolver(scopeOf(tApp, tTenant2), ResolverOptions{Provider: "fake"})
	if _, err := b.Resolve(bg, string(ref.RefID)); ReasonOf(err) != ReasonNotBound {
		t.Fatalf("other tenant: %v", err)
	}
	// no scope, no resolver: there is no default scope
	if _, err := g.svc.Resolver(contracts.Scope{}, ResolverOptions{Provider: "fake"}); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("zero scope: %v", err)
	}
	if _, err := g.svc.Resolver(g.scope, ResolverOptions{}); err == nil {
		t.Fatal("a resolver for no provider")
	}
	// the resolver holds nothing between calls and audits every one, under its purpose
	if rows := g.auditRows(EventUse.Name(), tApp, tTenant); len(rows) < 1 || rows[0]["purpose"] != "rag.embed" {
		t.Fatalf("audit = %v", rows)
	}

	// The adapters adopt it in a composition root: adapters/openapi's SecretProvider is satisfied (compile
	// time) by the adapter-shaped provider, and returns the adapter's own redacting type.
	var sp openapi.SecretProvider = AsSecretProvider[openapi.Secret](r)
	v, err := sp.Secret(bg, string(ref.RefID))
	if err != nil || v.Reveal() != tKey || fmt.Sprint(v) == tKey {
		t.Fatalf("through openapi.SecretProvider: %v", err)
	}
	if _, err := sp.Secret(bg, "env:ALIXAI_SECRET_FAKE"); err == nil {
		t.Fatal("openapi.SecretProvider resolved an env reference")
	}
	if _, err := (SecretProvider[openapi.Secret]{}).Secret(bg, string(ref.RefID)); err == nil {
		t.Fatal("an unwired provider resolved")
	}
}

// internal/credentials sits at the module-map tier "base, store, audit, pkg/contracts/gen/providers": it
// reaches no network, no shell, no SQLite driver, no adapter, and not the OS-store library (that is the
// subpackage oskeyring, which a composition root imports).
func TestImportsStayWithinCredentialsTier(t *testing.T) {
	const module = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts":               true,
		"pkg/contracts/gen/common":    true,
		"pkg/contracts/gen/providers": true,
		"internal/clock":              true,
		"internal/ids":                true,
		"internal/fsperm":             true, // owner-only files and directories (base tier: no network, no SQLite)
		"internal/fsperm/fspermtest":  true, // tests only
		"internal/store":              true,
		"internal/audit":              true,
		"internal/store/storetest":    true, // tests only
		"adapters/openapi":            true, // tests only: the Resolver satisfies its SecretProvider
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && d.Name() == "oskeyring" {
				return filepath.SkipDir // the one place that links the OS-store library
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		isTest := strings.HasSuffix(path, "_test.go")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case p == "net" || strings.HasPrefix(p, "net/") || p == "os/exec" || strings.Contains(p, "modernc.org/sqlite") || strings.Contains(p, "go-keyring"):
				t.Errorf("%s imports %s", path, p)
			case strings.HasPrefix(p, module) && !allowed[strings.TrimPrefix(p, module)]:
				t.Errorf("%s imports %s, which the credential service may not depend on", path, p)
			case !isTest && (strings.Contains(p, "storetest") || strings.Contains(p, "adapters/")):
				t.Errorf("%s: %s in production code", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var _ store.DB
