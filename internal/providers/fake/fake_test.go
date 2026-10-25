package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func scope(tenant string) contracts.Scope {
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

func userReq(text string) providers.ChatRequest {
	return providers.ChatRequest{Messages: []providers.Message{
		{Role: providers.MsgSystem, Content: "You propose typed tasks.", Trusted: true},
		{Role: providers.MsgUser, Content: text},
	}}
}

func jsonReq(text string) providers.ChatRequest {
	r := userReq(text)
	r.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
	return r
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

func TestFakeProviderScripted(t *testing.T) {
	ctx := context.Background()

	t.Run("JSON constrained reply is parsed", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Contains("book")).Reply(`{"capability_id":"booking.create"}`)
		res, err := f.Chat(ctx, scope("t1"), jsonReq("please book me in"))
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(`{"capability_id":"booking.create"}`, string(res.JSON)); diff != "" {
			t.Fatalf("JSON mismatch (-want +got):\n%s", diff)
		}
		if res.Text != string(res.JSON) || res.TraceID == "" || res.Model != "fake-model" || res.FinishReason != providers.FinishStop {
			t.Fatalf("unexpected result %+v", res)
		}
		if res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
			t.Fatalf("usage should be estimated, got %+v", res.Usage)
		}
	})

	t.Run("unmatched call is an error not a default reply", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Contains("book")).Reply("ok")
		_, err := f.Chat(ctx, scope("t1"), userReq("something else"))
		wantCode(t, err, contracts.ErrInternal)
		if !strings.Contains(err.Error(), "no scripted reply") {
			t.Fatalf("error should say why: %v", err)
		}
		if f.Calls() != 1 {
			t.Fatalf("the unmatched call still reached the runtime; Calls = %d", f.Calls())
		}
	})

	t.Run("newest rule wins and Times steps aside", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Any()).Reply("default")
		specific := f.On(fake.Contains("hello")).Reply("specific").Times(2)
		var got []string
		for i := 0; i < 3; i++ {
			res, err := f.Chat(ctx, scope("t1"), userReq("hello there"))
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, res.Text)
		}
		if diff := cmp.Diff([]string{"specific", "specific", "default"}, got); diff != "" {
			t.Fatalf("(-want +got):\n%s", diff)
		}
		if specific.Hits() != 2 {
			t.Fatalf("Hits = %d, want 2", specific.Hits())
		}
	})

	t.Run("ReplySeq repeats the last, ReplyJSON marshals, ReplyFunc sees the call", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Contains("seq")).ReplySeq("one", "two")
		f.On(fake.Contains("obj")).ReplyJSON(map[string]any{"a": 1})
		f.On(fake.Contains("echo")).ReplyFunc(func(c fake.Call) (string, error) {
			return "tenant=" + c.Scope.TenantID, nil
		})
		var seq []string
		for i := 0; i < 3; i++ {
			res, err := f.Chat(ctx, scope("t1"), userReq("seq"))
			if err != nil {
				t.Fatal(err)
			}
			seq = append(seq, res.Text)
		}
		if diff := cmp.Diff([]string{"one", "two", "two"}, seq); diff != "" {
			t.Fatalf("(-want +got):\n%s", diff)
		}
		if res, _ := f.Chat(ctx, scope("t1"), userReq("obj")); res.Text != `{"a":1}` {
			t.Fatalf("ReplyJSON text = %q", res.Text)
		}
		if res, _ := f.Chat(ctx, scope("tenant-x"), userReq("echo")); res.Text != "tenant=tenant-x" {
			t.Fatalf("ReplyFunc text = %q", res.Text)
		}
	})

	t.Run("matchers compose", func(t *testing.T) {
		f := fake.New()
		f.On(fake.All(fake.ForTenant("t2"), fake.LastUserContains("x"), fake.Not(fake.SystemContains("never")))).Reply("hit")
		if _, err := f.Chat(ctx, scope("t1"), userReq("x")); err == nil {
			t.Fatal("tenant t1 must not match a t2 rule")
		}
		if res, err := f.Chat(ctx, scope("t2"), userReq("x")); err != nil || res.Text != "hit" {
			t.Fatalf("got %v, %v", res.Text, err)
		}
		f.On(fake.Constrained(providers.ConstraintJSONSchema)).Reply(`{}`)
		f.On(fake.ForRole(providers.RoleRouter)).Reply("routed")
		rr := userReq("whatever")
		rr.Role = providers.RoleRouter
		if res, err := f.Chat(ctx, scope("t1"), rr); err != nil || res.Text != "routed" {
			t.Fatalf("router rule: %v, %v", res.Text, err)
		}
	})

	t.Run("stream chunks reassemble the reply and carry the final summary", func(t *testing.T) {
		f := fake.New(fake.WithChunkSize(5))
		reply := `{"capability_id":"booking.create","fields":{"day":"2026-10-03"}}`
		f.On(fake.Any()).Reply(reply).Usage(11, 22).Timings(5*time.Millisecond, 40*time.Millisecond)
		s, err := f.ChatStream(ctx, scope("t1"), jsonReq("go"))
		if err != nil {
			t.Fatal(err)
		}
		var chunks int
		var sb strings.Builder
		var last providers.ChatChunk
		for {
			c, err := s.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			chunks++
			sb.WriteString(c.Delta)
			last = c
		}
		if sb.String() != reply || chunks < 3 || !last.Done {
			t.Fatalf("text %q, chunks %d, last %+v", sb.String(), chunks, last)
		}
		if last.Usage != (providers.Usage{InputTokens: 11, OutputTokens: 22}) || last.Timings.Total != 40*time.Millisecond {
			t.Fatalf("final chunk summary wrong: %+v", last)
		}
		if f.OpenStreams() != 0 {
			t.Fatalf("OpenStreams = %d after EOF", f.OpenStreams())
		}
		_ = s.Close()

		s2, err := f.ChatStream(ctx, scope("t1"), jsonReq("go again"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.Collect(s2, jsonReq("").Output)
		if err != nil || string(res.JSON) != reply || res.Usage.OutputTokens != 22 {
			t.Fatalf("Collect = %+v, %v", res, err)
		}
	})

	t.Run("WithClock advances time by the scripted latency", func(t *testing.T) {
		clk := clock.NewFake(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
		f := fake.New(fake.WithClock(clk))
		f.On(fake.Any()).Reply("x").Timings(time.Millisecond, 250*time.Millisecond)
		if _, err := f.Chat(ctx, scope("t1"), userReq("a")); err != nil {
			t.Fatal(err)
		}
		if got := clk.Now(); !got.Equal(time.Date(2026, 10, 2, 9, 0, 0, int(250*time.Millisecond), time.UTC)) {
			t.Fatalf("clock = %v", got)
		}
	})

	t.Run("embeddings are deterministic and related text is closer", func(t *testing.T) {
		f := fake.New()
		req := providers.EmbedRequest{Inputs: []string{"the dentist appointment on friday", "the dentist appointment on friday", "quarterly invoice totals"}}
		res, err := f.Embed(ctx, scope("t1"), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Dimensions != fake.DefaultEmbedDim || len(res.Vectors) != 3 {
			t.Fatalf("result shape %d dims, %d vectors", res.Dimensions, len(res.Vectors))
		}
		if diff := cmp.Diff(res.Vectors[0], res.Vectors[1]); diff != "" {
			t.Fatalf("same text must embed identically: %s", diff)
		}
		if err := res.CheckResult(req); err != nil {
			t.Fatal(err)
		}
		q, _ := f.Embed(ctx, scope("t1"), providers.EmbedRequest{Inputs: []string{"dentist friday"}, Purpose: providers.EmbedQuery})
		near, far := cosine(q.Vectors[0], res.Vectors[0]), cosine(q.Vectors[0], res.Vectors[2])
		if !(near > far) {
			t.Fatalf("related text should be closer: near %.3f far %.3f", near, far)
		}
	})

	t.Run("rerank orders by overlap and honours TopN", func(t *testing.T) {
		f := fake.New()
		req := providers.RerankRequest{
			Query:     "refund policy",
			Documents: []string{"shipping times", "our refund policy explained", "policy on pets", "refund"},
			TopN:      2,
		}
		res, err := f.Rerank(ctx, scope("t1"), req)
		if err != nil {
			t.Fatal(err)
		}
		want := []providers.RerankHit{{Index: 1, Score: 1}, {Index: 2, Score: 0.5}}
		if diff := cmp.Diff(want, res.Hits); diff != "" {
			t.Fatalf("(-want +got):\n%s", diff)
		}
	})

	t.Run("an invalid scope or request never reaches the runtime", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Any()).Reply("x")
		_, err := f.Chat(ctx, contracts.Scope{}, userReq("a"))
		wantCode(t, err, contracts.ErrScopeMissing)
		_, err = f.Embed(ctx, contracts.Scope{}, providers.EmbedRequest{Inputs: []string{"a"}})
		wantCode(t, err, contracts.ErrScopeMissing)
		bad := userReq("a")
		bad.Messages[0].Trusted = false // untrusted system message
		_, err = f.Chat(ctx, scope("t1"), bad)
		wantCode(t, err, contracts.ErrValidationFailed)
		if f.Calls() != 0 {
			t.Fatalf("rejected calls were recorded as sent: %d", f.Calls())
		}
	})
}

func TestFakeProviderFailureInjection(t *testing.T) {
	ctx := context.Background()
	newFake := func() *fake.Provider {
		f := fake.New(fake.WithChunkSize(4))
		f.On(fake.Any()).Reply(`{"capability_id":"booking.create"}`)
		return f
	}

	type op struct {
		name string
		run  func(f *fake.Provider, constrained bool) error
	}
	chat := op{"chat", func(f *fake.Provider, constrained bool) error {
		req := userReq("x")
		if constrained {
			req = jsonReq("x")
		}
		_, err := f.Chat(ctx, scope("t1"), req)
		return err
	}}
	stream := op{"stream", func(f *fake.Provider, constrained bool) error {
		req := userReq("x")
		if constrained {
			req = jsonReq("x")
		}
		s, err := f.ChatStream(ctx, scope("t1"), req)
		if err != nil {
			return err
		}
		res, err := providers.Collect(s, req.Output)
		if err == nil && res.Text == "" {
			t.Error("Collect returned success without text")
		}
		return err
	}}
	embed := op{"embed", func(f *fake.Provider, _ bool) error {
		_, err := f.Embed(ctx, scope("t1"), providers.EmbedRequest{Inputs: []string{"x"}})
		return err
	}}
	rerank := op{"rerank", func(f *fake.Provider, _ bool) error {
		_, err := f.Rerank(ctx, scope("t1"), providers.RerankRequest{Query: "q", Documents: []string{"d"}})
		return err
	}}

	tests := []struct {
		kind        fake.FailKind
		constrained bool
		ops         []op
		code        contracts.ErrCode
		retryable   bool
	}{
		{fake.FailTimeout, false, []op{chat, stream, embed, rerank}, contracts.ErrTimeout, true},
		{fake.FailUnavailable, false, []op{chat, stream, embed, rerank}, contracts.ErrUnavailable, true},
		{fake.FailResourceExhausted, false, []op{chat, stream, embed, rerank}, contracts.ErrResourceExhausted, true},
		{fake.FailMalformedJSON, true, []op{chat, stream, embed, rerank}, contracts.ErrSchemaInvalid, false},
		{fake.FailPartialStream, false, []op{chat, stream, embed, rerank}, contracts.ErrUnavailable, true},
	}
	for _, tt := range tests {
		for _, o := range tt.ops {
			t.Run(string(tt.kind)+"/"+o.name, func(t *testing.T) {
				f := newFake()
				f.FailNext(tt.kind)
				err := o.run(f, tt.constrained)
				wantCode(t, err, tt.code)
				var ce *contracts.Error
				if !errors.As(err, &ce) || ce.Retryable != tt.retryable {
					t.Fatalf("retryable = %v, want %v (%v)", ce != nil && ce.Retryable, tt.retryable, err)
				}
				if f.OpenStreams() != 0 {
					t.Fatalf("a failed call left %d open stream(s)", f.OpenStreams())
				}
				// FailNext is consumed: the next call succeeds.
				if err := o.run(f, tt.constrained); err != nil {
					t.Fatalf("second call should succeed, got %v", err)
				}
			})
		}
	}

	t.Run("timeout wraps context.DeadlineExceeded", func(t *testing.T) {
		f := newFake()
		f.FailNext(fake.FailTimeout)
		err := chat.run(f, false)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("errors.Is(err, DeadlineExceeded) = false for %v", err)
		}
	})

	t.Run("malformed JSON on an unconstrained request is just text", func(t *testing.T) {
		f := newFake()
		f.FailNext(fake.FailMalformedJSON)
		res, err := f.Chat(ctx, scope("t1"), userReq("x"))
		if err != nil || res.Text != fake.MalformedJSON || res.JSON != nil {
			t.Fatalf("got %+v, %v", res, err)
		}
	})

	t.Run("partial stream delivers some chunks, then an error, never a result", func(t *testing.T) {
		f := newFake()
		f.FailNext(fake.FailPartialStream)
		s, err := f.ChatStream(ctx, scope("t1"), userReq("x"))
		if err != nil {
			t.Fatal(err)
		}
		var got int
		var final error
		for {
			c, err := s.Recv()
			if err != nil {
				final = err
				break
			}
			if c.Done {
				t.Fatal("partial stream reached its Done chunk")
			}
			got++
		}
		if got < 1 {
			t.Fatalf("a partial stream should deliver at least one chunk, got %d", got)
		}
		wantCode(t, final, contracts.ErrUnavailable)
		// The same stream through Collect: no text comes back, only the length.
		f.FailNext(fake.FailPartialStream)
		s2, _ := f.ChatStream(ctx, scope("t1"), userReq("x"))
		res, err := providers.Collect(s2, providers.OutputConstraint{})
		wantCode(t, err, contracts.ErrUnavailable)
		if res.Text != "" || len(res.JSON) != 0 {
			t.Fatalf("Collect leaked partial output %+v", res)
		}
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Details["partial_len"] == nil {
			t.Fatalf("expected partial_len detail, got %+v", ce)
		}
	})

	t.Run("FailTimes queues failures in order and a rule can fail", func(t *testing.T) {
		f := newFake()
		f.FailTimes(fake.FailUnavailable, 2)
		for i := 0; i < 2; i++ {
			wantCode(t, chat.run(f, false), contracts.ErrUnavailable)
		}
		if err := chat.run(f, false); err != nil {
			t.Fatal(err)
		}
		f.On(fake.Contains("boom")).Fail(fake.FailResourceExhausted)
		_, err := f.Chat(ctx, scope("t1"), userReq("boom"))
		wantCode(t, err, contracts.ErrResourceExhausted)
	})

	t.Run("a queued failure does not consume a rule's Times", func(t *testing.T) {
		f := fake.New()
		r := f.On(fake.Any()).Reply("ok").Times(1)
		f.FailNext(fake.FailUnavailable)
		wantCode(t, chat.run(f, false), contracts.ErrUnavailable)
		if err := chat.run(f, false); err != nil || r.Hits() != 1 {
			t.Fatalf("rule should still have its match: err %v hits %d", err, r.Hits())
		}
	})
}

func TestFakeProviderCapturesPrompts(t *testing.T) {
	ctx := context.Background()
	f := fake.New()
	f.On(fake.Any()).Reply(`{}`)

	schema := json.RawMessage(`{"type":"object","title":"CLINIC-A-CAPABILITIES"}`)
	req := providers.ChatRequest{
		Role:    providers.RoleController,
		Profile: "lite",
		Messages: []providers.Message{
			{Role: providers.MsgSystem, Content: "system rules", Trusted: true},
			{Role: providers.MsgUser, Content: "user says TENANT-A-SECRET"},
			{Role: providers.MsgTool, Content: "document text from tool"},
		},
		Output:      providers.SchemaConstraint(schema),
		MaxTokens:   256,
		Temperature: 0.2,
		Stop:        []string{"###"},
	}
	if _, err := f.Chat(ctx, scope("tenant-a"), req); err != nil {
		t.Fatal(err)
	}
	s, err := f.ChatStream(ctx, scope("tenant-b"), userReq("tenant b asks"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := f.Embed(ctx, scope("tenant-b"), providers.EmbedRequest{Inputs: []string{"TENANT-B-DOC one", "TENANT-B-DOC two"}, Purpose: providers.EmbedDocument}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Rerank(ctx, scope("tenant-b"), providers.RerankRequest{Query: "TENANT-B-QUERY", Documents: []string{"d1", "d2"}}); err != nil {
		t.Fatal(err)
	}

	got := f.Prompts()
	if len(got) != 4 || f.Calls() != 4 {
		t.Fatalf("captured %d prompts (Calls %d), want 4", len(got), f.Calls())
	}
	ops := []fake.Op{got[0].Op, got[1].Op, got[2].Op, got[3].Op}
	if diff := cmp.Diff([]fake.Op{fake.OpChat, fake.OpChatStream, fake.OpEmbed, fake.OpRerank}, ops); diff != "" {
		t.Fatalf("ops (-want +got):\n%s", diff)
	}
	for i, p := range got {
		if p.Seq != i+1 {
			t.Fatalf("prompt %d has Seq %d", i, p.Seq)
		}
	}

	first := got[0]
	if first.Scope.TenantID != "tenant-a" || first.Scope.SessionID != "ses_1" {
		t.Fatalf("scope not captured: %+v", first.Scope)
	}
	if diff := cmp.Diff(req, first.Chat); diff != "" {
		t.Fatalf("request not captured verbatim, incl. Trusted markers and constraint (-want +got):\n%s", diff)
	}
	if first.Chat.Messages[1].Trusted || !first.Chat.Messages[0].Trusted {
		t.Fatal("Trusted markers were not preserved")
	}

	// The M2 gate's question: did tenant B's text ever reach a prompt that ran as tenant A?
	for _, p := range got {
		if p.Scope.TenantID == "tenant-a" && (p.Contains("TENANT-B-DOC") || p.Contains("TENANT-B-QUERY")) {
			t.Fatalf("tenant B text reached a tenant A prompt: %+v", p)
		}
	}
	// Schema text counts as sent text; embed and rerank inputs are searchable too.
	if n := len(f.Containing("CLINIC-A-CAPABILITIES")); n != 1 {
		t.Fatalf("schema text found in %d prompts, want 1", n)
	}
	if n := len(f.Containing("TENANT-B-DOC")); n != 1 {
		t.Fatalf("embed inputs found in %d prompts, want 1", n)
	}
	if n := len(f.Containing("TENANT-B-QUERY")); n != 1 {
		t.Fatalf("rerank query found in %d prompts, want 1", n)
	}
	if n := len(f.Containing("TENANT-A-SECRET")); n != 1 || f.Containing("TENANT-A-SECRET")[0].Scope.TenantID != "tenant-a" {
		t.Fatalf("secret found in %d prompts", n)
	}

	// The record is immune to later mutation by the caller or by the reader.
	req.Messages[1].Content = "mutated after the call"
	req.Stop[0] = "mutated"
	schema[2] = 'X'
	got[0].Chat.Messages[0].Content = "mutated copy"
	again := f.Prompts()
	if again[0].Chat.Messages[1].Content != "user says TENANT-A-SECRET" || again[0].Chat.Stop[0] != "###" ||
		again[0].Chat.Messages[0].Content != "system rules" || strings.Contains(string(again[0].Chat.Output.JSONSchema), "XX") {
		t.Fatalf("captured prompts alias caller memory: %+v", again[0].Chat)
	}

	f.ClearPrompts()
	if f.Calls() != 0 || len(f.Prompts()) != 0 {
		t.Fatal("ClearPrompts did not clear")
	}
}

func TestContextCancellationStopsStream(t *testing.T) {
	long := strings.Repeat("0123456789", 20)

	t.Run("cancel after the first chunk produces no further chunks", func(t *testing.T) {
		f := fake.New(fake.WithChunkSize(10))
		f.On(fake.Any()).Reply(long)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := f.ChatStream(ctx, scope("t1"), userReq("go"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
		if f.OpenStreams() != 1 {
			t.Fatalf("OpenStreams = %d mid-stream, want 1", f.OpenStreams())
		}
		cancel()
		for i := 0; i < 3; i++ { // stays stopped
			_, err := s.Recv()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Recv %d after cancel = %v, want context.Canceled", i, err)
			}
		}
		if got := f.ChunksDelivered(); got != 1 {
			t.Fatalf("ChunksDelivered = %d, want exactly 1 (no chunk after cancel)", got)
		}
		if f.OpenStreams() != 0 {
			t.Fatalf("OpenStreams = %d after cancel, want 0", f.OpenStreams())
		}
	})

	t.Run("cancel unblocks a stalled Recv", func(t *testing.T) {
		f := fake.New(fake.WithChunkSize(10))
		f.On(fake.Any()).Reply(long).StallAfter(2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := f.ChatStream(ctx, scope("t1"), userReq("go"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for i := 0; i < 2; i++ {
			if _, err := s.Recv(); err != nil {
				t.Fatal(err)
			}
		}
		result := make(chan error, 1)
		go func() { _, err := s.Recv(); result <- err }() // blocks: the runtime is hung
		select {
		case err := <-result:
			t.Fatalf("Recv returned before cancel: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("stalled Recv = %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Recv stayed blocked after cancel")
		}
		if f.ChunksDelivered() != 2 || f.OpenStreams() != 0 {
			t.Fatalf("chunks %d open %d, want 2 and 0", f.ChunksDelivered(), f.OpenStreams())
		}
	})

	t.Run("deadline during a stall is timeout", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Any()).Reply(long).StallAfter(0)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		s, err := f.ChatStream(ctx, scope("t1"), userReq("go"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		_, err = s.Recv()
		wantCode(t, err, contracts.ErrTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout should wrap DeadlineExceeded: %v", err)
		}
	})

	t.Run("Close unblocks a stalled Recv and is idempotent", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Any()).Reply(long).StallAfter(0)
		s, err := f.ChatStream(context.Background(), scope("t1"), userReq("go"))
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, err := s.Recv(); result <- err }()
		time.Sleep(10 * time.Millisecond)
		_ = s.Close()
		_ = s.Close()
		select {
		case err := <-result:
			if !errors.Is(err, providers.ErrStreamClosed) {
				t.Fatalf("Recv after Close = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Close did not unblock Recv")
		}
		if f.OpenStreams() != 0 {
			t.Fatalf("OpenStreams = %d after Close", f.OpenStreams())
		}
	})

	t.Run("an already-cancelled context never starts a call", func(t *testing.T) {
		f := fake.New()
		f.On(fake.Any()).Reply(long)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.ChatStream(ctx, scope("t1"), userReq("go")); !errors.Is(err, context.Canceled) {
			t.Fatalf("ChatStream = %v", err)
		}
		if _, err := f.Chat(ctx, scope("t1"), userReq("go")); !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat = %v", err)
		}
		if f.Calls() != 0 {
			t.Fatalf("a cancelled call reached the runtime: %d", f.Calls())
		}
	})

	t.Run("Collect on a cancelled stream returns the error and no text", func(t *testing.T) {
		f := fake.New(fake.WithChunkSize(10))
		f.On(fake.Any()).Reply(long).StallAfter(3)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		s, err := f.ChatStream(ctx, scope("t1"), userReq("go"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.Collect(s, providers.OutputConstraint{})
		wantCode(t, err, contracts.ErrTimeout)
		if res.Text != "" {
			t.Fatalf("Collect returned partial text %q", res.Text)
		}
		if f.OpenStreams() != 0 {
			t.Fatal("Collect left the stream open")
		}
	})
}

func TestFakeHealthAndNames(t *testing.T) {
	f := fake.New(fake.WithName("scripted"))
	if f.Name() != "scripted" {
		t.Fatalf("Name = %q", f.Name())
	}
	rep, err := f.Health(context.Background())
	if err != nil || rep.Status != providers.HealthOK {
		t.Fatalf("default health = %+v, %v", rep, err)
	}
	f.SetHealth(providers.HealthDegraded, "warming up")
	rep, _ = f.Health(context.Background())
	if rep.Status != providers.HealthDegraded || rep.Reason != "warming up" {
		t.Fatalf("health = %+v", rep)
	}
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot // both vectors are unit length
}

func TestFakeOptionsAndHelpers(t *testing.T) {
	ctx := context.Background()

	f := fake.New(fake.WithModel("qwen-test"), fake.WithEmbedDim(8), fake.WithRerankFunc(func(r providers.RerankRequest) ([]providers.RerankHit, error) {
		// Deliberately unsorted: the fake normalises like any runtime.
		return []providers.RerankHit{{Index: 0, Score: 0.1}, {Index: 1, Score: 0.9}}, nil
	}))
	f.On(fake.OneOf(fake.Contains("alpha"), fake.Contains("beta"))).Reply("ab")
	f.On(fake.Constrained(providers.ConstraintNone)).Reply("plain").Times(1)

	res, err := f.Chat(ctx, scope("t1"), userReq("beta"))
	if err != nil || res.Text != "plain" || res.Model != "qwen-test" {
		t.Fatalf("newest rule (unconstrained) should win first: %+v, %v", res, err)
	}
	if res, err = f.Chat(ctx, scope("t1"), userReq("beta")); err != nil || res.Text != "ab" {
		t.Fatalf("OneOf rule after Times exhausted: %+v, %v", res, err)
	}
	if _, err = f.Chat(ctx, scope("t1"), userReq("gamma")); err == nil {
		t.Fatal("no rule matches gamma")
	}

	emb, err := f.Embed(ctx, scope("t1"), providers.EmbedRequest{Inputs: []string{"x"}})
	if err != nil || emb.Dimensions != 8 || len(fake.EmbedVector("x", 8)) != 8 {
		t.Fatalf("WithEmbedDim ignored: %+v, %v", emb, err)
	}
	if diff := cmp.Diff(emb.Vectors[0], fake.EmbedVector("x", 8)); diff != "" {
		t.Fatalf("Embed must equal EmbedVector: %s", diff)
	}
	rr, err := f.Rerank(ctx, scope("t1"), providers.RerankRequest{Query: "q", Documents: []string{"a", "b"}})
	if err != nil || rr.Hits[0].Index != 1 {
		t.Fatalf("WithRerankFunc result not normalised: %+v, %v", rr, err)
	}

	wantCode(t, mustErr(f.Embed(ctx, scope("t1"), providers.EmbedRequest{})), contracts.ErrValidationFailed)
	wantCode(t, mustErr(f.Embed(ctx, scope("t1"), providers.EmbedRequest{Inputs: []string{"  "}})), contracts.ErrValidationFailed)
	wantCode(t, mustErr(f.Rerank(ctx, scope("t1"), providers.RerankRequest{Query: "q"})), contracts.ErrValidationFailed)

	f.FailNext(fake.FailUnavailable)
	f.Reset()
	if f.Calls() != 0 {
		t.Fatal("Reset keeps prompts")
	}
	if _, err := f.Chat(ctx, scope("t1"), userReq("alpha")); err == nil || contracts.CodeOf(err) != contracts.ErrInternal {
		t.Fatalf("Reset must drop rules and queued failures: %v", err)
	}

	for name, fn := range map[string]func(){
		"FailNext":   func() { f.FailNext("bogus") },
		"Rule.Fail":  func() { f.On(nil).Fail("bogus") },
		"ReplySeq":   func() { f.On(nil).ReplySeq() },
		"StallAfter": func() { f.On(nil).StallAfter(-1) },
		"ReplyJSON":  func() { f.On(nil).ReplyJSON(make(chan int)) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s should panic on misuse", name)
				}
			}()
			fn()
		}()
	}
}

func mustErr[T any](_ T, err error) error { return err }

func TestFakeConcurrentUse(t *testing.T) {
	f := fake.New(fake.WithChunkSize(3))
	f.On(fake.Any()).Reply("concurrent reply")
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 50; i++ {
				switch i % 3 {
				case 0:
					_, _ = f.Chat(context.Background(), scope("t1"), userReq("x"))
				case 1:
					if s, err := f.ChatStream(context.Background(), scope("t2"), userReq("y")); err == nil {
						_, _ = providers.Collect(s, providers.OutputConstraint{})
					}
				default:
					_ = f.Prompts()
				}
			}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	if f.OpenStreams() != 0 {
		t.Fatalf("OpenStreams = %d", f.OpenStreams())
	}
	if f.Calls() == 0 {
		t.Fatal("no calls recorded")
	}
}
