package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The AG-UI area registers a serve provider that builds the run index on the process clock and gives it to the registry
// by its own type, once.
func TestAguiProviderGivesTheRegistryTheRunIndex(t *testing.T) {
	if _, byName := registeredServeProviders(); byName["agui"] == nil {
		t.Fatal("the agui provider did not register itself")
	}
	svc := gateway.NewServices()
	deps := serveDeps{
		Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Services: svc,
		Clock: clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)),
	}
	if err := aguiProvide(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.Lookup[*gateway.AGUIRuns](svc); !ok {
		t.Fatal("the run index is not in the registry under *gateway.AGUIRuns")
	}
	// The UI tool allow-list the engine is built with is the one the gateway reads for the capabilities document.
	if reg, ok := gateway.Lookup[dialogue.UIToolRegistry](svc); !ok || reg == nil {
		t.Fatal("the UI tool allow-list is not in the registry under dialogue.UIToolRegistry")
	}
	if err := aguiProvide(context.Background(), deps); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a second run index for one registry = %v, want %s", err, contracts.ErrConflict)
	}
}

// serve mounts the AG-UI routes beside the others, behind the identity middleware: a request with no token is refused
// first, and one with a token reaches the handler, which asks whether the application is active before anything else.
func TestAguiServeMountsTheAGUIRoutes(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	cfg, _ := serveConfig(t, f, identityYAML)
	provisionIssuer(t, cfg, iss)
	srv := startServe(t, cfg)

	thread := identitytest.DefaultSession
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/agui", `{"threadId":"` + thread + `","runId":"r1","messages":[]}`},
		{http.MethodGet, "/v1/agui/capabilities", ""},
		{http.MethodGet, "/v1/agui/threads/" + thread + "/runs/r1/events", ""},
		{http.MethodPost, "/v1/agui/threads/" + thread + "/runs/r1/cancel", ""},
	} {
		if code, body := srv.call(t, nil, rt.method, rt.path, rt.body); code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d: %s", rt.method, rt.path, code, body)
		}
		code, body := srv.call(t, iss, rt.method, rt.path, rt.body)
		var env contracts.Error
		if err := json.Unmarshal(body, &env); err != nil || code != http.StatusForbidden || env.Code != contracts.ErrQuarantined {
			t.Fatalf("%s %s for an application with no package = %d: %s (a 404 would mean the route is not mounted)", rt.method, rt.path, code, strings.TrimSpace(string(body)))
		}
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
}

// aguiLoadSection loads a YAML document into a fresh registry holding only an agui section, and returns the section.
func aguiLoadSection(t *testing.T, doc string) (aguiConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alixai.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var section aguiConfig
	reg := config.NewRegistry()
	reg.Register(aguiSectionName, &section)
	err := reg.LoadFrom(path, func(string) (string, bool) { return "", false })
	return section, err
}

// A UI tool exists only when the host registered it: with nothing configured (the default) no application has one, the
// section registers names per application and for no other, and a name that could never be a UI tool stops the load.
func TestAguiUIToolsAreRegisteredOnlyByTheConfiguredSection(t *testing.T) {
	ctx := context.Background()
	if names, err := aguiUITools.UITools(ctx, "app_demo"); err != nil || len(names) != 0 {
		t.Fatalf("the default registry has tools for an application: %v, %v", names, err)
	}

	section, err := aguiLoadSection(t, "agui:\n  ui_tools:\n    app_demo: [highlightRow, openPanel]\n    app_two: [scrollTo]\n")
	if err != nil {
		t.Fatal(err)
	}
	reg := aguiUIToolRegistry{section: &section}
	for app, want := range map[string][]string{"app_demo": {"highlightRow", "openPanel"}, "app_two": {"scrollTo"}, "app_other": nil, "": nil} {
		if got, err := reg.UITools(ctx, app); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("UITools(%q) = %v, %v; want %v", app, got, err, want)
		}
	}
	// The caller cannot reach into the registered list.
	got, _ := reg.UITools(ctx, "app_demo")
	got[0] = "tampered"
	if again, _ := reg.UITools(ctx, "app_demo"); again[0] != "highlightRow" {
		t.Errorf("a caller edited the registered list: %v", again)
	}
	if names, err := (aguiUIToolRegistry{}).UITools(ctx, "app_demo"); err != nil || len(names) != 0 {
		t.Errorf("a registry with no section has tools: %v, %v", names, err)
	}

	for name, doc := range map[string]string{
		"a reserved name":         "agui:\n  ui_tools:\n    app_demo: [alixai_confirm_action]\n",
		"a reserved name in case": "agui:\n  ui_tools:\n    app_demo: [AliXAI.panel]\n",
		"a malformed name":        "agui:\n  ui_tools:\n    app_demo: [\"open panel\"]\n",
		"an empty name":           "agui:\n  ui_tools:\n    app_demo: [\"\"]\n",
		"a name that is too long": "agui:\n  ui_tools:\n    app_demo: [" + strings.Repeat("a", 65) + "]\n",
		"a duplicate":             "agui:\n  ui_tools:\n    app_demo: [highlightRow, highlightRow]\n",
		"an unknown member":       "agui:\n  ui_tools:\n    app_demo: [highlightRow]\n  allow_all: true\n",
	} {
		if _, err := aguiLoadSection(t, doc); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The engine is built with the allow-list, unless the composition gave it one of its own, and the hook is installed on the
// builder wireV1 calls.
func TestAguiEngineIsBuiltWithTheUIToolAllowList(t *testing.T) {
	var seen dialogue.Deps
	stub := func(d dialogue.Deps) (*dialogue.Engine, error) { seen = d; return nil, nil }
	own := dialogue.StaticUITools(map[string][]string{"app_demo": {"own"}})
	reg := dialogue.StaticUITools(map[string][]string{"app_demo": {"highlightRow"}})

	if _, err := aguiEngineWithUITools(stub, reg)(dialogue.Deps{}); err != nil || seen.UITools == nil {
		t.Fatalf("the engine was built with no allow-list: %v", err)
	}
	if names, _ := seen.UITools.UITools(context.Background(), "app_demo"); !reflect.DeepEqual(names, []string{"highlightRow"}) {
		t.Errorf("the engine's allow-list = %v", names)
	}
	if _, err := aguiEngineWithUITools(stub, reg)(dialogue.Deps{UITools: own}); err != nil {
		t.Fatal(err)
	}
	if names, _ := seen.UITools.UITools(context.Background(), "app_demo"); !reflect.DeepEqual(names, []string{"own"}) {
		t.Errorf("the composition's own allow-list was replaced: %v", names)
	}
	if reflect.ValueOf(serveProfileNewEngine).Pointer() == reflect.ValueOf(dialogue.New).Pointer() {
		t.Error("serve builds its engine with dialogue.New: no UI tool allow-list is ever set")
	}
}
