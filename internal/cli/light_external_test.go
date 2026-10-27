//go:build !noexternal

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

const lightExtHintProfile = "pprf_01LIGHTCONTROLLER0000001"

type lightExtNoLocal struct{}

func (lightExtNoLocal) Usable(modes.Role) bool { return false }

// Config check accepts light setups 2 and 3, rejects an inconsistent one, and
// refuses external TTS with the roadmap message. Layer lines are printed.
func TestLightExternalConfigCheckAcceptsPresetsAndRefusesInconsistent(t *testing.T) {
	extWireSaveSection(t)
	voiceResetConfig(t)
	oldInf, oldLAI := inferenceSection, localaiSection
	t.Cleanup(func() { inferenceSection, localaiSection = oldInf, oldLAI })

	dataDir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := fmt.Sprintf("data_dir: %s\nidentity:\n  deployment_id: dep_light\n  audience: alixai\n", yamlPath(dataDir))
	ops := "external_providers:\n  enabled: true\n  deployment_operators: [\"app_demo/usr_ops\"]\n  tenant_admins: [\"app_demo/ten_acme/usr_admin\"]\n  openai_base_url: https://api.openai.com\n"

	resetSections := func() {
		externalSection = externalConfig{}
		inferenceSection = inferenceConfig{}
		localaiSection = localai.Config{}
	}

	t.Run("setup 3: everything external", func(t *testing.T) {
		resetSections()
		cfg := write("s3.yaml", base+ops+
			"  routing:\n    controller:\n      profiles: ["+lightExtHintProfile+"]\n    realisation:\n      profiles: ["+lightExtHintProfile+"]\n"+
			"inference:\n  models_dir: "+yamlPath(t.TempDir())+"\n  models: []\n")
		code, out, errOut := extWireEnvRun(t, "config", "check", "-config", cfg)
		if code != ExitOK || !strings.Contains(out, "config ok") || !strings.Contains(out, "llm: external") {
			t.Fatalf("exit %d out=%q err=%q", code, out, errOut)
		}
		if !strings.Contains(out, "stt: none") || !strings.Contains(out, "tts: none") {
			t.Fatalf("layer lines missing: %s", out)
		}
	})

	t.Run("setup 2: STT local pack + LLM external", func(t *testing.T) {
		resetSections()
		voiceUseTestManifests(t)
		voiceResetConfig(t)
		voiceEnableConfig(t, "en") // voice section lives on config.Default; layer view reads it
		cfg := write("s2.yaml", base+ops+
			"  routing:\n    controller:\n      profiles: ["+lightExtHintProfile+"]\n"+
			"localai:\n  endpoint: http://127.0.0.1:8080\n"+
			"inference:\n  models_dir: "+yamlPath(t.TempDir())+"\n  models: []\n")
		code, out, errOut := extWireEnvRun(t, "config", "check", "-config", cfg)
		if code != ExitOK || !strings.Contains(out, "llm: external") || !strings.Contains(out, "stt: local (pack en)") {
			t.Fatalf("exit %d out=%q err=%q", code, out, errOut)
		}
		if !strings.Contains(out, "tts: none") {
			t.Fatalf("STT-only pack must leave tts none: %s", out)
		}
	})

	t.Run("LocalAI without an STT pack is stt none", func(t *testing.T) {
		resetSections()
		voiceResetConfig(t)
		cfg := write("lai.yaml", base+ops+
			"  routing:\n    controller:\n      profiles: ["+lightExtHintProfile+"]\n"+
			"localai:\n  endpoint: http://127.0.0.1:8080\n"+
			"inference:\n  models_dir: "+yamlPath(t.TempDir())+"\n  models: []\n")
		code, out, errOut := extWireEnvRun(t, "config", "check", "-config", cfg)
		if code != ExitOK || !strings.Contains(out, "stt: none") || !strings.Contains(out, "tts: none") {
			t.Fatalf("exit %d out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("setup 1: local LLM with STT+TTS pack", func(t *testing.T) {
		resetSections()
		voiceLocalAIUseManifests(t)
		voiceResetConfig(t)
		voiceEnableConfig(t, "piper")
		cfg := write("s1.yaml", base+
			"localai:\n  endpoint: http://127.0.0.1:8080\n"+
			"inference:\n  models_dir: "+yamlPath(t.TempDir())+"\n  models: [ctrl-a]\n")
		code, out, errOut := extWireEnvRun(t, "config", "check", "-config", cfg)
		if code != ExitOK || !strings.Contains(out, "llm: local") ||
			!strings.Contains(out, "stt: local (pack piper)") || !strings.Contains(out, "tts: local (pack piper)") {
			t.Fatalf("exit %d out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("inconsistent: enabled, no controller route, no local models", func(t *testing.T) {
		resetSections()
		cfg := write("bad.yaml", base+ops+
			"inference:\n  models_dir: "+yamlPath(t.TempDir())+"\n  models: []\n")
		code, _, errOut := extWireEnvRun(t, "config", "check", "-config", cfg)
		if code == ExitOK || !strings.Contains(errOut, "no text model available") {
			t.Fatalf("exit %d err=%q, want refusal", code, errOut)
		}
	})

	t.Run("external TTS refused when no adapter is registered", func(t *testing.T) {
		resetSections()
		externalTTSReady.Store(false)
		t.Cleanup(func() { RegisterExternalTTSAdapter() })
		cfg := write("tts.yaml", base+ops+
			"  routing:\n    controller:\n      profiles: ["+lightExtHintProfile+"]\n    tts:\n      profiles: ["+lightExtHintProfile+"]\n"+
			"inference:\n  models_dir: "+yamlPath(t.TempDir())+"\n  models: []\n")
		code, _, errOut := extWireEnvRun(t, "config", "check", "-config", cfg)
		if code == ExitOK || !strings.Contains(errOut, "external TTS adapter is not registered") {
			t.Fatalf("exit %d err=%q, want TTS adapter refusal", code, errOut)
		}
	})

	t.Run("external TTS accepted when an adapter registers", func(t *testing.T) {
		resetSections()
		RegisterExternalTTSAdapter()
		cfg := write("tts-ok.yaml", base+ops+
			"  routing:\n    controller:\n      profiles: ["+lightExtHintProfile+"]\n    tts:\n      profiles: ["+lightExtHintProfile+"]\n"+
			"inference:\n  models_dir: "+yamlPath(t.TempDir())+"\n  models: []\n")
		code, out, errOut := extWireEnvRun(t, "config", "check", "-config", cfg)
		if code != ExitOK {
			t.Fatalf("exit %d err=%q out=%q", code, errOut, out)
		}
	})
}

// With no LocalAI and the controller routed externally, serve starts and /readyz
// is green for the text role. The serveEnv registry does not load the
// external_providers YAML section; the process-wide section (mutated here) is
// what the composition root reads, same as the other external serve tests.
func TestLightExternalServeReadyWithoutLocalAI(t *testing.T) {
	extWireSaveSection(t)
	oldInf := inferenceSection
	t.Cleanup(func() { inferenceSection = oldInf })
	externalSection = externalConfig{
		Enabled: true, DeploymentOperators: []string{"app_demo/usr_ops"},
		TenantAdmins:  []string{"app_demo/ten_acme/usr_admin"},
		OpenAIBaseURL: "https://api.openai.com",
		Routing:       map[string]externalRoute{"controller": {Profiles: []string{lightExtHintProfile}}},
	}

	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := filepath.Join(t.TempDir(), "alixai.yaml")
	doc := fmt.Sprintf(`data_dir: %s
server:
  listen: 127.0.0.1:0
identity:
  deployment_id: dep_cli
  audience: alixai
inference:
  models_dir: %s
  models: []
`, yamlPath(dataDir), yamlPath(t.TempDir()))
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := startServe(t, cfg)
	code, _, body := srv.doOnce(t, http.MethodGet, "/readyz", "")
	if code != http.StatusOK {
		t.Fatalf("/readyz = %d: %s", code, body)
	}
	var rep gateway.ReadyReport
	if err := json.Unmarshal(body, &rep); err != nil || !rep.Ready {
		t.Fatalf("ready report: %v %s", err, body)
	}
	var controller *gateway.RoleReport
	for i := range rep.Roles {
		if rep.Roles[i].Role == providers.RoleController {
			controller = &rep.Roles[i]
		}
	}
	if controller == nil || controller.Status != providers.HealthOK || controller.Reason != providers.ExternalCoverReason {
		t.Fatalf("controller role = %+v, want ok %q", controller, providers.ExternalCoverReason)
	}
	for _, c := range rep.Checks {
		if c.Name == "inference" && !c.OK {
			t.Fatalf("inference check must not fail a light external deployment: %+v", c)
		}
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
}

// A local-only deployment with empty inference.models still fails /readyz.
func TestLightExternalEmptyModelsWithoutRouteKeepsReadyzDown(t *testing.T) {
	extWireSaveSection(t)
	externalSection = externalConfig{}
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := filepath.Join(t.TempDir(), "alixai.yaml")
	doc := fmt.Sprintf(`data_dir: %s
server:
  listen: 127.0.0.1:0
identity:
  deployment_id: dep_cli
  audience: alixai
inference:
  models_dir: %s
  models: []
`, yamlPath(dataDir), yamlPath(t.TempDir()))
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := startServe(t, cfg)
	code, _, body := srv.doOnce(t, http.MethodGet, "/readyz", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503: %s", code, body)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
}

type lightExtOpenAI struct {
	srv      *httptest.Server
	mu       sync.Mutex
	paths    []string
	lastBody []byte
	lastOut  string
}

func newLightExtOpenAI(t *testing.T, fields map[string]any) *lightExtOpenAI {
	t.Helper()
	s := &lightExtOpenAI{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		model := "gpt-5-mini-2025-08-07"
		var out string
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(body, []byte(`"tools"`)) || bytes.Contains(body, []byte(`"name":"appointments_book"`)) {
			// External mode turns the proposal schema into tool calls.
			args, _ := json.Marshal(fields)
			out = fmt.Sprintf(`{"id":"resp_light","object":"response","status":"completed","model":%q,`+
				`"output":[{"type":"function_call","id":"fc_1","call_id":"call_light1","name":"appointments_book","arguments":%q}],`+
				`"usage":{"input_tokens":40,"input_tokens_details":{"cached_tokens":0},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":60}}`,
				model, string(args))
		} else {
			text, _ := json.Marshal("Your appointment is ready to confirm.")
			out = fmt.Sprintf(`{"id":"resp_light","object":"response","status":"completed","model":%q,`+
				`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":%s,"annotations":[]}]}],`+
				`"usage":{"input_tokens":40,"input_tokens_details":{"cached_tokens":0},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":60}}`,
				model, text)
		}
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.lastBody, s.lastOut = append([]byte(nil), body...), out
		s.mu.Unlock()
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *lightExtOpenAI) saw(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.paths {
		if p == path {
			return true
		}
	}
	return false
}

// Composition with no LocalAI LLM: controller routed to a fake OpenAI Responses
// server. One turn proposes a booking; confirm commits through the clinic
// adapter; egress and budget usage are recorded. Setup 2 keeps a fake LocalAI
// that answers only transcription and is never asked for an LLM route.
func TestLightExternalCompositionNoLocalAITurnAndCommit(t *testing.T) {
	extWireSaveSection(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC))

	// slot_free00002 is free on the clinic host (clk+2 days at 10:30).
	bookFields := map[string]any{"patient": "Luca Ferri", "slot": "slot_free00002"}

	var sttHits, otherLocalAI int
	localAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "transcription") || strings.Contains(r.URL.Path, "audio"):
			sttHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"hello"}`)
		default:
			otherLocalAI++
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(localAI.Close)

	for _, tc := range []struct {
		name        string
		withSTTOnly bool
	}{
		{"setup3_no_localai", false},
		{"setup2_stt_only_localai", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sttHits, otherLocalAI = 0, 0
			oa := newLightExtOpenAI(t, bookFields)
			db := storetest.Open(t, nil)
			alog, err := audit.New(db, audit.Options{Clock: clk})
			if err != nil {
				t.Fatal(err)
			}
			gw := policy.NewGateway(clk)
			host := conv.NewClinicHost(clk)
			scope := conv.RoleScope(conv.RoleReceptionist)
			sec := externalConfig{
				Enabled: true, DeploymentOperators: []string{scope.ApplicationID + "/usr_ops"},
				TenantAdmins:  []string{scope.ApplicationID + "/" + scope.TenantID + "/usr_admin"},
				OpenAIBaseURL: oa.srv.URL,
			}
			externalSection = sec
			admin, err := newExternalAdmin(externalAdminDeps{
				DB: db, Audit: alog, Clock: clk, DeploymentID: scope.DeploymentID,
				Dir: filepath.Join(t.TempDir(), "external"), Validator: externalAcceptKeys{}, Section: sec,
			})
			if err != nil {
				t.Fatal(err)
			}
			ops := scope.Clone()
			ops.PrincipalID, ops.Roles, ops.CapabilityScopes = "usr_ops", []string{"operator"}, []string{"providers.manage"}
			ops.SessionID = "ses_ops"
			for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
				if err := admin.modes.set(lv, ops, modes.ModeExternal); err != nil {
					t.Fatal(err)
				}
			}
			lightExtEgress(t, admin, ops, scope.DeploymentID)
			prof := lightExtQualify(t, admin, ops, clk)
			routeID := string(prof.ProfileID)
			sec.Routing = map[string]externalRoute{
				"controller":  {Profiles: []string{routeID}},
				"realisation": {Profiles: []string{routeID}},
			}
			externalSection = sec
			if _, err := admin.PutBudget(ctx, ops, rdyExtProviderBudget("openai", 5_000_000, 50_000_000)); err != nil {
				t.Fatal(err)
			}

			guard, err := adapters.Wrap(host.Adapter(), adapters.Options{ApplicationID: scope.ApplicationID})
			if err != nil {
				t.Fatal(err)
			}
			res, err := apppkg.Load(ctx, apppkg.FromDir(extGapClinicPackage), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
			if err != nil || res.Package == nil {
				t.Fatalf("load package: %v %v", err, res.Findings)
			}
			pkg := extGapActivePackage{capabilities.FromApppkg(res.Package)}
			capReg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
			if err := capReg.Activate(pkg); err != nil {
				t.Fatal(err)
			}
			adapterSrc := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })
			co, err := actions.New(actions.Deps{
				DB: db, Policy: gw, Audit: alog, Adapters: adapterSrc, Clock: clk,
				Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
			})
			if err != nil {
				t.Fatal(err)
			}
			sess, err := sessions.New(db, clk, sessions.Config{})
			if err != nil {
				t.Fatal(err)
			}
			budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
			localOff := localSelector{off: &inferenceOff{reason: "no model is configured for verification (inference.models)", err: fmt.Errorf("empty")}}
			pkgs := dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return pkg, nil })
			del := &extWireDelegate{local: localOff, guard: dialogue.LocalOnlyGuard{}, packages: pkgs, budget: budget}
			st, err := extWireCompose(extWireDeps{
				Admin: admin, Section: sec, Delegate: del, Local: lightExtNoLocal{},
				Audit: alog, Log: discardLog(), Clock: clk,
			})
			if err != nil {
				t.Fatal(err)
			}
			del.rt.Store(st.runtime)

			eng, err := dialogue.New(dialogue.Deps{
				Sessions: sess, Registry: capReg, Packages: pkgs, Policy: gw, Actions: co, Adapters: adapterSrc,
				Providers: del, Guard: del, Audit: alog, Clock: clk,
				Config: dialogue.Config{TemplateOnly: []conversation.DialogueDirectiveAct{conversation.DialogueDirectiveActRefuse}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sess.Open(ctx, scope); err != nil && contracts.CodeOf(err) != contracts.ErrConflict {
				t.Fatal(err)
			}
			got, err := eng.Turn(ctx, scope, dialogue.TurnInput{Text: "Book Luca Ferri for slot_free00002", Language: "en"})
			if err != nil {
				t.Fatalf("Turn: %v", err)
			}
			if got.Prepared == nil || got.Confirmation.IsZero() {
				oa.mu.Lock()
				out, req := oa.lastOut, string(oa.lastBody)
				oa.mu.Unlock()
				if len(req) > 800 {
					req = req[:800] + "..."
				}
				t.Fatalf("expected a prepared booking, act=%s code=%q resp=%q openai_hit=%v stages=%v\nout=%s\nreq=%s",
					got.Directive.Act, got.Code, got.Response.Text, oa.saw("/v1/responses"), got.Trace.Stages, out, req)
			}
			done, err := eng.Turn(ctx, scope, dialogue.TurnInput{
				Confirm: &dialogue.Confirmation{PreparationID: string(got.Prepared.PreparationID), Token: got.Confirmation},
			})
			if err != nil {
				t.Fatalf("confirm Turn: %v", err)
			}
			if done.Receipt == nil || done.Receipt.Status != actgen.ActionReceiptStatusCommitted {
				t.Fatalf("confirm did not commit: %+v code=%q", done.Receipt, done.Code)
			}
			if !oa.saw("/v1/responses") {
				t.Fatal("the fake OpenAI Responses endpoint was never called")
			}
			exp, err := admin.budget.ExportUsage(ctx, ops, providerbudget.UsageFilter{ApplicationWide: true})
			if err != nil || len(exp.Rows) == 0 {
				t.Fatalf("budget usage: rows=%d err=%v", len(exp.Rows), err)
			}
			if tc.withSTTOnly {
				_ = localAI.URL // LocalAI is reachable for STT; the text turn must not call it for an LLM.
				if otherLocalAI != 0 {
					t.Fatalf("LocalAI received %d non-transcription requests during a text turn", otherLocalAI)
				}
				if sttHits != 0 {
					t.Fatalf("unexpected STT hits during a text-only turn: %d", sttHits)
				}
			}
		})
	}
}

// With empty inference.models and a controller route, /readyz may be green, but a
// tenant still in Local or Hybrid has no text model. The same deployment works for
// a tenant whose effective mode is External; no external call is made for the others.
func TestLightExternalLocalOrHybridTenantFailsClosed(t *testing.T) {
	extWireSaveSection(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC))
	bookFields := map[string]any{"patient": "Luca Ferri", "slot": "slot_free00002"}
	oa := newLightExtOpenAI(t, bookFields)
	db := storetest.Open(t, nil)
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	gw := policy.NewGateway(clk)
	host := conv.NewClinicHost(clk)
	scope := conv.RoleScope(conv.RoleReceptionist)
	sec := externalConfig{
		Enabled: true, DeploymentOperators: []string{scope.ApplicationID + "/usr_ops"},
		TenantAdmins:  []string{scope.ApplicationID + "/" + scope.TenantID + "/usr_admin"},
		OpenAIBaseURL: oa.srv.URL,
	}
	externalSection = sec
	admin, err := newExternalAdmin(externalAdminDeps{
		DB: db, Audit: alog, Clock: clk, DeploymentID: scope.DeploymentID,
		Dir: filepath.Join(t.TempDir(), "external"), Validator: externalAcceptKeys{}, Section: sec,
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := scope.Clone()
	ops.PrincipalID, ops.Roles, ops.CapabilityScopes = "usr_ops", []string{"operator"}, []string{"providers.manage"}
	ops.SessionID = "ses_ops"
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication} {
		if err := admin.modes.set(lv, ops, modes.ModeExternal); err != nil {
			t.Fatal(err)
		}
	}
	lightExtEgress(t, admin, ops, scope.DeploymentID)
	prof := lightExtQualify(t, admin, ops, clk)
	routeID := string(prof.ProfileID)
	sec.Routing = map[string]externalRoute{
		"controller":  {Profiles: []string{routeID}},
		"realisation": {Profiles: []string{routeID}},
	}
	externalSection = sec
	if _, err := admin.PutBudget(ctx, ops, rdyExtProviderBudget("openai", 5_000_000, 50_000_000)); err != nil {
		t.Fatal(err)
	}
	guard, err := adapters.Wrap(host.Adapter(), adapters.Options{ApplicationID: scope.ApplicationID})
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(ctx, apppkg.FromDir(extGapClinicPackage), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil {
		t.Fatalf("load package: %v %v", err, res.Findings)
	}
	pkg := extGapActivePackage{capabilities.FromApppkg(res.Package)}
	capReg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
	if err := capReg.Activate(pkg); err != nil {
		t.Fatal(err)
	}
	adapterSrc := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })
	co, err := actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: alog, Adapters: adapterSrc, Clock: clk,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
	localOff := localSelector{off: &inferenceOff{reason: "no model is configured for verification (inference.models)", err: fmt.Errorf("empty")}}
	pkgs := dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return pkg, nil })
	del := &extWireDelegate{local: localOff, guard: dialogue.LocalOnlyGuard{}, packages: pkgs, budget: budget}
	st, err := extWireCompose(extWireDeps{
		Admin: admin, Section: sec, Delegate: del, Local: lightExtNoLocal{},
		Audit: alog, Log: discardLog(), Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	del.rt.Store(st.runtime)
	eng, err := dialogue.New(dialogue.Deps{
		Sessions: sess, Registry: capReg, Packages: pkgs, Policy: gw, Actions: co, Adapters: adapterSrc,
		Providers: del, Guard: del, Audit: alog, Clock: clk,
		Config: dialogue.Config{TemplateOnly: []conversation.DialogueDirectiveAct{conversation.DialogueDirectiveActRefuse}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Open(ctx, scope); err != nil && contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatal(err)
	}

	turn := func() dialogue.Result {
		t.Helper()
		got, err := eng.Turn(ctx, scope, dialogue.TurnInput{Text: "Book Luca Ferri for slot_free00002", Language: "en"})
		if err != nil {
			t.Fatalf("Turn error: %v", err)
		}
		return got
	}
	openaiHits := func() int {
		oa.mu.Lock()
		defer oa.mu.Unlock()
		n := 0
		for _, p := range oa.paths {
			if p == "/v1/responses" {
				n++
			}
		}
		return n
	}

	for _, mode := range []modes.Mode{modes.ModeLocal, modes.ModeHybrid} {
		t.Run("tenant_"+string(mode), func(t *testing.T) {
			if err := admin.modes.set(modes.LevelTenant, ops, mode); err != nil {
				t.Fatal(err)
			}
			before := openaiHits()
			got := turn()
			if got.Code != contracts.ErrUnavailable {
				t.Fatalf("code = %q act=%s prepared=%v, want unavailable", got.Code, got.Directive.Act, got.Prepared != nil)
			}
			if got.Prepared != nil {
				t.Fatal("Local/Hybrid must not prepare an action without a text model")
			}
			if openaiHits() != before {
				t.Fatal("no external provider call may leave for Local or Hybrid when inference is off")
			}
		})
	}

	t.Run("tenant_external", func(t *testing.T) {
		if err := admin.modes.set(modes.LevelTenant, ops, modes.ModeExternal); err != nil {
			t.Fatal(err)
		}
		before := openaiHits()
		got := turn()
		if got.Prepared == nil || got.Code != "" {
			t.Fatalf("External tenant must prepare: act=%s code=%q", got.Directive.Act, got.Code)
		}
		if openaiHits() <= before {
			t.Fatal("External tenant must call the routed provider")
		}
	})
}

func lightExtEgress(t *testing.T, a *externalAdmin, scope contracts.Scope, deployment string) {
	t.Helper()
	ctx := context.Background()
	admin := scope.Clone()
	admin.PrincipalID, admin.SessionID = "usr_egress_admin", "ses_egress_admin"
	admin.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		c, _ := dataegress.WriteCapability(lv)
		admin.CapabilityScopes = append(admin.CapabilityScopes, c)
	}
	base := func(level pv.EgressLevel) dataegress.PolicyInput {
		return dataegress.PolicyInput{
			Level: level, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
			AllowedProviders: []pv.ProviderID{"openai"},
			AllowedModels:    []pv.ModelRef{{ProviderID: "openai", ModelID: rdyExtModelA}},
			AllowedClasses:   []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal},
			AllowedRegions:   []pv.RegionCode{"us", "eu"},
			WebQuery: &pv.EgressPolicyWebQueryEgress{
				Enabled: true,
				AllowedProviders: []pv.EgressPolicyWebQueryProvider{
					pv.EgressPolicyWebQueryProviderSearxng,
					pv.EgressPolicyWebQueryProviderBrave,
					pv.EgressPolicyWebQueryProviderTavily,
					pv.EgressPolicyWebQueryProviderProviderNative,
				},
			},
		}
	}
	if _, err := a.egress.PutDeploymentPolicy(ctx, dataegress.Operator{DeploymentID: deployment, PrincipalID: "opr_root"}, base(pv.EgressLevelDeployment)); err != nil {
		t.Fatal(err)
	}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		if _, err := a.egress.PutPolicy(ctx, admin, base(lv)); err != nil {
			t.Fatal(err)
		}
	}
}

func lightExtQualify(t *testing.T, a *externalAdmin, scope contracts.Scope, clk *clock.Fake) pv.ProviderProfile {
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
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: "clinic-booking"}}
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
		t.Fatalf("SubmitCredential = %+v, %v", st, err)
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
		t.Fatalf("Qualify = %+v, %v", p.Lifecycle, err)
	}
	return p
}
