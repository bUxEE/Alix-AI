package degradation

import (
	"testing"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Section 18, "LLM unavailable": the application and search remain usable; return a clear local failure; never
// execute from partial model output.
func TestDegradeLLMUnavailable(t *testing.T) {
	if providers.RoleController.Effect() != providers.EffectLLMUnavailable || !providers.RoleController.Required() {
		t.Fatalf("the controller's effect is %q", providers.RoleController.Effect())
	}

	t.Run("control: a healthy model prepares the action", func(t *testing.T) {
		w := newWorld(t)
		res := w.prepareCreate()
		if res.Code != "" || res.Prepared == nil {
			t.Fatalf("act %s code %q", res.Directive.Act, res.Code)
		}
		if w.hostCalls(adapters.OpPrepare) != 1 {
			t.Fatalf("host ops %v: the control must reach the host's Prepare, or the failures below prove nothing", w.hostOps())
		}
	})

	failures := []struct {
		name string
		kind pfake.FailKind
		code contracts.ErrCode
	}{
		{"the model runtime is down", pfake.FailUnavailable, contracts.ErrUnavailable},
		{"the model times out", pfake.FailTimeout, contracts.ErrTimeout},
		{"the model cannot be loaded", pfake.FailResourceExhausted, contracts.ErrResourceExhausted},
		{"a partial stream is never acted on", pfake.FailPartialStream, contracts.ErrUnavailable},
		{"the model answers with truncated JSON", pfake.FailMalformedJSON, contracts.ErrSchemaInvalid},
	}
	for _, f := range failures {
		t.Run(f.name+": a clear local failure and nothing executed", func(t *testing.T) {
			w := newWorld(t)
			w.model.On(pfake.Any()).Fail(f.kind)
			res := w.say(sayCreate)

			if string(res.Directive.Act) != "report_failure" || res.Code != f.code {
				t.Fatalf("act %s code %q, want report_failure / %s", res.Directive.Act, res.Code, f.code)
			}
			// A clear local failure: a verified template, written by the core, not model text.
			if res.Response.Source != verify.SourceTemplate || len(res.Response.Text) < 10 {
				t.Fatalf("response %+v, want the core's template", res.Response)
			}
			// Never executed: the host saw no Prepare, no Commit, no effect; nothing was stored or kept.
			if w.hostCalls(adapters.OpPrepare) != 0 || w.hostCalls(adapters.OpCommit) != 0 || w.app.Effects() != 0 {
				t.Fatalf("the host saw %v and applied %d effects from a failed model", w.hostOps(), w.app.Effects())
			}
			if res.Prepared != nil || res.Receipt != nil {
				t.Fatalf("prepared %+v receipt %+v", res.Prepared, res.Receipt)
			}
			var n int
			if err := w.db.QueryRowContext(ctxb(), `SELECT count(*) FROM action_preparations`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%d preparations stored from a failed model (err %v)", n, err)
			}
			if _, _, present := w.goal(); present {
				t.Fatal("a failed model turn left a goal behind")
			}
		})
	}

	t.Run("the application stays usable: a prepared action is confirmed and committed with the model down", func(t *testing.T) {
		w := newWorld(t)
		prep := w.prepareCreate()
		// The model goes away after the user was asked to confirm.
		w.model.On(pfake.Any()).Fail(pfake.FailUnavailable)
		res, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}
		if res.Receipt == nil || res.Receipt.Status != act.ActionReceiptStatusCommitted || w.app.Effects() != 1 {
			t.Fatalf("receipt %+v effects %d: the structured confirmation must not need the model", res.Receipt, w.app.Effects())
		}
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("response %+v, want the template when the model is down", res.Response)
		}
	})

	t.Run("the application stays usable: a governed read needs no model", func(t *testing.T) {
		w := newWorld(t)
		w.app.Seed(tenantA, workspace, "item_1", "Alpha widget")
		w.model.On(pfake.Any()).Fail(pfake.FailUnavailable)
		out, err := w.co.Read(ctxb(), memberScope(), actions.ReadInput{Session: memberScope(), CapabilityID: capGet, Fields: map[string]any{"item_id": "item_1"}})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if out.Result.Status != act.DeterministicResultStatusOk || len(out.Result.Values) == 0 {
			t.Fatalf("result %+v", out.Result)
		}
		if n := len(w.model.Prompts()); n != 0 {
			t.Fatalf("the read reached the model %d times", n)
		}
	})

	t.Run("search stays usable: retrieval runs without any language model, rewriter included", func(t *testing.T) {
		e := newRag(t)
		a := ragScope("tenant-a")
		e.addSource(a, "Sprockets", "sprockets", sprockets())
		// The router model is registered and down: the optional query rewriter would call it.
		router := pfake.New(pfake.WithModel("test-router"))
		router.On(pfake.Any()).Fail(pfake.FailUnavailable)
		if err := e.reg.Register(providers.RoleRouter, router); err != nil {
			t.Fatal(err)
		}
		cfg := query.DefaultConfig()
		cfg.Rewrite = true
		cfg.RewriteBelowHits = 100 // always low confidence: the rewriter is asked
		d := e.deps()
		d.Rewriter = query.ModelRewriter{Registry: e.reg}
		res := e.mustQuery(e.pipeline(d, cfg), a, "sprocket drive")
		if len(res.Package.Items) == 0 {
			t.Fatal("search is not usable while the language model is down")
		}
		if router.Calls() == 0 {
			t.Fatal("the rewriter never called the down model: the test did not exercise the failure")
		}
		if res.Diagnostics.Rewrites != 0 {
			t.Errorf("%d rewrites reported from a model that failed", res.Diagnostics.Rewrites)
		}
	})
}
