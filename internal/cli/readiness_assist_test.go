package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/readiness/assist"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// rdyCLIUseModel makes every run of the readiness commands use m, restoring the real source afterwards.
func rdyCLIUseModel(t *testing.T, m *assist.AssistModel) {
	t.Helper()
	old := rdyCLIModelSource
	t.Cleanup(func() { rdyCLIModelSource = old })
	rdyCLIModelSource = func(context.Context, *Env, string, string) (rdyCLIAssistSetup, error) {
		return rdyCLIAssistSetup{Config: assist.AssistConfig{Model: m, ProjectClass: assist.AssistClassLocalOnly}}, nil
	}
}

// rdyCLIUseExternal installs the external hook for one test; it is nil in production code of this package.
func rdyCLIUseExternal(t *testing.T, hook func(context.Context, *Env, string, string) (*rdyCLIExternalPick, error)) {
	t.Helper()
	old := rdyCLIExternal
	t.Cleanup(func() { rdyCLIExternal = old })
	rdyCLIExternal = hook
}

func TestReadinessWithNothingConfiguredHasNoModel(t *testing.T) {
	// nothing configured: no model at all, whatever the external hook is
	setup, err := rdyCLIBuildModel(context.Background(), (&Env{Config: config.NewRegistry()}).withDefaults(), "", "")
	if err != nil || setup.Config.Model != nil || setup.Config.AllowExternal {
		t.Fatalf("setup = %+v, err %v", setup, err)
	}
}

// An external model is refused unless it comes with the data-egress guard and the project is classified above
// local_only; the refusal sends nothing and tries nothing else.
func TestReadinessExternalModelIsRefusedWithoutItsGuard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gate  assist.AssistEgressGate
		class string
	}{
		{"no guard", nil, "internal"},
		{"project is local_only", rdyCLIGate{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen := fake.New()
			gen.On(fake.Any()).Reply(`{"proposals":[]}`)
			ext := &assist.AssistModel{Name: "hosted", ModelID: "hosted-model-0", Local: false, Gen: gen}
			rdyCLIUseExternal(t, func(context.Context, *Env, string, string) (*rdyCLIExternalPick, error) {
				return &rdyCLIExternalPick{Model: ext, Gate: tc.gate, ProjectClass: tc.class}, nil
			})
			b := rdyCLINewBox(t, "")
			r := b.mustRun("readiness", "scan", b.project, "-format", "json")
			if gen.Calls() != 0 {
				t.Errorf("a refused external model was called %d times", gen.Calls())
			}
			if !strings.Contains(r.stdout, `"status": "refused"`) || !strings.Contains(r.stderr, "the model leg did not run (refused)") || !strings.Contains(r.stderr, "permission_denied") {
				t.Errorf("the refusal is not reported:\n%s\n%s", r.stdout, r.stderr)
			}
		})
	}
}

// rdyCLIGate is a guard that lets everything out; the engine must still refuse a local_only project.
type rdyCLIGate struct{}

func (rdyCLIGate) Release(_ context.Context, req assist.AssistEgressRequest) ([]assist.AssistEgressItem, error) {
	return req.Items, nil
}

func TestReadinessLocalModelCompositionFailsClosedAndLabelsHonestly(t *testing.T) {
	t.Cleanup(func() { localaiSection, inferenceSection = localai.Config{}, inferenceConfig{} })
	b := rdyCLINewBox(t, "inference:\n  models_dir: "+strings.ReplaceAll(t.TempDir(), "\\", "/")+"\n  models: [granite-4.0-350m-q4_k_m]\n")
	b.sections = true

	// configured, but the verified model file is not in models_dir: the leg is unavailable, not "not configured"
	r := b.mustRun("readiness", "scan", b.project, "-format", "json")
	if !strings.Contains(r.stdout, `"status": "unavailable"`) || strings.Contains(r.stdout, `"not_configured"`) {
		t.Errorf("a configured model that cannot be used must report unavailable:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "the model leg did not run (unavailable)") {
		t.Errorf("stderr = %q", r.stderr)
	}
	// -model must be one of inference.models; free text is a usage error before any scan
	if r := b.run("readiness", "scan", b.project, "-model", "qwen3-8b"); r.code != ExitUsage || !strings.Contains(r.stderr, "inference.models") {
		t.Errorf("-model outside inference.models: exit %d\n%s", r.code, r.stderr)
	}
	if r := b.run("readiness", "scan", b.project, "-model", "granite-4.0-350m-q4_k_m"); r.code != ExitOK {
		t.Errorf("-model inside inference.models: exit %d\n%s", r.code, r.stderr)
	}
}

func TestReadinessQualifiedOnlyWhenTheRegistryShowsItForTheControllerRole(t *testing.T) {
	ctrl := []models.Role{models.RoleConversationalController}
	for _, tc := range []struct {
		status models.Status
		roles  []models.Role
		want   bool
	}{
		{models.StatusCandidate, ctrl, false},
		{models.StatusRejected, ctrl, false},
		{models.StatusRetired, ctrl, false},
		{models.StatusQualifiedEmulated, ctrl, true},
		{models.StatusCertified, ctrl, true},
		{models.StatusCertified, []models.Role{models.RoleEmbedding}, false},
	} {
		if got := rdyCLIQualified(models.Entry{Status: tc.status, Roles: tc.roles}); got != tc.want {
			t.Errorf("%s %v: qualified = %v, want %v", tc.status, tc.roles, got, tc.want)
		}
	}
	// every controller model of the shipped registry is a candidate or rejected today: all would be labelled unqualified
	mr, err := loadModelRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"granite-4.0-350m-q4_k_m", "qwen3-8b"} {
		if e, ok := mr.Get(id); !ok || rdyCLIQualified(e) {
			t.Errorf("%s: known %v, qualified %v: the Lite controller candidates are not qualified", id, ok, rdyCLIQualified(e))
		}
	}
}

func TestReadinessUnavailableModelFailsEveryCall(t *testing.T) {
	g := rdyCLIUnavailable{cause: context.DeadlineExceeded}
	if _, err := g.Chat(context.Background(), contracts.Scope{}, providers.ChatRequest{}); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("Chat = %v", err)
	}
	if _, err := g.ChatStream(context.Background(), contracts.Scope{}, providers.ChatRequest{}); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("ChatStream = %v", err)
	}
}
