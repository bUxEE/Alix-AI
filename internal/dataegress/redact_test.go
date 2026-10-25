package dataegress

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// fakeJWT and fakePEM are built at run time so no secret-shaped literal sits in the
// source; none is a real credential.
func fakeJWT() string {
	return "eyJhbGciOiJub25lIn0" + "." + "eyJzdWIiOiJzZW50aW5lbCJ9" + "." + "c2VudGluZWw"
}

func fakePEM() string {
	return "-----BEGIN " + "PRIVATE KEY-----\nMIIFAKESENTINELKEYMATERIAL\n-----END " + "PRIVATE KEY-----"
}

func TestSecretRedaction(t *testing.T) {
	secret := tSecret()
	cases := []struct {
		name string
		in   string
		gone []string // must not survive
		keep []string // must survive
	}{
		{"bearer token", "call it with Bearer " + secret + " now", []string{secret}, []string{"call it with", "now"}},
		{"authorization header", "Authorization: Basic dXNlcjpwdw==\nHost: h", []string{"dXNlcjpwdw"}, []string{"Host: h"}},
		{"cookie header", "Cookie: session=" + secret, []string{secret}, nil},
		{"key=value", "retry with api_key=" + secret + "&x=1", []string{secret}, []string{"&x=1"}},
		{"password pair", `password = "hunter2 hunter3"`, []string{"hunter2", "hunter3"}, nil},
		{"json member", `{"client_secret":"` + secret + `","n":1}`, []string{secret}, []string{`"n":1`}},
		{"jwt", "token is " + fakeJWT(), []string{fakeJWT(), "c2VudGluZWw"}, nil},
		{"pem private key", "key:\n" + fakePEM() + "\nend", []string{"MIIFAKESENTINEL"}, []string{"end"}},
		{"url userinfo", "see https://svc:" + "pw99" + "@host.example/x", []string{"pw99"}, []string{"host.example"}},
		{"secret-shaped key", "use " + secret + " for it", []string{secret}, []string{"for it"}},
		{"cloud key", "id " + "AKIA" + "IOSFODNN7EXAMPLE" + " end", []string{"AKIAIOSFODNN7EXAMPLE"}, []string{"end"}},
		{"vcs token", "tok ghp_" + strings.Repeat("a1B2", 6) + " end", []string{"ghp_a1B2"}, []string{"end"}},
		{"credential reference", "ref cred_01J0000000000000000000000C end", []string{"cred_01J"}, []string{"end"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, found := scrub(tc.in, true)
			assertAbsent(t, "scrubbed text", out, tc.gone...)
			assertPresent(t, "scrubbed text", out, tc.keep...)
			if !hasCategory(found, providers.BlockedCategoryCredential) {
				t.Errorf("found = %v, want credential", found)
			}
			if again, more := scrub(out, true); again != out || len(more) != 0 {
				t.Errorf("not idempotent: %q -> %q (%v)", out, again, more)
			}
		})
	}
	// Plain text is left alone, so the model still gets the user's words.
	for _, plain := range []string{
		"Please review the token usage report for last week and the api key rotation schedule.",
		"What is 3/4 of 120? Answer and/or explain; see 2026/10/03 and https://example.com/docs/page.",
		"Order ORD-1009 for Ada Lovelace, quantity 3, status open.",
		"",
	} {
		if out, found := scrub(plain, true); out != plain || len(found) != 0 {
			t.Errorf("scrub(%q) = %q (%v), want it unchanged", plain, out, found)
		}
	}

	// End to end: the same secret, in every text the guard sees, never reaches the wire.
	f := newFixture(t)
	f.enableAll()
	src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	c := f.candidate(excerptPart(src, "excerpt Bearer "+secret, "label "+secret))
	c.Messages[0].Parts[0].Text = "user says " + secret
	c.Instructions = "instructions api_key=" + secret
	res := f.mustAllow(c)
	assertAbsent(t, "envelope", whole(t, res.Envelope), secret)
	meta := Part{Kind: PartText, Label: "meta " + secret, Category: string(providers.SendCategoryRAGMetadata), Class: string(providers.DataClassificationInternal), SourceID: string(src)}
	assertAbsent(t, "envelope", whole(t, f.mustAllow(f.candidate(meta)).Envelope), secret)
}

func TestEnvelopeStripsPathsCredentialsAndMCPURLs(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	secret := tSecret()

	leaks := map[providers.BlockedCategory][]string{
		providers.BlockedCategoryFilesystemPath: {
			`C:\Users\ada\` + tPathMarker + `\notes.txt`,
			`D:/data/` + tPathMarker + `/export.csv`,
			`\\fileserver\share\` + tPathMarker + `\a.xlsx`,
			`/home/ada/` + tPathMarker + `/.env`,
			`/etc/` + tPathMarker + `/hosts`,
			`~/` + tPathMarker + `/keys`,
			`../../` + tPathMarker + `/up`,
			`file:///var/` + tPathMarker + `/db.sqlite`,
			`/srv/app/` + tPathMarker + `/store/blob.bin`,
		},
		providers.BlockedCategoryDatabaseHandle: {
			`postgres://svc:pw1@db.internal:5432/` + tPathMarker,
			`mongodb+srv://u:pw2@cluster0.internal/` + tPathMarker,
			`redis://:pw3@cache.internal:6379/0`,
			`sqlite:///var/data/` + tPathMarker + `.db`,
			`jdbc:postgresql://h.internal/` + tPathMarker,
			`Server=db.internal;Database=` + tPathMarker + `;User Id=svc;Password=pw4;`,
		},
		providers.BlockedCategoryMCPURL: {
			`mcp://tools.internal/` + tPathMarker,
			`https://mcp.vendor.example/` + tPathMarker,
			`https://app.internal/v1/mcp/` + tPathMarker + `/sse`,
			`http://127.0.0.1:7471/mcp?x=` + tPathMarker,
		},
	}
	for category, samples := range leaks {
		for _, sample := range samples {
			t.Run(string(category)+"/"+sample, func(t *testing.T) {
				c := f.candidate()
				c.Messages[0].Parts[0].Text = "please look at " + sample + " and report"
				res := f.mustAllow(c)
				out := whole(t, res.Envelope)
				assertAbsent(t, "envelope", out, tPathMarker, "pw1", "pw2", "pw3", "pw4", "db.internal", "cache.internal", "tools.internal")
				assertPresent(t, "wire", wire(t, res.Envelope), "please look at", "and report")
				if !hasCategory(res.Decision.Categories.Stripped, category) {
					t.Errorf("stripped = %v, want %s", res.Decision.Categories.Stripped, category)
				}
			})
		}
	}

	// Everything at once, in one prompt, plus a credential: every category is recorded and none survives.
	all := "paths C:\\Users\\ada\\" + tPathMarker + " and /home/ada/" + tPathMarker + "; db postgres://svc:pw5@db.internal/x; mcp https://mcp.vendor.example/s; key " + secret
	c := f.candidate()
	c.Messages[0].Parts[0].Text = all
	res := f.mustAllow(c)
	assertAbsent(t, "envelope", whole(t, res.Envelope), tPathMarker, "pw5", "db.internal", "vendor.example", secret)
	for _, want := range []providers.BlockedCategory{providers.BlockedCategoryFilesystemPath, providers.BlockedCategoryDatabaseHandle, providers.BlockedCategoryMCPURL, providers.BlockedCategoryCredential} {
		if !hasCategory(res.Decision.Categories.Stripped, want) {
			t.Errorf("stripped = %v, want %s", res.Decision.Categories.Stripped, want)
		}
	}
	if o := res.Parts[0]; !o.Kept || o.Why != "scrubbed" {
		t.Errorf("a scrubbed part is kept and reported as scrubbed: %+v", o)
	}

	// Ordinary web links and file names stay: only what the provider must not have is removed.
	c = f.candidate()
	c.Messages[0].Parts[0].Text = "docs at https://docs.example.com/guide/start and the file report.pdf, ratio 3/4"
	res = f.mustAllow(c)
	assertPresent(t, "wire", wire(t, res.Envelope), "https://docs.example.com/guide/start", "report.pdf", "3/4")
	if len(res.Decision.Categories.Stripped) != 0 {
		t.Errorf("nothing to strip: %v", res.Decision.Categories.Stripped)
	}

	// Tool schemas keep their JSON pointers and patterns (no path rules) but lose an MCP URL or DSN placed in them.
	c = f.candidate()
	c.Tools = []providers.RequestEnvelopeToolSpec{{
		Name: "orders_create", Description: "Create an order. Endpoint https://mcp.vendor.example/orders is internal.",
		Parameters: map[string]any{
			"type": "object", "$ref": "#/$defs/order", "pattern": "^/api/v1/orders/[0-9]+$",
			"properties": map[string]any{"dsn": map[string]any{"default": "postgres://svc:pw6@db.internal/orders"}},
		},
	}}
	res = f.mustAllow(c)
	w := wire(t, res.Envelope)
	assertAbsent(t, "wire", w, "vendor.example", "pw6", "db.internal")
	assertPresent(t, "wire", w, "#/$defs/order", "^/api/v1/orders/[0-9]+$", "Create an order")
}
