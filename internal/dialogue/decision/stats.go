package decision

import "sync"

// Snapshot is a copy of the layer's counters. Counters hold numbers only: never an
// utterance, a candidate label or a value.
type Snapshot struct {
	// Decisions is how many times the stage was asked (shortlist turns).
	Decisions int
	// ExactSelected is how many the exact id/title rule routed with no scorer.
	ExactSelected int
	// EngineRequests is how many decision requests were handed to the engine (stage and
	// detectors). The engine's guard may still refuse one before any provider is called.
	EngineRequests int
	// ScorerSelected is how many scorer proposals passed the gates and were re-resolved.
	ScorerSelected int
	// Fallbacks counts the typed fallback reasons, stage and detectors together.
	Fallbacks map[string]int
	// RejectCodes counts the candidate guard's reject codes behind candidate_set_rejected.
	RejectCodes map[string]int

	// ControlRuleHits and ControlScorerHits count control detections (help, cancel,
	// reject, confirm) made by the exact rules and by the scorer (never a reject, nor a
	// cancel while a preparation is pending).
	ControlRuleHits   int
	ControlScorerHits int

	// TurnsObserved is how many turns went through Layer.Wrap.
	TurnsObserved int
	// ControllerCallsAvoidedByRules and ...ByScorer are the turns that a control detection
	// ended with no proposal call to the controller. They are counted from the turn's own
	// Trace, so a detection the engine ignored is not counted.
	ControllerCallsAvoidedByRules  int
	ControllerCallsAvoidedByScorer int
	// GeneratedTokensAvoided is an ESTIMATE: the avoided proposal calls times
	// Config.ProposalTokens (the size of the smallest control proposal), split like the
	// calls. It is not a measurement of any model's output.
	GeneratedTokensAvoidedByRules  int
	GeneratedTokensAvoidedByScorer int
	// PromptCapabilitiesSaved is how many capability descriptions a selection removed from
	// the proposal prompt, measured from the turn's own Trace (shortlist size minus the
	// capabilities the proposal prompt listed).
	PromptCapabilitiesSaved int
}

// ControllerCallsAvoided is the total of the two sources.
func (s Snapshot) ControllerCallsAvoided() int {
	return s.ControllerCallsAvoidedByRules + s.ControllerCallsAvoidedByScorer
}

// GeneratedTokensAvoided is the estimated total of the two sources.
func (s Snapshot) GeneratedTokensAvoided() int {
	return s.GeneratedTokensAvoidedByRules + s.GeneratedTokensAvoidedByScorer
}

type stats struct {
	mu sync.Mutex
	s  Snapshot
}

func (t *stats) with(f func(*Snapshot)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.s)
}

func (t *stats) fallback(reason string, reject string) {
	t.with(func(s *Snapshot) {
		if s.Fallbacks == nil {
			s.Fallbacks = map[string]int{}
		}
		s.Fallbacks[reason]++
		if reject != "" {
			if s.RejectCodes == nil {
				s.RejectCodes = map[string]int{}
			}
			s.RejectCodes[reject]++
		}
	})
}

func (t *stats) snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.s
	out.Fallbacks = copyMap(t.s.Fallbacks)
	out.RejectCodes = copyMap(t.s.RejectCodes)
	return out
}

func copyMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
