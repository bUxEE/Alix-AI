package providersuite

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

//go:embed profile_fake.json
var fakeProfileJSON []byte

// FakeOptions configures the reference fake external provider.
type FakeOptions struct {
	// Defect names the check this variant is built to fail ("" for the
	// reference provider that passes everything). Each defect breaks exactly
	// one behaviour; see broken.go.
	Defect string
	// Image, File and Audio make the provider declare (and implement) the
	// capability, in its capability record and in its profile.
	Image, File, Audio bool
}

// Fake is the reference fake external provider: a small adapter-shaped
// MultimodalGenerator over an in-process upstream, written to satisfy every
// check, with one switchable defect per check. It is also the Upstream the
// checks script. It makes no network call.
type Fake struct {
	opts FakeOptions
	keys *KeyRing
	caps providers.ModelCapabilities

	mu     sync.Mutex
	queue  []Scenario
	seen   []Seen
	down   bool
	rel    chan struct{}
	logBuf bytes.Buffer
	log    *slog.Logger
	n      int
	prev   string
	cache  map[string]string
	shared string
}

var (
	_ providers.MultimodalGenerator = (*Fake)(nil)
	_ providers.CapabilityProvider  = (*Fake)(nil)
	_ providers.AIRuntimeProvider   = (*Fake)(nil)
	_ Upstream                      = (*Fake)(nil)
)

// NewFake builds the fake over its own key ring.
func NewFake(opts FakeOptions) *Fake {
	f := &Fake{opts: opts, keys: NewKeyRing(), rel: make(chan struct{}), cache: map[string]string{}}
	f.log = slog.New(slog.NewJSONHandler(&f.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	in := []providers.Modality{providers.ModText}
	out := []providers.Modality{providers.ModText}
	caps := providers.ModelCapabilities{
		Provider: "fakeext", Model: "scripted", Version: "1", Streaming: true,
		StructuredOutput: providers.StructuredStrict,
		Tools:            providers.ToolCapability{Supported: true, StrictArguments: true, ParallelCalls: true, MaxTools: 32},
		MaxContextToks:   128000, MaxOutputToks: 16384, Metering: providers.MeteringPerResponse,
	}
	if opts.Image {
		in, out = append(in, providers.ModImage), append(out, providers.ModImage)
		caps.ImageGeneration = true
	}
	if opts.File {
		in, out = append(in, providers.ModFile), append(out, providers.ModFile)
	}
	if opts.Audio {
		in, out = append(in, providers.ModAudio), append(out, providers.ModAudio)
		caps.SpeechToText, caps.TextToSpeech = true, true
	}
	caps.InputModalities, caps.OutputModalities = in, out
	f.caps = caps
	return f
}

// Subject returns the fake wired as a conformance subject.
func (f *Fake) Subject() Subject {
	return Subject{
		Name: "fakeext", Generator: f, Caps: f, Runtime: f, Profile: f.profile(), Upstream: f, Keys: f.keys,
		Logs: func() string { f.mu.Lock(); defer f.mu.Unlock(); return f.logBuf.String() },
	}
}

// profile derives the registry profile from the embedded base so that the
// declared media match the capability record.
func (f *Fake) profile() pv.ProviderProfile {
	var doc map[string]any
	if err := json.Unmarshal(fakeProfileJSON, &doc); err != nil {
		panic(err)
	}
	mods := func(ms []providers.Modality) []any {
		var out []any
		for _, m := range ms {
			out = append(out, string(m))
		}
		return out
	}
	doc["modalities"] = map[string]any{"input": mods(f.caps.InputModalities), "output": mods(f.caps.OutputModalities)}
	roles := []any{"conversational_controller", "language_realization"}
	if f.opts.Image {
		roles = append(roles, "vision", "image_generation")
		doc["image_generation"] = map[string]any{"supported": true, "max_images_per_request": 4, "output_retention": "limited"}
	}
	if f.opts.Audio {
		roles = append(roles, "stt", "tts")
		doc["voice"] = map[string]any{"stt": true, "tts": true, "realtime": false, "audio_retention": "transient"}
	}
	doc["roles"] = roles
	media := map[string]any{}
	handling := func(types ...any) map[string]any {
		return map[string]any{"max_bytes": 20971520, "mime_types": types, "retention": "transient"}
	}
	if f.opts.Image {
		media["images"] = handling("image/png")
	}
	if f.opts.File {
		media["files"] = handling("application/pdf")
	}
	if f.opts.Audio {
		media["audio"] = handling("audio/wav")
	}
	if len(media) > 0 {
		doc["media"] = media
	}
	raw, _ := json.Marshal(doc)
	var p pv.ProviderProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		panic(fmt.Sprintf("providersuite: the fake profile does not decode: %v", err))
	}
	return p
}

func (f *Fake) has(d string) bool { return f.opts.Defect == d }

// Upstream ------------------------------------------------------------------

// Script implements Upstream.
func (f *Fake) Script(sc ...Scenario) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = nil
	for _, s := range sc {
		n := s.Times
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			f.queue = append(f.queue, s)
		}
	}
}

// Seen implements Upstream.
func (f *Fake) Seen() []Seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Seen(nil), f.seen...)
}

// SetDown implements Upstream.
func (f *Fake) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// Release implements Upstream.
func (f *Fake) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.rel)
	f.rel = make(chan struct{})
}

// Provider surface -----------------------------------------------------------

// Name implements providers.AIRuntimeProvider.
func (*Fake) Name() string { return "fakeext" }

// String describes the provider without any credential.
func (*Fake) String() string { return "fakeext.Fake{scripted}" }

// Health implements providers.AIRuntimeProvider.
func (f *Fake) Health(context.Context) (providers.HealthReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down && !f.has("fallback_and_outage_behavior") {
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: "the provider did not answer"}, nil
	}
	return providers.HealthReport{Status: providers.HealthOK}, nil
}

// Capabilities implements providers.CapabilityProvider.
func (f *Fake) Capabilities(context.Context) (providers.ModelCapabilities, error) {
	c := f.caps.Clone()
	if f.has("terms_and_capability_profile_validation") {
		c.InputModalities = append(c.InputModalities, providers.ModAudio)
	}
	return c, nil
}

func localErr(code contracts.ErrCode, kind pv.ProviderErrorKind, msg string) error {
	return contracts.New(code, "fakeext: "+msg).
		WithDetails("provider_error_kind", string(kind)).WithDetails("sent", false)
}

func sentErr(code contracts.ErrCode, kind pv.ProviderErrorKind, msg string) error {
	return contracts.New(code, "fakeext: "+msg).
		WithDetails("provider_error_kind", string(kind)).WithDetails("sent", true)
}

func notSent(err error) error {
	if ce := apiError(err); ce != nil {
		cp := *ce
		cp.Details = map[string]any{"sent": false}
		for k, v := range ce.Details {
			cp.Details[k] = v
		}
		cp.Details["sent"] = false
		return &cp
	}
	return err
}

// exchange is one request after the provider's own checks passed.
type exchange struct {
	req    providers.RichRequest
	stream bool
	sc     Scenario
	seq    int
	key    string
}

func (f *Fake) credential(scope contracts.Scope) (string, error) {
	f.mu.Lock()
	if f.has("tenant_isolation") && f.shared != "" {
		k := f.shared
		f.mu.Unlock()
		return k, nil
	}
	if f.has("provider_disablement") {
		if k, ok := f.cache[scope.TenantID]; ok {
			f.mu.Unlock()
			return k, nil
		}
	}
	f.mu.Unlock()
	key, err := f.keys.Resolve(scope)
	if err != nil {
		return "", contracts.Wrap(contracts.CodeOf(err), err, "fakeext: the credential could not be used").
			WithDetails("provider_error_kind", string(pv.ProviderErrorKindAuthenticationFailed)).WithDetails("sent", false)
	}
	f.mu.Lock()
	f.cache[scope.TenantID] = key
	if f.shared == "" {
		f.shared = key
	}
	f.mu.Unlock()
	return key, nil
}

// begin runs the provider's own gates, records the request at the upstream and
// takes the scenario that answers it.
func (f *Fake) begin(ctx context.Context, scope contracts.Scope, req providers.RichRequest, stream bool) (*exchange, error) {
	if err := ctx.Err(); err != nil {
		return nil, notSent(providers.ContextError(err))
	}
	if err := scope.Validate(); err != nil {
		return nil, notSent(err)
	}
	if err := req.Validate(); err != nil {
		return nil, notSent(err)
	}
	caps, _ := f.Capabilities(ctx)
	caps.InputModalities = f.caps.InputModalities
	if !f.has("bounded_context") {
		if err := caps.Check(req); err != nil {
			return nil, notSent(err)
		}
	}
	key, err := f.credential(scope)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.down {
		f.mu.Unlock()
		return nil, localErr(contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, "the provider is unreachable")
	}
	f.n++
	seen := f.seenOf(req, key, stream)
	f.seen = append(f.seen, seen)
	f.prev = seen.AllText()
	var sc Scenario
	if len(f.queue) > 0 {
		sc, f.queue = f.queue[0], f.queue[1:]
	} else {
		f.mu.Unlock()
		return nil, sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindInternal, "no scenario is scripted")
	}
	seq := f.n
	f.mu.Unlock()

	f.log.Debug("fakeext request", "method", "POST", "path", "/v1/respond", "seq", seq, "stream", stream)
	if f.has("creds_never_in_responses_or_logs") {
		f.log.Debug("fakeext request headers", "authorization", "Bearer "+key)
	}
	return &exchange{req: req.Clone(), stream: stream, sc: sc, seq: seq, key: key}, nil
}

// seenOf maps a request to what the upstream receives. The mapping is honest;
// the defects bend it in one place each.
func (f *Fake) seenOf(req providers.RichRequest, key string, stream bool) Seen {
	s := Seen{Stream: stream, Key: key, HasSchema: req.Output.Kind() == providers.ConstraintJSONSchema}
	for _, m := range req.Messages {
		var texts []string
		for _, p := range m.Parts {
			if p.Kind == providers.ModText {
				texts = append(texts, p.Text)
			} else if !f.has("image_input_output") {
				s.Media = append(s.Media, string(p.Kind))
			}
		}
		text := strings.Join(texts, "\n")
		switch m.Role {
		case providers.MsgSystem:
			s.System = append(s.System, text)
		case providers.MsgUser:
			if f.has("prompt_injection_resistance") && strings.HasPrefix(text, "Evidence (data") {
				s.System = append(s.System, text)
				continue
			}
			if f.has("authorized_excerpt_handling") && len(text) > 1024 {
				text = text[:1024]
			}
			s.Items = append(s.Items, SeenItem{Role: "user", Text: text})
		case providers.MsgAssistant:
			if text != "" {
				s.Items = append(s.Items, SeenItem{Role: "assistant", Text: text})
			}
			for _, c := range m.ToolCalls {
				s.Items = append(s.Items, SeenItem{Role: "assistant", CallID: c.ID, Name: c.Name, Text: string(c.Arguments)})
			}
		case providers.MsgTool:
			if f.has("directive_and_receipt_handling") {
				s.System = append(s.System, text)
				continue
			}
			s.Items = append(s.Items, SeenItem{Role: "tool", CallID: m.ToolCallID, Text: text})
		}
	}
	for _, t := range req.Tools {
		s.Tools = append(s.Tools, SeenTool{Name: t.Name, Kind: "function"})
	}
	if len(req.Tools) > 0 {
		switch {
		case f.has("confirmation_and_cancellation"):
			s.ToolChoice = "auto"
		case req.ToolChoice == providers.ToolChoiceNone:
			s.ToolChoice = "none"
		case req.ToolChoice == providers.ToolChoiceRequired:
			s.ToolChoice = "required"
		default:
			s.ToolChoice = "auto"
		}
	}
	switch {
	case f.has("quota_and_budget_enforcement"):
		s.MaxOutputToks = f.caps.MaxOutputToks
	case req.MaxToks > 0:
		s.MaxOutputToks = req.MaxToks
	case f.has("bounded_context"):
		s.MaxOutputToks = 0
	default:
		s.MaxOutputToks = min(4096, f.caps.MaxOutputToks)
	}
	if f.has("no_path_or_cred_leakage") {
		s.System = append(s.System, `client_context: C:\alixai\data\sources\clinic.pdf`)
	}
	if f.has("local_only_source_enforcement") && f.prev != "" {
		s.Items = append([]SeenItem{{Role: "user", Text: f.prev}}, s.Items...)
	}
	s.Body = s.AllText()
	for _, t := range s.Tools {
		s.Body += t.Name + "\n"
	}
	return s
}

// Generate implements providers.MultimodalGenerator.
func (f *Fake) Generate(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichResult, error) {
	ex, err := f.begin(ctx, scope, req, false)
	if err != nil {
		return providers.RichResult{}, err
	}
	res, err := f.answer(ctx, ex)
	if err != nil {
		return providers.RichResult{}, err
	}
	if f.has("no_direct_adapter_invocation") && len(res.ToolCalls) > 0 {
		// the defect: run the proposal and go back to the model with its result
		f.follow(ctx, scope, ex, res)
	}
	return res, nil
}

func (f *Fake) follow(ctx context.Context, scope contracts.Scope, ex *exchange, res providers.RichResult) {
	next := ex.req.Clone()
	next.Messages = append(next.Messages,
		providers.RichMessage{Role: providers.MsgAssistant, ToolCalls: res.ToolCalls},
		providers.RichMessage{Role: providers.MsgTool, ToolCallID: res.ToolCalls[0].ID, Parts: []providers.ContentPart{providers.TextPart("executed")}})
	f.mu.Lock()
	f.queue = append([]Scenario{{Kind: ScText, Text: "done", ToksIn: 1, ToksOut: 1}}, f.queue...)
	f.mu.Unlock()
	if ex2, err := f.begin(ctx, scope, next, false); err == nil {
		_, _ = f.answer(ctx, ex2)
	}
}

func (f *Fake) wait(ctx context.Context) error {
	f.mu.Lock()
	rel := f.rel
	f.mu.Unlock()
	if f.has("cancellation_and_timeout") {
		<-rel // the defect: the context is not consulted
		return nil
	}
	select {
	case <-ctx.Done():
		return sentCtx(ctx.Err())
	case <-rel:
		return sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, "the connection was released")
	}
}

func sentCtx(err error) error {
	e := providers.ContextError(err)
	if ce := apiError(e); ce != nil {
		return ce.WithDetails("provider_error_kind", string(pv.ProviderErrorKindTimeout)).WithDetails("sent", true)
	}
	return e
}

// failure maps a failure scenario to its typed error; ok is false for a
// scenario that is an answer.
func (f *Fake) failure(sc Scenario) (error, bool) {
	switch sc.Kind {
	case ScServerError:
		if f.has("malformed_and_error_mapping") {
			return nil, false
		}
		return sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, "the provider is unavailable"), true
	case ScAuthError:
		return sentErr(contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, "the provider rejected the API key"), true
	case ScQuota:
		return sentErr(contracts.ErrResourceExhausted, pv.ProviderErrorKindQuotaExceeded, "the provider account has no quota left"), true
	case ScRateLimit:
		e := sentErr(contracts.ErrRateLimited, pv.ProviderErrorKindRateLimited, "the provider rate limit was reached")
		return e.(*contracts.Error).WithDetails("retry_after_ms", int(sc.RetryAfter/time.Millisecond)), true
	case ScMalformed, ScHostedTool, ScActiveMedia:
		if sc.Kind == ScActiveMedia && f.has("sanitized_rich_blocks") {
			return nil, false
		}
		return sentErr(contracts.ErrSchemaInvalid, pv.ProviderErrorKindMalformedResponse, "the provider sent an unexpected answer"), true
	}
	return nil, false
}

// answer turns the scenario into a result (the non-streamed path).
func (f *Fake) answer(ctx context.Context, ex *exchange) (providers.RichResult, error) {
	sc := ex.sc
	switch sc.Kind {
	case ScStall:
		if err := f.wait(ctx); err != nil {
			return providers.RichResult{}, err
		}
		return providers.RichResult{}, sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, "no answer")
	case ScTruncated:
		return providers.RichResult{}, sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, "the connection was lost")
	}
	if err, ok := f.failure(sc); ok {
		return providers.RichResult{}, err
	}
	if sc.Kind == ScServerError { // the defect: an error becomes an empty success
		return providers.RichResult{FinishReason: providers.FinishStop}, nil
	}
	parts, calls := f.content(ex)
	if sc.Kind == ScToolCall && f.has("typed_task_proposal") {
		parts, calls = []providers.ContentPart{providers.TextPart(fmt.Sprintf(`{"tool":%q,"arguments":%s}`, sc.Call.Name, sc.Call.Args))}, nil
	}
	choice := ex.req.ToolChoice
	if f.has("confirmation_and_cancellation") && choice == providers.ToolChoiceNone {
		choice = providers.ToolChoiceAuto
	}
	var err error
	if f.has("unknown_capability_refusal") {
		for i := range calls {
			calls[i].Arguments = normalise(calls[i].Arguments)
		}
	} else if calls, err = providers.ValidateProposals(calls, ex.req.Tools, choice); err != nil {
		return providers.RichResult{}, sentWrap(err)
	}
	if len(calls) > 0 && f.has("argument_schema_compliance") {
		for i := range calls {
			calls[i].Arguments = repair(calls[i].Arguments)
		}
	}
	for _, p := range parts {
		if (f.has("file_input_output") && p.Kind == providers.ModFile) || (f.has("audio_input_output") && p.Kind == providers.ModAudio) {
			continue
		}
		if f.has("sanitized_rich_blocks") && p.Kind != providers.ModText {
			continue
		}
		if err := p.ValidateOutput(); err != nil {
			return providers.RichResult{}, sentWrap(err)
		}
		if p.Kind != providers.ModText && !ex.req.WantsOutput(p.Kind) {
			return providers.RichResult{}, sentErr(contracts.ErrSchemaInvalid, pv.ProviderErrorKindMalformedResponse, "the provider produced a modality that was not requested")
		}
	}
	usage := providers.CostUsage{ToksIn: sc.ToksIn, ToksOut: sc.ToksOut, Estimated: true}
	if usage.ToksIn == 0 && usage.ToksOut == 0 {
		usage.ToksIn, usage.ToksOut = 50, 10
	}
	if err := usage.Validate(); err != nil {
		return providers.RichResult{}, sentWrap(err)
	}
	res := providers.RichResult{Parts: parts, ToolCalls: calls, FinishReason: providers.FinishStop, Usage: usage, Model: "scripted-1", TraceID: fmt.Sprintf("fake-%06d", ex.seq)}
	if len(calls) > 0 {
		res.FinishReason = providers.FinishToolCalls
	}
	if f.has("audit_completeness") {
		res.TraceID, res.Model = "", ""
	}
	if ex.req.Output.Kind() == providers.ConstraintJSONSchema {
		j, err := providers.ParseJSONOutput(res.Text())
		switch {
		case err == nil:
			res.JSON = j
		case !f.has("structured_output"):
			return providers.RichResult{}, sentWrap(err)
		}
	}
	return res, nil
}

func sentWrap(err error) error {
	if ce := apiError(err); ce != nil {
		return ce.WithDetails("provider_error_kind", string(pv.ProviderErrorKindMalformedResponse)).WithDetails("sent", true)
	}
	return contracts.Wrap(contracts.ErrSchemaInvalid, err, "fakeext: the provider sent an unexpected answer").
		WithDetails("provider_error_kind", string(pv.ProviderErrorKindMalformedResponse)).WithDetails("sent", true)
}

func normalise(a json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(a)) == 0 {
		return json.RawMessage("{}")
	}
	return a
}

// repair is the defect of argument_schema_compliance: it "fixes" arguments.
func repair(a json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(a, &m) != nil {
		return a
	}
	for k := range m {
		if k != "patient" && k != "slot" {
			delete(m, k)
		}
	}
	if v, ok := m["slot"].(float64); ok {
		m["slot"] = fmt.Sprint(v)
	}
	b, _ := json.Marshal(m)
	return b
}

// content builds the reply parts and proposals of a scenario.
func (f *Fake) content(ex *exchange) ([]providers.ContentPart, []providers.ToolCallProposal) {
	sc := ex.sc
	text := sc.Text
	if f.has("source_reference_preservation") {
		text = strings.NewReplacer("["+srcOne+"]", "", "["+srcFake+"]", "", srcOne, "").Replace(text)
	}
	if f.has("no_unsupported_citations") && sc.Kind == ScText {
		text += " [src_01J000000000000000000000Y9]"
	}
	var parts []providers.ContentPart
	var calls []providers.ToolCallProposal
	if text != "" {
		parts = append(parts, providers.TextPart(text))
	}
	switch sc.Kind {
	case ScToolCall:
		calls = append(calls, providers.ToolCallProposal{ID: sc.Call.ID, Name: sc.Call.Name, Arguments: json.RawMessage(sc.Call.Args)})
	case ScMedia:
		data := append([]byte(nil), sc.Media.Data...)
		mt := sc.Media.MediaType
		switch {
		case f.has("file_input_output") && sc.Media.Kind == providers.ModFile:
			mt = "text/html"
			data = []byte("<html><script>alert(1)</script></html>")
		case f.has("audio_input_output") && sc.Media.Kind == providers.ModAudio:
			data = nil
		}
		parts = append(parts, providers.ContentPart{Kind: sc.Media.Kind, MediaType: mt, Data: data})
	case ScActiveMedia:
		parts = append(parts, providers.ContentPart{Kind: providers.ModImage, MediaType: "image/svg+xml", Data: []byte("<svg onload=alert(1)/>")})
	}
	return parts, calls
}

// Streaming ---------------------------------------------------------------

// GenerateStream implements providers.MultimodalGenerator.
func (f *Fake) GenerateStream(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichStream, error) {
	ex, err := f.begin(ctx, scope, req, true)
	if err != nil {
		return nil, err
	}
	if err, ok := f.failure(ex.sc); ok {
		return nil, err
	}
	var res providers.RichResult
	switch ex.sc.Kind {
	case ScStall:
		if ex.sc.Text == "" {
			if err := f.wait(ctx); err != nil {
				return nil, err
			}
			return nil, sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, "no answer")
		}
	case ScTruncated:
	default:
		parts, calls := f.content(ex)
		res = providers.RichResult{Parts: parts, ToolCalls: calls}
	}
	return f.newStream(ctx, ex, res), nil
}

type fakeStream struct {
	f      *Fake
	ctx    context.Context
	ex     *exchange
	chunks []providers.RichChunk
	pos    int
	mode   Kind
	err    error
	closed bool
	mu     sync.Mutex
}

func (f *Fake) newStream(ctx context.Context, ex *exchange, res providers.RichResult) providers.RichStream {
	st := &fakeStream{f: f, ctx: ctx, ex: ex, mode: ex.sc.Kind}
	seq := 0
	add := func(c providers.RichChunk) {
		seq++
		c.Seq = seq
		st.chunks = append(st.chunks, c)
	}
	switch ex.sc.Kind {
	case ScStall, ScTruncated:
		add(providers.RichChunk{Kind: providers.DeltaText, Text: ex.sc.Text})
		return st
	}
	var lastText = -1
	for _, p := range res.Parts {
		if p.Kind == providers.ModText {
			rs := []rune(p.Text)
			for i := 0; i < len(rs); i += 8 {
				add(providers.RichChunk{Kind: providers.DeltaText, Text: string(rs[i:min(i+8, len(rs))])})
				lastText = len(st.chunks) - 1
			}
			continue
		}
		pc := p
		add(providers.RichChunk{Kind: providers.DeltaPart, Part: &pc})
	}
	if f.has("text_generation_and_streaming") && lastText > 0 {
		st.chunks = append(st.chunks[:lastText], st.chunks[lastText+1:]...)
		for i := range st.chunks {
			st.chunks[i].Seq = i + 1
		}
		seq = len(st.chunks)
	}
	for i, c := range res.ToolCalls {
		add(providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: i, ToolID: c.ID, ToolName: c.Name})
		args := string(c.Arguments)
		for j := 0; j < len(args); j += 12 {
			add(providers.RichChunk{Kind: providers.DeltaToolArgs, ToolIndex: i, ArgsDelta: args[j:min(j+12, len(args))]})
		}
	}
	usage := &providers.CostUsage{ToksIn: ex.sc.ToksIn, ToksOut: ex.sc.ToksOut, Estimated: true}
	if usage.ToksIn == 0 && usage.ToksOut == 0 {
		usage.ToksIn, usage.ToksOut = 50, 10
	}
	if f.has("usage_reporting") {
		usage = nil
	}
	done := providers.RichChunk{Kind: providers.DeltaDone, FinishReason: providers.FinishStop, Usage: usage, Model: "scripted-1", TraceID: fmt.Sprintf("fake-%06d", ex.seq)}
	if len(res.ToolCalls) > 0 {
		done.FinishReason = providers.FinishToolCalls
	}
	if f.has("audit_completeness") {
		done.Model, done.TraceID = "", ""
	}
	add(done)
	return st
}

func (s *fakeStream) Recv() (providers.RichChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return providers.RichChunk{}, providers.ErrStreamClosed
	}
	if s.err != nil {
		return providers.RichChunk{}, s.err
	}
	if err := s.ctx.Err(); err != nil && !s.f.has("cancellation_and_timeout") {
		s.err = sentCtx(err)
		return providers.RichChunk{}, s.err
	}
	if s.pos < len(s.chunks) {
		c := s.chunks[s.pos]
		s.pos++
		return c, nil
	}
	switch s.mode {
	case ScStall:
		s.mu.Unlock()
		err := s.f.wait(s.ctx)
		s.mu.Lock()
		s.err = err
		if s.err == nil {
			s.err = sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, "no answer")
		}
		return providers.RichChunk{}, s.err
	case ScTruncated:
		if s.f.has("cancellation_and_partial_stream") {
			// the defect: the cut is reported as a complete answer
			s.mode = ""
			s.chunks = append(s.chunks, providers.RichChunk{Seq: s.pos + 1, Kind: providers.DeltaDone, FinishReason: providers.FinishStop, Usage: &providers.CostUsage{ToksIn: 1, ToksOut: 1, Estimated: true}})
			c := s.chunks[s.pos]
			s.pos++
			return c, nil
		}
		s.err = sentErr(contracts.ErrUnavailable, pv.ProviderErrorKindConnectionLost, "the stream ended before it completed")
		return providers.RichChunk{}, s.err
	}
	s.err = io.EOF
	return providers.RichChunk{}, io.EOF
}

func (s *fakeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
