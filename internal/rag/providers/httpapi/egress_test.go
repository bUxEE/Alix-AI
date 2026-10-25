package httpapi

import (
	"net/http"
	"testing"

	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// allCalls runs every operation of the provider against def and returns what
// each returned, so a test can say that all of them are refused.
func allCalls(h *harness, def source.Definition) map[string]error {
	prev := []source.Record{{Key: "a.md", Hash: hashOf("a\n"), MIME: source.MIMEText, Size: 2, SeenAt: clockStart}}
	out := map[string]error{}
	_, out["Discover"] = h.p.Discover(h.ctx, scope(), def, prev)
	_, out["DetectDeletions"] = h.p.DetectDeletions(h.ctx, scope(), def, prev)
	_, out["Stat"] = h.p.Stat(h.ctx, scope(), def, "a.md")
	_, out["FetchVersion"] = h.p.FetchVersion(h.ctx, scope(), def, "a.md")
	_, out["Open"] = h.p.Open(h.ctx, scope(), def, "a.md")
	// Probe reports a refusal as an unreachable source.
	if hl, err := h.p.Probe(h.ctx, scope(), def); err != nil {
		out["Probe"] = err
	} else if hl.Reachable() {
		out["Probe"] = nil
	} else {
		out["Probe"] = hl.Err
	}
	return out
}

// TestEgressDenied (SEC-055): the provider reaches only what the application's
// egress allowlist names, by exact host and port and scheme, and a source that
// declares no egress entry reaches nothing. A refusal is permission_denied
// (egress.IsDenied), it happens before any request leaves, and it is never
// reported as success, as an empty source or as a deletion.
// SEC-055
func TestEgressDenied(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t, "/api/docs")
		h.api.put("a.md", "a\n")
		return h
	}

	t.Run("an allowlisted loopback endpoint works (control)", func(t *testing.T) {
		h := setup(t)
		for name, err := range allCalls(h, h.def()) {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if len(h.api.requests()) == 0 {
			t.Fatal("the control made no request, so the denial cases below prove nothing")
		}
	})

	cases := []struct {
		name    string
		reason  egress.Reason
		arrange func(t *testing.T, h *harness)
	}{
		{"no allowlist for the application (default deny)", egress.ReasonNoAllowlist, func(t *testing.T, h *harness) { h.reg.Revoke(testApp) }},
		{"an allowlist of another application only", egress.ReasonNoAllowlist, func(t *testing.T, h *harness) {
			h.reg.Revoke(testApp)
			if err := h.reg.Set("another-app", allowlistFor(t, h.api.port())); err != nil {
				t.Fatal(err)
			}
		}},
		{"an empty allowlist", egress.ReasonNoAllowlist, func(t *testing.T, h *harness) {
			al, err := egress.NewAllowlist()
			if err != nil {
				t.Fatal(err)
			}
			if err := h.reg.Set(testApp, al); err != nil {
				t.Fatal(err)
			}
		}},
		{"the right host on the wrong port", egress.ReasonNotAllowlisted, func(t *testing.T, h *harness) {
			if err := h.reg.Set(testApp, allowlistFor(t, h.api.port()+1)); err != nil {
				t.Fatal(err)
			}
		}},
		{"the right port on another host name", egress.ReasonNotAllowlisted, func(t *testing.T, h *harness) {
			al, err := egress.NewAllowlist(egress.Destination{Host: "localhost", Ports: []int{h.api.port()}, Scheme: egress.SchemeHTTP})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.reg.Set(testApp, al); err != nil {
				t.Fatal(err)
			}
		}},
		{"the right host and port under another scheme", egress.ReasonScheme, func(t *testing.T, h *harness) {
			al, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: []int{h.api.port()}, Scheme: egress.SchemeHTTPS})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.reg.Set(testApp, al); err != nil {
				t.Fatal(err)
			}
		}},
		{"no egress source configured at all", egress.ReasonNoAllowlist, func(t *testing.T, h *harness) {
			p, err := New(Config{Clock: h.clk})
			if err != nil {
				t.Fatal(err)
			}
			h.p = p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			tc.arrange(t, h)
			for name, err := range allCalls(h, h.def()) {
				if !egress.IsDenied(err) || egress.ReasonOf(err) != tc.reason {
					t.Errorf("%s: err = %v (reason %q), want an egress denial %q", name, err, egress.ReasonOf(err), tc.reason)
				}
				if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
					t.Errorf("%s: code = %s, want permission_denied", name, contracts.CodeOf(err))
				}
			}
			if n := len(h.api.requests()); n != 0 {
				t.Errorf("the API received %d requests although the destination is not allowed", n)
			}
		})
	}

	t.Run("a source that declares no egress entry makes no connection", func(t *testing.T) {
		h := setup(t) // the allowlist itself is fine
		def := h.def(func(d *ap.RAGSource) { d.Egress.Entries = []ap.Slug{} })
		for name, err := range allCalls(h, def) {
			if !egress.IsDenied(err) || contracts.CodeOf(err) != contracts.ErrPermissionDenied {
				t.Errorf("%s: err = %v, want permission_denied", name, err)
			}
		}
		// Probe must not be answered "reachable" and a scan must not be answered
		// "empty": both are errors, never a result.
		cs, err := h.p.Discover(h.ctx, scope(), def, nil)
		if err == nil || len(cs.Next) != 0 {
			t.Errorf("Discover = %+v, %v", cs, err)
		}
		if n := len(h.api.requests()); n != 0 {
			t.Errorf("the API received %d requests from a source with no egress entry", n)
		}
	})

	t.Run("an address that is never reachable is refused whatever the allowlist says", func(t *testing.T) {
		for _, host := range []string{"169.254.169.254", "169.254.170.2", "100.100.100.200", "fd00:ec2::254"} {
			h := newHarness(t, "/api/docs")
			addr := host
			if addr[0] == 'f' {
				addr = "[" + host + "]"
			}
			root := ap.HTTPURL("https://" + addr + "/latest/meta-data")
			al, err := egress.NewAllowlist(egress.Destination{Host: host, Ports: []int{443}, Scheme: egress.SchemeHTTPS})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.reg.Set(testApp, al); err != nil {
				t.Fatal(err)
			}
			def := h.def(func(d *ap.RAGSource) { d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", URL: &root}} })
			for name, err := range allCalls(h, def) {
				if egress.ReasonOf(err) != egress.ReasonBlockedAddress {
					t.Errorf("%s %s: err = %v, want blocked_address", host, name, err)
				}
			}
		}
	})

	t.Run("a refusal is audited through the deny hook", func(t *testing.T) {
		var denials []egress.Denial
		h := newHarness(t, "/api/docs")
		reg := egress.NewRegistry(egress.WithDenyHook(func(d egress.Denial) { denials = append(denials, d) }))
		h.p, _ = New(Config{Clock: h.clk, Egress: reg})
		if _, err := h.p.Discover(h.ctx, scope(), h.def(), nil); !egress.IsDenied(err) {
			t.Fatalf("err = %v", err)
		}
		if len(denials) == 0 || denials[0].Application != testApp || denials[0].Reason != egress.ReasonNoAllowlist {
			t.Errorf("denials = %+v", denials)
		}
	})

	t.Run("a redirect to a destination that is not allowed is not followed", func(t *testing.T) {
		h := setup(t)
		other := newFakeAPI(t, "/loot") // reachable on the network, not on the allowlist
		other.put("a.md", "elsewhere\n")
		h.api.itemHook = func(w http.ResponseWriter, r *http.Request, key string) bool {
			http.Redirect(w, r, other.rootURL()+"/a.md", http.StatusFound)
			return true
		}
		if _, err := h.p.FetchVersion(h.ctx, scope(), h.def(), "a.md"); err == nil {
			t.Error("a redirect was accepted")
		}
		if n := len(other.requests()); n != 0 {
			t.Errorf("the redirect target received %d requests", n)
		}
	})
}
