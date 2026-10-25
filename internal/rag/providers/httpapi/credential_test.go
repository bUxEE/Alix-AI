package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// randomKey is a generated test credential: no secret lives in the repository.
func randomKey(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

func withCredential(ref string) func(*ap.RAGSource) {
	return func(d *ap.RAGSource) {
		r := ap.CredentialRef(ref)
		d.CredentialRef = &r
	}
}

// leakCheck fails the test when any of the texts contains a forbidden string.
type leakCheck struct {
	t         *testing.T
	forbidden map[string]string // value -> what it is
	seen      int
}

func (l *leakCheck) check(where, text string) {
	l.t.Helper()
	l.seen++
	for v, what := range l.forbidden {
		if strings.Contains(text, v) {
			l.t.Errorf("%s contains the %s (%q):\n%s", where, what, v, text)
		}
	}
}

// TestCredentialNotLeaked (SEC-083): a source's credential is resolved from an
// opaque reference for each request, goes in one header to the source's own
// origin and nowhere else, and does not appear in a URL, a log record, an
// error, a result or any formatting of the Secret, whatever fails.
// SEC-083
func TestCredentialNotLeaked(t *testing.T) {
	secret := randomKey(t, "sk-test-")
	ref := "cred:docs-api"
	marker := randomKey(t, "FIXED-")
	lk := &leakCheck{t: t, forbidden: map[string]string{secret: "credential", ref: "credential reference", marker: "fixed query parameter (a URL fragment)"}}
	// The URL legitimately carries the fixed parameter; it must never carry the
	// credential or its reference.
	lkURL := &leakCheck{t: t, forbidden: map[string]string{secret: "credential", ref: "credential reference"}}
	fixed := map[string]string{"collection": marker}
	qs := withQueryScope(scopeAllowed, fixed)

	// -- the credential reaches the API, in the header and nowhere else.
	h := newHarness(t, "/api/docs", func(c *Config) { c.Secrets = StaticSecrets{ref: secret} })
	h.api.token = secret
	for _, k := range []string{"a.md", "b.md", "c.md"} {
		h.api.put(k, "content of "+k+"\n")
	}
	def := h.def(withCredential(ref), qs)

	hl, err := h.p.Probe(h.ctx, scope(), def)
	if err != nil || !hl.Reachable() {
		t.Fatalf("Probe with the credential = %+v, %v", hl, err)
	}
	cs := h.discover(def, nil)
	if len(cs.Added) != 3 {
		t.Fatalf("Added = %v", keysOf(cs.Added))
	}
	if _, err := h.p.DetectDeletions(h.ctx, scope(), def, cs.Next); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.Stat(h.ctx, scope(), def, "a.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.FetchVersion(h.ctx, scope(), def, "a.md"); err != nil {
		t.Fatal(err)
	}
	openAll(t, h, def, "b.md")

	reqs := h.api.requests()
	if len(reqs) < 10 {
		t.Fatalf("only %d requests were made", len(reqs))
	}
	for _, r := range reqs {
		if got := r.header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("%s %s: Authorization = %q, want the bearer credential", r.method, r.path, got)
		}
		lkURL.check("the URL of "+r.method+" "+r.path, r.raw)
		for name, vals := range r.header {
			if name == "Authorization" {
				continue
			}
			lkURL.check("header "+name, strings.Join(vals, ","))
		}
	}
	lk.check("the log", h.logs.String())
	lk.check("the change set", fmt.Sprintf("%+v %#v", cs, cs))

	// -- failures: whatever goes wrong, nothing leaks into the error text.
	t.Run("errors and logs", func(t *testing.T) {
		lk := &leakCheck{t: t, forbidden: lk.forbidden}
		h := newHarness(t, "/api/docs", func(c *Config) { c.Secrets = StaticSecrets{ref: secret} })
		for _, k := range []string{"a.md", "b.md"} {
			h.api.put(k, "content of "+k+"\n")
		}
		def := h.def(withCredential(ref), qs)
		prev := h.discover(def, nil).Next

		// A server that reflects what it was sent into its error pages.
		echo := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, fmt.Sprintf("denied %s %s %v", r.Header.Get("Authorization"), r.URL.RawQuery, r.Header), http.StatusInternalServerError)
		}
		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool { echo(w, r); return true }
		_, err := h.p.Discover(h.ctx, scope(), def, prev)
		lk.check("a list error", fmt.Sprint(err))
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("err = %v", err)
		}
		hl, err := h.p.Probe(h.ctx, scope(), def)
		if err != nil || hl.Err == nil {
			t.Fatalf("Probe = %+v, %v", hl, err)
		}
		lk.check("a health error", hl.Err.Error())
		h.api.listHook = nil

		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool { echo(w, r); return true }
		h.api.update("a.md", "changed\n", oldTime.Add(1))
		h.clk.Advance(1)
		cs := h.discover(def, prev)
		if len(cs.Failed) != 1 {
			t.Fatalf("Failed = %+v", cs.Failed)
		}
		lk.check("an item failure", cs.Failed[0].Err.Error())
		lk.check("a change set", fmt.Sprintf("%+v", cs))
		_, err = h.p.Stat(h.ctx, scope(), def, "a.md")
		lk.check("a Stat error", fmt.Sprint(err))
		_, err = h.p.FetchVersion(h.ctx, scope(), def, "a.md")
		lk.check("a FetchVersion error", fmt.Sprint(err))
		_, err = h.p.Open(h.ctx, scope(), def, "a.md")
		lk.check("an Open error", fmt.Sprint(err))
		h.api.itemHook = nil

		// The API refuses the credential.
		h.api.token = "another-" + secret[:6]
		_, err = h.p.Discover(h.ctx, scope(), def, prev)
		lk.check("a refused-credential error", fmt.Sprint(err))
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("err = %v, want permission_denied", err)
		}

		// The connection fails: net/http would put the whole URL, query
		// included, in its error.
		h.api.srv.Close()
		for name, call := range map[string]func() error{
			"Discover":  func() error { _, err := h.p.Discover(h.ctx, scope(), def, prev); return err },
			"Stat":      func() error { _, err := h.p.Stat(h.ctx, scope(), def, "b.md"); return err },
			"Open":      func() error { _, err := h.p.Open(h.ctx, scope(), def, "b.md"); return err },
			"Detect":    func() error { _, err := h.p.DetectDeletions(h.ctx, scope(), def, prev); return err },
			"FetchVers": func() error { _, err := h.p.FetchVersion(h.ctx, scope(), def, "b.md"); return err },
		} {
			err := call()
			if contracts.CodeOf(err) != contracts.ErrUnavailable {
				t.Errorf("%s: err = %v, want unavailable", name, err)
			}
			lk.check(name+" connection error", fmt.Sprint(err))
		}
		hl, _ = h.p.Probe(h.ctx, scope(), def)
		if hl.Err == nil {
			t.Fatal("a closed server was reported reachable")
		}
		lk.check("a health error for a closed server", hl.Err.Error())
		lk.check("the log", h.logs.String())
		if lk.seen < 12 {
			t.Errorf("only %d texts were checked", lk.seen)
		}
	})

	t.Run("a credential that cannot be used sends nothing", func(t *testing.T) {
		for name, cfg := range map[string]func(*Config){
			"unknown reference":  func(c *Config) { c.Secrets = StaticSecrets{"cred:other": secret} },
			"no secret provider": func(c *Config) { c.Secrets = nil },
			"malformed value":    func(c *Config) { c.Secrets = StaticSecrets{ref: "line\nbreak-" + secret} },
			"empty value":        func(c *Config) { c.Secrets = StaticSecrets{ref: ""} },
		} {
			t.Run(name, func(t *testing.T) {
				lk := &leakCheck{t: t, forbidden: lk.forbidden}
				h := newHarness(t, "/api/docs", cfg)
				h.api.put("a.md", "a\n")
				def := h.def(withCredential(ref))
				for call, fn := range map[string]func() error{
					"Discover": func() error { _, err := h.p.Discover(h.ctx, scope(), def, nil); return err },
					"Stat":     func() error { _, err := h.p.Stat(h.ctx, scope(), def, "a.md"); return err },
					"Open":     func() error { _, err := h.p.Open(h.ctx, scope(), def, "a.md"); return err },
					"Detect": func() error {
						_, err := h.p.DetectDeletions(h.ctx, scope(), def, []source.Record{{Key: "a.md", Hash: hashOf("a\n"), MIME: source.MIMEText}})
						return err
					},
				} {
					err := fn()
					if contracts.CodeOf(err) != contracts.ErrUnavailable {
						t.Errorf("%s: err = %v, want unavailable", call, err)
					}
					lk.check(call+" error", fmt.Sprint(err))
				}
				if hl, err := h.p.Probe(h.ctx, scope(), def); err != nil || hl.Reachable() || hl.Err == nil {
					t.Errorf("Probe = %+v, %v: a source whose credential cannot be used is not reachable", hl, err)
				} else {
					lk.check("health error", hl.Err.Error())
				}
				if n := len(h.api.requests()); n != 0 {
					t.Errorf("%d requests were sent although the credential could not be used", n)
				}
				lk.check("the log", h.logs.String())
			})
		}
	})

	t.Run("a redirect is not followed and carries no credential", func(t *testing.T) {
		h := newHarness(t, "/api/docs", func(c *Config) { c.Secrets = StaticSecrets{ref: secret} })
		other := newFakeAPI(t, "/loot")
		other.put("a.md", "elsewhere\n")
		if err := h.reg.Set(testApp, allowlistFor(t, h.api.port(), other.port())); err != nil {
			t.Fatal(err)
		}
		h.api.token = secret
		h.api.put("a.md", "a\n")
		h.api.put("b.md", "b\n")
		def := h.def(withCredential(ref))
		prev := h.discover(def, nil).Next
		h.clk.Advance(1)
		h.api.update("a.md", "changed\n", oldTime.Add(1))

		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			http.Redirect(w, r, other.rootURL()+"/a.md", http.StatusFound)
			return true
		}
		cs := h.discover(def, prev)
		if len(cs.Failed) != 1 || cs.Failed[0].Err.Details["reason"] != ReasonRedirect {
			t.Errorf("Failed = %+v, want one redirect failure", cs.Failed)
		}
		h.api.itemHook = nil

		h.api.listHook = func(w http.ResponseWriter, r *http.Request, page int) bool {
			http.Redirect(w, r, other.rootURL(), http.StatusTemporaryRedirect)
			return true
		}
		cs2, err := h.p.Discover(h.ctx, scope(), def, prev)
		if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("a redirected list: %v", err)
		}
		noChange(t, cs2)
		if hl, _ := h.p.Probe(h.ctx, scope(), def); hl.Reachable() {
			t.Error("a source that redirects was reported reachable")
		}
		if n := len(other.requests()); n != 0 {
			t.Errorf("the other origin received %d requests; the redirect was followed", n)
		}
	})

	t.Run("without a credential_ref nothing is sent, and a bare header scheme is honoured", func(t *testing.T) {
		h := newHarness(t, "/api/docs", func(c *Config) { c.Secrets = StaticSecrets{ref: secret} })
		h.api.put("a.md", "a\n")
		h.discover(h.def(), nil)
		for _, r := range h.api.requests() {
			if r.header.Get("Authorization") != "" {
				t.Errorf("%s %s: a credential was sent for a source that declares none", r.method, r.path)
			}
		}

		h2 := newHarness(t, "/api/docs", func(c *Config) {
			c.Secrets = StaticSecrets{ref: secret}
			c.AuthHeader, c.AuthBare = "X-Api-Key", true
		})
		h2.api.put("a.md", "a\n")
		h2.discover(h2.def(withCredential(ref)), nil)
		for _, r := range h2.api.requests() {
			if r.header.Get("X-Api-Key") != secret || r.header.Get("Authorization") != "" {
				t.Errorf("%s %s: headers = %v", r.method, r.path, r.header)
			}
		}
	})

	t.Run("a Secret prints as a placeholder in every form", func(t *testing.T) {
		s := Secret(secret)
		type holder struct {
			Name   string
			Secret Secret
			Ptr    *Secret
		}
		var buf syncBuf
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("resolved", "secret_value", s, "grp", slog.GroupValue(slog.Any("k", s)), "plain", s)
		js, _ := json.Marshal(holder{Name: "n", Secret: s, Ptr: &s})
		texts := []string{
			fmt.Sprintf("%v %+v %#v %s %q %x %X %d %T", s, s, s, s, s, s, s, s, s),
			fmt.Sprintf("%v %+v %#v", holder{Secret: s, Ptr: &s}, holder{Secret: s}, holder{Secret: s}),
			fmt.Sprint(s, []Secret{s}, map[string]Secret{"k": s}),
			string(js),
			buf.String(),
		}
		for i, tx := range texts {
			lk.check(fmt.Sprintf("formatting %d", i), tx)
		}
		if s.Reveal() != secret {
			t.Error("Reveal must return the value")
		}
	})

	t.Run("the credential header cannot be a protocol header", func(t *testing.T) {
		for _, name := range []string{"Host", "Content-Length", "Cookie", "Accept-Encoding", "bad header", "X Y"} {
			if _, err := New(Config{AuthHeader: name}); name != "" && err == nil {
				t.Errorf("header %q was accepted", name)
			}
		}
		if _, err := New(Config{AuthHeader: "X-Api-Key"}); err != nil {
			t.Errorf("X-Api-Key: %v", err)
		}
	})

}
