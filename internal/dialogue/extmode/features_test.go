package extmode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func (w *world) externalFor(sc contracts.Scope) {
	w.t.Helper()
	if err := w.modes.Set(modes.LevelTenant, sc, modes.ModeExternal); err != nil {
		w.t.Fatal(err)
	}
	w.enableEgress(sc)
}

// Provider-native agents, hosted memory, file search and server-side tool loops are off for every
// consequential operation, however the operator configured them, and for anything else they are on
// only when the operator classified them as conveniences.
func TestProviderNativeAgentFeaturesDisabledForConsequentialOps(t *testing.T) {
	all := Features{Agents: true, Memory: true, FileSearch: true, ServerToolLoop: true}
	t.Run("a proposal turn sends with none", func(t *testing.T) {
		w := newWorld(t, withFeatures(all, FeatureAgents, FeatureMemory, FeatureFileSearch, FeatureServerToolLoop))
		w.toolCall("appointments_book", `{"patient":"Luca Ferri","slot":"2026-10-14 10:30"}`)
		res := w.say(receptionist(), sayBook)
		wantAct(t, res, "request_confirmation")
		reqs := w.sender.sent()
		if len(reqs) == 0 {
			t.Fatal("nothing was sent")
		}
		for _, r := range reqs {
			if r.Features.Any() {
				t.Fatalf("a consequential call was sent with native features on: %+v", r.Features)
			}
		}
		w.noCommits()
	})
	t.Run("a response is sent with only the classified conveniences", func(t *testing.T) {
		w := newWorld(t, withModelResponses(), withDirectiveClass("internal"),
			withFeatures(all, FeatureMemory, FeatureServerToolLoop))
		w.toolCall("availability_list", `{"date":"2026-10-14"}`)
		w.respond("Here are the free slots.")
		w.say(receptionist(), sayFree)
		reqs := w.sender.sent()
		if len(reqs) != 2 {
			t.Fatalf("sends = %d, want the proposal and the response", len(reqs))
		}
		if reqs[0].Features.Any() {
			t.Fatalf("the proposal call used native features: %+v", reqs[0].Features)
		}
		got := reqs[1].Features
		if !got.Memory || got.Agents || got.FileSearch || got.ServerToolLoop {
			t.Fatalf("response features = %+v, want memory only (classified); a server-side tool loop is never on", got)
		}
	})
	t.Run("nothing is on without being classified", func(t *testing.T) {
		w := newWorld(t, withFeatures(all))
		st := &staged{}
		if f := w.rt.effectiveFeatures(st, false); f.Any() {
			t.Fatalf("features = %+v", f)
		}
		st = &staged{commit: true}
		if f := w.rt.effectiveFeatures(st, false); f.Any() {
			t.Fatalf("features = %+v", f)
		}
	})
}

// Rich provider output is data: Markdown is rewritten, raw HTML and media never become blocks, and
// a chart that states a number the application did not state is refused.
func TestProviderRichOutputBecomesValidatedBlocks(t *testing.T) {
	facts := blocks.NewFactSet(nil)
	res := providers.RichResult{Parts: []providers.ContentPart{
		providers.TextPart("**Free slots**\n\n<script>alert(1)</script>\n\n- 09:00\n- 10:30"),
		providers.ImageRef("fil_01J0000000000000000000000A", "image/png"),
		providers.TextPart(`{"type":"table","columns":[{"key":"one","label":"Time","kind":"text"}],"rows":[["09:00"]]}`),
		providers.TextPart(`{"type":"chart","title":"Bookings","description":"Bookings per clinic.","mark":"bar","data":[{"clinic":"North","n":4242}],"encoding":{"x":{"field":"clinic","type":"nominal"},"y":{"field":"n","type":"quantitative"}}}`),
		providers.TextPart(`{"type":"markdown","text":"x","onclick":"steal()"}`),
		providers.TextPart("  "),
	}}
	out, refused := RichBlocks(blocks.Default(), blocks.Context{Facts: facts}, res)

	var md, table int
	for _, b := range out {
		switch v := b.Value.(type) {
		case conversation.MarkdownBlock:
			md++
			if strings.Contains(v.Text, "<script") || strings.Contains(v.Text, "alert(1)") {
				t.Fatalf("raw HTML survived in a Markdown block: %q", v.Text)
			}
		case conversation.TableBlock:
			table++
		case conversation.ChartBlock:
			t.Fatal("a chart with a number the application never stated became a block")
		}
	}
	if md != 1 || len(out) < 1 {
		t.Fatalf("blocks = %d (markdown %d, table %d), refused = %+v", len(out), md, table, refused)
	}
	rules := map[string]bool{}
	kinds := map[providers.Modality]bool{}
	for _, r := range refused {
		rules[r.Rule] = true
		kinds[r.Kind] = true
	}
	if !rules["media_is_not_a_block"] || !kinds[providers.ModImage] {
		t.Fatalf("the image was not refused as a block: %+v", refused)
	}
	if !rules["chart.number_not_in_facts"] {
		t.Fatalf("the chart was not refused for its number: %+v", refused)
	}
	if !rules["schema"] {
		t.Fatalf("a block with an unknown member (onclick) was not refused by the schema: %+v", refused)
	}
	_ = pv.RoleVision
}

// A provider's text part that is a valid action card or confirmation panel is refused (not_model_kind): only the
// core assembles a block that sits next to a confirm button.
func TestProviderRichOutputCannotBeACoreOnlyBlock(t *testing.T) {
	var parts []providers.ContentPart
	for _, name := range []string{"action-card.json", "confirmation-panel.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "conversation", "examples", "rich-block", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, providers.TextPart(string(raw)))
	}
	parts = append(parts, providers.TextPart(`{"type":"code","code":"SELECT 1"}`))
	out, refused := RichBlocks(blocks.Default(), blocks.Context{Facts: blocks.NewFactSet(nil)}, providers.RichResult{Parts: parts})
	if len(out) != 1 {
		t.Fatalf("blocks = %+v, want the code block only", out)
	}
	if _, ok := out[0].Value.(conversation.CodeBlock); !ok {
		t.Fatalf("block = %T", out[0].Value)
	}
	if len(refused) != 2 || refused[0].Rule != string(blocks.RuleNotModelKind) || refused[1].Rule != string(blocks.RuleNotModelKind) {
		t.Fatalf("refused = %+v, want two not_model_kind", refused)
	}
}

// With external mode off nothing is initialised or contacted: the local runtime serves the turn, and
// a denied egress never falls back to another provider.
func TestLocalModeAndDeniedEgressStayLocalAndTyped(t *testing.T) {
	t.Run("local mode never reaches the external provider", func(t *testing.T) {
		w := newWorld(t, withMode(modes.ModeLocal))
		p, err := w.rt.Select(ctxb(), receptionist(), providers.RoleController)
		if err != nil || !p.Local {
			t.Fatalf("provider = %+v, %v; want the local one", p, err)
		}
		if w.ext.Calls() != 0 || len(w.sender.sent()) != 0 {
			t.Fatal("the external provider was contacted")
		}
	})
	t.Run("denied egress is permission_denied and sends nothing", func(t *testing.T) {
		w := newWorld(t, withoutEgressPolicy())
		w.toolCall("appointments_book", `{"patient":"Luca Ferri","slot":"2026-10-14 10:30"}`)
		res := w.say(receptionist(), sayBook)
		if res.Code != contracts.ErrPermissionDenied {
			t.Fatalf("code = %q (act %s), want permission_denied", res.Code, res.Directive.Act)
		}
		if w.ext.Calls() != 0 || len(w.budgets.reserved) != 0 {
			t.Fatal("something was sent or reserved after a denial")
		}
		w.noLocalCalls() // no hidden fallback to the local provider
		w.noCommits()
	})
	t.Run("the kill switch serves locally", func(t *testing.T) {
		w := newWorld(t)
		if err := w.eg.EngageKillSwitch(ctxb(), dataegress.Operator{DeploymentID: receptionist().DeploymentID, PrincipalID: "opr_root"}, "test"); err != nil {
			t.Fatalf("kill switch: %v", err)
		}
		p, err := w.rt.Select(ctxb(), receptionist(), providers.RoleController)
		if err != nil || !p.Local {
			t.Fatalf("provider = %+v, %v; want the local one under the kill switch", p, err)
		}
	})
}
