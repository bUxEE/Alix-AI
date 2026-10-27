package dialogue

import (
	"context"
	"io"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// respondStream realises a reply through ChatStream and the incremental verifier (ADR 0046).
// ok=false means the provider does not stream (or the stream could not start): the caller uses Chat.
// Verified segments are handed to OnVerifiedSegment as they pass; a failing segment emits Replace
// with the template. Unverified text is never emitted.
func (t *turn) respondStream(ctx context.Context, built builtPrompt, out outcome, opts verify.Options) (verify.Outcome, bool, error) {
	stream, call, err := t.openChatStream(ctx, built)
	if err != nil {
		if contracts.CodeOf(err) == contracts.ErrUnavailable {
			return verify.Outcome{}, false, nil
		}
		t.recordCall(call)
		o, derr := verify.Degraded(out.dir, out.receipt, opts)
		return o, true, derr
	}
	defer stream.Close()
	t.recordCall(call)

	vs, err := verify.NewStream(out.dir, out.receipt, opts)
	if err != nil {
		o, derr := verify.Degraded(out.dir, out.receipt, opts)
		return o, true, derr
	}
	var usage providers.Usage
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			o, derr := verify.Degraded(out.dir, out.receipt, opts)
			return o, true, derr
		}
		if chunk.Done {
			usage = chunk.Usage
		}
		if chunk.Delta == "" {
			continue
		}
		step, serr := vs.Push(chunk.Delta)
		if serr != nil {
			o, derr := verify.Degraded(out.dir, out.receipt, opts)
			return o, true, derr
		}
		if err := t.applyStreamStep(ctx, step); err != nil {
			return step.Outcome, true, err
		}
		if step.Done {
			if call.TokIn == 0 && call.TokOut == 0 {
				call.TokIn, call.TokOut = usage.InputTokens, usage.OutputTokens
			}
			return step.Outcome, true, nil
		}
	}
	fin, ferr := vs.Finish()
	if ferr != nil {
		o, derr := verify.Degraded(out.dir, out.receipt, opts)
		return o, true, derr
	}
	if err := t.applyStreamStep(ctx, fin); err != nil {
		return fin.Outcome, true, err
	}
	call.TokIn, call.TokOut = usage.InputTokens, usage.OutputTokens
	// Refresh the last recorded call with usage from the Done chunk.
	if n := len(t.trace.ModelCalls); n > 0 {
		t.trace.ModelCalls[n-1].TokIn = call.TokIn
		t.trace.ModelCalls[n-1].TokOut = call.TokOut
	}
	return fin.Outcome, true, nil
}

func (t *turn) applyStreamStep(ctx context.Context, step verify.StreamStep) error {
	if step.Replace {
		t.emitSegment(ctx, VerifiedSegment{Text: step.Outcome.Text, Replace: true, Reason: step.Outcome.Reason})
		return nil
	}
	for _, seg := range step.Segments {
		t.emitSegment(ctx, VerifiedSegment{Text: seg, Reason: verify.ReasonVerified})
	}
	return nil
}

// openChatStream is callModel but ChatStream. An ErrUnavailable (or a generator that refuses
// to stream) lets the caller fall back to Chat without recording a failed response.
func (t *turn) openChatStream(ctx context.Context, built builtPrompt) (providers.ChatStream, ModelCall, error) {
	call := ModelCall{
		Purpose: "response", PromptToks: built.EstTokens, PromptBytes: built.Bytes,
		Capabilities: len(built.Caps), TurnsReplayed: built.TurnsKept, HistoryToks: built.HistoryToks, Trimmed: built.Trimmed,
	}
	prov, err := t.e.providers.Select(ctx, t.scope, built.Req.Role)
	if err != nil {
		call.Err = failureCode(err, contracts.ErrUnavailable)
		return nil, call, err
	}
	if prov.Generator == nil {
		call.Err = contracts.ErrUnavailable
		return nil, call, contracts.New(contracts.ErrUnavailable, "no model is available for the role")
	}
	call.Provider = prov.Name
	guarded, err := t.e.guard.Guard(ctx, GuardInput{Scope: t.scope.Clone(), Provider: ProviderInfo{Name: prov.Name, Local: prov.Local}, Request: built.Req})
	if err != nil {
		call.Err = failureCode(err, contracts.ErrPermissionDenied)
		return nil, call, contracts.Wrap(call.Err, err, "the prompt guard denied the request")
	}
	chat := guarded.ChatRequest()
	chat, err = t.e.budget.CheckChat(chat)
	if err != nil {
		call.Err = failureCode(err, contracts.ErrValidationFailed)
		return nil, call, err
	}
	stream, err := prov.Generator.ChatStream(ctx, t.scope, chat)
	if err != nil {
		call.Err = failureCode(err, contracts.ErrUnavailable)
		if isCanceled(err) {
			call.Err = ""
		}
		return nil, call, err
	}
	return stream, call, nil
}
