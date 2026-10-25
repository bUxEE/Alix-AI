package extprovidersec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/adapters/external/openai"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// keyCanary stands for a provider API key. It is a TEST-ONLY value, not a real key and not shaped like one; it must
// appear in exactly one place, the Authorization header of the loopback provider, and nowhere else.
const keyCanary = "TESTONLY-canary-key-7f3a91c2d4e6b805"

const keyCanary2 = "TESTONLY-canary-key-second-0b1c2d3e4f"

const toolReply = `{
  "id": "resp_1", "status": "completed", "model": "gpt-4.1-mini-2025-04-14",
  "output": [{"type": "function_call", "id": "fc_1", "call_id": "call_abc", "name": "availability_list", "arguments": "{\"date\":\"2026-10-14\"}"}],
  "usage": {"input_tokens": 100, "output_tokens": 20}
}`

// seen is one request the loopback provider received.
type seen struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
}

// provider is a loopback HTTP server that plays the hosted provider: it records what it receives and answers as
// scripted.
type provider struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []seen
	reply func(n int, w http.ResponseWriter, r *http.Request)
}

func newProvider(t *testing.T, reply func(n int, w http.ResponseWriter, r *http.Request)) *provider {
	p := &provider{reply: reply}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		p.mu.Lock()
		p.got = append(p.got, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		n := len(p.got)
		p.mu.Unlock()
		p.reply(n, w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *provider) requests() []seen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]seen(nil), p.got...)
}

// adaptiveReply answers a request that offers tools with a tool call, and any other with text.
func adaptiveReply(n int, w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if bytes.Contains(body, []byte(`"tools"`)) {
		jsonReply(toolReply)(n, w, r)
		return
	}
	jsonReply(textReplyBody)(n, w, r)
}

const textReplyBody = `{"id":"resp_2","status":"completed","model":"gpt-4.1-mini-2025-04-14","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello there.","annotations":[]}]}],"usage":{"input_tokens":100,"output_tokens":20}}`

func jsonReply(body string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// keyed is the world wired to the REAL credential service (encrypted file backend), the REAL OpenAI adapter and a
// loopback provider, through the real network egress dialer.
type keyed struct {
	*world
	svc      *credentials.Service
	ref      pv.CredentialRef
	srv      *provider
	client   *openai.Client
	credLogs *bytes.Buffer
	adLogs   *bytes.Buffer
	secrets  string // the directory of the encrypted file
	keys     string // the directory of its master key
}

type acceptAll struct{}

func (acceptAll) ValidateKey(context.Context, credentials.ValidationRequest) (credentials.ValidationResult, error) {
	return credentials.ValidationResult{Accepted: true}, nil
}

func newKeyed(t *testing.T, reply func(int, http.ResponseWriter, *http.Request)) *keyed {
	t.Helper()
	w := newWorld(t, withTenantTwo())
	k := &keyed{world: w, credLogs: &bytes.Buffer{}, adLogs: &bytes.Buffer{}}
	root := t.TempDir()
	k.secrets, k.keys = filepath.Join(root, "secrets"), filepath.Join(root, "keys")
	enc, err := credentials.NewEncryptedFile(filepath.Join(k.secrets, credentials.FileName), credentials.KeyFileSource{Path: filepath.Join(k.keys, "master.key")})
	if err != nil {
		t.Fatal(err)
	}
	k.svc, err = credentials.New(credentials.Config{
		DB: w.db, Audit: w.alog, DeploymentID: receptionist().DeploymentID, Clock: w.clk, Validator: acceptAll{},
		Backends: []credentials.Backend{enc}, Default: credentials.BackendEncryptedFile,
		Logger: slog.New(slog.NewJSONHandler(k.credLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if k.ref, err = k.svc.AddAPIKey(ctxb(), receptionist(), credentials.AddAPIKeyRequest{ProviderID: "openai", Label: "ops key", Key: credentials.Secret(keyCanary)}); err != nil {
		t.Fatalf("AddAPIKey: %v", err)
	}
	k.srv = newProvider(t, reply)
	dest, err := egress.DestinationFromURL(k.srv.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(openai.ProviderName, al); err != nil {
		t.Fatal(err)
	}
	k.client, err = openai.New(openai.Config{BaseURL: k.srv.srv.URL, CredentialRef: string(k.ref.RefID), Model: "gpt-5-mini"}, reg.Dialer(openai.ProviderName),
		openai.ServiceKeys{Service: k.svc},
		openai.WithLogger(slog.New(slog.NewJSONHandler(k.adLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		openai.WithSleeper(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	w.snd.gen = k.client
	return k
}

// dumpDB renders every row of every table of the store, the way a stolen database file would read.
func dumpDB(t *testing.T, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) string {
	t.Helper()
	tables, err := db.QueryContext(ctxb(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		_ = tables.Scan(&n)
		names = append(names, n)
	}
	tables.Close()
	var sb strings.Builder
	for _, n := range names {
		rows, err := db.QueryContext(ctxb(), `SELECT * FROM "`+n+`"`)
		if err != nil {
			continue // a virtual table that cannot be read this way
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if rows.Scan(ptrs...) == nil {
				for _, v := range vals {
					switch x := v.(type) {
					case []byte:
						sb.Write(x)
					default:
						fmt.Fprint(&sb, x)
					}
					sb.WriteByte('|')
				}
				sb.WriteByte('\n')
			}
		}
		rows.Close()
	}
	return sb.String()
}

// mustNotContain fails when any of the surfaces holds the canary.
func mustNotContain(t *testing.T, canaries []string, surfaces map[string]string) {
	t.Helper()
	for name, s := range surfaces {
		for _, c := range canaries {
			if strings.Contains(s, c) {
				t.Fatalf("the key canary %q is in %s", c, name)
			}
		}
	}
}

// bannedProperty names the properties a response may never carry. authorization_url is a URL with a state and a
// challenge, never a credential.
var bannedProperty = regexp.MustCompile(`(?i)secret|password|api_?key|token|verifier|bearer|private_?key|^key$|^authorization$`)

// responseProperties collects "path.property" for every property under a "properties" map of a decoded document,
// with whether the property is write-only.
func responseProperties(path string, n any, out map[string]bool) {
	switch v := n.(type) {
	case map[string]any:
		if props, ok := v["properties"].(map[string]any); ok {
			for k, p := range props {
				wo := false
				if pm, ok := p.(map[string]any); ok {
					wo, _ = pm["writeOnly"].(bool)
				}
				out[path+"."+k] = wo
			}
		}
		for k, c := range v {
			responseProperties(path+"/"+k, c, out)
		}
	case []any:
		for _, c := range v {
			responseProperties(path, c, out)
		}
	}
}

// secretProperties returns the properties of the document whose name looks like a secret and that are not write-only.
func secretProperties(t *testing.T, raw []byte, isYAML bool) []string {
	t.Helper()
	var doc any
	var err error
	if isYAML {
		err = yaml.Unmarshal(raw, &doc)
	} else {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	props := map[string]bool{}
	responseProperties("", doc, props)
	var bad []string
	for p, writeOnly := range props {
		name := p[strings.LastIndex(p, ".")+1:]
		if bannedProperty.MatchString(name) && !writeOnly {
			bad = append(bad, p)
		}
	}
	return bad
}

// SEC-114
func TestSecExtAPIKeyNeverInClientResponsesOrLogs(t *testing.T) {
	sc := receptionist()

	t.Run("control: the key reaches the provider in the Authorization header and nowhere else on the wire", func(t *testing.T) {
		k := newKeyed(t, jsonReply(toolReply))
		res := k.say(sc, sayFree)
		wantAct(t, res, "present_result")
		reqs := k.srv.requests()
		if len(reqs) != 1 {
			t.Fatalf("provider requests = %d, want 1", len(reqs))
		}
		r := reqs[0]
		if got := r.Header.Get("Authorization"); got != "Bearer "+keyCanary {
			t.Fatalf("Authorization = %q: the control must show the key is used, or the checks below prove nothing", got)
		}
		for name, vals := range r.Header {
			if name != "Authorization" && strings.Contains(strings.Join(vals, ","), keyCanary) {
				t.Fatalf("the key is in header %s", name)
			}
		}
		if strings.Contains(r.Path+r.RawQuery, keyCanary) || r.RawQuery != "" || bytes.Contains(r.Body, []byte(keyCanary)) {
			t.Fatalf("the key is in the URL %q?%q or the body", r.Path, r.RawQuery)
		}
	})

	t.Run("not in anything a client or an operator can read, nor in any log, audit row, error or file at rest", func(t *testing.T) {
		k := newKeyed(t, jsonReply(toolReply))
		res := k.say(sc, sayFree)
		k.say(tenantTwo(), sayFree)

		resJSON, _ := json.Marshal(res)
		ref, err := k.svc.Get(ctxb(), sc, k.ref.RefID)
		if err != nil {
			t.Fatal(err)
		}
		list, err := k.svc.List(ctxb(), sc, "openai")
		if err != nil {
			t.Fatal(err)
		}
		refJSON, _ := json.Marshal(ref)
		listJSON, _ := json.Marshal(list)
		profJSON, _ := json.Marshal(k.profile(k.profA))
		disc, _ := k.eg.Disclose(ctxb(), sc, k.profile(k.profA), dataegressDisclosure())
		discJSON, _ := json.Marshal(disc)
		secretText := fmt.Sprintf("%v %+v %#v %s %q", credentials.Secret(keyCanary), credentials.Secret(keyCanary), credentials.Secret(keyCanary), credentials.Secret(keyCanary), credentials.Secret(keyCanary))
		secretJSON, _ := json.Marshal(map[string]any{"key": credentials.Secret(keyCanary)})
		var slogBuf bytes.Buffer
		slog.New(slog.NewJSONHandler(&slogBuf, nil)).Info("x", "key", credentials.Secret(keyCanary))
		fileBytes, _ := os.ReadFile(filepath.Join(k.secrets, credentials.FileName))
		audit := dumpAudit(t, k)

		mustNotContain(t, []string{keyCanary}, map[string]string{
			"the turn result":          string(resJSON) + fmt.Sprintf("%+v", res),
			"the credential reference": string(refJSON) + string(listJSON),
			"the provider profile":     string(profJSON),
			"the disclosure":           string(discJSON),
			"a printed secret":         secretText + string(secretJSON) + slogBuf.String(),
			"the adapter log":          k.adLogs.String(),
			"the credential log":       k.credLogs.String(),
			"the audit log":            audit,
			"the database":             dumpDB(t, k.db),
			"the encrypted file":       string(fileBytes),
			"the adapter description":  k.client.String(),
		})
		// A control for the file check: it is there, and it is not empty.
		if len(fileBytes) == 0 {
			t.Fatal("control: the encrypted file is empty, so the key is not stored where it should be")
		}
		if ref.Fingerprint == nil || strings.Contains(*ref.Fingerprint, keyCanary[10:]) {
			t.Fatalf("fingerprint %v: a masked fingerprint only", ref.Fingerprint)
		}
	})

	t.Run("a provider error that echoes the key never reaches the user, the outcome, the logs or the audit trail", func(t *testing.T) {
		echo := func(status int) func(int, http.ResponseWriter, *http.Request) {
			return func(_ int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"error":{"message":"Incorrect API key provided: %s","type":"invalid_request_error","code":"invalid_api_key"}}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			}
		}
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusBadRequest} {
			k := newKeyed(t, echo(status))
			k.proposeBook()
			res := k.say(sc, sayBook)
			if !res.Failed() {
				t.Fatalf("status %d: act %s, the failure was papered over", status, res.Directive.Act)
			}
			k.nothingExecuted(res)
			if len(k.srv.requests()) == 0 {
				t.Fatalf("status %d: control: the provider was never reached", status)
			}
			o := k.lastOutcome()
			resJSON, _ := json.Marshal(res)
			mustNotContain(t, []string{keyCanary}, map[string]string{
				"the turn result":  string(resJSON) + res.Response.Text,
				"the outcome":      fmt.Sprintf("%+v", o) + fmt.Sprint(o.Err()),
				"the adapter log":  k.adLogs.String(),
				"the audit log":    dumpAudit(t, k),
				"the usage record": dumpDB(t, k.db),
			})
		}
		// The adapter's own error, called directly, is a fixed message.
		k := newKeyed(t, echo(http.StatusUnauthorized))
		_, err := k.client.Generate(ctxb(), sc, simpleRich("hello"))
		if err == nil || strings.Contains(err.Error(), keyCanary) || strings.Contains(fmt.Sprintf("%#v", err), keyCanary) {
			t.Fatalf("error %v", err)
		}
	})

	t.Run("a redirect from the provider does not carry the key to another host", func(t *testing.T) {
		other := newProvider(t, jsonReply(`{}`))
		k := newKeyed(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.srv.URL+"/v1/responses", http.StatusTemporaryRedirect)
		})
		_, err := k.client.Generate(ctxb(), sc, simpleRich("hello"))
		if err == nil {
			t.Fatal("a redirect was treated as success")
		}
		if got := len(other.requests()); got != 0 {
			t.Fatalf("the redirect target received %d requests (with the key: %v)", got, other.requests())
		}
	})

	t.Run("another tenant cannot use the credential, a revoked key is never sent, and a rotated key takes effect on the next call", func(t *testing.T) {
		k := newKeyed(t, adaptiveReply)
		if _, err := k.client.Generate(ctxb(), sc, simpleRich("hello")); err != nil {
			t.Fatalf("control: %v", err)
		}
		n := len(k.srv.requests())
		if _, err := k.client.Generate(ctxb(), tenantTwo(), simpleRich("hello")); err == nil {
			t.Fatal("tenant B used tenant A's credential")
		}
		if len(k.srv.requests()) != n {
			t.Fatal("a request was sent with another tenant's credential")
		}
		if _, err := k.svc.Rotate(ctxb(), sc, k.ref.RefID, credentials.Rotation{Key: credentials.Secret(keyCanary2)}); err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if _, err := k.client.Generate(ctxb(), sc, simpleRich("hello")); err != nil {
			t.Fatalf("after rotation: %v", err)
		}
		reqs := k.srv.requests()
		if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "Bearer "+keyCanary2 {
			t.Fatalf("Authorization after rotation = %q", got)
		}
		if _, err := k.svc.Revoke(ctxb(), sc, k.ref.RefID); err != nil {
			t.Fatal(err)
		}
		n = len(k.srv.requests())
		if _, err := k.client.Generate(ctxb(), sc, simpleRich("hello")); err == nil || len(k.srv.requests()) != n {
			t.Fatalf("a revoked key was used (err %v)", err)
		}
		fileBytes, _ := os.ReadFile(filepath.Join(k.secrets, credentials.FileName))
		mustNotContain(t, []string{keyCanary, keyCanary2}, map[string]string{
			"the adapter log": k.adLogs.String(), "the credential log": k.credLogs.String(), "the encrypted file": string(fileBytes),
			"the audit log": dumpAudit(t, k), "the database": dumpDB(t, k.db),
		})
	})

	t.Run("no response schema of the providers API can carry a secret, and the one request that does is write-only", func(t *testing.T) {
		// The checker is run over a planted schema first, so a checker that finds nothing is caught.
		planted := []byte(`{"properties": {"api_key": {"type": "string"}, "ok": {"type": "string"}, "pw": {"properties": {"secret": {"type": "string", "writeOnly": true}}}}}`)
		if got := secretProperties(t, planted, false); len(got) != 1 || !strings.HasSuffix(got[0], ".api_key") {
			t.Fatalf("control: the checker flagged %v in a schema with one secret response property", got)
		}
		root := secutil.RepoRoot(t)
		for _, f := range []string{"schemas/api/paths/providers.yaml", "schemas/api/paths/oauth.yaml"} {
			raw := secutil.ReadFile(t, f)
			if bad := secretProperties(t, raw, true); len(bad) > 0 {
				t.Fatalf("%s: properties that may carry a secret and are not write-only: %v", f, bad)
			}
		}
		schemas, err := filepath.Glob(filepath.Join(root, "schemas", "providers", "*.schema.json"))
		if err != nil || len(schemas) < 10 {
			t.Fatalf("found %d provider schemas (%v)", len(schemas), err)
		}
		for _, f := range schemas {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if bad := secretProperties(t, raw, false); len(bad) > 0 {
				t.Fatalf("%s: properties that may carry a secret: %v", filepath.Base(f), bad)
			}
		}
		raw := secutil.ReadFile(t, "schemas/api/paths/providers.yaml")
		if !strings.Contains(string(raw), "writeOnly: true") {
			t.Fatal("control: the manual credential submit is not marked write-only")
		}
	})

	t.Run("no client code, SDK or example reads a provider key: the key lives in the credential service only", func(t *testing.T) {
		keyEnv := regexp.MustCompile(`OPENAI_API_KEY|ANTHROPIC_API_KEY|GEMINI_API_KEY|GOOGLE_API_KEY|META_API_KEY|LLAMA_API_KEY`)
		keyLit := regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}|\bAIza[0-9A-Za-z_-]{30,}`)
		scanned := 0
		for _, rel := range secutil.RepoFiles(t) {
			if !(strings.HasPrefix(rel, "sdk/") || strings.HasPrefix(rel, "pkg/sdk-go/") || strings.HasPrefix(rel, "examples/") || strings.HasPrefix(rel, "web/")) {
				continue
			}
			if !strings.HasSuffix(rel, ".ts") && !strings.HasSuffix(rel, ".tsx") && !strings.HasSuffix(rel, ".js") && !strings.HasSuffix(rel, ".vue") &&
				!strings.HasSuffix(rel, ".py") && !strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, ".json") {
				continue
			}
			if isTestSource(rel) {
				continue // a fixture key in a test of the SDK is a canary, not a credential
			}
			b := secutil.ReadFile(t, rel)
			scanned++
			if keyEnv.Match(b) || keyLit.Match(b) {
				t.Fatalf("%s reads or embeds a provider key: keys never reach a client", rel)
			}
		}
		if scanned < 50 {
			t.Fatalf("the client scan looked at %d files only", scanned)
		}
	})
}

// isTestSource reports whether a path is a test file of any of the repository languages.
func isTestSource(rel string) bool {
	return strings.HasSuffix(rel, "_test.go") || strings.Contains(rel, ".test.") || strings.Contains(rel, ".spec.") ||
		strings.Contains(rel, "/tests/") || strings.Contains(rel, "/test/") || strings.Contains(rel, "__tests__") || strings.Contains(rel, "/test_")
}

// dumpAudit renders the data of every audit event of the store.
func dumpAudit(t *testing.T, k *keyed) string {
	t.Helper()
	rows, err := k.db.QueryContext(ctxb(), `SELECT event_type, data FROM audit_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var typ, data string
		if err := rows.Scan(&typ, &data); err != nil {
			t.Fatal(err)
		}
		sb.WriteString(typ + " " + data + "\n")
	}
	return sb.String()
}

func dataegressDisclosure() dataegress.DisclosureInput {
	return dataegress.DisclosureInput{Role: pv.RoleConversationalController}
}

// simpleRich is a one-message request.
func simpleRich(text string) providers.RichRequest {
	return providers.RichRequest{Messages: []providers.RichMessage{
		{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart("You propose typed tasks.")}},
		{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart(text)}},
	}}
}

var _ = contracts.ErrInternal
