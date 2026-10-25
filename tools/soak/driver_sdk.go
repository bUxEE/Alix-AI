package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// sdkDriver puts the mixed load on a running stack through its public API with
// the Go SDK, as a host application would: one session of conversation turns,
// and the re-ingestion of one RAG source.
//
// Two clients, because they speak as two principals. Turns come from the
// interactive principal; Sync and Rebuild need the deployment's operator action
// manage_sources (rag.operators in the configuration), which is a principal, not
// a capability, so the ingestion client may be a different one.
type sdkDriver struct {
	turns   *sdkgo.Client
	ingest  *sdkgo.Client
	session string
	mix     IngestionMix
	// pollEvery is how often a running re-ingestion is looked at.
	pollEvery time.Duration

	sourceID string
}

// newSDKDriver wraps the two clients. ingest may be nil when the mix has no ingestion.
func newSDKDriver(turns, ingest *sdkgo.Client, session string, mix IngestionMix) (*sdkDriver, error) {
	if turns == nil || (mix.Enabled && ingest == nil) {
		return nil, errors.New("the driver needs a client for the turns and, with ingestion, one for the ingestion")
	}
	return &sdkDriver{turns: turns, ingest: ingest, session: session, mix: mix, pollEvery: time.Second}, nil
}

// Preflight implements Driver: the stack is ready, a session opens, and the
// ingestion source exists (registered if the package declares it).
func (d *sdkDriver) Preflight(ctx context.Context) error {
	rep, err := d.turns.GetReadiness(ctx)
	if err != nil {
		return fmt.Errorf("readiness: %w", err)
	}
	if !rep.Ready {
		return fmt.Errorf("the stack is not ready (status %q): it is not soaked", rep.Status)
	}
	if _, err := d.turns.CreateSession(ctx); err != nil {
		return fmt.Errorf("creating the session: %w", err)
	}
	if !d.mix.Enabled {
		return nil
	}
	src, err := d.findSource(ctx)
	if errors.Is(err, errSourceNotRegistered) {
		var reg *sdkgo.Source
		if reg, err = d.ingest.RAG().RegisterSource(ctx, d.mix.Source); err == nil {
			src = reg
		}
	}
	if err != nil {
		return fmt.Errorf("the ingestion source %q: %w", d.mix.Source, err)
	}
	d.sourceID = string(src.Status.SourceID)
	return nil
}

var errSourceNotRegistered = errors.New("the source is not registered")

// findSource looks the declared source up among the registered ones.
func (d *sdkDriver) findSource(ctx context.Context) (*sdkgo.Source, error) {
	all, err := d.ingest.RAG().ListSources(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Declaration == d.mix.Source {
			return &all[i], nil
		}
	}
	return nil, errSourceNotRegistered
}

// Turn implements Driver. A turn that ends in a prepared action is rejected, in
// the same call: the soak never confirms, so the clinic's slots last as long as
// the run does.
func (d *sdkDriver) Turn(ctx context.Context, step Step) error {
	res, err := d.turns.SendTurnJSON(ctx, d.session, sdkgo.TextTurn(step.Text).WithLanguage(step.Language))
	if err != nil {
		return err
	}
	if err := turnFailure(res); err != nil {
		return err
	}
	if res.Confirmation == nil {
		return nil
	}
	rej, err := d.turns.SendTurnJSON(ctx, d.session, sdkgo.RejectTurn(res.Confirmation.PreparationID))
	if err != nil {
		return fmt.Errorf("rejecting the prepared action: %w", err)
	}
	return turnFailure(rej)
}

// turnFailure is the error a finished turn carries, if it ended on one. A turn
// whose last event says failed is a failure even when HTTP said 200.
func turnFailure(res *sdkgo.TurnResult) error {
	var failed bool
	var code contracts.ErrCode
	for _, ev := range res.Events {
		switch v := ev.Value.(type) {
		case conversation.ErrorEvent:
			code = contracts.ErrCode(v.Error.Code)
		case conversation.DoneEvent:
			failed = v.FinishReason == conversation.DoneEventFinishReasonFailed
		}
	}
	if !failed && code == "" {
		return nil
	}
	if code == "" {
		code = contracts.ErrInternal
	}
	return contracts.Newf(code, "the turn ended with an error")
}

// Ingest implements Driver: request the re-ingestion, then wait until the
// source is serving the result. "Ready" right after the request may only mean
// the old generation still serves, so the source must either show a new
// generation (or a later last success), or stay ready for the settle time. A
// rebuild that is queued behind live work (background classes yield to a turn)
// may read ready for longer than the settle time; the recorded latency of that
// ingestion is then optimistic, while the load is not: the next request joins
// the same job (requests that arrive while one is queued or running are one job).
func (d *sdkDriver) Ingest(ctx context.Context) error {
	if d.sourceID == "" {
		return errors.New("the ingestion source was not resolved: Preflight did not run")
	}
	limit := d.mix.Timeout()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	timedOut := func(err error) error {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("the re-ingestion did not finish in %s: %w", limit, context.DeadlineExceeded)
		}
		return err
	}

	before, err := d.sourceStatus(ctx)
	if err != nil {
		return timedOut(err)
	}
	switch d.mix.Op {
	case IngestSync:
		_, err = d.ingest.RAG().SyncSource(ctx, d.sourceID)
	default:
		_, err = d.ingest.RAG().RebuildSource(ctx, d.sourceID)
	}
	if err != nil {
		return timedOut(err)
	}

	var readySince time.Time
	for {
		st, err := d.sourceStatus(ctx)
		if err != nil {
			return timedOut(err)
		}
		switch st.State {
		case rag.SourceStatusStateIndexing, rag.SourceStatusStateRegistered:
			readySince = time.Time{}
		case rag.SourceStatusStateReady:
			if sourceAdvanced(before, st) {
				return nil
			}
			if readySince.IsZero() {
				readySince = time.Now()
			}
			if time.Since(readySince) >= d.mix.Settle() {
				return nil
			}
		default:
			return contracts.Newf(contracts.ErrUnavailable, "the source is %s after the re-ingestion was requested", st.State)
		}
		if !sleep(ctx, d.pollEvery) {
			return timedOut(ctx.Err())
		}
	}
}

func (d *sdkDriver) sourceStatus(ctx context.Context) (rag.SourceStatus, error) {
	all, err := d.ingest.RAG().ListSources(ctx)
	if err != nil {
		return rag.SourceStatus{}, err
	}
	for _, s := range all {
		if string(s.Status.SourceID) == d.sourceID {
			return s.Status, nil
		}
	}
	return rag.SourceStatus{}, contracts.New(contracts.ErrUnavailable, "the ingestion source is no longer registered")
}

// sourceAdvanced reports whether the source shows the result of the
// re-ingestion: another active generation, or a later last success.
func sourceAdvanced(before, now rag.SourceStatus) bool {
	gen := func(s rag.SourceStatus) string {
		if s.ActiveGenerationID == nil {
			return ""
		}
		return string(*s.ActiveGenerationID)
	}
	ok := func(s rag.SourceStatus) string {
		if s.LastSuccessAt == nil {
			return ""
		}
		return string(*s.LastSuccessAt)
	}
	return gen(now) != gen(before) || ok(now) > ok(before)
}

// Close implements Driver.
func (d *sdkDriver) Close(ctx context.Context) {
	_, _ = d.turns.EndSession(ctx, d.session)
}
