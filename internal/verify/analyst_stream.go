package verify

import (
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// AnalystStream verifies analyst answer text sentence-by-sentence (ADR 0046).
// A failing segment replaces the stream with the analyst template.
// Reference tokens (ADR 0049) that straddle segment boundaries are held until
// complete; emitted segments are resolved for the client while the accumulator
// keeps the token form for model history.
type AnalystStream struct {
	answer AnalystAnswer
	facts  AnalystFactSet
	opts   AnalystOptions
	buf    strings.Builder
	acc    strings.Builder // token form (unresolved)
	done   bool
}

// NewAnalystStream prepares incremental analyst verification.
func NewAnalystStream(answer AnalystAnswer, facts AnalystFactSet, opts AnalystOptions) (*AnalystStream, error) {
	if opts.Language != LangEN && opts.Language != LangIT {
		return nil, failure(contracts.ErrValidationFailed, ErrUnsupportedLanguage, "language %q is not supported", string(opts.Language))
	}
	return &AnalystStream{answer: answer, facts: facts, opts: opts}, nil
}

// AnalystStreamStep mirrors StreamStep for analyst turns.
type AnalystStreamStep struct {
	Segments []string
	Replace  bool
	Outcome  AnalystOutcome
	Done     bool
}

// Push appends a model delta and verifies ready segments.
func (s *AnalystStream) Push(delta string) (AnalystStreamStep, error) {
	if s == nil || s.done {
		return AnalystStreamStep{Done: true}, nil
	}
	if delta != "" {
		s.buf.WriteString(delta)
	}
	ready, rest := splitAnalystSegments(s.buf.String(), s.opts.Language)
	s.buf.Reset()
	s.buf.WriteString(rest)
	return s.accept(ready)
}

// Finish flushes remainder and runs full VerifyAnalyst.
func (s *AnalystStream) Finish() (AnalystStreamStep, error) {
	if s == nil {
		return AnalystStreamStep{}, invalidf("analyst stream is nil")
	}
	if s.done {
		text := s.acc.String()
		out := AnalystOutcome{Text: text, Source: SourceModel, Reason: ReasonVerified, Streamed: true}
		if len(ParseRefTokens(text)) > 0 {
			out.ModelText = text
			if resolved, err := ResolveAnalystRefs(text, s.facts, s.opts.Language); err == nil {
				out.Text = resolved
			}
		}
		return AnalystStreamStep{Done: true, Outcome: out}, nil
	}
	ready := flushAnalystSegments(s.buf.String(), s.opts.Language)
	s.buf.Reset()
	step, err := s.accept(ready)
	if err != nil || step.Replace || step.Done {
		return step, err
	}
	ans := s.answer
	ans.Text = s.acc.String()
	out, err := VerifyAnalyst(ans, s.facts, s.opts)
	if err != nil {
		s.done = true
		return AnalystStreamStep{}, err
	}
	s.done = true
	if out.Source != SourceModel {
		out.Streamed = s.acc.Len() > 0
		return AnalystStreamStep{Replace: true, Outcome: out, Done: true}, nil
	}
	out.Streamed = true
	if out.ModelText != "" {
		if resolved, rerr := ResolveAnalystRefs(out.ModelText, s.facts, s.opts.Language); rerr == nil {
			out.Text = resolved
		}
	} else if len(ParseRefTokens(out.Text)) > 0 {
		out.ModelText = out.Text
		if resolved, rerr := ResolveAnalystRefs(out.Text, s.facts, s.opts.Language); rerr == nil {
			out.Text = resolved
		}
	}
	return AnalystStreamStep{Segments: step.Segments, Outcome: out, Done: true}, nil
}

func (s *AnalystStream) accept(ready []string) (AnalystStreamStep, error) {
	var emitted []string
	for _, seg := range ready {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		cand := s.acc.String() + seg
		if res := CheckAnalystRefs(cand, s.facts); !res.OK {
			return s.fail(res.Violations, emitted)
		}
		res := checkAnalyst(cand, s.answer.SourceSpans, s.facts, s.opts.Language)
		if !res.OK {
			return s.fail(res.Violations, emitted)
		}
		resolved, rerr := ResolveAnalystRefs(seg, s.facts, s.opts.Language)
		if rerr != nil {
			return s.fail([]Violation{{Kind: ViolationUnknownIdentifier, Fingerprint: fingerprint(seg)}}, emitted)
		}
		s.acc.WriteString(seg)
		emitted = append(emitted, resolved)
	}
	return AnalystStreamStep{Segments: emitted}, nil
}

func (s *AnalystStream) fail(viol []Violation, emitted []string) (AnalystStreamStep, error) {
	out, terr := analystTemplate(s.facts, s.answer.FinalDatasetID, s.opts.Language)
	if terr != nil {
		s.done = true
		return AnalystStreamStep{}, terr
	}
	out.Reason = ReasonVerificationFailed
	out.Violations = viol
	out.Streamed = s.acc.Len() > 0 || len(emitted) > 0
	s.done = true
	return AnalystStreamStep{Replace: true, Outcome: out, Done: true}, nil
}

// splitAnalystSegments is SplitSegments plus a hold on incomplete {{ref:…}} tokens
// so a half-token is never emitted (ADR 0049 / ADR 0046).
func splitAnalystSegments(buf string, lang Language) (ready []string, rest string) {
	ready, rest = SplitSegments(buf, lang)
	if HasIncompleteRefToken(rest) {
		return ready, rest
	}
	if len(ready) == 0 {
		return ready, rest
	}
	// If the last ready segment ends with an incomplete token, pull it back into rest.
	last := ready[len(ready)-1]
	if HasIncompleteRefToken(last) {
		ready = ready[:len(ready)-1]
		rest = last + rest
	}
	return ready, rest
}

func flushAnalystSegments(buf string, lang Language) []string {
	ready, rest := splitAnalystSegments(buf, lang)
	if strings.TrimSpace(rest) != "" {
		ready = append(ready, rest)
	}
	return ready
}
