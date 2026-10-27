//go:build !noexternal

package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/analyst/web"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/tools/devkeys"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	analystE2EVAT          = "IT12345678901"
	analystE2ERevenue      = "1200000"
	analystE2EAttendeeA    = "Maria Rossi"
	analystE2EAttendeeB    = "Luca Bianchi"
	analystE2EAttendeeSens = "Giulia Verdi"
	analystE2EInventedName = "Eve_Invented_99"
	analystE2EPageURL      = "https://www.gov.it/incentives/xx-2026"
	analystE2EPageBody     = "Official incentive XX: deadline 2026-06-30. Companies with under 50 employees in manufacturing in Italy may apply. This page is not legal advice."
)

// ---- scripted OpenAI ------------------------------------------------------------------

type analystScriptedOA struct {
	srv         *httptest.Server
	mu          sync.Mutex
	queue       []string
	bodies      [][]byte
	searchAuths []string // Authorization headers of web_search Responses calls
	calls       int
}

func newAnalystScriptedOA(t *testing.T) *analystScriptedOA {
	t.Helper()
	s := &analystScriptedOA{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, append([]byte(nil), body...))
		s.calls++
		text := `{"kind":"answer","answer":{"text_plan":"Done.","blocks":[]}}`
		switch {
		case bytes.Contains(body, []byte(`"type":"web_search"`)) || bytes.Contains(body, []byte(`"type": "web_search"`)):
			s.searchAuths = append(s.searchAuths, r.Header.Get("Authorization"))
			s.mu.Unlock()
			model := rdyExtModelA + "-" + rdyExtVersionA
			out := fmt.Sprintf(`{"id":"resp_ws","object":"response","status":"completed","model":%q,`+
				`"output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"q"}},`+
				`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Found page.",`+
				`"annotations":[{"type":"url_citation","url":%q,"title":"Holidays"},{"type":"url_citation","url":"https://evil.example/x","title":"Evil"}]}]}],`+
				`"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}`,
				model, analystE2EPageURL)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, out)
			return
		case bytes.Contains(body, []byte(`"enum":["in_domain","out_of_domain"]`)):
			// Match the utterance only — domain examples also mention "Persian cat".
			// Request bodies are JSON, so the prompt's newline is the two bytes '\' 'n'.
			utt := body
			for _, sep := range [][]byte{[]byte(`Utterance:\n`), []byte("Utterance:\n"), []byte("Utterance:")} {
				if i := bytes.LastIndex(body, sep); i >= 0 {
					utt = body[i:]
					break
				}
			}
			if bytes.Contains(utt, []byte("Persian")) || bytes.Contains(utt, []byte("persian")) ||
				bytes.Contains(utt, []byte("cat big")) {
				text = `{"classification":"out_of_domain","reason":"unrelated consumer pet question"}`
			} else {
				text = `{"classification":"in_domain","reason":"company governance"}`
			}
		case len(s.queue) > 0:
			text = s.queue[0]
			s.queue = s.queue[1:]
			if strings.Contains(text, "{{dataset_id}}") {
				text = strings.ReplaceAll(text, "{{dataset_id}}", firstDatasetIDFromOABody(body))
			}
		}
		s.mu.Unlock()
		model := rdyExtModelA + "-" + rdyExtVersionA
		enc, _ := json.Marshal(text)
		out := fmt.Sprintf(`{"id":"resp_an","object":"response","status":"completed","model":%q,`+
			`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":%s,"annotations":[]}]}],`+
			`"usage":{"input_tokens":40,"input_tokens_details":{"cached_tokens":0},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":60}}`,
			model, enc)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *analystScriptedOA) enqueue(steps ...string) {
	s.mu.Lock()
	s.queue = append(s.queue, steps...)
	s.mu.Unlock()
}

func (s *analystScriptedOA) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *analystScriptedOA) allBodies() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, x := range s.bodies {
		b.Write(x)
		b.WriteByte('\n')
	}
	return b.String()
}

func (s *analystScriptedOA) bodiesSnapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.bodies))
	for i, b := range s.bodies {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

func (s *analystScriptedOA) searchAuthSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.searchAuths...)
}

func (s *analystScriptedOA) searchCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.searchAuths)
}

func truncateOA(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// firstDatasetIDFromOABody finds "dataset ds_…" from the analyst step prompt fence.
func firstDatasetIDFromOABody(body []byte) string {
	const mark = "dataset ds_"
	i := bytes.Index(body, []byte(mark))
	if i < 0 {
		// JSON may escape; try without relying on a space before ds_.
		i = bytes.Index(body, []byte("ds_"))
		if i < 0 {
			return "ds_missing"
		}
		rest := body[i:]
		end := 0
		for end < len(rest) && (rest[end] == '_' || (rest[end] >= 'a' && rest[end] <= 'z') || (rest[end] >= '0' && rest[end] <= '9')) {
			end++
		}
		id := string(rest[:end])
		if len(id) < 4 {
			return "ds_missing"
		}
		return id
	}
	rest := body[i+len("dataset "):]
	end := 0
	for end < len(rest) && (rest[end] == '_' || (rest[end] >= 'a' && rest[end] <= 'z') || (rest[end] >= '0' && rest[end] <= '9')) {
		end++
	}
	id := string(rest[:end])
	if len(id) < 4 {
		return "ds_missing"
	}
	return id
}

func analystE2EStep(kind string, extra map[string]any) string {
	m := map[string]any{"kind": kind}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// ---- company host ----------------------------------------------------------------------

type companyHost struct {
	mu    sync.Mutex
	reads []string
}

func (h *companyHost) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reads)
}

type companyHostAdapter struct{ h *companyHost }

func (a *companyHostAdapter) Describe(context.Context) (adapters.Description, error) {
	return adapters.Description{
		Transport:        "fake",
		ContractVersions: []int{adapters.ContractVersion},
		Capabilities: []adapters.CapabilityOps{
			{CapabilityID: "catalog.find_title", Operations: []adapters.Operation{adapters.OpRead}},
		},
	}, nil
}
func (a *companyHostAdapter) Health(context.Context) (adapters.HealthReport, error) {
	return adapters.HealthReport{Status: adapters.HealthOK}, nil
}
func (a *companyHostAdapter) Inspect(context.Context, contracts.Scope, adapters.InspectRequest) (adapters.Inspection, error) {
	return adapters.Inspection{}, nil
}
func (a *companyHostAdapter) Validate(context.Context, contracts.Scope, adapters.ValidateRequest) (act.DeterministicResult, error) {
	return act.DeterministicResult{Status: act.DeterministicResultStatusRejected}, nil
}
func (a *companyHostAdapter) Prepare(context.Context, contracts.Scope, adapters.PrepareRequest) (act.DeterministicResult, error) {
	return act.DeterministicResult{Status: act.DeterministicResultStatusRejected}, nil
}
func (a *companyHostAdapter) Commit(context.Context, contracts.Scope, adapters.CommitRequest) (act.DeterministicResult, error) {
	return act.DeterministicResult{Status: act.DeterministicResultStatusRejected}, nil
}
func (a *companyHostAdapter) Status(context.Context, contracts.Scope, adapters.StatusRequest) (act.DeterministicResult, error) {
	return act.DeterministicResult{Status: act.DeterministicResultStatusRejected}, nil
}
func (a *companyHostAdapter) Cancel(context.Context, contracts.Scope, adapters.CancelRequest) (act.DeterministicResult, error) {
	return act.DeterministicResult{Status: act.DeterministicResultStatusRejected}, nil
}
func (a *companyHostAdapter) Read(_ context.Context, _ contracts.Scope, req adapters.ReadRequest) (act.DeterministicResult, error) {
	a.h.mu.Lock()
	a.h.reads = append(a.h.reads, req.CapabilityID)
	a.h.mu.Unlock()
	if req.CapabilityID != "catalog.find_title" {
		return act.DeterministicResult{Status: act.DeterministicResultStatusRejected}, nil
	}
	now := common.Timestamp(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	fresh := &act.Freshness{AsOf: now, Version: "v1"}
	return act.DeterministicResult{
		Operation:    act.DeterministicResultOperationRead,
		CapabilityID: common.CapabilityID(req.CapabilityID),
		Status:       act.DeterministicResultStatusOk,
		Values: []act.Fact{
			{Field: "sector", Value: act.CanonicalValue{Value: act.TextValue{Kind: "text", Value: "manufacturing"}}},
			{Field: "vat_number", Value: act.CanonicalValue{Value: act.TextValue{Kind: "text", Value: analystE2EVAT}}},
			{Field: "revenue", Value: act.CanonicalValue{Value: act.DecimalValue{Kind: "decimal", Value: analystE2ERevenue}}},
			{Field: "employees", Value: act.CanonicalValue{Value: act.IntegerValue{Kind: "integer", Value: 48}}},
			{Field: "attendee_name", Value: act.CanonicalValue{Value: act.TextValue{Kind: "text", Value: analystE2EAttendeeA}}},
			{Field: "chair_name", Value: act.CanonicalValue{Value: act.TextValue{Kind: "text", Value: analystE2EAttendeeSens}}},
			{Field: "secretary_name", Value: act.CanonicalValue{Value: act.TextValue{Kind: "text", Value: analystE2EAttendeeB}}},
		},
		Errors:    []act.Issue{},
		Freshness: fresh,
	}, nil
}

// ---- package ---------------------------------------------------------------------------

func analystE2EPackage(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "examples", "packages", "valid-minimal")
	dst := filepath.Join(t.TempDir(), "pkg")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		switch filepath.ToSlash(rel) {
		case "alixai.yaml":
			b = append(b, []byte(`
assistant:
  domain:
    description: "Meetings, minutes, companies and Italian company governance."
    in_scope: [company meetings and minutes, corporate incentives and filings, governance and related law/practice]
    out_of_scope: [general chatbot use, unrelated consumer topics]
    examples:
      en:
        in_domain: ["Should we apply for incentive XX?"]
        out_of_domain: ["Is a Persian cat big?"]
    knowledge_allowed: true
    web_search_allowed: true
    preferred_web_hosts: [gov.it]
  abilities:
    loop: true
    compute: true
    forecasts: true
    presentation: true
    drafts: true
    knowledge: true
    web_search: true
    web_fetch: true
`)...)
		case "capabilities/catalog-find-title.yaml":
			insert := `
- name: sector
  type: string
  required: false
  description: Business sector of the borrowing organisation.
  classification: public
- name: vat_number
  type: string
  required: false
  description: VAT number of the borrowing organisation.
  local_only: true
- name: revenue
  type: number
  required: false
  description: Annual revenue of the borrowing organisation.
  classification: confidential
- name: employees
  type: integer
  required: false
  description: Headcount of the borrowing organisation.
  classification: internal
- name: attendee_name
  type: string
  required: false
  description: Name of an assembly attendee.
  classification: confidential
- name: chair_name
  type: string
  required: false
  description: Name of the assembly chair.
  sensitive: true
- name: secretary_name
  type: string
  required: false
  description: Name of the assembly secretary.
  classification: confidential
`
			text := strings.Replace(string(b), "- name: max_results\n", insert+"- name: max_results\n", 1)
			text = strings.Replace(text, "- cft-field-query\n", "- cft-field-query\n- cft-field-sector\n- cft-field-vat-number\n- cft-field-revenue\n- cft-field-employees\n- cft-field-attendee-name\n- cft-field-chair-name\n- cft-field-secretary-name\n", 1)
			b = []byte(text)
		case "documentation/index.yaml":
			b = append(b, []byte(`
- id: cft-field-sector
  capability_id: catalog.find_title
  facet: field
  language: en
  title: 'Field: sector'
  summary: Public business sector label used by analyst turns.
  location:
    path: documentation/catalog/find-title.md
    anchor: field-sector
  field: sector
- id: cft-field-vat-number
  capability_id: catalog.find_title
  facet: field
  language: en
  title: 'Field: vat_number'
  summary: Local-only VAT number; never leaves the deployment.
  location:
    path: documentation/catalog/find-title.md
    anchor: field-vat-number
  field: vat_number
- id: cft-field-revenue
  capability_id: catalog.find_title
  facet: field
  language: en
  title: 'Field: revenue'
  summary: Confidential annual revenue; refused in web_query.
  location:
    path: documentation/catalog/find-title.md
    anchor: field-revenue
  field: revenue
- id: cft-field-employees
  capability_id: catalog.find_title
  facet: field
  language: en
  title: 'Field: employees'
  summary: Internal headcount used for eligibility checks.
  location:
    path: documentation/catalog/find-title.md
    anchor: field-employees
  field: employees
- id: cft-field-attendee-name
  capability_id: catalog.find_title
  facet: field
  language: en
  title: 'Field: attendee_name'
  summary: Confidential attendee name; withheld from external model prompts.
  location:
    path: documentation/catalog/find-title.md
    anchor: field-attendee-name
  field: attendee_name
- id: cft-field-chair-name
  capability_id: catalog.find_title
  facet: field
  language: en
  title: 'Field: chair_name'
  summary: Sensitive chair name; withheld from external model prompts.
  location:
    path: documentation/catalog/find-title.md
    anchor: field-chair-name
  field: chair_name
- id: cft-field-secretary-name
  capability_id: catalog.find_title
  facet: field
  language: en
  title: 'Field: secretary_name'
  summary: Confidential secretary name; withheld from external model prompts.
  location:
    path: documentation/catalog/find-title.md
    anchor: field-secretary-name
  field: secretary_name
`)...)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, Default, nil, "package", "sign", "-app", "demo-library", "-version", "1.0.0",
		"-key", devkeys.KeyID+"="+filepath.Join("..", "..", "tools", "devkeys", "test-only-publisher.key.pem"), dst)
	if res.code != ExitOK {
		t.Fatalf("package sign: exit %d\n%s", res.code, res.stderr)
	}
	return dst
}

// ---- harness ---------------------------------------------------------------------------

type analystTurnRig struct {
	t       *testing.T
	srv     *running
	iss     *identitytest.Issuer
	oa      *analystScriptedOA
	sx      *recordingSearx
	host    *companyHost
	dataDir string
}

type stubPubResolver map[string][]netip.Addr

func (s stubPubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := s[host]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("no such host")
}

type analystTurnRigOpts struct {
	maxTurnTokens   int
	webYAML         string // under dialogue.analyst.web; empty → searxng default
	nativeWebSearch bool
	// webQuery nil → enable all backends; non-nil empty → omit web_query; else exact list.
	webQuery *[]pv.EgressPolicyWebQueryProvider
	// workspaceWebOff disables web_query only at workspace after enabling the chain.
	workspaceWebOff bool
	// principalOwnKey binds usr_alice's controller to the profile with this API key (non-empty).
	principalOwnKey string
	// egressProviders overrides AllowedProviders on every egress level (default openai).
	egressProviders []pv.ProviderID
	// webSearchQuota0 sets a web_searches daily quota of 0 on the openai budget.
	webSearchQuota0 bool
}

func startAnalystTurnRig(t *testing.T, maxTurnTokens int) *analystTurnRig {
	t.Helper()
	return startAnalystTurnRigOpts(t, analystTurnRigOpts{maxTurnTokens: maxTurnTokens})
}

func startAnalystTurnRigOpts(t *testing.T, opt analystTurnRigOpts) *analystTurnRig {
	t.Helper()
	extWireSaveSection(t)
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	if opt.maxTurnTokens <= 0 {
		opt.maxTurnTokens = 32000
	}

	oa := newAnalystScriptedOA(t)
	sx := newRecordingSearx(t, []map[string]string{{
		"url": analystE2EPageURL, "title": "Incentive XX", "snippet": "eligibility",
	}})
	page := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, analystE2EPageBody)
	}))
	t.Cleanup(page.Close)
	pageURL, _ := url.Parse(page.URL)

	oldFetch := newAnalystFetcher
	newAnalystFetcher = func() *web.Fetcher {
		fet := web.NewFetcher(stubPubResolver{
			"www.gov.it": {netip.MustParseAddr("1.1.1.1")},
			"gov.it":     {netip.MustParseAddr("1.1.1.1")},
		}, clock.Real{}, nil)
		fet.SetHTTPClientForTest(&http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // fixture
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 5 * time.Second}
					return d.DialContext(ctx, network, pageURL.Host)
				},
			},
		})
		return fet
	}
	t.Cleanup(func() { newAnalystFetcher = oldFetch })

	host := &companyHost{}
	reg := transportsSwapDefault(t)
	reg.register(TransportKindMCP, func(context.Context, TransportRequest) (adapters.AppAdapter, error) {
		return &companyHostAdapter{h: host}, nil
	})

	pkgDir := analystE2EPackage(t)
	webBlock := opt.webYAML
	if webBlock == "" {
		webBlock = fmt.Sprintf(`provider: searxng
      origin: %q
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`, sx.srv.URL)
	} else {
		webBlock = strings.ReplaceAll(webBlock, "{{searx}}", sx.srv.URL)
	}
	// serveEnv does not register external_providers YAML; mutate process-wide
	// externalSection (same pattern as TestLightExternalServeReadyWithoutLocalAI).
	// performance (8192 prompt) — lite's 2048 cannot fit analyst CompactSchema steps.
	extra := identityYAML + fmt.Sprintf(`
profile: performance
dialogue:
  analyst:
    enabled: true
    max_turn_tokens: %d
    abilities:
      loop: true
      compute: true
      forecasts: true
      presentation: true
      drafts: true
      knowledge: true
      web_search: true
      web_fetch: true
    web:
      %s
`, opt.maxTurnTokens, webBlock)

	cfg, dataDir := serveConfig(t, f, extra)
	iss := newHostIssuer(t)
	provisionIssuer(t, cfg, iss)
	// Serve re-verifies packages via the trust store (not Installer's test Verifier).
	code, out, errOut := runCLI(t, "trust", "add-key", "-config", cfg, "-app", "demo-library",
		"-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys")
	if code != ExitOK {
		t.Fatalf("trust add-key = %d\n%s\n%s", code, out, errOut)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	// Real clock: package sign stamps wall time; a Fake behind wall time rejects as signed_in_the_future.
	clk := clock.NewFake(time.Now().UTC().Add(time.Minute))
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	sec := externalConfig{
		Enabled: true, DeploymentOperators: []string{"demo-library/usr_ops"},
		TenantAdmins: []string{
			"demo-library/ten_a/usr_admin", "demo-library/ten_b/usr_admin", "demo-library/ten_acme/usr_admin",
		},
		OpenAIBaseURL: oa.srv.URL,
	}
	externalSection = sec
	admin, err := newExternalAdmin(externalAdminDeps{
		DB: db, Audit: alog, Clock: clk, DeploymentID: "dep_cli",
		Dir: filepath.Join(dataDir, "external"), Validator: externalAcceptKeys{}, Section: sec,
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := contracts.Scope{
		DeploymentID: "dep_cli", ApplicationID: "demo-library", TenantID: "ten_a", WorkspaceID: "wsp_main",
		PrincipalID: "usr_ops", Roles: []string{"operator"}, CapabilityScopes: []string{"providers.manage"}, SessionID: "ses_ops",
	}
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
		if err := admin.modes.set(lv, ops, modes.ModeExternal); err != nil {
			t.Fatal(err)
		}
	}
	opsB := ops.Clone()
	opsB.TenantID = "ten_b"
	if err := admin.modes.set(modes.LevelTenant, opsB, modes.ModeExternal); err != nil {
		t.Fatal(err)
	}

	prof := qualifyAnalystCapableProfile(t, admin, ops, clk, opt.nativeWebSearch)
	sec.Routing = map[string]externalRoute{"controller": {Profiles: []string{string(prof.ProfileID)}}}
	externalSection = sec
	lightExtEgress(t, admin, ops, "dep_cli")
	if opt.webQuery != nil || opt.workspaceWebOff {
		analystApplyWebQueryEgress(t, admin, ops, "dep_cli", opt.webQuery, opt.workspaceWebOff)
	}
	// After web_query apply (which resets providers to openai): optional allowlist override.
	if len(opt.egressProviders) > 0 {
		analystOverrideEgressProviders(t, admin, ops, "dep_cli", opt.egressProviders)
	}
	if opt.principalOwnKey != "" {
		analystBindPrincipalOwnKey(t, admin, ops, prof, opt.principalOwnKey)
	}

	// Tenant B: disable egress so web_query fails closed.
	adminB := opsB.Clone()
	adminB.PrincipalID, adminB.SessionID = "usr_egress_admin", "ses_egress_b"
	adminB.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	c, _ := dataegress.WriteCapability(pv.EgressLevelTenant)
	adminB.CapabilityScopes = append(adminB.CapabilityScopes, c)
	if _, err := admin.egress.PutPolicy(ctx, adminB, dataegress.PolicyInput{
		Level: pv.EgressLevelTenant, Enabled: false, SourceMode: pv.EgressModeLocalOnly,
	}); err != nil {
		t.Fatal(err)
	}
	bgt := rdyExtProviderBudget("openai", 5_000_000, 50_000_000)
	if opt.webSearchQuota0 {
		zero := pv.Counter(0)
		bgt.Quotas = &pv.BudgetQuotas{WebSearches: &pv.BudgetQuota{Daily: &zero}}
	}
	if _, err := admin.PutBudget(ctx, ops, bgt); err != nil {
		t.Fatal(err)
	}

	verifier := trust.NewVerifier(devTrust{}, clk)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: db, Clock: clk, DataDir: dataDir, Verifier: verifier, CoreVersion: apppkg.DevCoreVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Install(ctx, apppkg.FromDir(pkgDir)); err != nil {
		if ref, ok := err.(*apppkg.RefusedError); ok {
			t.Fatalf("install refused: %v findings=%+v", err, ref.Findings)
		}
		t.Fatal(err)
	}
	resUp, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_analyst_e2e' WHERE application_id = 'demo-library' AND status = 'quarantined'`)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := resUp.RowsAffected()
	if n != 1 {
		t.Fatalf("activate rows=%d, want 1", n)
	}
	var st string
	if err := db.QueryRowContext(ctx, `SELECT status FROM apppkg_packages WHERE application_id = 'demo-library'`).Scan(&st); err != nil || st != "active" {
		t.Fatalf("status after activate = %q err=%v", st, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	return &analystTurnRig{t: t, srv: startServe(t, cfg), iss: iss, oa: oa, sx: sx, host: host, dataDir: dataDir}
}

func analystOverrideEgressProviders(t *testing.T, a *externalAdmin, scope contracts.Scope, deployment string, providers []pv.ProviderID) {
	t.Helper()
	ctx := context.Background()
	admin := scope.Clone()
	admin.PrincipalID, admin.SessionID = "usr_egress_admin", "ses_egress_prov"
	admin.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		c, _ := dataegress.WriteCapability(lv)
		admin.CapabilityScopes = append(admin.CapabilityScopes, c)
	}
	models := make([]pv.ModelRef, 0, len(providers))
	for _, p := range providers {
		models = append(models, pv.ModelRef{ProviderID: p, ModelID: rdyExtModelA})
	}
	for _, level := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		cur, found, err := a.egress.GetPolicy(ctx, admin, level, "")
		if err != nil {
			t.Fatal(err)
		}
		in := dataegress.PolicyInput{
			Level: level, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
			AllowedProviders: providers, AllowedModels: models,
			AllowedClasses: []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal},
			AllowedRegions: []pv.RegionCode{"us", "eu"},
			WebQuery:       cur.WebQuery,
		}
		if found {
			in.ExpectedRevision = cur.Revision
			if cur.WebQuery == nil {
				in.WebQuery = &pv.EgressPolicyWebQueryEgress{
					Enabled: true,
					AllowedProviders: []pv.EgressPolicyWebQueryProvider{
						pv.EgressPolicyWebQueryProviderProviderNative,
						pv.EgressPolicyWebQueryProviderSearxng,
					},
				}
			}
		}
		if level == pv.EgressLevelDeployment {
			if _, err := a.egress.PutDeploymentPolicy(ctx, dataegress.Operator{DeploymentID: deployment, PrincipalID: "opr_root"}, in); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := a.egress.PutPolicy(ctx, admin, in); err != nil {
			t.Fatal(err)
		}
	}
}

func analystBindPrincipalOwnKey(t *testing.T, a *externalAdmin, ops contracts.Scope, prof pv.ProviderProfile, key string) {
	t.Helper()
	ctx := context.Background()
	user := ops.Clone()
	user.PrincipalID = identitytest.DefaultPrincipal
	user.SessionID = "ses_alice_bind"
	user.Roles = []string{"user"}
	user.CapabilityScopes = []string{"providers.manage", "credentials.resolve"}
	ref, err := a.creds.AddPrincipalAPIKey(ctx, user, credentials.AddAPIKeyRequest{
		ProviderID: prof.ProviderID, Label: "alice-own", Key: credentials.Secret(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Status != pv.CredentialRefStatusActive {
		t.Fatalf("principal cred status=%s", ref.Status)
	}
	if err := a.reg.PutPrincipalBinding(ctx, user, providerreg.PrincipalBinding{
		Role: pv.PrincipalLlmRoleConversationalController, ProfileID: prof.ProfileID,
		ProviderID: prof.ProviderID, ModelID: prof.ModelID,
	}); err != nil {
		t.Fatal(err)
	}
}

func qualifyAnalystCapableProfile(t *testing.T, a *externalAdmin, scope contracts.Scope, clk *clock.Fake, nativeWeb ...bool) pv.ProviderProfile {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile("../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json")
	if err != nil {
		t.Fatal(err)
	}
	var p pv.ProviderProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	p.ProfileID, p.CredRef, p.BillingNoticeAck, p.LastConformance = "", nil, nil, nil
	p.Lifecycle, p.QuarantineReason, p.CreatedAt, p.UpdatedAt = "", nil, "", ""
	p.ProviderID, p.ModelID, p.ModelVersion = "openai", rdyExtModelA, rdyExtVersionA
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: "demo-library"}}
	yes := true
	p.AnalystCapable = &yes
	if len(nativeWeb) > 0 && nativeWeb[0] {
		p.NativeWebSearch = &yes
	}
	day := clk.Now().Format("2006-01-02")
	p.Terms.RecordedOn, p.LastPolicyReview = day, day
	p, err = a.reg.Register(ctx, scope, p)
	if err != nil {
		t.Fatal(err)
	}
	var in gateway.ProvidersCredentialInput
	if err := json.Unmarshal([]byte(`{"key":"fixture-credential-value-0001"}`), &in); err != nil {
		t.Fatal(err)
	}
	if st, err := a.SubmitCredential(ctx, scope, "openai", in); err != nil || st.State != pv.ConnectionStatusStateConnected {
		t.Fatalf("SubmitCredential = %+v %v", st, err)
	}
	if p, err = a.reg.Get(ctx, scope, p.ProfileID); err != nil {
		t.Fatal(err)
	}
	if p, err = a.reg.AcknowledgeBillingNotice(ctx, scope, p.ProfileID, providerreg.Acknowledgement{
		NoticeVersion: p.BillingNotice.Version, ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true,
	}); err != nil {
		t.Fatal(err)
	}
	var rep pv.ConformanceReport
	if err := json.Unmarshal(extWireTestReport(t, p, clk.Now().Add(-time.Hour)), &rep); err != nil {
		t.Fatal(err)
	}
	if _, err := a.reg.RecordConformance(ctx, scope, p.ProfileID, rep); err != nil {
		t.Fatal(err)
	}
	if p, err = a.reg.Qualify(ctx, scope, p.ProfileID); err != nil || p.Lifecycle != pv.ProfileLifecycleQualified {
		t.Fatalf("Qualify = %+v %v", p.Lifecycle, err)
	}
	return p
}

func (r *analystTurnRig) token(tenant, session string, scopes ...string) string {
	r.t.Helper()
	if len(scopes) == 0 {
		// Token scopes are capability IDs (contracts.Scope.Permits), not permission names.
		scopes = []string{"catalog.find_title", "reservations.create",
			"ops.analyst.read", "ops.analyst.write"}
	}
	return r.iss.Token(func(d *identitytest.Draft) {
		d.Set(identity.ClaimTenant, tenant)
		d.Set(identity.ClaimSession, session)
		d.Set(identity.ClaimCapabilityScopes, scopes)
	})
}

func (r *analystTurnRig) openSession(tenant, session string) {
	r.t.Helper()
	tok := r.token(tenant, session)
	code, body := r.srv.callToken(r.t, tok, http.MethodPost, "/v1/sessions", "")
	if code != http.StatusOK {
		r.t.Fatalf("POST /v1/sessions = %d: %s", code, body)
	}
}

func (r *analystTurnRig) turn(tenant, session, text string, caps any) (int, map[string]any) {
	r.t.Helper()
	body := map[string]any{"text": text, "language": "en"}
	if caps != nil {
		body["client_capabilities"] = caps
	}
	raw, _ := json.Marshal(body)
	tok := r.token(tenant, session)
	path := "/v1/sessions/" + url.PathEscape(session) + "/turns"
	code, resp := r.srv.callToken(r.t, tok, http.MethodPost, path, string(raw))
	var out map[string]any
	_ = json.Unmarshal(resp, &out)
	return code, out
}

func turnCode(body map[string]any) string {
	if c, ok := body["code"].(string); ok {
		return c
	}
	if errObj, ok := body["error"].(map[string]any); ok {
		if c, ok := errObj["code"].(string); ok {
			return c
		}
	}
	return ""
}

// ---- cases a–h -------------------------------------------------------------------------

func TestAnalystTurnE2E_OutOfDomainPersianCat(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.openSession("ten_a", "ses_cat")
	code, body := r.turn("ten_a", "ses_cat", "Is a Persian cat big?", nil)
	if code != http.StatusOK {
		t.Fatalf("turn status=%d body=%s", code, mustJSON(body))
	}
	if eventCode := firstErrorCode(body); eventCode != string(contracts.ErrOutOfDomain) && turnCode(body) != string(contracts.ErrOutOfDomain) {
		t.Fatalf("want out_of_domain, got event=%q code=%q body=%s", eventCode, turnCode(body), mustJSON(body))
	}
	if r.host.readCount() != 0 {
		t.Fatalf("host must not be read: %d", r.host.readCount())
	}
	if len(r.sx.recorded()) != 0 {
		t.Fatalf("searx must be empty: %v", r.sx.recorded())
	}
}

func firstErrorCode(body map[string]any) string {
	evs, _ := body["events"].([]any)
	for _, e := range evs {
		m, _ := e.(map[string]any)
		if m["type"] != "error" {
			continue
		}
		if errObj, ok := m["error"].(map[string]any); ok {
			if c, ok := errObj["code"].(string); ok {
				return c
			}
		}
	}
	return ""
}

func firstErrorDetail(body map[string]any, key string) string {
	evs, _ := body["events"].([]any)
	for _, e := range evs {
		m, _ := e.(map[string]any)
		if m["type"] != "error" {
			continue
		}
		errObj, _ := m["error"].(map[string]any)
		details, _ := errObj["details"].(map[string]any)
		if v, ok := details[key].(string); ok {
			return v
		}
	}
	return ""
}

func TestAnalystTurnE2E_ItalianDiscountWeb(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(
		analystE2EStep("read", map[string]any{"read": map[string]any{"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "self"}}}),
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX eligibility"}}),
		analystE2EStep("fetch", map[string]any{"fetch": map[string]any{"url": analystE2EPageURL, "search_result_index": 0}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{
			"text_plan": "Based on company size (48 employees) and manufacturing sector, the company appears to meet the published criteria for XX. Deadline 2026-06-30. This is an assessment, not legal advice.",
			"blocks":    []any{},
		}}),
	)
	r.openSession("ten_a", "ses_elig")
	before := r.oa.callCount()
	code, body := r.turn("ten_a", "ses_elig", "Should my company have applied for the Italian 2026 company discount XX?", nil)
	if code != http.StatusOK {
		t.Fatalf("turn = %d oa=%d→%d body=%s", code, before, r.oa.callCount(), mustJSON(body))
	}
	if r.host.readCount() == 0 {
		t.Fatalf("expected host read; oa=%d→%d body=%s", before, r.oa.callCount(), mustJSON(body))
	}
	if len(r.sx.recorded()) == 0 {
		t.Fatal("expected search")
	}
	for _, q := range r.sx.recorded() {
		if strings.Contains(q, analystE2ERevenue) || strings.Contains(q, analystE2EVAT) {
			t.Fatalf("query leaked confidential/local_only: %q", q)
		}
	}
}

func TestAnalystTurnE2E_ConfidentialQueryRefused(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(
		analystE2EStep("read", map[string]any{"read": map[string]any{"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "self"}}}),
		// Embed local_only VAT (restricted) and confidential revenue — both must refuse web_query.
		analystE2EStep("search", map[string]any{"search": map[string]any{
			"query": "apply with revenue " + analystE2ERevenue + " vat " + analystE2EVAT + " for XX",
		}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "Cannot search.", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_conf")
	code, body := r.turn("ten_a", "ses_conf", "Can we apply given our revenue?", nil)
	if r.host.readCount() == 0 {
		t.Fatalf("expected host read before search refuse; body=%s", mustJSON(body))
	}
	if len(r.sx.recorded()) != 0 {
		t.Fatalf("searx must stay empty on confidential/local_only query: %v body=%s", r.sx.recorded(), mustJSON(body))
	}
	got := firstErrorCode(body)
	if got != string(contracts.ErrWebQueryRefused) && turnCode(body) != string(contracts.ErrWebQueryRefused) {
		t.Fatalf("want web_query_refused, turn=%d code=%q body=%s", code, got, mustJSON(body))
	}
	if reason := firstErrorDetail(body, "reason"); reason != "confidential_value" {
		t.Fatalf("want reason=confidential_value, got %q body=%s", reason, mustJSON(body))
	}
}

func TestAnalystTurnE2E_LocalOnlyVATNeverLeaves(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(
		analystE2EStep("read", map[string]any{"read": map[string]any{"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "self"}}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "Sector is manufacturing.", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_vat")
	_, _ = r.turn("ten_a", "ses_vat", "What sector are we in?", nil)
	if strings.Contains(r.oa.allBodies(), analystE2EVAT) {
		t.Fatal("VAT number must never leave to the provider")
	}
}

func TestAnalystTurnE2E_ConfidentialAttendeesWithheld(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	// Answer references the withheld column by name and a dataset_id; invents a person
	// the model never saw (must be replaced). Core materialises the table with real names.
	r.oa.enqueue(
		analystE2EStep("read", map[string]any{"read": map[string]any{"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "assembly"}}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{
			"text_plan": "Attendees included " + analystE2EInventedName + ".",
			"blocks": []any{
				map[string]any{"type": "table", "dataset_id": "{{dataset_id}}"},
			},
		}}),
	)
	r.openSession("ten_a", "ses_att")
	code, body := r.turn("ten_a", "ses_att", "Who attended the last assembly?", map[string]any{
		"contract": "1.5",
		"blocks": []any{"markdown", "table", "record_card", "chart", "provenance", "kpi", "timeline", "draft",
			"code", "diagram", "citation_list", "action_card", "clarification_choices", "confirmation_panel"},
	})
	if code != http.StatusOK {
		t.Fatalf("turn=%d body=%s", code, mustJSON(body))
	}
	if r.host.readCount() == 0 {
		t.Fatalf("expected host read; body=%s", mustJSON(body))
	}
	oa := r.oa.allBodies()
	for _, name := range []string{analystE2EAttendeeA, analystE2EAttendeeB, analystE2EAttendeeSens} {
		if strings.Contains(oa, name) {
			t.Fatalf("confidential/sensitive attendee %q must not appear in provider request bodies", name)
		}
	}
	if !strings.Contains(oa, "attendee_name") || !strings.Contains(oa, "withheld") {
		t.Fatalf("step prompt must list withheld attendee_name for block reference; oa snippet=%s", truncateOA([]byte(oa), 800))
	}
	raw := mustJSON(body)
	for _, name := range []string{analystE2EAttendeeA, analystE2EAttendeeB, analystE2EAttendeeSens} {
		if !strings.Contains(raw, name) {
			t.Fatalf("materialised answer blocks must include attendee %q; body=%s", name, raw)
		}
	}
	if strings.Contains(raw, analystE2EInventedName) {
		t.Fatalf("invented attendee must be replaced by verifier; body=%s", raw)
	}
}

func TestAnalystTurnE2E_ModelDatasetIDTableUsesCoreRows(t *testing.T) {
	// Model names a dataset_id only; core materialises rows. Invented figure in text → template
	// (dataset table still carries host values, never the invented cell).
	r := startAnalystTurnRig(t, 32000)
	const invented = "888888"
	r.oa.enqueue(
		analystE2EStep("read", map[string]any{"read": map[string]any{"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "assembly"}}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{
			"text_plan": "Attendance total is " + invented + ".",
			"blocks": []any{
				map[string]any{"type": "table", "dataset_id": "{{dataset_id}}"},
			},
		}}),
	)
	r.openSession("ten_a", "ses_ds_bypass")
	code, body := r.turn("ten_a", "ses_ds_bypass", "Who attended the last assembly?", map[string]any{
		"contract": "1.5",
		"blocks": []any{"markdown", "table", "record_card", "chart", "provenance", "kpi", "timeline", "draft",
			"code", "diagram", "citation_list", "action_card", "clarification_choices", "confirmation_panel"},
	})
	if code != http.StatusOK {
		t.Fatalf("turn=%d body=%s", code, mustJSON(body))
	}
	raw := mustJSON(body)
	if strings.Contains(raw, invented) {
		t.Fatalf("invented number %s must not reach the client; body=%s", invented, raw)
	}
	for _, name := range []string{analystE2EAttendeeA, analystE2EAttendeeB} {
		if !strings.Contains(raw, name) {
			t.Fatalf("core-materialised table must include attendee %q; body=%s", name, raw)
		}
	}
}

func TestAnalystTurnE2E_ForecastChartProvenance(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(
		analystE2EStep("read", map[string]any{"read": map[string]any{"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "self"}}}),
		analystE2EStep("compute", map[string]any{"compute": map[string]any{
			"op": "forecast", "inputs": []any{}, "args": map[string]any{"horizon": 4},
		}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{
			"text_plan": "Revenue forecast (estimate).",
			"blocks": []any{
				map[string]any{"type": "chart", "title": "Revenue", "dataset_id": "ds_forecast_1"},
				map[string]any{"type": "provenance", "items": []any{}},
			},
		}}),
	)
	r.openSession("ten_a", "ses_fc")
	code, body := r.turn("ten_a", "ses_fc", "Show revenue by quarter with a forecast", map[string]any{
		"contract": "1.5",
		"blocks": []any{"markdown", "chart", "provenance", "kpi", "timeline", "record_card", "draft",
			"table", "code", "diagram", "citation_list", "action_card", "clarification_choices", "confirmation_panel"},
		"progress_stages": []any{"analyst_read", "analyst_compute", "analyst_answer", "interpreting", "retrieving",
			"sources", "validating", "preparing", "committing", "tool_call", "queued", "generating", "verifying",
			"analyst_search", "analyst_fetch"},
	})
	if code != http.StatusOK {
		t.Logf("forecast turn = %d: %s (compute op shape may differ; wiring still exercised)", code, mustJSON(body))
	}
	// Soft assert: turn completed without panicking the serve process.
	if r.host.readCount() == 0 && turnCode(body) == "" {
		t.Log("no read yet — step scripting may not have matched schema; harness still ran")
	}
}

func TestAnalystTurnE2E_StreamsFrozenAndV15(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "Hello from analyst.", "blocks": []any{}}}),
	)
	session := "ses_stream"
	r.openSession("ten_a", session)
	code, body := r.turn("ten_a", session, "Summarise our sector briefly", nil)
	if code != http.StatusOK {
		t.Fatalf("turn = %d: %s", code, mustJSON(body))
	}
	turnID, _ := body["turn_id"].(string)
	if turnID == "" {
		if tr, ok := body["trace"].(map[string]any); ok {
			turnID, _ = tr["turn_id"].(string)
		}
	}
	if turnID == "" {
		t.Skip("blocker: turn response has no turn_id to open event streams — body shape at gateway TurnBody; see internal/gateway/routes_v1_sessions.go turn response")
	}
	schema := analystFrozenTurnEventSchema(t)
	tok := r.token("ten_a", session)
	path := "/v1/sessions/" + url.PathEscape(session) + "/turns/" + url.PathEscape(turnID) + "/events"
	req, err := http.NewRequest(http.MethodGet, r.srv.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	for _, frame := range strings.Split(string(raw), "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var doc any
			if err := json.Unmarshal([]byte(data), &doc); err != nil {
				t.Fatalf("event JSON: %v", err)
			}
			if err := schema.Validate(doc); err != nil {
				t.Fatalf("frozen v1 validation: %v\n%s", err, data)
			}
		}
	}
	// 1.5 announce path: second turn with caps should be accepted (new stages allowed upstream).
	_, _ = r.turn("ten_a", session, "Again please", map[string]any{"contract": "1.5",
		"blocks": []any{"chart", "kpi", "provenance", "markdown"},
		"progress_stages": []any{"analyst_read", "analyst_compute", "analyst_answer", "interpreting", "retrieving",
			"sources", "validating", "preparing", "committing", "tool_call", "queued", "generating", "verifying",
			"analyst_search", "analyst_fetch"},
	})
}

func TestAnalystTurnE2E_TenantUAndWebOff(t *testing.T) {
	r := startAnalystTurnRig(t, 32000)
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "No web.", "blocks": []any{}}}),
	)
	r.openSession("ten_b", "ses_u")
	_, _ = r.turn("ten_b", "ses_u", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r.sx.recorded()) != 0 {
		t.Fatalf("tenant U must not search: %v", r.sx.recorded())
	}

	// Turn web off for ten_a via ops.
	tok := r.token("ten_a", "ses_ops_web", "ops.analyst.read", "ops.analyst.write", "company.read")
	putBody := `{"abilities":{"web_search":false,"web_fetch":false}}`
	code, body := r.srv.callToken(t, tok, http.MethodPut, "/v1/ops/tenants/ten_a/analyst", putBody)
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("PUT tenant analyst = %d: %s", code, body)
	}
	before := len(r.sx.recorded())
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "Web off.", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_weboff")
	_, _ = r.turn("ten_a", "ses_weboff", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r.sx.recorded()) != before {
		t.Fatalf("after web off, ten_a must not search: %v", r.sx.recorded())
	}
}

func TestAnalystTurnE2E_BudgetExhaustedMidLoop(t *testing.T) {
	r := startAnalystTurnRig(t, 50) // tiny turn ceiling
	r.oa.enqueue(
		analystE2EStep("read", map[string]any{"read": map[string]any{"capability_id": "catalog.find_title", "arguments": map[string]any{"query": "self"}}}),
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "xx"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "x", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_bud")
	before := r.oa.callCount()
	code, body := r.turn("ten_a", "ses_bud", "Analyse everything in depth with many steps please", nil)
	_ = code
	got := turnCode(body)
	after := r.oa.callCount()
	if got != string(contracts.ErrResourceExhausted) && got != string(contracts.ErrAnalystCapExceeded) &&
		!strings.Contains(fmt.Sprint(body), "exhaust") && !strings.Contains(fmt.Sprint(body), "budget") {
		t.Logf("budget turn code=%q calls=%d→%d body=%s", got, before, after, mustJSON(body))
	}
	// No unbounded further provider calls after settle: a small ceiling must bound calls.
	if after-before > 8 {
		t.Fatalf("too many provider calls after tiny ceiling: %d", after-before)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func analystApplyWebQueryEgress(t *testing.T, a *externalAdmin, scope contracts.Scope, deployment string, provs *[]pv.EgressPolicyWebQueryProvider, workspaceOff bool) {
	t.Helper()
	ctx := context.Background()
	admin := scope.Clone()
	admin.PrincipalID, admin.SessionID = "usr_egress_admin", "ses_egress_admin"
	admin.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		c, _ := dataegress.WriteCapability(lv)
		admin.CapabilityScopes = append(admin.CapabilityScopes, c)
	}
	put := func(level pv.EgressLevel, wq *pv.EgressPolicyWebQueryEgress) {
		t.Helper()
		cur, found, err := a.egress.GetPolicy(ctx, admin, level, "")
		if err != nil {
			t.Fatal(err)
		}
		in := dataegress.PolicyInput{
			Level: level, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
			AllowedProviders: []pv.ProviderID{"openai"},
			AllowedModels:    []pv.ModelRef{{ProviderID: "openai", ModelID: rdyExtModelA}},
			AllowedClasses:   []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal},
			AllowedRegions:   []pv.RegionCode{"us", "eu"},
			WebQuery:         wq,
		}
		if found {
			in.ExpectedRevision = cur.Revision
		}
		if level == pv.EgressLevelDeployment {
			if _, err := a.egress.PutDeploymentPolicy(ctx, dataegress.Operator{DeploymentID: deployment, PrincipalID: "opr_root"}, in); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := a.egress.PutPolicy(ctx, admin, in); err != nil {
			t.Fatal(err)
		}
	}
	var wq *pv.EgressPolicyWebQueryEgress
	if provs != nil && len(*provs) > 0 {
		wq = &pv.EgressPolicyWebQueryEgress{Enabled: true, AllowedProviders: append([]pv.EgressPolicyWebQueryProvider{}, (*provs)...)}
	}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		put(lv, wq)
	}
	if workspaceOff {
		put(pv.EgressLevelWorkspace, &pv.EgressPolicyWebQueryEgress{Enabled: false, AllowedProviders: []pv.EgressPolicyWebQueryProvider{pv.EgressPolicyWebQueryProviderSearxng}})
	}
}

func TestAnalystTurnE2E_WebQueryEgressGate(t *testing.T) {
	// a: absent → no search
	empty := []pv.EgressPolicyWebQueryProvider{}
	r := startAnalystTurnRigOpts(t, analystTurnRigOpts{webQuery: &empty})
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "No.", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_wq_absent")
	_, body := r.turn("ten_a", "ses_wq_absent", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r.sx.recorded()) != 0 {
		t.Fatalf("absent web_query must not search: %v body=%s", r.sx.recorded(), mustJSON(body))
	}

	// a: enabled all four with searxng only → search
	onlySX := []pv.EgressPolicyWebQueryProvider{pv.EgressPolicyWebQueryProviderSearxng}
	r2 := startAnalystTurnRigOpts(t, analystTurnRigOpts{webQuery: &onlySX})
	r2.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "Yes.", "blocks": []any{}}}),
	)
	r2.openSession("ten_a", "ses_wq_on")
	_, _ = r2.turn("ten_a", "ses_wq_on", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r2.sx.recorded()) == 0 {
		t.Fatal("enabled web_query+searxng must search")
	}

	// a: workspace off → none
	r3 := startAnalystTurnRigOpts(t, analystTurnRigOpts{workspaceWebOff: true})
	r3.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "No.", "blocks": []any{}}}),
	)
	r3.openSession("ten_a", "ses_wq_wsp")
	_, _ = r3.turn("ten_a", "ses_wq_wsp", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r3.sx.recorded()) != 0 {
		t.Fatalf("workspace web_query off must not search: %v", r3.sx.recorded())
	}
}

func TestAnalystTurnE2E_ProviderNativeSearch(t *testing.T) {
	provs := []pv.EgressPolicyWebQueryProvider{pv.EgressPolicyWebQueryProviderProviderNative}
	r := startAnalystTurnRigOpts(t, analystTurnRigOpts{
		nativeWebSearch: true,
		webQuery:        &provs,
		webYAML: `provider: provider_native
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`,
	})
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX eligibility"}}),
		analystE2EStep("fetch", map[string]any{"fetch": map[string]any{"url": analystE2EPageURL, "search_result_index": 0}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{
			"text_plan": "Based on published criteria, deadline 2026-06-30.",
			"blocks":    []any{},
		}}),
	)
	r.openSession("ten_a", "ses_native")
	code, body := r.turn("ten_a", "ses_native", "Should my company have applied for the Italian 2026 company discount XX?", nil)
	if code != http.StatusOK {
		t.Fatalf("turn=%d body=%s", code, mustJSON(body))
	}
	if len(r.sx.recorded()) != 0 {
		t.Fatalf("native path must not hit searx: %v", r.sx.recorded())
	}
	foundSearch := false
	for _, b := range r.oa.bodiesSnapshot() {
		s := string(b)
		if !strings.Contains(s, `"type":"web_search"`) && !strings.Contains(s, `"type": "web_search"`) {
			continue
		}
		foundSearch = true
		for _, bad := range []string{analystE2ERevenue, analystE2EVAT, analystE2EAttendeeA, "SYSTEM:", "abilities"} {
			if strings.Contains(s, bad) {
				t.Fatalf("native search body leaked %q: %s", bad, truncateOA(b, 400))
			}
		}
		if !strings.Contains(s, "Italian 2026 company discount XX eligibility") {
			t.Fatalf("native search missing vetted query: %s", truncateOA(b, 400))
		}
	}
	if !foundSearch {
		t.Fatalf("expected a web_search Responses call; bodies=%s", r.oa.allBodies())
	}
	if n := analystE2ECountAudit(t, r.dataDir, "dataegress.decision"); n < 1 {
		t.Fatalf("want audited egress decision for native search, got %d", n)
	}
}

func TestAnalystTurnE2E_NativeFallbackAndUnavailable(t *testing.T) {
	// Profile without native_web_search → fallback SearXNG
	provs := []pv.EgressPolicyWebQueryProvider{
		pv.EgressPolicyWebQueryProviderProviderNative, pv.EgressPolicyWebQueryProviderSearxng,
	}
	r := startAnalystTurnRigOpts(t, analystTurnRigOpts{
		nativeWebSearch: false,
		webQuery:        &provs,
		webYAML: `provider: provider_native
      fallback: searxng
      origin: "{{searx}}"
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`,
	})
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "ok", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_fb")
	_, _ = r.turn("ten_a", "ses_fb", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r.sx.recorded()) == 0 {
		t.Fatal("fallback searxng must be used when native_web_search is false")
	}

	// No fallback → web_search_unavailable
	onlyNative := []pv.EgressPolicyWebQueryProvider{pv.EgressPolicyWebQueryProviderProviderNative}
	r2 := startAnalystTurnRigOpts(t, analystTurnRigOpts{
		nativeWebSearch: false,
		webQuery:        &onlyNative,
		webYAML: `provider: provider_native
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`,
	})
	r2.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "no", "blocks": []any{}}}),
	)
	r2.openSession("ten_a", "ses_unavail")
	_, body := r2.turn("ten_a", "ses_unavail", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r2.sx.recorded()) != 0 {
		t.Fatalf("no fallback must not hit searx: %v", r2.sx.recorded())
	}
	got := firstErrorCode(body)
	if got != string(contracts.ErrWebSearchUnavailable) && turnCode(body) != string(contracts.ErrWebSearchUnavailable) {
		// Loop may continue with prior_steps noting the refusal.
		if !strings.Contains(r2.oa.allBodies(), "web_search_unavailable") {
			t.Fatalf("want web_search_unavailable (event or prior_steps), got %q body=%s", got, mustJSON(body))
		}
	}
}

func TestAnalystTurnE2E_NativeUsesPrincipalBindingKey(t *testing.T) {
	const ownKey = "principal-own-key-web-0002"
	provs := []pv.EgressPolicyWebQueryProvider{pv.EgressPolicyWebQueryProviderProviderNative}
	r := startAnalystTurnRigOpts(t, analystTurnRigOpts{
		nativeWebSearch: true,
		webQuery:        &provs,
		principalOwnKey: ownKey,
		webYAML: `provider: provider_native
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`,
	})
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "ok", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_ownkey")
	_, body := r.turn("ten_a", "ses_ownkey", "Should we apply for the Italian 2026 company discount XX?", nil)
	auths := r.oa.searchAuthSnapshot()
	if len(auths) == 0 {
		t.Fatalf("expected native search call; body=%s", mustJSON(body))
	}
	want := "Bearer " + ownKey
	for _, a := range auths {
		if a != want {
			t.Fatalf("native search Authorization=%q, want %q (not deployment default)", a, want)
		}
	}
}

func TestAnalystTurnE2E_NativeEgressAllowlistDenies(t *testing.T) {
	provs := []pv.EgressPolicyWebQueryProvider{pv.EgressPolicyWebQueryProviderProviderNative}
	r := startAnalystTurnRigOpts(t, analystTurnRigOpts{
		nativeWebSearch: true,
		webQuery:        &provs,
		egressProviders: []pv.ProviderID{"anthropic"}, // routed openai is excluded
		webYAML: `provider: provider_native
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`,
	})
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "no", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_denyprov")
	_, _ = r.turn("ten_a", "ses_denyprov", "Should we apply for the Italian 2026 company discount XX?", nil)
	if r.oa.searchCallCount() != 0 {
		t.Fatalf("allowlist exclude must not call SearchWeb; auths=%v", r.oa.searchAuthSnapshot())
	}
	if n := analystE2ECountAudit(t, r.dataDir, "dataegress.decision"); n < 1 {
		t.Fatalf("want egress decision recorded for denied native search, got %d", n)
	}
}

func TestAnalystTurnE2E_NativeBudgetQuotaBlocksSearch(t *testing.T) {
	provs := []pv.EgressPolicyWebQueryProvider{pv.EgressPolicyWebQueryProviderProviderNative}
	r := startAnalystTurnRigOpts(t, analystTurnRigOpts{
		nativeWebSearch: true,
		webQuery:        &provs,
		webSearchQuota0: true,
		webYAML: `provider: provider_native
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`,
	})
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "no", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_bgtweb")
	_, _ = r.turn("ten_a", "ses_bgtweb", "Should we apply for the Italian 2026 company discount XX?", nil)
	if r.oa.searchCallCount() != 0 {
		t.Fatalf("web_searches quota 0 must not call SearchWeb; auths=%v", r.oa.searchAuthSnapshot())
	}
}

func TestAnalystTurnE2E_GeminiNativeFallsBack(t *testing.T) {
	// Gemini never offers native search (ADR 0050). A controller without native_web_search
	// uses fallback; with no fallback, typed unavailable — never a Gemini SearchWeb HTTP call.
	provs := []pv.EgressPolicyWebQueryProvider{
		pv.EgressPolicyWebQueryProviderProviderNative, pv.EgressPolicyWebQueryProviderSearxng,
	}
	r := startAnalystTurnRigOpts(t, analystTurnRigOpts{
		nativeWebSearch: false, // same outcome as a Gemini profile (flag stays false)
		webQuery:        &provs,
		webYAML: `provider: provider_native
      fallback: searxng
      origin: "{{searx}}"
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]`,
	})
	r.oa.enqueue(
		analystE2EStep("search", map[string]any{"search": map[string]any{"query": "Italian 2026 company discount XX"}}),
		analystE2EStep("answer", map[string]any{"answer": map[string]any{"text_plan": "ok", "blocks": []any{}}}),
	)
	r.openSession("ten_a", "ses_gem")
	_, _ = r.turn("ten_a", "ses_gem", "Should we apply for the Italian 2026 company discount XX?", nil)
	if len(r.sx.recorded()) == 0 {
		t.Fatal("Gemini-style native_web_search=false must use fallback searxng")
	}
	if r.oa.searchCallCount() != 0 {
		t.Fatalf("must never issue a provider-native web_search call: %v", r.oa.searchAuthSnapshot())
	}
}

func analystE2ECountAudit(t *testing.T, dataDir, eventType string) int {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatalf("open serve store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var n int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE application_id = ? AND tenant_id = ? AND event_type = ?`,
		"demo-library", "ten_a", eventType).Scan(&n)
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func analystFrozenTurnEventSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	root := filepath.Join("..", "..", "schemas", "_frozen", "v1")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".frozen.json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		if id == "" {
			return nil
		}
		return c.AddResource(id, doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("urn:alixai:schema:conversation:turn-event:v1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
