package dialogue

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ctxReplayedRows counts the replayed turn rows of a request: the messages fenced as an earlier
// message or an earlier reply.
func ctxReplayedRows(chat providers.ChatRequest) (users, replies int) {
	for _, m := range chat.Messages {
		switch {
		case strings.HasPrefix(m.Content, "<earlier_message>"):
			users++
		case strings.HasPrefix(m.Content, "<earlier_reply>"):
			replies++
		}
	}
	return users, replies
}

// SEC-076: the engine builds every prompt through the bounded context builder, within the
// budget of its profile.
func TestEngineBuildsPromptsThroughBoundedContext(t *testing.T) {
	lite := dctx.Lite()

	t.Run("the state, the goals set aside and the capsules in the prompt are the builder's", func(t *testing.T) {
		c := newLongConv(t)
		var res Result
		for range 46 { // turn 46 asks the model about a new request with the state full
			res = c.step()
		}
		call, ok := proposalCall(res)
		if !ok {
			t.Fatalf("turn 46 made no proposal call: %+v", res.Trace.Stages)
		}
		p := ctxLastProposalPrompt(t, c.e)
		state, ok := ctxMessageWith(p, "session_state")
		if !ok {
			t.Fatalf("no session state in the prompt: %v", p.Texts())
		}
		// The M1-14 engine showed the goal only. Set-aside goals and capsule lines are what
		// the builder adds, and nothing but the builder writes these headings.
		mustContain(t, state, "Set aside:", "the state")
		mustContain(t, state, "Completed earlier, oldest first:", "the state")
		mustContain(t, state, "- items.create: success", "a capsule line")

		// What reached the model is within the profile, counted again with the runtime's
		// own estimator, not with a number the builder reports.
		est := lite.Budget().EstimateChat(p.Chat)
		if est != call.PromptToks || est > lite.MaxPromptTokens {
			t.Fatalf("the model was sent %d estimated tokens, the trace says %d, the profile allows %d", est, call.PromptToks, lite.MaxPromptTokens)
		}
		if call.HistoryToks == 0 || call.HistoryToks > lite.MaxHistoryTokens {
			t.Fatalf("history is %d estimated tokens, the profile caps it at %d", call.HistoryToks, lite.MaxHistoryTokens)
		}
		users, replies := ctxReplayedRows(p.Chat)
		if users+replies != call.TurnsReplayed || call.TurnsReplayed == 0 || call.TurnsReplayed > lite.MaxTurnRows {
			t.Fatalf("rows replayed: %d user + %d replies in the prompt, trace says %d, profile allows %d", users, replies, call.TurnsReplayed, lite.MaxTurnRows)
		}
		if replies == 0 {
			t.Fatal("no assistant row was replayed as an earlier reply")
		}
		if p.Chat.MaxTokens != lite.MaxOutputTokens || call.Capabilities > lite.MaxShortlist {
			t.Fatalf("max tokens = %d (profile %d), capabilities = %d (profile %d)", p.Chat.MaxTokens, lite.MaxOutputTokens, call.Capabilities, lite.MaxShortlist)
		}

		// No identifier of the state reaches the prompt: not the goals', not a receipt's.
		sess := c.e.session(c.sc)
		var ids []string
		for _, g := range sess.State.Suspended {
			ids = append(ids, g.ID)
		}
		if sess.State.Goal != nil {
			ids = append(ids, sess.State.Goal.ID)
		}
		for _, cp := range sess.State.Capsules {
			ids = append(ids, cp.GoalID, cp.ReceiptID)
		}
		if len(ids) == 0 {
			t.Fatal("the state holds no identifier to look for")
		}
		all := strings.Join(p.Texts(), "\n")
		for _, id := range ids {
			if id != "" {
				mustNotContain(t, all, id, "the prompt")
			}
		}
		mustNotContain(t, all, c.sc.SessionID, "the prompt")
	})

	t.Run("the profile is the only budget: every cap of experimental-4g holds", func(t *testing.T) {
		p4g, err := dctx.ProfileByName(dctx.ProfileExperimental4G)
		if err != nil {
			t.Fatal(err)
		}
		c := newLongConv(t, withCtxProfile(p4g))
		proposals := 0
		for i := 1; i <= 60; i++ {
			res := c.step()
			call, ok := proposalCall(res)
			if !ok {
				continue
			}
			proposals++
			pr := ctxLastProposalPrompt(t, c.e)
			if got := p4g.Budget().EstimateChat(pr.Chat); got != call.PromptToks || got > p4g.MaxPromptTokens {
				t.Fatalf("turn %d: %d estimated tokens (trace %d), the profile allows %d", i, got, call.PromptToks, p4g.MaxPromptTokens)
			}
			if call.HistoryToks > p4g.MaxHistoryTokens || call.TurnsReplayed > p4g.MaxTurnRows || call.Capabilities > p4g.MaxShortlist {
				t.Fatalf("turn %d: %+v exceeds %v", i, call, p4g)
			}
			if pr.Chat.MaxTokens != p4g.MaxOutputTokens {
				t.Fatalf("turn %d: max tokens %d, the profile's is %d", i, pr.Chat.MaxTokens, p4g.MaxOutputTokens)
			}
		}
		if proposals < 30 {
			t.Fatalf("only %d proposals were built", proposals)
		}
	})

	t.Run("the output constraint has its own cap, and a prompt that cannot meet it is not sent", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withCtxProfile(ctxLiteWith(func(p *dctx.Profile) { p.MaxConstraintTokens = 50 })))
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		res := e.say(memberScope(), sayCreate)
		wantAct(t, res, "report_failure")
		if res.Code != contracts.ErrValidationFailed {
			t.Fatalf("code = %s, want validation_failed", res.Code)
		}
		if len(e.prompts()) != 0 || len(e.app.Calls()) != 0 {
			t.Fatalf("model calls = %d, host calls = %d, want none", len(e.prompts()), len(e.app.Calls()))
		}
		if len(res.Trace.ModelCalls) != 1 || res.Trace.ModelCalls[0].Err != contracts.ErrValidationFailed {
			t.Fatalf("model calls = %+v", res.Trace.ModelCalls)
		}
	})

	t.Run("the response prompt has its own cap", func(t *testing.T) {
		build := func(p dctx.Profile) (*env, Result) {
			e := newEnv(t, withCtxProfile(p))
			e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
			e.onProposal(proposalJSON(capGet, "task", map[string]any{"item_id": "item_1"}))
			e.onAct("present_result", "Here is what I found. Name: Alpha widget.")
			return e, e.say(memberScope(), sayShow)
		}
		// Within the cap (512 estimated tokens on Lite): the model writes the reply.
		e, res := build(lite)
		if len(e.prompts()) != 2 || res.Response.Source != verify.SourceModel {
			t.Fatalf("prompts = %d, response = %+v, want the model's verified reply", len(e.prompts()), res.Response)
		}
		for _, mc := range res.Trace.ModelCalls {
			if mc.Purpose == "response" && mc.PromptToks > lite.MaxResponsePromptTokens {
				t.Fatalf("a response prompt of %d estimated tokens is over the cap %d", mc.PromptToks, lite.MaxResponsePromptTokens)
			}
		}
		// Over it: the response call is not made, and the reply is the template of the same
		// directive (a degradation, never a missing answer).
		e, res = build(ctxLiteWith(func(p *dctx.Profile) { p.MaxResponsePromptTokens = 100 }))
		wantAct(t, res, "present_result")
		if len(e.prompts()) != 1 {
			t.Fatalf("model calls = %d, want the proposal only", len(e.prompts()))
		}
		if res.Response.Source != verify.SourceTemplate || res.Response.Reason != verify.ReasonDegraded {
			t.Fatalf("response = %+v, want the degraded template", res.Response)
		}
		last := res.Trace.ModelCalls[len(res.Trace.ModelCalls)-1]
		if last.Purpose != "response" || last.Err != contracts.ErrValidationFailed {
			t.Fatalf("last model call = %+v, want a response refused for its budget", last)
		}
	})

	t.Run("the shortlist a prompt describes is the profile's", func(t *testing.T) {
		say := "show me item item_1 and how many items are there in total"
		run := func(p dctx.Profile) (Result, int) {
			e := newEnv(t, withLevel(level2), withCount(), withCtxProfile(p))
			e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
			e.onProposal(proposalJSON(capGet, "task", map[string]any{"item_id": "item_1"}))
			e.onAct("present_result", "Here is what I found. Name: Alpha widget.")
			res := e.say(scopeFor("member", capGet, capCreate, capCount), say)
			call, ok := proposalCall(res)
			if !ok {
				t.Fatalf("no proposal call: %v", res.Trace.Stages)
			}
			return res, call.Capabilities
		}
		wide, wideCaps := run(lite)
		if len(wide.Trace.Shortlist) < 2 || wideCaps < 2 {
			t.Fatalf("the fixture shortlists %v and describes %d capabilities: it does not exercise the cap", wide.Trace.Shortlist, wideCaps)
		}
		narrow, narrowCaps := run(ctxLiteWith(func(p *dctx.Profile) { p.MaxShortlist = 1 }))
		if len(narrow.Trace.Shortlist) != 1 || narrowCaps != 1 {
			t.Fatalf("shortlist = %v, capabilities described = %d, want 1 of each", narrow.Trace.Shortlist, narrowCaps)
		}
	})

	t.Run("a profile that is not coherent is an error, not a default", func(t *testing.T) {
		e := newEnv(t)
		bad := ctxLiteWith(func(p *dctx.Profile) { p.MaxHistoryTokens = p.MaxPromptTokens })
		_, err := New(Deps{
			Sessions: e.store, Registry: e.reg, Packages: PackageSourceFunc(ctxPackagesOf(e)), Policy: e.gw, Actions: e.co,
			Adapters: ctxAdaptersOf(e), Providers: ctxLocalSelectorOf(t, e), Config: Config{Profile: bad},
		})
		wantCode(t, err, contracts.ErrValidationFailed)
		if _, err := dctx.ProfileByName("lite-ish"); err == nil {
			t.Fatal("an unknown profile name was accepted")
		}
	})
}

// SEC-076: a 500-turn conversation, driven through the real engine, keeps every proposal prompt
// within the profile and stops growing: the prompt of turn 500 is no larger than that of turn 50.
func TestLongConversationStaysWithinProfileBudget(t *testing.T) {
	const turns = 500
	marker := regexp.MustCompile(`#(\d{3})`)
	for _, name := range dctx.ProfileNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := dctx.ProfileByName(name)
			if err != nil {
				t.Fatal(err)
			}
			c := newLongConv(t, withCtxProfile(p))
			var st longConvStats
			var maxHist int
			for i := 1; i <= turns; i++ {
				res := c.step()
				call, ok := proposalCall(res)
				if !ok {
					continue
				}
				if call.Err != "" || call.PromptToks > p.MaxPromptTokens || call.HistoryToks > p.MaxHistoryTokens ||
					call.TurnsReplayed > p.MaxTurnRows || call.Capabilities > p.MaxShortlist {
					t.Fatalf("turn %d: %+v is outside %v (history cap %d, rows %d)", i, call, p, p.MaxHistoryTokens, p.MaxTurnRows)
				}
				maxHist = max(maxHist, call.HistoryToks)
				st.turn, st.toks = append(st.turn, i), append(st.toks, call.PromptToks)
				st.max = max(st.max, call.PromptToks)
			}
			if len(st.turn) < turns/2 {
				t.Fatalf("only %d of %d turns built a proposal prompt", len(st.turn), turns)
			}
			for _, w := range [][2]int{{1, 10}, {41, 50}, {91, 100}, {491, 500}} {
				mean, n := st.meanBetween(w[0], w[1])
				t.Logf("%-16s turns %3d-%3d: mean proposal prompt %4.0f est toks over %d proposals, max %d", name, w[0], w[1], mean, n, st.maxBetween(w[0], w[1]))
			}
			t.Logf("%-16s over all %d turns: max proposal prompt %d of %d est toks, max history %d of %d", name, turns, st.max, p.MaxPromptTokens, maxHist, p.MaxHistoryTokens)

			// The plateau is the claim: the last cycle costs no more than the cycle at turn 50.
			m50, _ := st.meanBetween(41, 50)
			m500, _ := st.meanBetween(491, 500)
			if slack := float64(p.MaxRowRunes/2 + 64); m500 > m50+slack {
				t.Errorf("the prompt grows: %.0f est tokens at turn 500 against %.0f at turn 50", m500, m50)
			}

			// What reached the model at turn 500 is the bounded window, not the conversation: every
			// turn marker in the last prompt is one of the newest CandidateRows turns, and the
			// prompt is nowhere near the whole transcript.
			last := ctxLastProposalPrompt(t, c.e)
			newest, oldest := 0, turns
			for _, m := range marker.FindAllStringSubmatch(strings.Join(last.Texts(), "\n"), -1) {
				n, _ := strconv.Atoi(m[1])
				newest, oldest = max(newest, n), min(oldest, n)
			}
			if newest == 0 || newest-oldest > p.CandidateRows {
				t.Errorf("the last prompt replays turns %d to %d: older than the %d-row window", oldest, newest, p.CandidateRows)
			}
			transcript := 0
			for after := int64(0); ; {
				page, err := c.e.store.Transcript(context.Background(), c.sc, after, sessions.MaxTranscriptPage)
				if err != nil {
					t.Fatal(err)
				}
				if len(page) == 0 {
					break
				}
				for _, r := range page {
					transcript += p.Budget().EstimateChat(providers.ChatRequest{Messages: []providers.Message{{Content: fence(tagEarlier, r.Body)}}})
				}
				after = page[len(page)-1].N
			}
			t.Logf("%-16s replaying the whole transcript would add %d est toks to the prompt; the bounded history is %d", name, transcript, maxHist)
			if transcript < 3*p.MaxPromptTokens {
				t.Errorf("the transcript is %d est tokens against a cap of %d: under 3 times the cap, the conversation is too short to show the cap working", transcript, p.MaxPromptTokens)
			}
			sess := c.e.session(c.sc)
			if sess.TurnCount != turns || len(sess.State.Capsules) == 0 {
				t.Errorf("turn count = %d, capsules = %d: the script did not run as intended", sess.TurnCount, len(sess.State.Capsules))
			}
		})
	}
}

// TestExistingDialogueBehaviourUnchanged replays the first 30 turns of the long conversation and
// compares what the engine DID with what the M1-14 / M3-02 engine did before it adopted the
// bounded context builder: the act of every turn, the goal and its phase, how many goals were set
// aside and how many capsules existed, how many writes the host applied, and the host's own call
// counts. The expected rows were recorded on the engine of commit 8b5457f, before the adoption.
// The model is scripted the same way, so what may differ is only what the prompts say.
func TestExistingDialogueBehaviourUnchanged(t *testing.T) {
	want := []struct {
		act     string
		goal    string
		susp    int
		capsule int
		effects int
	}{
		{"request_confirmation", "items.create/prepared", 0, 0, 0},
		{"present_result", "items.get/success", 1, 0, 0},
		{"request_confirmation", "items.create/prepared", 0, 1, 0},
		{"report_outcome", "items.create/success", 0, 1, 1},
		{"present_result", "items.get/success", 0, 2, 1},
		{"ask_missing", "items.create/", 0, 3, 1},
		{"request_confirmation", "items.create/prepared", 0, 3, 1},
		{"present_result", "items.get/success", 1, 3, 1},
		{"request_confirmation", "items.create/prepared", 1, 4, 1},
		{"acknowledge_cancel", "-", 1, 5, 1},
		{"request_confirmation", "items.create/prepared", 1, 5, 1},
		{"present_result", "items.get/success", 2, 5, 1},
		{"request_confirmation", "items.create/prepared", 1, 6, 1},
		{"report_outcome", "items.create/success", 1, 6, 2},
		{"present_result", "items.get/success", 1, 7, 2},
		{"ask_missing", "items.create/", 1, 8, 2},
		{"request_confirmation", "items.create/prepared", 1, 8, 2},
		{"present_result", "items.get/success", 2, 8, 2},
		{"request_confirmation", "items.create/prepared", 2, 8, 2},
		{"acknowledge_cancel", "-", 2, 8, 2},
		{"request_confirmation", "items.create/prepared", 2, 8, 2},
		{"present_result", "items.get/success", 3, 8, 2},
		{"request_confirmation", "items.create/prepared", 2, 8, 2},
		{"report_outcome", "items.create/success", 2, 8, 3},
		{"present_result", "items.get/success", 2, 8, 3},
		{"ask_missing", "items.create/", 2, 8, 3},
		{"request_confirmation", "items.create/prepared", 2, 8, 3},
		{"present_result", "items.get/success", 3, 8, 3},
		{"request_confirmation", "items.create/prepared", 3, 8, 3},
		{"acknowledge_cancel", "-", 3, 8, 3},
	}
	c := newLongConv(t)
	for i, w := range want {
		res := c.step()
		st := c.e.session(c.sc).State
		goal := "-"
		if st.Goal != nil {
			goal = st.Goal.CapabilityID + "/" + st.Goal.Phase
		}
		if string(res.Directive.Act) != w.act || goal != w.goal || len(st.Suspended) != w.susp || len(st.Capsules) != w.capsule || c.e.app.Effects() != w.effects || res.Failed() {
			t.Fatalf("turn %d: act=%s goal=%s suspended=%d capsules=%d effects=%d code=%q; the engine before the adoption did act=%s goal=%s suspended=%d capsules=%d effects=%d",
				i+1, res.Directive.Act, goal, len(st.Suspended), len(st.Capsules), c.e.app.Effects(), res.Code, w.act, w.goal, w.susp, w.capsule, w.effects)
		}
	}
	for op, n := range map[adapters.Operation]int{adapters.OpPrepare: 12, adapters.OpCommit: 3, adapters.OpRead: 9} {
		if got := c.e.hostCalls(op); got != n {
			t.Errorf("host %s calls = %d, the engine before the adoption made %d", op, got, n)
		}
	}
	if got := c.e.app.Effects(); got != 3 {
		t.Errorf("the host applied %d writes, want 3", got)
	}
}
