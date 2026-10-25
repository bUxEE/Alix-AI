package decision

import (
	"context"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// act is a dialogue act the control detectors may ask the scorer about. actTask is the
// "this is an ordinary request, not a control act" answer: without it the scorer would be
// forced to read every utterance as some control act.
type act string

const (
	actHelp    act = "help"
	actCancel  act = "cancel"
	actConfirm act = "confirm"
	actReject  act = "reject"
	actTask    act = "task"
)

func (a act) id() string { return actPrefix + string(a) }

func actByID(id string) (act, bool) {
	switch a := act(strings.TrimPrefix(id, actPrefix)); a {
	case actHelp, actCancel, actConfirm, actReject, actTask:
		return a, true
	}
	return "", false
}

// Detectors returns the control detectors for dialogue.Deps.Detectors: the core's exact
// rule detectors with a scorer behind them for help, cancel and confirm. Reject, and cancel
// while a preparation is pending, stay rule-only (see applicable): advice never declines a
// preparation. Restart, resume and correct are not in the result and so stay the core's rules
// (the TaskProposal schema has no such intents, and a correction still needs the controller's
// values).
//
// A detector asks, in order: the exact rule (no scorer, ever, when it matches); then, only
// when the layer is on, the turn is bound (Layer.Wrap), the utterance is short and something
// in the session can be cancelled or confirmed, one scorer request per turn shared by all the
// detectors. The state flags (a question pending, choices offered) go to the scorer as bounded
// state, and "task" is always a candidate, so an answer to the pending question is not a
// control act. A detection is only advisory: the engine checks the state and takes the transition itself, and a confirmation
// still commits nothing.
func (l *Layer) Detectors() dialogue.Detectors {
	rules := dialogue.RuleDetectors()
	all := []dialogue.ControlDetector{rules.Help, rules.Cancel, rules.Restart, rules.Resume, rules.Reject, rules.Confirm, rules.Correct}
	return dialogue.Detectors{
		Help:    l.detector(actHelp, rules.Help, all),
		Cancel:  l.detector(actCancel, rules.Cancel, all),
		Reject:  l.detector(actReject, rules.Reject, all),
		Confirm: l.detector(actConfirm, rules.Confirm, all),
	}
}

// detector wraps one rule detector. The engine asks the detectors in its own order (help,
// cancel, restart, resume, reject, confirm), so a scorer behind the first of them would
// pre-empt an exact phrase that a later one recognises. The scorer is therefore asked only
// when no rule of any kind (correct included, which means "the controller must read values")
// recognises the utterance.
func (l *Layer) detector(kind act, rule dialogue.ControlDetector, all []dialogue.ControlDetector) dialogue.ControlDetector {
	return dialogue.ControlDetectorFunc(func(ctx context.Context, req dialogue.ControlRequest) dialogue.ControlDetection {
		d := rule.Detect(ctx, req)
		b := boundOf(ctx)
		if d.Matched {
			l.st.with(func(s *Snapshot) { s.ControlRuleHits++ })
			if b != nil {
				b.note(sourceRule)
			}
			return d
		}
		if b == nil || !l.enabled() || !applicable(kind, req.State) || anyRule(ctx, all, req) {
			return d
		}
		if l.classify(ctx, b, req) == kind {
			l.st.with(func(s *Snapshot) { s.ControlScorerHits++ })
			b.note(sourceScorer)
			return dialogue.ControlDetection{Matched: true}
		}
		return d
	})
}

func anyRule(ctx context.Context, rules []dialogue.ControlDetector, req dialogue.ControlRequest) bool {
	for _, r := range rules {
		if r.Detect(ctx, req).Matched {
			return true
		}
	}
	return false
}

// applicable says whether the scorer's reading of the act may be used in the state. The
// engine would ignore a detection that does not fit; the layer does not even ask. A reject,
// and a cancel while a preparation is pending, decline that preparation for good, so advice
// never takes them: only the exact rules do, and otherwise the controller decides, as it does
// with the layer off.
func applicable(kind act, st dialogue.ControlState) bool {
	switch kind {
	case actHelp:
		return true
	case actCancel:
		return st.HasGoal && !st.Prepared
	case actConfirm:
		return st.Prepared
	}
	return false
}

// classify asks the scorer once per turn which act the utterance is, among the acts the
// state permits plus "task". It returns the act, or "" for task and every fallback.
func (l *Layer) classify(ctx context.Context, b *boundTurn, req dialogue.ControlRequest) act {
	if got, done := b.classified(); done {
		return got
	}
	got := l.classifyOnce(ctx, b.scope, req)
	b.setClassified(got)
	return got
}

func (l *Layer) classifyOnce(ctx context.Context, scope contracts.Scope, req dialogue.ControlRequest) act {
	st := req.State
	text := strings.TrimSpace(req.Text)
	if text == "" || len(text) > l.cfg.MaxUtteranceBytes || len(strings.Fields(text)) > l.cfg.MaxControlWords ||
		(!st.HasGoal && !st.Prepared) {
		// The utterance is not the shape of a control act, or there is nothing to cancel or
		// confirm: the rules decided, and the scorer is not asked.
		return ""
	}
	budget := l.budget(ctx)
	if budget <= 0 {
		l.st.fallback(string(gen.FallbackReasonTimeout), "")
		return ""
	}
	acts := []act{actHelp}
	var next []actions.PermittedNextAction
	if st.HasGoal {
		acts = append(acts, actCancel)
		next = append(next, actions.PermittedNextAction{Action: actions.PermittedNextActionActionCancel})
	}
	if st.Prepared {
		acts = append(acts, actConfirm, actReject)
		next = append(next,
			actions.PermittedNextAction{Action: actions.PermittedNextActionActionConfirm},
			actions.PermittedNextAction{Action: actions.PermittedNextActionActionReject})
	}
	acts = append(acts, actTask)
	cands := make([]gen.CandidateSetCandidate, len(acts))
	for i, a := range acts {
		cands[i] = gen.CandidateSetCandidate{CandidateID: common.Identifier(a.id()), Label: string(a)}
	}
	state := map[string]gen.BoundedScalar{
		"user_text": text, "language": string(req.Language),
		"has_goal": st.HasGoal, "finished": st.Finished, "prepared": st.Prepared, "pending": st.Pending,
		"choices": st.Choices, "suspended": int64(st.Suspended),
	}
	greq := l.request(scope, gen.DecisionTypeDialogueAct, ContractAct, string(req.Language), state, next, cands, budget)
	sel, reason := l.rank(ctx, scope, greq)
	if reason != dialogue.FallbackNone {
		return ""
	}
	if a, ok := actByID(sel.id); ok && a != actTask {
		return a
	}
	return ""
}

// ---- binding a verified scope to a turn ----

// Turner is what Layer.Wrap wraps: the dialogue engine's Turn.
type Turner interface {
	Turn(ctx context.Context, scope contracts.Scope, in dialogue.TurnInput) (dialogue.Result, error)
}

type source int

const (
	sourceNone source = iota
	sourceRule
	sourceScorer
)

// boundTurn is the state of one wrapped turn: the verified scope (the control detectors
// receive none), the one classification the four detectors share, and which source made the
// control detection, for counting.
type boundTurn struct {
	scope contracts.Scope

	mu     sync.Mutex
	done   bool
	result act
	by     source
}

type boundKey struct{}

func boundOf(ctx context.Context) *boundTurn {
	b, _ := ctx.Value(boundKey{}).(*boundTurn)
	return b
}

func (b *boundTurn) classified() (act, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result, b.done
}

func (b *boundTurn) setClassified(a act) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.result, b.done = a, true
}

func (b *boundTurn) note(s source) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.by = s
}

type wrapped struct {
	l *Layer
	e Turner
}

// Wrap returns a Turner that binds the verified scope to each turn's context, so the
// detectors can call the engine, and counts what the turn avoided. Without it the detectors
// are exactly the core's rules. The scope bound is the one the caller passes to Turn, so a
// turn's detectors can only ever ask about the turn's own tenant and session.
func (l *Layer) Wrap(e Turner) Turner { return wrapped{l: l, e: e} }

// Turn implements Turner.
func (w wrapped) Turn(ctx context.Context, scope contracts.Scope, in dialogue.TurnInput) (dialogue.Result, error) {
	b := &boundTurn{scope: scope.Clone()}
	res, err := w.e.Turn(context.WithValue(ctx, boundKey{}, b), scope, in)
	if err == nil {
		w.l.observe(b, res)
	}
	return res, err
}

// observe counts, from the turn's own Trace, what the turn avoided.
func (l *Layer) observe(b *boundTurn, res dialogue.Result) {
	proposals, capsInPrompt := 0, -1
	for _, c := range res.Trace.ModelCalls {
		if c.Purpose == "proposal" {
			proposals++
			capsInPrompt = c.Capabilities
		}
	}
	b.mu.Lock()
	by := b.by
	b.mu.Unlock()
	avoidedByScorer := false
	l.st.with(func(s *Snapshot) {
		s.TurnsObserved++
		if proposals == 0 {
			switch by {
			case sourceRule:
				s.ControllerCallsAvoidedByRules++
				s.GeneratedTokensAvoidedByRules += l.cfg.ProposalTokens
			case sourceScorer:
				s.ControllerCallsAvoidedByScorer++
				s.GeneratedTokensAvoidedByScorer += l.cfg.ProposalTokens
				avoidedByScorer = true
			}
		}
		if res.Trace.Decision == "selected" && capsInPrompt >= 0 {
			if saved := len(res.Trace.Shortlist) - capsInPrompt; saved > 0 {
				s.PromptCapabilitiesSaved += saved
			}
		}
	})
	// Only the scorer's detections count for the metrics: an exact rule would have saved the call anyway.
	if avoidedByScorer && l.d.Observer != nil {
		l.d.Observer.ControllerCallAvoided()
	}
}
