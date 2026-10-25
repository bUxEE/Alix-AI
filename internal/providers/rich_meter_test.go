package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func usdPrices() providers.PriceTable {
	return providers.PriceTable{
		Currency: "USD",
		ToksIn:   &providers.Price{PerUnits: 1000000, MicroUnits: 150000},
		ToksOut:  &providers.Price{PerUnits: 1000000, MicroUnits: 600000},
	}
}

func TestUsageCostIsIntegerMicroUnits(t *testing.T) {
	// No float anywhere in a type that carries money or usage.
	for _, typ := range []reflect.Type{
		reflect.TypeOf(providers.CostUsage{}),
		reflect.TypeOf(providers.Price{}),
		reflect.TypeOf(providers.PriceTable{}),
		reflect.TypeOf(providers.RateLimitInfo{}),
		reflect.TypeOf(providers.ModelCapabilities{}),
	} {
		walkTypes(typ, map[reflect.Type]bool{}, func(path string, f reflect.StructField, ft reflect.Type) {
			if ft.Kind() == reflect.Float32 || ft.Kind() == reflect.Float64 {
				t.Errorf("%s.%s is a float", path, f.Name)
			}
		})
	}

	t.Run("prices round up and stay in integers", func(t *testing.T) {
		p := providers.Price{PerUnits: 1000000, MicroUnits: 150000}
		for _, tc := range []struct{ units, want int64 }{
			{0, 0}, {1, 1}, {6, 1}, {7, 2}, {1000000, 150000}, {1000001, 150001}, {2000000, 300000},
		} {
			got, err := p.Of(tc.units)
			if err != nil || got != tc.want {
				t.Errorf("Of(%d) = %d, %v; want %d", tc.units, got, err, tc.want)
			}
		}
		if _, err := p.Of(-1); err == nil {
			t.Error("negative units priced")
		}
		if _, err := (providers.Price{PerUnits: 1, MicroUnits: math.MaxInt64 / 2}).Of(math.MaxInt64 / 4); err == nil {
			t.Error("overflow priced")
		}
		if _, err := (providers.Price{PerUnits: 0, MicroUnits: 1}).Of(1); err == nil {
			t.Error("zero per_units priced")
		}
	})

	t.Run("a table prices usage as an upper-bound estimate", func(t *testing.T) {
		got, err := usdPrices().Cost(providers.CostUsage{ToksIn: 1000, ToksOut: 500})
		if err != nil {
			t.Fatal(err)
		}
		// ceil(1000*0.15) + ceil(500*0.6) micro-units
		if got.CostMicros != 150+300 || got.Currency != "USD" || !got.Estimated {
			t.Fatalf("cost = %+v", got)
		}
		if _, err := usdPrices().Cost(providers.CostUsage{ImagesOut: 1}); err == nil {
			t.Fatal("a unit with no price was costed instead of refused")
		}
		if err := (providers.PriceTable{Currency: "usd"}).Validate(); err == nil {
			t.Fatal("lower-case currency accepted")
		}
	})

	t.Run("usage records are validated", func(t *testing.T) {
		for name, u := range map[string]providers.CostUsage{
			"negative toks":         {ToksIn: -1},
			"cost without currency": {CostMicros: 5},
			"bad currency":          {CostMicros: 5, Currency: "us"},
			"cost beyond range":     {CostMicros: 1 << 60, Currency: "USD"},
			"negative cost":         {CostMicros: -1, Currency: "USD"},
		} {
			if err := u.Validate(); !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
				t.Errorf("%s: Validate = %v", name, err)
			}
		}
		if err := (providers.CostUsage{ToksIn: 1, CostMicros: 1500000, Currency: "EUR"}).Validate(); err != nil {
			t.Error(err)
		}
	})

	t.Run("the JSON holds integers and no key names a token", func(t *testing.T) {
		u := providers.CostUsage{ToksIn: 3, ToksOut: 4, CostMicros: 1500000, Currency: "USD", Estimated: true}
		b, _ := json.Marshal(providers.RichResult{Usage: u, Parts: []providers.ContentPart{providers.TextPart("x")}})
		if !strings.Contains(string(b), `"cost_micros":1500000`) || !strings.Contains(string(b), `"toks_in":3`) {
			t.Fatalf("usage JSON = %s", b)
		}
		for _, v := range []any{providers.RichResult{}, providers.RichChunk{}, providers.CostUsage{}, providers.RateLimitInfo{}, fake.DefaultCapabilities("p", "m"), providers.PriceTable{}} {
			walkTypes(reflect.TypeOf(v), map[reflect.Type]bool{}, func(path string, f reflect.StructField, ft reflect.Type) {
				if tag := f.Tag.Get("json"); strings.Contains(strings.ToLower(tag), "token") {
					t.Errorf("%s.%s (json %q) has a json key with the substring token", path, f.Name, tag)
				}
			})
		}
	})

	t.Run("the fake reports estimated or provider-stated cost", func(t *testing.T) {
		ctx := context.Background()
		caps := fake.DefaultCapabilities("fake", "fake-model")
		caps.Pricing = func() *providers.PriceTable { p := usdPrices(); return &p }()
		f := fake.New(fake.WithCapabilities(caps))
		f.On(fake.Contains("priced")).Reply("ok").Usage(1000, 500)
		f.On(fake.Contains("stated")).Reply("ok").Usage(1, 1).Cost(777, "EUR")
		res, err := f.Generate(ctx, testScope("ten_a"), richReq(providers.TextPart("priced")))
		if err != nil {
			t.Fatal(err)
		}
		if res.Usage.CostMicros != 450 || !res.Usage.Estimated || res.Usage.Currency != "USD" {
			t.Fatalf("estimated usage = %+v", res.Usage)
		}
		res, err = f.Generate(ctx, testScope("ten_a"), richReq(providers.TextPart("stated")))
		if err != nil {
			t.Fatal(err)
		}
		if res.Usage.CostMicros != 777 || res.Usage.Estimated || res.Usage.Currency != "EUR" {
			t.Fatalf("stated usage = %+v", res.Usage)
		}
	})

	t.Run("the budget prices the worst case before the call", func(t *testing.T) {
		b := providers.Budget{MaxPromptTokens: 4000, MaxOutputTokens: 500, MaxBatchItems: 1, Timeout: time.Second, MediaPartToks: 100}
		req := richReq(providers.TextPart("describe"), providers.ImageRef("src_1", "image/png"))
		in := b.EstimateRich(req)
		prices := usdPrices()
		prices.ImagesIn = &providers.Price{PerUnits: 1, MicroUnits: 1000}
		got, err := b.EstimateRichCost(req, prices)
		if err != nil {
			t.Fatal(err)
		}
		want := int64((in*150000 + 999999) / 1000000) // ceil
		want += int64((500*600000 + 999999) / 1000000)
		want += 1000
		if got.CostMicros != want || got.ToksIn != in || got.ToksOut != 500 || got.ImagesIn != 1 || !got.Estimated {
			t.Fatalf("estimate = %+v, want %d micro-units for %d toks in", got, want, in)
		}
		// a table with no image-input price cannot budget a request with an image
		if _, err := b.EstimateRichCost(req, usdPrices()); err == nil {
			t.Fatal("an unpriced image input was budgeted")
		}
		req.OutputModalities = []providers.Modality{providers.ModImage}
		if _, err := b.EstimateRichCost(req, prices); err == nil {
			t.Fatal("an unpriced requested image was budgeted")
		}
	})
}

func TestRateLimitInfoMapped(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"120", 120 * time.Second, true},
		{" 3 ", 3 * time.Second, true},
		{"1.5", 1500 * time.Millisecond, true},
		{"0", 0, true},
		{"-5", 0, false},
		{"", 0, false},
		{"soon", 0, false},
		{"NaN", 0, false},
		{"99999999", providers.MaxRetryAfter, true},
		{"Wed, 07 Oct 2026 12:00:30 GMT", 30 * time.Second, true},
		{"Wed, 07 Oct 2026 11:00:00 GMT", 0, true},
		{"Fri, 01 Jan 2100 00:00:00 GMT", providers.MaxRetryAfter, true},
	} {
		got, ok := providers.ParseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseRetryAfter(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}

	t.Run("the error carries retry-after and survives wrapping", func(t *testing.T) {
		err := providers.RateLimitError(providers.RateLimitInfo{RetryAfter: 1500 * time.Millisecond, Scope: "toks"})
		wantCode(t, err, contracts.ErrRateLimited)
		if !contracts.CodeOf(err).Retryable() {
			t.Fatal("rate_limited must be retryable")
		}
		info, ok := providers.RateLimitFrom(errorsJoin(err))
		if !ok || info.RetryAfter != 1500*time.Millisecond || info.Scope != "toks" {
			t.Fatalf("RateLimitFrom = %+v, %v", info, ok)
		}
		if _, ok := providers.RateLimitFrom(errors.New("plain")); ok {
			t.Fatal("a plain error read as a rate limit")
		}
		clamped, _ := providers.RateLimitFrom(providers.RateLimitError(providers.RateLimitInfo{RetryAfter: -time.Second}))
		if clamped.RetryAfter != 0 {
			t.Fatalf("negative retry-after survived: %v", clamped.RetryAfter)
		}
		huge, _ := providers.RateLimitFrom(providers.RateLimitError(providers.RateLimitInfo{RetryAfter: 1000 * time.Hour}))
		if huge.RetryAfter != providers.MaxRetryAfter {
			t.Fatalf("retry-after not capped: %v", huge.RetryAfter)
		}
	})

	t.Run("injected limits reach the caller through the decorator", func(t *testing.T) {
		ctx, scope := context.Background(), testScope("ten_a")
		f := fake.New()
		f.On(nil).Fail(fake.FailRateLimited).RetryAfter(3 * time.Second)
		g := richGen(t, f, richBudget())
		_, err := g.Generate(ctx, scope, richReq())
		wantCode(t, err, contracts.ErrRateLimited)
		if info, _ := providers.RateLimitFrom(err); info.RetryAfter != 3*time.Second {
			t.Fatalf("Generate retry-after = %v", info.RetryAfter)
		}
		_, err = g.GenerateStream(ctx, scope, richReq())
		wantCode(t, err, contracts.ErrRateLimited)

		f2 := fake.New()
		f2.On(nil).Reply("x")
		f2.FailNext(fake.FailRateLimited)
		_, err = f2.Chat(ctx, scope, chatReq("hi"))
		wantCode(t, err, contracts.ErrRateLimited)
		if info, _ := providers.RateLimitFrom(err); info.RetryAfter != fake.DefaultRetryAfter {
			t.Fatalf("queued failure retry-after = %v", info.RetryAfter)
		}
	})

	t.Run("a successful reply can carry limit info; a negative one is rejected", func(t *testing.T) {
		left := 41
		f := fake.New()
		f.On(nil).Reply("ok").RateInfo(providers.RateLimitInfo{RemainingRequests: &left, Scope: "requests"})
		res, err := richGen(t, f, richBudget()).Generate(context.Background(), testScope("ten_a"), richReq())
		if err != nil {
			t.Fatal(err)
		}
		if res.RateLimit == nil || res.RateLimit.RemainingRequests == nil || *res.RateLimit.RemainingRequests != 41 {
			t.Fatalf("rate limit info lost: %+v", res.RateLimit)
		}
		f2 := fake.New()
		f2.On(nil).Reply("ok").RateInfo(providers.RateLimitInfo{RetryAfter: -time.Second})
		_, err = richGen(t, f2, richBudget()).Generate(context.Background(), testScope("ten_a"), richReq())
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
}

func errorsJoin(err error) error { return errors.Join(errors.New("while generating"), err) }

func TestCapabilityDiscoveryRoundTrip(t *testing.T) {
	caps := fake.DefaultCapabilities("fake", "fake-model")
	caps.Pricing = func() *providers.PriceTable { p := usdPrices(); return &p }()
	if err := caps.Validate(); err != nil {
		t.Fatal(err)
	}

	t.Run("JSON round trip is exact and decoding is strict", func(t *testing.T) {
		b, err := json.Marshal(caps)
		if err != nil {
			t.Fatal(err)
		}
		back, err := providers.DecodeModelCapabilities(b)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(caps, back); diff != "" {
			t.Fatalf("round trip (-want +got):\n%s", diff)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["api_key"] = "x"
		extra, _ := json.Marshal(m)
		if _, err := providers.DecodeModelCapabilities(extra); err == nil {
			t.Fatal("an unknown member was accepted")
		}
		if _, err := providers.DecodeModelCapabilities(append(b, []byte(`{}`)...)); err == nil {
			t.Fatal("trailing data was accepted")
		}
	})

	t.Run("incoherent records are rejected", func(t *testing.T) {
		mut := map[string]func(c *providers.ModelCapabilities){
			"no provider":   func(c *providers.ModelCapabilities) { c.Provider = "" },
			"no text input": func(c *providers.ModelCapabilities) { c.InputModalities = []providers.Modality{providers.ModImage} },
			"duplicate modality": func(c *providers.ModelCapabilities) {
				c.OutputModalities = []providers.Modality{providers.ModText, providers.ModText}
			},
			"unknown modality":           func(c *providers.ModelCapabilities) { c.InputModalities = append(c.InputModalities, "video") },
			"output above context":       func(c *providers.ModelCapabilities) { c.MaxOutputToks = c.MaxContextToks + 1 },
			"zero context":               func(c *providers.ModelCapabilities) { c.MaxContextToks = 0 },
			"strict args without tools":  func(c *providers.ModelCapabilities) { c.Tools = providers.ToolCapability{StrictArguments: true} },
			"image generation, no image": func(c *providers.ModelCapabilities) { c.OutputModalities = []providers.Modality{providers.ModText} },
			"unknown structured mode":    func(c *providers.ModelCapabilities) { c.StructuredOutput = "magic" },
			"price with a bad currency":  func(c *providers.ModelCapabilities) { c.Pricing.Currency = "dollars" },
		}
		for name, f := range mut {
			c := caps.Clone()
			f(&c)
			if err := c.Validate(); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("%s: Validate = %v", name, err)
			}
		}
	})

	t.Run("the fake reports what was scripted", func(t *testing.T) {
		f := fake.New(fake.WithCapabilities(caps))
		var cp providers.CapabilityProvider = f
		got, err := cp.Capabilities(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(caps, got); diff != "" {
			t.Fatalf("capabilities (-want +got):\n%s", diff)
		}
		got.InputModalities[0] = "mutated"
		again, _ := f.Capabilities(context.Background())
		if again.InputModalities[0] != providers.ModText {
			t.Fatal("the caller can mutate the provider's record")
		}
	})

	t.Run("an impossible request is rejected before anything is sent", func(t *testing.T) {
		textOnly := caps.Clone()
		textOnly.InputModalities = []providers.Modality{providers.ModText}
		textOnly.OutputModalities = []providers.Modality{providers.ModText}
		textOnly.ImageGeneration, textOnly.SpeechToText, textOnly.TextToSpeech = false, false, false
		textOnly.Tools = providers.ToolCapability{}
		textOnly.StructuredOutput = providers.StructuredNone
		textOnly.Grammar = false
		textOnly.MaxOutputToks = 100

		withImage := richReq(providers.TextPart("look"), providers.ImageRef("src_1", "image/png"))
		withTools := richReq()
		withTools.Tools = []providers.ToolSpec{bookTool()}
		wantImageOut := richReq()
		wantImageOut.OutputModalities = []providers.Modality{providers.ModImage}
		schema := richReq()
		schema.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
		grammar := richReq()
		grammar.Output = providers.GrammarConstraint(`root ::= "x"`)
		long := richReq()
		long.MaxToks = 101

		for name, tc := range map[string]struct {
			req  providers.RichRequest
			what string
		}{
			"image input":      {withImage, "image_input"},
			"tool calls":       {withTools, "tool_calls"},
			"image output":     {wantImageOut, "image_output"},
			"structured":       {schema, "structured_output"},
			"grammar":          {grammar, "grammar_output"},
			"output too large": {long, "that many output toks"},
		} {
			err := textOnly.Check(tc.req)
			if what, ok := providers.IsUnsupported(err); !ok || what != tc.what {
				t.Errorf("%s: Check = %v (unsupported %q, %v), want %q", name, err, what, ok, tc.what)
			}
			f := fake.New(fake.WithCapabilities(textOnly))
			f.On(nil).Reply("ok")
			if _, err := f.Generate(context.Background(), testScope("ten_a"), tc.req); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("%s: fake.Generate = %v", name, err)
			}
			if f.Calls() != 0 {
				t.Errorf("%s: the request reached the provider", name)
			}
		}
		if err := textOnly.Check(richReq()); err != nil {
			t.Fatalf("a plain text request was refused: %v", err)
		}

		// Streaming off is refused by the decorator before the call.
		noStream := caps.Clone()
		noStream.Streaming = false
		f := fake.New(fake.WithCapabilities(noStream))
		f.On(nil).Reply("ok")
		_, err := richGen(t, f, richBudget()).GenerateStream(context.Background(), testScope("ten_a"), richReq())
		if what, ok := providers.IsUnsupported(err); !ok || what != "streaming" {
			t.Fatalf("GenerateStream on a non-streaming model = %v", err)
		}
		if f.Calls() != 0 {
			t.Fatal("a stream was started on a model that cannot stream")
		}
	})
}

func TestBudgetCoversMultimodalRequest(t *testing.T) {
	ctx, scope := context.Background(), testScope("ten_a")
	small := providers.Budget{MaxPromptTokens: 300, MaxOutputTokens: 50, MaxBatchItems: 3, Timeout: time.Second, MediaPartToks: 100}

	images := func(n int) []providers.ContentPart {
		out := []providers.ContentPart{providers.TextPart("look")}
		for i := 0; i < n; i++ {
			out = append(out, providers.ImageRef("src_img"+string(rune('a'+i)), "image/png"))
		}
		return out
	}
	bigTool := bookTool()
	bigTool.Description = strings.Repeat("d", 900)
	bigTool.Parameters = json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", 6000) + `"}`)
	withBigTool := richReq()
	withBigTool.Tools = []providers.ToolSpec{bigTool}
	maxToks := richReq()
	maxToks.MaxToks = 51
	grammar := richReq()
	grammar.Output = providers.GrammarConstraint(strings.Repeat(`root ::= "x"`, 100))

	cases := []struct {
		name   string
		req    providers.RichRequest
		budget string
	}{
		{"media counted at a fixed estimate", richReq(images(3)...), "max_prompt_tokens"},
		{"too many media parts", richReq(images(9)...), "max_media_parts"},
		{"tool schema counted in the prompt", withBigTool, "max_prompt_tokens"},
		{"output cap", maxToks, "max_output_tokens"},
		{"grammar counted in the prompt", grammar, "max_prompt_tokens"},
	}
	for _, tc := range cases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			f := fake.New()
			f.On(nil).Reply("ok")
			b := small
			if tc.budget == "max_media_parts" {
				b.MediaPartToks, b.MaxPromptTokens = 1, 4000
			}
			_, err := richGen(t, f, b).Generate(ctx, scope, tc.req)
			wantCode(t, err, contracts.ErrValidationFailed)
			if which, ok := providers.IsBudgetViolation(err); !ok || which != tc.budget {
				t.Fatalf("budget violation = %q, %v; want %q (%v)", which, ok, tc.budget, err)
			}
			if f.Calls() != 0 {
				t.Fatalf("an over-budget request reached the provider (%d calls)", f.Calls())
			}
			_, err = richGen(t, f, b).GenerateStream(ctx, scope, tc.req)
			wantCode(t, err, contracts.ErrValidationFailed)
			if f.Calls() != 0 {
				t.Fatal("an over-budget stream request reached the provider")
			}
		})
	}

	t.Run("a request within budget passes and gets the output cap", func(t *testing.T) {
		f := fake.New()
		f.On(nil).Reply("ok")
		if _, err := richGen(t, f, small).Generate(ctx, scope, richReq(images(1)...)); err != nil {
			t.Fatal(err)
		}
		if got := f.Prompts()[0].Rich.MaxToks; got != 50 {
			t.Fatalf("provider saw MaxToks %d, want the budget's 50", got)
		}
	})

	t.Run("an older Budget literal stays valid and the new fields default", func(t *testing.T) {
		old := providers.Budget{MaxPromptTokens: 100, MaxOutputTokens: 50, MaxBatchItems: 3, Timeout: time.Second}
		if err := old.Validate(); err != nil {
			t.Fatal(err)
		}
		f := fake.New()
		f.On(nil).Reply("ok")
		// the default media estimate (1024) is far over 100 prompt toks
		_, err := richGen(t, f, old).Generate(ctx, scope, richReq(images(1)...))
		if which, _ := providers.IsBudgetViolation(err); which != "max_prompt_tokens" {
			t.Fatalf("default media estimate not applied: %v", err)
		}
	})

	t.Run("replies are checked too", func(t *testing.T) {
		png := []byte{0x89, 'P', 'N', 'G', 1}
		b := small
		b.MaxOutputBytes = 4
		f := fake.New()
		f.On(nil).ReplyParts(providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png", Data: png})
		req := richReq()
		req.OutputModalities = []providers.Modality{providers.ModImage}
		_, err := richGen(t, f, b).Generate(ctx, scope, req)
		wantCode(t, err, contracts.ErrSchemaInvalid)

		// media nobody asked for
		_, err = richGen(t, f, small).Generate(ctx, scope, richReq())
		wantCode(t, err, contracts.ErrSchemaInvalid)

		// constrained output that does not parse
		f2 := fake.New()
		f2.On(nil).ReplyParts(providers.TextPart("not json"))
		cr := richReq()
		cr.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object"}`))
		_, err = richGen(t, f2, small).Generate(ctx, scope, cr)
		wantCode(t, err, contracts.ErrSchemaInvalid)

		// a stream whose media exceeds the budget is cut
		b.MaxOutputBytes = 4
		s, err := richGen(t, f, b).GenerateStream(ctx, scope, req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = providers.CollectRich(s, req)
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})

	t.Run("the timeout and the caller's cancellation apply", func(t *testing.T) {
		b := small
		b.Timeout = 20 * time.Millisecond
		f := fake.New()
		f.On(nil).Reply("slow").StallAfter(0)
		_, err := richGen(t, f, b).Generate(ctx, scope, richReq())
		wantCode(t, err, contracts.ErrTimeout)

		cctx, cancel := context.WithCancel(ctx)
		cancel()
		f2 := fake.New()
		f2.On(nil).Reply("x")
		_, err = richGen(t, f2, small).Generate(cctx, scope, richReq())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled call = %v", err)
		}
		if f2.Calls() != 0 {
			t.Fatal("a cancelled call reached the provider")
		}
	})

	t.Run("scope is required and a nil generator or bad budget is refused", func(t *testing.T) {
		f := fake.New()
		f.On(nil).Reply("x")
		_, err := richGen(t, f, small).Generate(ctx, contracts.Scope{}, richReq())
		wantCode(t, err, contracts.ErrScopeMissing)
		if _, err := providers.WithRichBudget(nil, small); err == nil {
			t.Fatal("nil generator accepted")
		}
		if _, err := providers.WithRichBudget(f, providers.Budget{}); err == nil {
			t.Fatal("zero budget accepted")
		}
	})
}
