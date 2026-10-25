package dialogue

import (
	"fmt"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// longConv drives the REAL engine (router, policy gateway, session store, coordinator,
// host, verifier) through a long, scripted conversation of a demo application: items are
// created and confirmed, looked up, set aside, resumed and cancelled, so that the session
// state fills the way a long conversation fills it (completed-goal capsules, goals set
// aside, a transcript that keeps growing). Only the model is scripted. It is the
// 500-turn conversation of docs/efficiency/M3-18.md.
type longConv struct {
	t    testing.TB
	e    *env
	sc   contracts.Scope
	n    int
	last Result // the last turn that asked for a confirmation
}

// longConvPeriod is the length of one cycle of the script.
const longConvPeriod = 10

func newLongConv(t testing.TB, opts ...envOpt) *longConv {
	t.Helper()
	opts = append([]envOpt{withLevel(level2), noLimit()}, opts...)
	e := newEnv(t, opts...)
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
	prop := func(sub string, capability string, fields map[string]any) {
		e.model.On(pfake.All(pfake.Constrained(providers.ConstraintJSONSchema), pfake.LastUserContains(sub))).
			Reply(proposalJSON(capability, "task", fields))
	}
	prop("quantity 3", capCreate, map[string]any{"name": "widget", "quantity": 3})
	prop("quantity 5", capCreate, map[string]any{"name": "gizmo", "quantity": 5})
	prop("named gadget", capCreate, map[string]any{"name": "gadget"})
	prop("the quantity is 2", capCreate, map[string]any{"quantity": 2})
	prop("show me item", capGet, map[string]any{"item_id": "item_1"})
	e.onAct("request_confirmation", "Please confirm: create the item.")
	e.onAct("ask_missing", "How many would you like?")
	e.onAct("present_result", "Here is what I found. Name: Alpha widget.")
	return &longConv{t: t, e: e, sc: memberScope()}
}

// step plays the next turn of the script and returns its result. Every turn that goes to the
// model carries a marker of its own ("#017"), so a test can tell which turns a prompt replays;
// the bounded control phrases (resume, cancel) are exact words and carry none.
func (c *longConv) step() Result {
	c.t.Helper()
	i := c.n % longConvPeriod
	c.n++
	say := func(s string) Result { return c.e.say(c.sc, s) }
	marked := func(s string) string { return fmt.Sprintf("%s #%03d", s, c.n) }
	var res Result
	switch i {
	case 0:
		res = say(marked("create an item named widget with quantity 3"))
	case 1:
		res = say(marked("show me item item_1"))
	case 2:
		res = say("go back to the create item")
	case 3:
		if c.last.Prepared == nil {
			res = say("what can you do")
			break
		}
		res = c.e.turn(c.sc, confirmInput(c.last))
	case 4:
		res = say(marked("show me item item_1 again"))
	case 5:
		res = say(marked("create an item named gadget"))
	case 6:
		res = say(marked("the quantity is 2"))
	case 7:
		res = say(marked("show me item item_1 please"))
	case 8:
		res = say(marked("create an item named gizmo with quantity 5"))
	case 9:
		res = say("cancel")
	}
	if res.Prepared != nil {
		c.last = res
	}
	return res
}

// proposalCall returns the proposal model call of a turn, if the turn made one.
func proposalCall(res Result) (ModelCall, bool) {
	for _, mc := range res.Trace.ModelCalls {
		if mc.Purpose == "proposal" {
			return mc, true
		}
	}
	return ModelCall{}, false
}

// longConvStats are the proposal prompts of a run of the long conversation.
type longConvStats struct {
	// turn and toks are, for every turn that made a proposal call, its number and the
	// estimated prompt.
	turn, toks []int
	max        int
}

// meanBetween is the mean estimated proposal prompt over the proposal calls of turns lo..hi.
func (s longConvStats) meanBetween(lo, hi int) (mean float64, n int) {
	sum := 0
	for i, t := range s.turn {
		if t >= lo && t <= hi {
			sum += s.toks[i]
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(sum) / float64(n), n
}

// maxBetween is the largest estimated proposal prompt over turns lo..hi.
func (s longConvStats) maxBetween(lo, hi int) (m int) {
	for i, t := range s.turn {
		if t >= lo && t <= hi && s.toks[i] > m {
			m = s.toks[i]
		}
	}
	return m
}
