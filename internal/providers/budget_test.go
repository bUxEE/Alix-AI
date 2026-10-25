package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func testScope(tenant string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_1",
		ApplicationID:    "app_clinic",
		TenantID:         tenant,
		WorkspaceID:      "ws_main",
		PrincipalID:      "usr_1",
		Roles:            []string{"receptionist"},
		CapabilityScopes: []string{"booking.create"},
		SessionID:        "ses_1",
	}
}

func chatReq(userText string) providers.ChatRequest {
	return providers.ChatRequest{Messages: []providers.Message{
		{Role: providers.MsgSystem, Content: "You propose typed tasks.", Trusted: true},
		{Role: providers.MsgUser, Content: userText},
	}}
}

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want code %s", code)
	}
	if !contracts.HasCode(err, code) {
		t.Fatalf("error %v has code %s, want %s", err, contracts.CodeOf(err), code)
	}
}

func smallBudget() providers.Budget {
	return providers.Budget{MaxPromptTokens: 100, MaxOutputTokens: 50, MaxBatchItems: 3, Timeout: time.Second}
}

// SEC-076: prompt budgets are enforced before the runtime is called.
func TestBudgetRejectsOversizePrompt(t *testing.T) {
	ctx := context.Background()
	inner := fake.New()
	inner.On(fake.Any()).Reply("fine")
	g, err := providers.WithBudget(inner, smallBudget())
	if err != nil {
		t.Fatal(err)
	}

	// A 500-turn history is far over a 100-token prompt budget.
	var history []providers.Message
	history = append(history, providers.Message{Role: providers.MsgSystem, Content: "rules", Trusted: true})
	for i := 0; i < 500; i++ {
		history = append(history, providers.Message{Role: providers.MsgUser, Content: "message number " + strings.Repeat("x", 20)})
	}

	for name, call := range map[string]func() error{
		"chat": func() error {
			_, err := g.Chat(ctx, testScope("t1"), providers.ChatRequest{Messages: history})
			return err
		},
		"stream": func() error {
			s, err := g.ChatStream(ctx, testScope("t1"), providers.ChatRequest{Messages: history})
			if s != nil {
				_ = s.Close()
			}
			return err
		},
		"one huge message": func() error {
			_, err := g.Chat(ctx, testScope("t1"), chatReq(strings.Repeat("a", 10_000)))
			return err
		},
		"huge schema counts too": func() error {
			r := chatReq("hi")
			r.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object","description":"` + strings.Repeat("d", 5000) + `"}`))
			_, err := g.Chat(ctx, testScope("t1"), r)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			wantCode(t, err, contracts.ErrValidationFailed)
			which, ok := providers.IsBudgetViolation(err)
			if !ok || which != "max_prompt_tokens" {
				t.Fatalf("IsBudgetViolation = %q, %v", which, ok)
			}
			var ce *contracts.Error
			if !errors.As(err, &ce) || ce.Retryable {
				t.Fatalf("an oversize prompt must not be retryable: %+v", ce)
			}
			if ce.Details["limit"] != 100 || ce.Details["requested"].(int) <= 100 {
				t.Fatalf("details should report limit and requested size: %+v", ce.Details)
			}
		})
	}

	// The point of "before calling the runtime": the runtime saw nothing.
	if inner.Calls() != 0 || len(inner.Prompts()) != 0 {
		t.Fatalf("oversize prompts reached the runtime: %d call(s)", inner.Calls())
	}

	// A prompt inside the budget goes through, with MaxTokens defaulted to the cap.
	if _, err := g.Chat(ctx, testScope("t1"), chatReq("short")); err != nil {
		t.Fatal(err)
	}
	got := inner.Prompts()
	if len(got) != 1 || got[0].Chat.MaxTokens != 50 {
		t.Fatalf("in-budget call: %d prompts, MaxTokens %d, want 1 and 50", len(got), got[0].Chat.MaxTokens)
	}
}

func TestBudgetOutputCapAndValidation(t *testing.T) {
	ctx := context.Background()
	inner := fake.New()
	inner.On(fake.Any()).Reply("ok")
	g, _ := providers.WithBudget(inner, smallBudget())

	over := chatReq("hi")
	over.MaxTokens = 51
	_, err := g.Chat(ctx, testScope("t1"), over)
	wantCode(t, err, contracts.ErrValidationFailed)
	if which, _ := providers.IsBudgetViolation(err); which != "max_output_tokens" {
		t.Fatalf("budget = %q", which)
	}
	exact := chatReq("hi")
	exact.MaxTokens = 50
	if _, err := g.Chat(ctx, testScope("t1"), exact); err != nil {
		t.Fatalf("a request at the cap must pass: %v", err)
	}

	// Scope is checked before anything else; an invalid request is rejected too.
	_, err = g.Chat(ctx, contracts.Scope{}, chatReq("hi"))
	wantCode(t, err, contracts.ErrScopeMissing)
	_, err = g.Chat(ctx, testScope("t1"), providers.ChatRequest{})
	wantCode(t, err, contracts.ErrValidationFailed)
	if inner.Calls() != 1 {
		t.Fatalf("only the one valid call may reach the runtime, got %d", inner.Calls())
	}

	// A budget with a zero limit is refused outright: there is no "unlimited".
	for _, b := range []providers.Budget{
		{}, {MaxPromptTokens: 1, MaxOutputTokens: 1, MaxBatchItems: 1},
		{MaxPromptTokens: 1, MaxOutputTokens: 1, Timeout: time.Second},
		{MaxPromptTokens: -1, MaxOutputTokens: 1, MaxBatchItems: 1, Timeout: time.Second},
	} {
		if _, err := providers.WithBudget(inner, b); err == nil {
			t.Fatalf("budget %+v should be rejected", b)
		}
	}
	if err := providers.DefaultBudget().Validate(); err != nil {
		t.Fatalf("DefaultBudget invalid: %v", err)
	}
	if _, err := providers.WithBudget(nil, smallBudget()); err == nil {
		t.Fatal("nil generator should be rejected")
	}

	// A custom estimator is honoured (and cannot report zero for real text).
	b := smallBudget()
	b.Estimator = func(string) int { return 1000 }
	if _, err := b.CheckChat(chatReq("hi")); err == nil {
		t.Fatal("custom estimator ignored")
	}
	b.Estimator = func(string) int { return 0 }
	if got := b.EstimateChat(chatReq("hi")); got < 2 {
		t.Fatalf("zero estimates must be raised to 1 per non-empty string, got %d", got)
	}
}

func TestBudgetTimeoutIsEnforced(t *testing.T) {
	ctx := context.Background()
	b := smallBudget()
	b.Timeout = 30 * time.Millisecond

	t.Run("chat", func(t *testing.T) {
		inner := fake.New()
		inner.On(fake.Any()).Reply("x").StallAfter(0)
		g, _ := providers.WithBudget(inner, b)
		start := time.Now()
		_, err := g.Chat(ctx, testScope("t1"), chatReq("hi"))
		wantCode(t, err, contracts.ErrTimeout)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatalf("err %v after %v", err, time.Since(start))
		}
	})

	t.Run("stream deadline covers the whole stream and is released on end", func(t *testing.T) {
		inner := fake.New(fake.WithChunkSize(2))
		inner.On(fake.Any()).Reply("abcdefgh").StallAfter(2)
		g, _ := providers.WithBudget(inner, b)
		s, err := g.ChatStream(ctx, testScope("t1"), chatReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for i := 0; i < 2; i++ {
			if _, err := s.Recv(); err != nil {
				t.Fatal(err)
			}
		}
		_, err = s.Recv()
		wantCode(t, err, contracts.ErrTimeout)
		if inner.OpenStreams() != 0 {
			t.Fatalf("OpenStreams = %d", inner.OpenStreams())
		}
	})

	t.Run("the caller's shorter deadline wins and cancellation passes through", func(t *testing.T) {
		inner := fake.New()
		inner.On(fake.Any()).Reply("x").StallAfter(0)
		long := smallBudget()
		long.Timeout = time.Hour
		g, _ := providers.WithBudget(inner, long)
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		_, err := g.Chat(cctx, testScope("t1"), chatReq("hi"))
		wantCode(t, err, contracts.ErrTimeout)

		cctx2, cancel2 := context.WithCancel(ctx)
		cancel2()
		_, err = g.Chat(cctx2, testScope("t1"), chatReq("hi"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	})

	t.Run("a finished stream through the wrapper still collects", func(t *testing.T) {
		inner := fake.New(fake.WithChunkSize(3))
		inner.On(fake.Any()).Reply(`{"a":1}`)
		g, _ := providers.WithBudget(inner, smallBudget())
		r := chatReq("hi")
		r.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
		s, err := g.ChatStream(ctx, testScope("t1"), r)
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.Collect(s, r.Output)
		if err != nil || string(res.JSON) != `{"a":1}` {
			t.Fatalf("Collect = %+v, %v", res, err)
		}
		if _, err := s.Recv(); err == nil {
			t.Fatal("Recv after Collect closed the stream should fail")
		} else if err == io.EOF {
			t.Fatal("a closed stream must not report a clean EOF")
		}
	})
}

type badJSONGenerator struct{ providers.TextGenerator }

func (badJSONGenerator) Chat(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatResult, error) {
	return providers.ChatResult{Text: `{"half":`, JSON: json.RawMessage(`{"half":`)}, nil
}

func TestBudgetVerifiesConstrainedResults(t *testing.T) {
	g, _ := providers.WithBudget(badJSONGenerator{}, smallBudget())
	r := chatReq("hi")
	r.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
	res, err := g.Chat(context.Background(), testScope("t1"), r)
	wantCode(t, err, contracts.ErrSchemaInvalid)
	if res.Text != "" || res.JSON != nil {
		t.Fatalf("an unparseable constrained result must not be returned: %+v", res)
	}
	// Unconstrained text passes through untouched.
	if _, err := g.Chat(context.Background(), testScope("t1"), chatReq("hi")); err != nil {
		t.Fatalf("unconstrained: %v", err)
	}
}

func TestBudgetEmbedAndRerank(t *testing.T) {
	ctx := context.Background()
	inner := fake.New()
	e, err := providers.WithEmbedBudget(inner, smallBudget())
	if err != nil {
		t.Fatal(err)
	}
	r, err := providers.WithRerankBudget(inner, smallBudget())
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.Embed(ctx, testScope("t1"), providers.EmbedRequest{Inputs: []string{"a", "b", "c", "d"}})
	wantCode(t, err, contracts.ErrValidationFailed)
	if which, _ := providers.IsBudgetViolation(err); which != "max_batch_items" {
		t.Fatalf("budget = %q", which)
	}
	_, err = e.Embed(ctx, testScope("t1"), providers.EmbedRequest{Inputs: []string{strings.Repeat("w ", 300)}})
	wantCode(t, err, contracts.ErrValidationFailed)
	if which, _ := providers.IsBudgetViolation(err); which != "max_prompt_tokens" {
		t.Fatalf("budget = %q", which)
	}
	_, err = r.Rerank(ctx, testScope("t1"), providers.RerankRequest{Query: "q", Documents: []string{"a", "b", "c", "d"}})
	wantCode(t, err, contracts.ErrValidationFailed)
	_, err = r.Rerank(ctx, testScope("t1"), providers.RerankRequest{Query: strings.Repeat("q ", 300), Documents: []string{"a"}})
	wantCode(t, err, contracts.ErrValidationFailed)
	_, err = e.Embed(ctx, contracts.Scope{}, providers.EmbedRequest{Inputs: []string{"a"}})
	wantCode(t, err, contracts.ErrScopeMissing)
	if inner.Calls() != 0 {
		t.Fatalf("rejected calls reached the runtime: %d", inner.Calls())
	}

	res, err := e.Embed(ctx, testScope("t1"), providers.EmbedRequest{Inputs: []string{"hello world"}})
	if err != nil || len(res.Vectors) != 1 {
		t.Fatalf("Embed = %+v, %v", res, err)
	}
	rr, err := r.Rerank(ctx, testScope("t1"), providers.RerankRequest{Query: "hello", Documents: []string{"nothing", "hello there"}})
	if err != nil || rr.Hits[0].Index != 1 {
		t.Fatalf("Rerank = %+v, %v", rr, err)
	}
}

// rawReranker returns its hits untouched, like a misbehaving runtime.
type rawReranker struct{ hits []providers.RerankHit }

func (r rawReranker) Rerank(context.Context, contracts.Scope, providers.RerankRequest) (providers.RerankResult, error) {
	return providers.RerankResult{Hits: r.hits}, nil
}

// NaN in a vector index poisons every search: the wrapper refuses it.
func TestBudgetRejectsMalformedEmbedAndRerankResults(t *testing.T) {
	ctx := context.Background()
	nan := func() float32 { var z float32; return z / z }()

	bad := fake.New(fake.WithEmbedFunc(func(providers.EmbedRequest) ([][]float32, error) {
		return [][]float32{{1, nan}}, nil
	}))
	e, _ := providers.WithEmbedBudget(bad, smallBudget())
	_, err := e.Embed(ctx, testScope("t1"), providers.EmbedRequest{Inputs: []string{"a"}})
	wantCode(t, err, contracts.ErrSchemaInvalid)

	short := fake.New(fake.WithEmbedFunc(func(providers.EmbedRequest) ([][]float32, error) {
		return [][]float32{{1, 0}}, nil
	}))
	e, _ = providers.WithEmbedBudget(short, smallBudget())
	_, err = e.Embed(ctx, testScope("t1"), providers.EmbedRequest{Inputs: []string{"a", "b"}})
	wantCode(t, err, contracts.ErrSchemaInvalid)

	oob := rawReranker{hits: []providers.RerankHit{{Index: 7, Score: 1}}}
	r, _ := providers.WithRerankBudget(oob, smallBudget())
	_, err = r.Rerank(ctx, testScope("t1"), providers.RerankRequest{Query: "q", Documents: []string{"a"}})
	wantCode(t, err, contracts.ErrSchemaInvalid)

	dup := rawReranker{hits: []providers.RerankHit{{Index: 0, Score: 1}, {Index: 0, Score: 2}}}
	r, _ = providers.WithRerankBudget(dup, smallBudget())
	_, err = r.Rerank(ctx, testScope("t1"), providers.RerankRequest{Query: "q", Documents: []string{"a", "b"}})
	wantCode(t, err, contracts.ErrSchemaInvalid)
}
