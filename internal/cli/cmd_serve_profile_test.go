package cli

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/dialogue"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// serveProfileDocRow is one column of docs/operations/resource-budgets.md section
// 6.2 "Caps per profile". The numbers are written out here, not read from dctx:
// the document is the source of truth, and a test that computed its expectation
// from the table under test would only compare dctx with itself.
type serveProfileDocRow struct {
	name      string
	prompt    int // "Prompt, hard cap", est tokens
	output    int // "Output cap per call", generated tokens
	shortlist int // "Shortlist, default"
	utterance int // "User message, runes"
}

var serveProfileDocRows = []serveProfileDocRow{
	{name: "experimental-4g", prompt: 1536, output: 192, shortlist: 3, utterance: 600},
	{name: "lite", prompt: 2048, output: 256, shortlist: 5, utterance: 1000},
	{name: "balanced", prompt: 4096, output: 384, shortlist: 8, utterance: 2000},
	{name: "performance", prompt: 8192, output: 512, shortlist: 12, utterance: 4000},
}

var serveProfileStopped = errors.New("serve profile test: stopped once the engine was built")

// serveProfileRun starts `alixai serve` on the fixture with extraYAML appended to
// the configuration (a top-level `profile:` line, or nothing) and lets it get as
// far as building the dialogue engine. It returns what the engine was built from;
// serve itself is stopped there, before it listens.
func serveProfileRun(t *testing.T, f *fixture, extraYAML string, env func(string) (string, bool)) dialogue.Deps {
	t.Helper()
	var got dialogue.Deps
	built := false
	old := serveProfileNewEngine
	serveProfileNewEngine = func(d dialogue.Deps) (*dialogue.Engine, error) {
		got, built = d, true
		return nil, serveProfileStopped
	}
	t.Cleanup(func() { serveProfileNewEngine = old })

	cfg, _ := serveConfig(t, f, "")
	if extraYAML != "" {
		raw, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, append(raw, extraYAML...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := serveEnv(t, io.Discard, io.Discard)
	if env != nil {
		e.LookupEnv = env
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := runServe(ctx, e, []string{"-config", cfg})
	if !errors.Is(err, serveProfileStopped) || !built {
		t.Fatalf("serve did not reach the engine: err = %v", err)
	}
	return got
}

// The engine serve builds runs on the deployment profile's caps, not on the Lite
// default under every profile.
func TestServeEngineUsesTheDeploymentProfile(t *testing.T) {
	for _, row := range serveProfileDocRows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			deps := serveProfileRun(t, f, "profile: "+row.name+"\n", nil)
			p := deps.Config.Profile
			if p.Name != row.name || p.MaxPromptTokens != row.prompt || p.MaxOutputTokens != row.output ||
				p.MaxShortlist != row.shortlist || p.MaxUtteranceRunes != row.utterance {
				t.Errorf("engine profile = %v (utterance %d), want %s: prompt %d, output %d, shortlist %d, utterance %d",
					p, p.MaxUtteranceRunes, row.name, row.prompt, row.output, row.shortlist, row.utterance)
			}
			// The prompt cap the engine enforces is the one its builder derives from
			// that profile.
			b, err := dctx.New(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := b.Budget(); got.MaxPromptTokens != row.prompt || got.MaxOutputTokens != row.output {
				t.Errorf("the engine's builder budget = %d/%d, want %d/%d", got.MaxPromptTokens, got.MaxOutputTokens, row.prompt, row.output)
			}
		})
	}
}

// With no profile configured the deployment is Lite, and ALIXAI_PROFILE overrides
// the file, as every other setting does.
func TestServeProfileDefaultsToLiteAndTakesTheEnvironment(t *testing.T) {
	f := newFixture(t)
	if p := serveProfileRun(t, f, "", nil).Config.Profile; p.Name != "lite" || p.MaxPromptTokens != 2048 {
		t.Errorf("no profile configured: engine profile = %v, want lite with a 2048 prompt cap", p)
	}
	env := func(k string) (string, bool) { return "balanced", k == "ALIXAI_PROFILE" }
	if p := serveProfileRun(t, f, "profile: experimental-4g\n", env).Config.Profile; p.Name != "balanced" || p.MaxPromptTokens != 4096 {
		t.Errorf("ALIXAI_PROFILE=balanced over a file that says experimental-4g: engine profile = %v", p)
	}
}

// The runtime boundary serve gives the engine enforces the profile's prompt and
// output caps (providers.WithBudget), not providers.DefaultBudget's 4096 and 1024:
// the same numbers as the engine's, per profile.
func TestServeRuntimeBudgetComesFromTheProfile(t *testing.T) {
	scope := contracts.Scope{
		DeploymentID: "dep_profile", ApplicationID: "app_profile", TenantID: "ten_profile", WorkspaceID: "wsp_profile",
		PrincipalID: "usr_profile", Roles: []string{"member"}, CapabilityScopes: []string{"demo.create"}, SessionID: "ses_profile",
	}
	// A message of 3*(n-4) bytes estimates to n tokens with the 4 of framing, and
	// one byte more to n+1 (providers.EstimateTokens: one token per three bytes).
	promptOf := func(est int) []providers.Message {
		return []providers.Message{{Role: providers.MsgUser, Content: strings.Repeat("a", 3*(est-4))}}
	}
	for _, row := range serveProfileDocRows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
			deps := serveProfileRun(t, f, "profile: "+row.name+"\n", nil)
			ctx := context.Background()
			prov, err := deps.Providers.Select(ctx, scope, providers.RoleController)
			if err != nil {
				t.Fatal(err)
			}
			over := func(req providers.ChatRequest, budget string, limit, got int) {
				t.Helper()
				_, err := prov.Generator.Chat(ctx, scope, req)
				var ce *contracts.Error
				if !errors.As(err, &ce) || ce.Code != contracts.ErrValidationFailed ||
					ce.Details["budget"] != budget || ce.Details["limit"] != limit || ce.Details["requested"] != got {
					t.Errorf("%s over the cap: err = %v, want validation_failed on %s (%d > %d)", budget, err, budget, got, limit)
				}
			}
			before := len(f.rt.chatModels())

			// The caps themselves are allowed through to the runtime.
			if _, err := prov.Generator.Chat(ctx, scope, providers.ChatRequest{Messages: promptOf(row.prompt), MaxTokens: row.output}); err != nil {
				t.Errorf("a prompt of %d and an output of %d (the section 6.2 caps) was refused: %v", row.prompt, row.output, err)
			}
			if n := len(f.rt.chatModels()) - before; n != 1 {
				t.Errorf("the runtime saw %d chat requests for the one at the caps, want 1", n)
			}
			// One estimated token over the prompt cap, or one generated token over the
			// output cap, never reaches the runtime. The lite row is the case the old
			// default let through: 2049 is far below DefaultBudget's 4096.
			over(providers.ChatRequest{Messages: promptOf(row.prompt + 1)}, "max_prompt_tokens", row.prompt, row.prompt+1)
			over(providers.ChatRequest{Messages: promptOf(8), MaxTokens: row.output + 1}, "max_output_tokens", row.output, row.output+1)
			if n := len(f.rt.chatModels()) - before; n != 1 {
				t.Errorf("a request over a cap reached the runtime (%d chat requests, want 1)", n)
			}
		})
	}
}

// A profile name that is not one of dctx's refuses the start with a fixed reason,
// before the store is created and before the engine is built. The name that was
// given is not echoed.
func TestServeRefusesAnUnknownProfileName(t *testing.T) {
	for _, bad := range []string{"experimental", "4g", "Lite", "lite ", "tiny\\nlite", "../lite"} {
		t.Run(bad, func(t *testing.T) {
			f := newFixture(t)
			built := false
			old := serveProfileNewEngine
			serveProfileNewEngine = func(dialogue.Deps) (*dialogue.Engine, error) { built = true; return nil, serveProfileStopped }
			t.Cleanup(func() { serveProfileNewEngine = old })

			cfg, dataDir := serveConfig(t, f, "")
			raw, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg, append(raw, "profile: \""+bad+"\"\n"...), 0o600); err != nil {
				t.Fatal(err)
			}
			err, stdout, stderr := refusedServe(t, cfg)
			switch {
			case err == nil:
				t.Fatalf("serve started on profile %q\nstdout: %s\nstderr: %s", bad, stdout, stderr)
			case !strings.Contains(err.Error(), serveProfileUnknownReason):
				t.Errorf("err = %v, want the fixed reason %q", err, serveProfileUnknownReason)
			case !strings.Contains(err.Error(), "lite"):
				t.Errorf("err = %v, want it to list the profiles that exist", err)
			case strings.Contains(err.Error(), "../lite") || strings.Contains(err.Error(), "tiny"):
				t.Errorf("err = %v echoes the profile name it was given", err)
			}
			if built {
				t.Error("the engine was built although the profile was refused")
			}
			if _, statErr := os.Stat(dataDir); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the data directory exists after the refusal (stat: %v)", statErr)
			}
		})
	}
	// `alixai config check` refuses it too, through the same section validation.
	e := serveEnv(t, io.Discard, io.Discard)
	var errb strings.Builder
	e.Stderr = &errb
	cfg := writeConfig(t, "profile: tiny\n")
	if code := Default.Run(context.Background(), []string{"config", "check", "-config", cfg}, e); code != ExitError || !strings.Contains(errb.String(), "profile") {
		t.Errorf("config check: code = %d, stderr = %q; want a refusal that names the profile section", code, errb.String())
	}
}

// serveProfileResolve: every known name, the empty default, and the typed error.
func TestServeProfileResolve(t *testing.T) {
	for _, row := range serveProfileDocRows {
		p, err := serveProfileResolve(row.name)
		if err != nil || p.Name != row.name || p.MaxPromptTokens != row.prompt {
			t.Errorf("resolve(%q) = %v, %v", row.name, p, err)
		}
	}
	if p, err := serveProfileResolve(""); err != nil || p.Name != "lite" {
		t.Errorf("resolve(\"\") = %v, %v, want lite", p, err)
	}
	if _, err := serveProfileResolve("tiny"); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("resolve(tiny) code = %v, want validation_failed", contracts.CodeOf(err))
	}
	// A caller that selected nothing gets Lite, never a zero profile whose budget
	// would refuse every call.
	if p := serveProfileOrLite(dctx.Profile{}); p.Name != "lite" || p.Budget().Validate() != nil {
		t.Errorf("serveProfileOrLite(zero) = %v", p)
	}
	if p := serveProfileOrLite(dctx.Profile{Name: "x", MaxPromptTokens: 1}); p.Name != "x" {
		t.Errorf("serveProfileOrLite changed a profile that was set: %v", p)
	}
}
