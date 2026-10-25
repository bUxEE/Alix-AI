package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func richBudget() providers.Budget {
	return providers.Budget{MaxPromptTokens: 4000, MaxOutputTokens: 500, MaxBatchItems: 3, Timeout: time.Second}
}

func bookTool() providers.ToolSpec {
	return providers.ToolSpec{
		Name:        "book",
		Description: "propose a booking",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"slot":{"type":"string"}}}`),
	}
}

func manyParts(n int) []providers.ContentPart {
	out := make([]providers.ContentPart, n)
	for i := range out {
		out[i] = providers.TextPart("x")
	}
	return out
}

func richReq(parts ...providers.ContentPart) providers.RichRequest {
	if len(parts) == 0 {
		parts = []providers.ContentPart{providers.TextPart("hello")}
	}
	return providers.RichRequest{Messages: []providers.RichMessage{
		{Role: providers.MsgSystem, Parts: []providers.ContentPart{providers.TextPart("You propose typed tasks.")}, Trusted: true},
		{Role: providers.MsgUser, Parts: parts},
	}}
}

func richGen(t *testing.T, f providers.MultimodalGenerator, b providers.Budget) providers.MultimodalGenerator {
	t.Helper()
	g, err := providers.WithRichBudget(f, b)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// walkTypes visits every type reachable from t through fields, elements and
// map values.
func walkTypes(t reflect.Type, seen map[reflect.Type]bool, visit func(path string, f reflect.StructField, ft reflect.Type)) {
	if seen[t] {
		return
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		walkTypes(t.Elem(), seen, visit)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			visit(t.Name(), f, f.Type)
			walkTypes(f.Type, seen, visit)
		}
	}
}

// A tool call is a proposal: nothing in the types or the interface can run one
// (master plan rule 7, additive plan section 3).
func TestToolCallIsProposalOnly(t *testing.T) {
	// Structure: no function, channel or interface member anywhere in what a
	// provider receives or returns, so there is no handle through which a tool
	// or an adapter could be invoked.
	for _, typ := range []reflect.Type{
		reflect.TypeOf(providers.ToolSpec{}),
		reflect.TypeOf(providers.ToolCallProposal{}),
		reflect.TypeOf(providers.RichRequest{}),
		reflect.TypeOf(providers.RichResult{}),
		reflect.TypeOf(providers.RichChunk{}),
	} {
		walkTypes(typ, map[reflect.Type]bool{}, func(path string, f reflect.StructField, ft reflect.Type) {
			switch ft.Kind() {
			case reflect.Func, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
				t.Errorf("%s.%s is a %s: a provider-facing type must be plain data", path, f.Name, ft.Kind())
			}
		})
		for _, rt := range []reflect.Type{typ, reflect.PointerTo(typ)} {
			for i := 0; i < rt.NumMethod(); i++ {
				name := rt.Method(i).Name
				for _, bad := range []string{"Exec", "Invoke", "Run", "Call", "Dispatch", "Do"} {
					if strings.HasPrefix(name, bad) {
						t.Errorf("%s has method %s: nothing here may execute", rt, name)
					}
				}
			}
		}
	}
	gen := reflect.TypeOf((*providers.MultimodalGenerator)(nil)).Elem()
	if gen.NumMethod() != 2 {
		t.Fatalf("MultimodalGenerator has %d methods, want Generate and GenerateStream", gen.NumMethod())
	}
	for i := 0; i < gen.NumMethod(); i++ {
		m := gen.Method(i)
		for j := 0; j < m.Type.NumIn(); j++ {
			in := m.Type.In(j)
			if in.Kind() == reflect.Func || in.Kind() == reflect.Chan {
				t.Errorf("%s takes a %s", m.Name, in.Kind())
			}
		}
	}

	scope := testScope("ten_a")
	ctx := context.Background()

	t.Run("an offered tool comes back as a candidate and nothing runs", func(t *testing.T) {
		f := fake.New()
		f.On(fake.OffersTool("book")).ProposeToolCall("call_1", "book", `{"slot":"10:00"}`)
		g := richGen(t, f, richBudget())
		req := richReq()
		req.Tools = []providers.ToolSpec{bookTool()}
		res, err := g.Generate(ctx, scope, req)
		if err != nil {
			t.Fatal(err)
		}
		want := []providers.ToolCallProposal{{ID: "call_1", Name: "book", Arguments: json.RawMessage(`{"slot":"10:00"}`)}}
		if diff := cmp.Diff(want, res.ToolCalls); diff != "" {
			t.Fatalf("proposals (-want +got):\n%s", diff)
		}
		if res.FinishReason != providers.FinishToolCalls {
			t.Fatalf("finish reason %q, want tool_calls", res.FinishReason)
		}
		if f.Calls() != 1 {
			t.Fatalf("provider saw %d calls, want exactly the one generation", f.Calls())
		}
	})

	rejects := []struct {
		name   string
		setup  func(f *fake.Provider)
		tools  []providers.ToolSpec
		choice providers.ToolChoice
	}{
		{"a tool nobody offered", func(f *fake.Provider) { f.On(nil).ProposeToolCall("call_1", "delete_all", `{}`) }, []providers.ToolSpec{bookTool()}, ""},
		{"arguments that are not an object", func(f *fake.Provider) { f.On(nil).ProposeToolCall("call_1", "book", `[1]`) }, []providers.ToolSpec{bookTool()}, ""},
		{"truncated arguments", func(f *fake.Provider) { f.On(nil).ProposeToolCall("call_1", "book", `{"slot":`) }, []providers.ToolSpec{bookTool()}, ""},
		{"a proposal while no tool is offered", func(f *fake.Provider) { f.On(nil).ProposeToolCall("call_1", "book", `{}`) }, nil, ""},
		{"a proposal while tools are off", func(f *fake.Provider) { f.On(nil).ProposeToolCall("call_1", "book", `{}`) }, []providers.ToolSpec{bookTool()}, providers.ToolChoiceNone},
		{"a duplicate call id", func(f *fake.Provider) {
			f.On(nil).ProposeToolCall("call_1", "book", `{}`).ProposeToolCall("call_1", "book", `{}`)
		}, []providers.ToolSpec{bookTool()}, ""},
		{"no proposal when one is required", func(f *fake.Provider) { f.On(nil).Reply("I will not call it") }, []providers.ToolSpec{bookTool()}, providers.ToolChoiceRequired},
		{"an injected malformed call", func(f *fake.Provider) { f.On(nil).Fail(fake.FailMalformedToolCall) }, []providers.ToolSpec{bookTool()}, ""},
	}
	for _, tc := range rejects {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			f := fake.New()
			tc.setup(f)
			g := richGen(t, f, richBudget())
			req := richReq()
			req.Tools, req.ToolChoice = tc.tools, tc.choice
			res, err := g.Generate(ctx, scope, req)
			wantCode(t, err, contracts.ErrSchemaInvalid)
			if len(res.ToolCalls) != 0 || len(res.Parts) != 0 {
				t.Fatalf("a rejected reply leaked a result: %+v", res)
			}
			if strings.Contains(err.Error(), "delete_all") {
				t.Fatalf("error echoes the model's output: %v", err)
			}
		})
	}

	t.Run("tool results and echoed calls are shaped", func(t *testing.T) {
		req := richReq()
		req.Tools = []providers.ToolSpec{bookTool()}
		req.Messages = append(req.Messages,
			providers.RichMessage{Role: providers.MsgAssistant, ToolCalls: []providers.ToolCallProposal{{ID: "call_1", Name: "book", Arguments: json.RawMessage(`{}`)}}},
			providers.RichMessage{Role: providers.MsgTool, Parts: []providers.ContentPart{providers.TextPart(`{"ok":true}`)}, ToolCallID: "call_1"},
		)
		if err := req.Validate(); err != nil {
			t.Fatal(err)
		}
		bad := req.Clone()
		bad.Messages[3].ToolCallID = ""
		wantCode(t, bad.Validate(), contracts.ErrValidationFailed)
		bad = req.Clone()
		bad.Messages[1].ToolCalls = req.Messages[2].ToolCalls
		wantCode(t, bad.Validate(), contracts.ErrValidationFailed)
		bad = req.Clone()
		bad.Tools = append(bad.Tools, bookTool())
		wantCode(t, bad.Validate(), contracts.ErrValidationFailed)
		bad = req.Clone()
		bad.Tools[0].Parameters = json.RawMessage(`"not an object"`)
		wantCode(t, bad.Validate(), contracts.ErrValidationFailed)
	})
}

func TestMultimodalPartsValidated(t *testing.T) {
	ctx := context.Background()
	scope := testScope("ten_a")

	valid := richReq(
		providers.TextPart("what is on this?"),
		providers.ImageRef("src_img1", "image/png"),
		providers.FileRef("src_doc1", "application/pdf"),
		providers.AudioRef("src_aud1", "audio/wav"),
	)
	if err := valid.Validate(); err != nil {
		t.Fatalf("a well-formed multimodal request was rejected: %v", err)
	}

	userWith := func(p providers.ContentPart) providers.RichRequest { return richReq(p) }
	badInputs := []struct {
		name string
		req  providers.RichRequest
	}{
		{"url as a reference", userWith(providers.ImageRef("https://evil.example/x.png", "image/png"))},
		{"path as a reference", userWith(providers.FileRef("../../etc/passwd", "text/plain"))},
		{"empty reference", userWith(providers.ImageRef("", "image/png"))},
		{"svg image", userWith(providers.ImageRef("src_1", "image/svg+xml"))},
		{"html file", userWith(providers.FileRef("src_1", "text/html"))},
		{"media type of another kind", userWith(providers.AudioRef("src_1", "image/png"))},
		{"unknown kind", userWith(providers.ContentPart{Kind: "video", RefID: "src_1", MediaType: "video/mp4"})},
		{"inline bytes in a request", userWith(providers.ContentPart{Kind: providers.ModImage, RefID: "src_1", MediaType: "image/png", Data: []byte{1}})},
		{"empty text part", userWith(providers.TextPart(""))},
		{"text part with a reference", userWith(providers.ContentPart{Kind: providers.ModText, Text: "x", RefID: "src_1"})},
		{"message without content", providers.RichRequest{Messages: []providers.RichMessage{{Role: providers.MsgUser}}}},
		{"unknown role", providers.RichRequest{Messages: []providers.RichMessage{{Role: "admin", Parts: []providers.ContentPart{providers.TextPart("x")}}}}},
		{"untrusted system message", providers.RichRequest{Messages: []providers.RichMessage{{Role: providers.MsgSystem, Parts: []providers.ContentPart{providers.TextPart("be nice")}}}}},
		{"image in a system message", providers.RichRequest{Messages: []providers.RichMessage{{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.ImageRef("src_1", "image/png")}}}}},
		{"image in a tool result", providers.RichRequest{Messages: []providers.RichMessage{{Role: providers.MsgTool, ToolCallID: "c1", Parts: []providers.ContentPart{providers.ImageRef("src_1", "image/png")}}}}},
		{"too many parts", richReq(manyParts(33)...)},
	}

	f := fake.New()
	f.On(nil).Reply("ok")
	for _, tc := range badInputs {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			wantCode(t, tc.req.Validate(), contracts.ErrValidationFailed)
			_, err := f.Generate(ctx, scope, tc.req)
			wantCode(t, err, contracts.ErrValidationFailed)
		})
	}
	if f.Calls() != 0 {
		t.Fatalf("a rejected request reached the provider (%d calls)", f.Calls())
	}

	t.Run("a valid request reaches the provider as references, never bytes", func(t *testing.T) {
		res, err := f.Generate(ctx, scope, valid)
		if err != nil {
			t.Fatal(err)
		}
		if res.Text() != "ok" {
			t.Fatalf("text %q", res.Text())
		}
		got := f.Prompts()[0].Rich
		var refs int
		for _, m := range got.Messages {
			for _, p := range m.Parts {
				if p.Kind != providers.ModText {
					refs++
					if len(p.Data) != 0 || p.RefID == "" {
						t.Fatalf("part %+v is not a bare reference", p)
					}
				}
			}
		}
		if refs != 3 {
			t.Fatalf("provider saw %d references, want 3", refs)
		}
	})

	t.Run("provider output is carried as untrusted bytes and text, never interpreted", func(t *testing.T) {
		html := "<script>alert(1)</script> **bold** ![x](http://evil/x.png)"
		png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
		f := fake.New()
		f.On(nil).ReplyParts(providers.TextPart(html), providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png", Data: png})
		req := richReq()
		req.OutputModalities = []providers.Modality{providers.ModImage}
		res, err := richGen(t, f, richBudget()).Generate(ctx, scope, req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Text() != html {
			t.Fatalf("text was altered: %q", res.Text())
		}
		if len(res.Parts) != 2 || !reflect.DeepEqual(res.Parts[1].Data, png) || res.Parts[1].RefID != "" {
			t.Fatalf("media part not carried as bytes: %+v", res.Parts)
		}
	})

	badOutputs := []struct {
		name string
		part providers.ContentPart
	}{
		{"media that names an AlixAI reference", providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png", Data: []byte{1}, RefID: "src_1"}},
		{"svg output", providers.ContentPart{Kind: providers.ModImage, MediaType: "image/svg+xml", Data: []byte("<svg/>")}},
		{"html file output", providers.ContentPart{Kind: providers.ModFile, MediaType: "text/html", Data: []byte("<p/>")}},
		{"empty media", providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png"}},
		{"oversize media", providers.ContentPart{Kind: providers.ModAudio, MediaType: "audio/wav", Data: make([]byte, providers.MaxPartBytes+1)}},
		{"unknown kind", providers.ContentPart{Kind: "video", MediaType: "video/mp4", Data: []byte{1}}},
	}
	for _, tc := range badOutputs {
		t.Run("rejects reply with "+tc.name, func(t *testing.T) {
			f := fake.New()
			f.On(nil).ReplyParts(tc.part)
			req := richReq()
			req.OutputModalities = []providers.Modality{providers.ModImage, providers.ModAudio}
			_, err := richGen(t, f, richBudget()).Generate(ctx, scope, req)
			wantCode(t, err, contracts.ErrSchemaInvalid)
		})
	}

	t.Run("a rich request converts to text only when it is text only", func(t *testing.T) {
		chat := chatReq("hello")
		back, err := providers.FromChat(chat).TextOnly()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(chat.Messages, back.Messages); diff != "" {
			t.Fatalf("round trip (-want +got):\n%s", diff)
		}
		if _, err := valid.TextOnly(); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Fatalf("media request downgraded silently: %v", err)
		} else if what, ok := providers.IsUnsupported(err); !ok || what != "image_input" {
			t.Fatalf("IsUnsupported = %q, %v", what, ok)
		}
		withTools := richReq()
		withTools.Tools = []providers.ToolSpec{bookTool()}
		if _, err := withTools.TextOnly(); err == nil {
			t.Fatal("a request with tools was downgraded silently")
		}
	})
}

// scriptStream replays fixed chunks (a hostile or buggy provider).
type scriptStream struct {
	mu     sync.Mutex
	chunks []providers.RichChunk
	i      int
	closed bool
}

func (s *scriptStream) Recv() (providers.RichChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.i >= len(s.chunks) {
		return providers.RichChunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *scriptStream) Close() error { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }

type scriptGen struct{ chunks []providers.RichChunk }

func (g scriptGen) Generate(context.Context, contracts.Scope, providers.RichRequest) (providers.RichResult, error) {
	return providers.RichResult{}, errors.New("not scripted")
}

func (g scriptGen) GenerateStream(context.Context, contracts.Scope, providers.RichRequest) (providers.RichStream, error) {
	return &scriptStream{chunks: g.chunks}, nil
}

func TestStreamDeltasOrderedAndCancellable(t *testing.T) {
	scope := testScope("ten_a")
	png := []byte{0x89, 'P', 'N', 'G', 9}
	newFake := func() *fake.Provider {
		f := fake.New(fake.WithChunkSize(4))
		f.On(nil).
			ReplyParts(providers.TextPart("hello streaming world"), providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png", Data: png}).
			ProposeToolCall("call_1", "book", `{"slot":"10:00"}`).
			Usage(11, 22)
		return f
	}
	req := richReq()
	req.Tools = []providers.ToolSpec{bookTool()}
	req.OutputModalities = []providers.Modality{providers.ModImage}

	t.Run("chunks arrive numbered, grouped and complete", func(t *testing.T) {
		f := newFake()
		s, err := f.GenerateStream(context.Background(), scope, req)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []providers.DeltaKind
		var text, args strings.Builder
		for want := 1; ; want++ {
			c, err := s.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Seq != want {
				t.Fatalf("chunk %d has seq %d", want, c.Seq)
			}
			kinds = append(kinds, c.Kind)
			text.WriteString(c.Text)
			args.WriteString(c.ArgsDelta)
		}
		s.Close()
		if text.String() != "hello streaming world" || args.String() != `{"slot":"10:00"}` {
			t.Fatalf("deltas reassemble to %q / %q", text.String(), args.String())
		}
		if kinds[len(kinds)-1] != providers.DeltaDone || kinds[len(kinds)-2] != providers.DeltaUsage {
			t.Fatalf("stream does not end with usage then done: %v", kinds)
		}
		if f.OpenStreams() != 0 {
			t.Fatal("stream leaked")
		}
	})

	t.Run("CollectRich equals the one-shot result", func(t *testing.T) {
		f := newFake()
		one, err := f.Generate(context.Background(), scope, req)
		if err != nil {
			t.Fatal(err)
		}
		s, err := richGen(t, f, richBudget()).GenerateStream(context.Background(), scope, req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := providers.CollectRich(s, req)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(one.Parts, got.Parts); diff != "" {
			t.Fatalf("parts (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(one.ToolCalls, got.ToolCalls); diff != "" {
			t.Fatalf("tool calls (-want +got):\n%s", diff)
		}
		if got.Usage.ToksIn != 11 || got.Usage.ToksOut != 22 || got.FinishReason != providers.FinishToolCalls {
			t.Fatalf("usage / finish: %+v %q", got.Usage, got.FinishReason)
		}
	})

	t.Run("cancellation stops the stream at once", func(t *testing.T) {
		f := newFake()
		ctx, cancel := context.WithCancel(context.Background())
		s, err := f.GenerateStream(ctx, scope, req)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := s.Recv(); err != nil {
				t.Fatal(err)
			}
		}
		delivered := f.ChunksDelivered()
		cancel()
		for i := 0; i < 2; i++ {
			if _, err := s.Recv(); !errors.Is(err, context.Canceled) {
				t.Fatalf("Recv after cancel = %v, want context.Canceled", err)
			}
		}
		if f.ChunksDelivered() != delivered {
			t.Fatal("a chunk was delivered after cancellation")
		}
		s.Close()
		if f.OpenStreams() != 0 {
			t.Fatal("cancelled stream leaked")
		}
	})

	t.Run("a deadline ends a stalled stream as timeout, Close unblocks it", func(t *testing.T) {
		f := fake.New()
		f.On(nil).Reply("slow reply").StallAfter(1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		s, err := richGen(t, f, richBudget()).GenerateStream(ctx, scope, richReq())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
		_, err = s.Recv()
		wantCode(t, err, contracts.ErrTimeout)
		s.Close()

		f2 := fake.New()
		f2.On(nil).Reply("slow reply").StallAfter(0)
		s2, err := f2.GenerateStream(context.Background(), scope, richReq())
		if err != nil {
			t.Fatal(err)
		}
		errc := make(chan error, 1)
		go func() { _, err := s2.Recv(); errc <- err }()
		time.Sleep(10 * time.Millisecond)
		s2.Close()
		select {
		case err := <-errc:
			if !errors.Is(err, providers.ErrStreamClosed) {
				t.Fatalf("blocked Recv returned %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not unblock Recv")
		}
		if f2.OpenStreams() != 0 {
			t.Fatal("stalled stream leaked")
		}
	})

	t.Run("an interrupted stream yields no result", func(t *testing.T) {
		f := newFake()
		f.FailNext(fake.FailPartialStream)
		s, err := f.GenerateStream(context.Background(), scope, req)
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.CollectRich(s, req)
		wantCode(t, err, contracts.ErrUnavailable)
		if len(res.Parts) != 0 || len(res.ToolCalls) != 0 {
			t.Fatalf("partial output leaked: %+v", res)
		}
	})

	done := providers.RichChunk{Kind: providers.DeltaDone, FinishReason: providers.FinishStop}
	text := func(seq int, s string) providers.RichChunk {
		return providers.RichChunk{Seq: seq, Kind: providers.DeltaText, Text: s}
	}
	tool := func(seq, idx int, id, name, args string) providers.RichChunk {
		return providers.RichChunk{Seq: seq, Kind: providers.DeltaToolArgs, ToolIndex: idx, ToolID: id, ToolName: name, ArgsDelta: args}
	}
	d := func(seq int) providers.RichChunk { c := done; c.Seq = seq; return c }
	hostile := []struct {
		name   string
		chunks []providers.RichChunk
	}{
		{"seq starts late", []providers.RichChunk{text(2, "a"), d(3)}},
		{"seq repeats", []providers.RichChunk{text(1, "a"), text(1, "b"), d(2)}},
		{"seq skips", []providers.RichChunk{text(1, "a"), d(3)}},
		{"data after done", []providers.RichChunk{text(1, "a"), d(2), text(3, "b")}},
		{"no done", []providers.RichChunk{text(1, "a")}},
		{"unknown kind", []providers.RichChunk{{Seq: 1, Kind: "weird"}, d(2)}},
		{"tool not offered", []providers.RichChunk{tool(1, 0, "c1", "rm_rf", "{}"), d(2)}},
		{"tool index skipped", []providers.RichChunk{tool(1, 1, "c1", "book", "{}"), d(2)}},
		{"tool arguments not an object", []providers.RichChunk{tool(1, 0, "c1", "book", "[1]"), d(2)}},
		{"tool arguments truncated", []providers.RichChunk{tool(1, 0, "c1", "book", `{"a":`), d(2)}},
		{"tool renamed mid-call", []providers.RichChunk{tool(1, 0, "c1", "book", "{"), tool(2, 0, "", "other", "}"), d(3)}},
		{"negative usage", []providers.RichChunk{{Seq: 1, Kind: providers.DeltaUsage, Usage: &providers.CostUsage{ToksIn: -1}}, d(2)}},
	}
	tr := richReq()
	tr.Tools = []providers.ToolSpec{bookTool()}
	for _, tc := range hostile {
		t.Run("rejects "+tc.name+" in CollectRich and in the decorated stream", func(t *testing.T) {
			s := &scriptStream{chunks: tc.chunks}
			if _, err := providers.CollectRich(s, tr); err == nil {
				t.Fatal("CollectRich accepted the stream")
			}
			g := richGen(t, scriptGen{tc.chunks}, richBudget())
			rs, err := g.GenerateStream(context.Background(), scope, tr)
			if err != nil {
				t.Fatal(err)
			}
			defer rs.Close()
			var last error
			for i := 0; i < len(tc.chunks)+2 && last == nil; i++ {
				_, last = rs.Recv()
			}
			if last == nil || last == io.EOF {
				t.Fatalf("decorated stream ended with %v, want an error", last)
			}
			if _, again := rs.Recv(); again == nil || again == io.EOF {
				t.Fatalf("stream recovered after an error: %v", again)
			}
		})
	}
}
