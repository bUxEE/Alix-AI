package verify

import (
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Stream verifies model text sentence-by-sentence (ADR 0046). Each Accept of a complete
// segment checks the cumulative verified text with the same disallow rules as Verify
// (numbers, entities, citations, completion, invisible characters) but does not require
// must_state yet. Finish runs a full Realise on the concatenation (must_state included).
// The first failing segment stops the stream; Outcome is the deterministic template and
// Replace is true so already-shown verified text is superseded.
type Stream struct {
	d    *conversation.DialogueDirective
	r    *actions.ActionReceipt
	opts Options
	buf  strings.Builder // unfinished model bytes
	acc  strings.Builder // verified cumulative text
	done bool
}

// NewStream prepares incremental verification for one reply.
func NewStream(d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (*Stream, error) {
	if _, err := prepare(d, r, opts); err != nil {
		return nil, err
	}
	return &Stream{d: d, r: r, opts: opts}, nil
}

// StreamStep is one result of feeding the model stream.
type StreamStep struct {
	// Segments are newly verified pieces to show, in order. Empty when nothing is ready yet
	// or when Replace is set (then Text on Outcome is the template).
	Segments []string
	// Replace is true when a segment failed: discard every previously emitted segment and
	// show Outcome.Text (the template) instead. No unverified text is in Segments.
	Replace bool
	// Outcome is set when Replace is true, or when Finish succeeds / degrades.
	Outcome Outcome
	// Done is true after Replace or Finish; further Push/Finish are no-ops.
	Done bool
}

// Push appends a model delta, splits ready segments, and verifies each. On failure it
// returns Replace with the template and marks the stream done.
func (s *Stream) Push(delta string) (StreamStep, error) {
	if s == nil || s.done {
		return StreamStep{Done: true}, nil
	}
	if delta != "" {
		s.buf.WriteString(delta)
	}
	ready, rest := SplitSegments(s.buf.String(), s.opts.Language)
	s.buf.Reset()
	s.buf.WriteString(rest)
	return s.accept(ready)
}

// Finish flushes the remainder, verifies it, then runs full Realise on the concatenation.
// If any remainder fails, Replace+template. If Realise rejects (must_state, etc.), same.
func (s *Stream) Finish() (StreamStep, error) {
	if s == nil {
		return StreamStep{}, invalidf("stream is nil")
	}
	if s.done {
		return StreamStep{Done: true, Outcome: Outcome{Text: s.acc.String(), Source: SourceModel, Reason: ReasonVerified, Streamed: true}}, nil
	}
	ready := FlushSegments(s.buf.String(), s.opts.Language)
	s.buf.Reset()
	step, err := s.accept(ready)
	if err != nil || step.Replace || step.Done {
		return step, err
	}
	text := s.acc.String()
	out, err := RealiseRich(text, nil, s.d, s.r, s.opts)
	if err != nil {
		s.done = true
		return StreamStep{}, err
	}
	s.done = true
	if out.Source != SourceModel {
		// Full verify failed after segments had passed the prefix check (e.g. missing must_state).
		if s.acc.Len() > 0 {
			out.Streamed = true
			return StreamStep{Replace: true, Outcome: out, Done: true}, nil
		}
		out.Streamed = false
		return StreamStep{Outcome: out, Done: true}, nil
	}
	out.Streamed = true
	return StreamStep{Segments: step.Segments, Outcome: out, Done: true}, nil
}

func (s *Stream) accept(ready []string) (StreamStep, error) {
	var emitted []string
	for _, seg := range ready {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		cand := s.acc.String() + seg
		res, err := VerifyPrefix(cand, s.d, s.r, s.opts)
		if err != nil {
			s.done = true
			return StreamStep{}, err
		}
		if !res.OK {
			out, terr := Degraded(s.d, s.r, s.opts)
			if terr != nil {
				s.done = true
				return StreamStep{}, terr
			}
			out.Reason = ReasonVerificationFailed
			out.Violations = res.Violations
			out.Streamed = s.acc.Len() > 0 || len(emitted) > 0
			s.done = true
			return StreamStep{Replace: true, Outcome: out, Done: true}, nil
		}
		s.acc.WriteString(seg)
		emitted = append(emitted, seg)
	}
	return StreamStep{Segments: emitted}, nil
}

// VerifyPrefix is Verify without must_state completeness: a growing reply may omit facts
// that appear only in later segments. Everything else (unknown numbers, entities, citations,
// completion without receipt, invisible characters, empty/too-long) fails closed the same way.
func VerifyPrefix(text string, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (Result, error) {
	v, err := prepare(d, r, opts)
	if err != nil {
		return Result{}, err
	}
	return v.checkPrefix(text), nil
}

func (v *verification) checkPrefix(text string) Result {
	saved := v.must
	v.must = nil
	res := v.check(text)
	v.must = saved
	// Empty text is not a failure for a prefix that has not started; callers skip blank segments.
	if res.Has(ViolationEmptyText) && strings.TrimSpace(text) == "" {
		return Result{OK: true}
	}
	return res
}
