package decision

import (
	"sort"
	"strconv"
	"testing"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/policy"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

const corpusPath = "../../../tests/performance/corpus/conversation-v1.jsonl"

// TestCorpusExactRuleCoverage measures, on the existing conversation corpus
// (conversation-v1), how many turns need no model at all (the core's exact rules, the router's
// refusal and the client's structured inputs), how many of the remaining turns this layer's
// exact id/title rule resolves, and what is left for a scorer. The numbers go to
// docs/efficiency/D-04.md; the test asserts only that the accounting is consistent, because a
// corpus change is meant to change them.
//
// The routing of a model turn is measured statelessly (a fresh session: no goal), the way the
// stage sees the first turn of a conversation; a later turn of a goal is the controller's
// anyway (the stage abstains while a goal is active).
func TestCorpusExactRuleCoverage(t *testing.T) {
	cases, err := conv.LoadCorpus(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	router, err := capabilities.NewRouter(w.reg, w.gw)
	if err != nil {
		t.Fatal(err)
	}

	var turns, byDecider = 0, map[string]int{}
	var modelTurns, exactTitle, single, decisive, scorerEligible, controlIntent, needsFields int
	var firstEligible, firstDropUpper int
	shortlist := map[int]int{}
	for _, c := range cases {
		sc := conv.RoleScope(c.Principal)
		for ti, tu := range c.Turns {
			turns++
			byDecider[tu.DecidedBy]++
			if tu.DecidedBy != "model" || tu.User == "" {
				continue
			}
			modelTurns++
			if tu.Expect.Typed != nil {
				switch tu.Expect.Typed.Intent {
				case "cancel", "help":
					controlIntent++
				default:
					needsFields++
				}
			}
			route, err := router.Route(ctxb(), capabilities.Request{
				Scope: sc, Binding: policy.BindingOf(sc), Utterance: tu.User, Language: c.Language,
			})
			if err != nil {
				t.Fatalf("%s: %v", c.ID, err)
			}
			shortlist[len(route.Candidates)]++
			if len(route.Candidates) == 0 {
				continue
			}
			named := make([]Named, len(route.Candidates))
			for i, cd := range route.Candidates {
				named[i] = Named{ID: cd.CapabilityID, Names: []string{cd.CapabilityID}}
				for _, e := range c.Catalogue {
					if e.ID == cd.CapabilityID {
						named[i].Names = append(named[i].Names, e.Title)
					}
				}
			}
			switch _, exact := ExactCapability(tu.User, named); {
			case exact:
				exactTitle++
			case len(route.Candidates) < 2:
				single++
			case route.Decisive:
				decisive++
				scorerEligible++
			default:
				scorerEligible++
			}
			if ti == 0 && len(route.Candidates) >= 2 {
				if _, exact := ExactCapability(tu.User, named); !exact {
					firstEligible++
					firstDropUpper += len(route.Candidates) - 1
				}
			}
		}
	}
	noModel := byDecider["rule"] + byDecider["router"] + byDecider["client"]
	if turns != noModel+byDecider["model"] {
		t.Fatalf("accounting: %d turns, %d no-model, %d model", turns, noModel, byDecider["model"])
	}
	keys := make([]int, 0, len(shortlist))
	for k := range shortlist {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	t.Logf("cases=%d turns=%d  decided by: rule=%d router=%d client=%d model=%d", len(cases), turns, byDecider["rule"], byDecider["router"], byDecider["client"], byDecider["model"])
	t.Logf("no-model turns (rules, router refusal, client input) = %d of %d (%.1f%%)", noModel, turns, 100*float64(noModel)/float64(turns))
	t.Logf("model turns with text = %d: expected intent control (cancel/help) = %d, task or correction (fields needed) = %d", modelTurns, controlIntent, needsFields)
	t.Logf("model turns by stateless shortlist size: %v", func() []string {
		var out []string
		for _, k := range keys {
			out = append(out, strconv.Itoa(k)+":"+strconv.Itoa(shortlist[k]))
		}
		return out
	}())
	t.Logf("of the model turns: exact id/title = %d, single candidate (scorer not asked) = %d, scorer-eligible = %d (of which router-decisive = %d)", exactTitle, single, scorerEligible, decisive)
	t.Logf("first turns of a conversation (no goal yet: the stage may act) that the scorer is asked about = %d; upper bound of capability descriptions a selection could remove from their proposal prompts = %d", firstEligible, firstDropUpper)
}

// TestPromptSizeNarrowedBySelection measures, on the clinic world, what a selection removes
// from the controller's proposal prompt: the same turn with the full shortlist and narrowed to
// the one selected capability. Sizes are bytes of the built prompt (providers.EstimateTokens
// counts one token per three bytes, an overestimate); they are the only effect a selection has,
// because the proposal call itself still happens.
func TestPromptSizeNarrowedBySelection(t *testing.T) {
	run := func(opts ...worldOpt) (int, int) {
		w := newWorld(t, opts...)
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		res := w.say(recep(), sayBook)
		for _, c := range res.Trace.ModelCalls {
			if c.Purpose == "proposal" {
				return c.PromptBytes, c.Capabilities
			}
		}
		t.Fatal("no proposal call")
		return 0, 0
	}
	fullBytes, fullCaps := run(withoutLayer())
	narrowBytes, narrowCaps := run(withScript(pick("availability.list", 0.97)))
	t.Logf("proposal prompt, controller only: %d bytes, %d capabilities; scorer-narrowed: %d bytes, %d capabilities (saved %d bytes, about %d estimated tokens)",
		fullBytes, fullCaps, narrowBytes, narrowCaps, fullBytes-narrowBytes, (fullBytes-narrowBytes+2)/3)
	if narrowBytes >= fullBytes || narrowCaps != 1 || fullCaps != 3 {
		t.Fatalf("a selection did not narrow the prompt: %d/%d -> %d/%d", fullBytes, fullCaps, narrowBytes, narrowCaps)
	}
}
