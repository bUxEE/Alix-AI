package verify

import (
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// AnalystStream verifies analyst answer text sentence-by-sentence (ADR 0046).
// A failing segment replaces the stream with the analyst template.
type AnalystStream struct {
	answer AnalystAnswer
	facts  AnalystFactSet
	opts   AnalystOptions
	buf    strings.Builder
	acc    strings.Builder
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
	ready, rest := SplitSegments(s.buf.String(), s.opts.Language)
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
		return AnalystStreamStep{Done: true, Outcome: AnalystOutcome{Text: s.acc.String(), Source: SourceModel, Reason: ReasonVerified, Streamed: true}}, nil
	}
	ready := FlushSegments(s.buf.String(), s.opts.Language)
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
	return AnalystStreamStep{Segments: step.Segments, Outcome: out, Done: true}, nil
}

func (s *AnalystStream) accept(ready []string) (AnalystStreamStep, error) {
	var emitted []string
	for _, seg := range ready {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		cand := s.acc.String() + seg
		res := checkAnalyst(cand, s.answer.SourceSpans, s.facts, s.opts.Language)
		if !res.OK {
			out, terr := analystTemplate(s.facts, s.answer.FinalDatasetID, s.opts.Language)
			if terr != nil {
				s.done = true
				return AnalystStreamStep{}, terr
			}
			out.Reason = ReasonVerificationFailed
			out.Violations = res.Violations
			out.Streamed = s.acc.Len() > 0 || len(emitted) > 0
			s.done = true
			return AnalystStreamStep{Replace: true, Outcome: out, Done: true}, nil
		}
		s.acc.WriteString(seg)
		emitted = append(emitted, seg)
	}
	return AnalystStreamStep{Segments: emitted}, nil
}
