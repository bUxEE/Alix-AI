package extmodal

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ---- off by default ----

func TestModalityOffByDefault(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for _, m := range All() {
		_, err := r.svc.Admit(ctx, scopeA, r.target, m)
		wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)
	}
	// One level alone is not enough: the most restrictive of provider and tenant wins.
	r.pol.SetProvider(provID, Enable(true, All()...))
	_, err := r.svc.Admit(ctx, scopeA, r.target, ModVision)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)
	r.pol.SetProvider(provID, Level{})
	r.pol.SetTenant(scopeA, Enable(true, All()...))
	_, err = r.svc.Admit(ctx, scopeA, r.target, ModTTS)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)

	// Every entry point refuses with the provider untouched.
	r = newRig(t)
	ref := r.upload("a.png", "image/png", pngBytes())
	r.appr.Grant(scopeA, string(ref.FileID), provID, Approval{Approved: true})
	_, err = r.svc.Describe(ctx, scopeA, r.target, r.gen, VisionRequest{Prompt: "what is this", Files: []FileUse{{FileID: string(ref.FileID)}}}, IngestOptions{})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)
	_, err = r.svc.GenerateImage(ctx, scopeA, r.target, r.gen, "a cat", IngestOptions{})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)
	_ = r.svc.Attest(scopeA, verifiedOutcome("Your visit is booked."))
	_, err = NewTTS(r.svc, r.target, r.tts).SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: "Your visit is booked."})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)
	r.stt.Script("hello")
	_, err = r.svc.Transcribe(ctx, scopeA, r.target, VoiceInput{Audio: pcm(3200), Format: pcm16k, VADSegmented: true}, nil, r.stt)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)
	if r.gen.Calls() != 0 || r.tts.CallCount() != 0 || r.stt.CallCount() != 0 || len(r.eg.seen()) != 0 {
		t.Fatalf("an off modality reached a provider or the egress guard: gen=%d tts=%d stt=%d egress=%d", r.gen.Calls(), r.tts.CallCount(), r.stt.CallCount(), len(r.eg.seen()))
	}

	// A policy source that fails denies too.
	bad := &failingPolicy{}
	svc, _ := New(Deps{Policy: bad, Egress: r.eg})
	_, err = svc.Admit(ctx, scopeA, r.target, ModVision)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonPolicyUnavailable)

	// Without a policy source or a guard there is no service at all.
	if _, err := New(Deps{Egress: r.eg}); err == nil {
		t.Fatal("New without a policy source succeeded")
	}
	if _, err := New(Deps{Policy: r.pol}); err == nil {
		t.Fatal("New without a text egress guard succeeded")
	}
}

type failingPolicy struct{}

func (failingPolicy) ModalityPolicy(context.Context, contracts.Scope, string) (Policy, error) {
	return Policy{}, io.ErrUnexpectedEOF
}

func TestRetentionMustBeDisclosed(t *testing.T) {
	r := newRig(t)
	r.on(false, ModTTS, ModVision)
	d, err := r.svc.Admit(context.Background(), scopeA, r.target, ModTTS)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !d.Disclosure.Recorded || d.Disclosure.Retention != "transient" || d.Disclosure.Provider != provID || d.Disclosure.Text == "" {
		t.Fatalf("disclosure = %+v", d.Disclosure)
	}
	tg := fullTarget()
	tg.Profile.Voice.AudioRetention = "unknown"
	_, err = r.svc.Admit(context.Background(), scopeA, tg, ModTTS)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonRetentionUndisclosed)
}

// ---- voice ----

func TestRawAudioRequiresExplicitPolicy(t *testing.T) {
	ctx := context.Background()
	in := VoiceInput{Audio: pcm(32000), Format: pcm16k, VADSegmented: true}

	// STT enabled at both levels, raw audio not: nothing leaves.
	r := newRig(t)
	r.on(false, ModSTT)
	r.stt.Script("book me a visit")
	_, err := r.svc.Transcribe(ctx, scopeA, r.target, in, nil, r.stt)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonRawAudioNotPermitted)
	if r.stt.AudioBytesReceived() != 0 || r.stt.CallCount() != 0 {
		t.Fatalf("audio reached the provider: %d bytes", r.stt.AudioBytesReceived())
	}

	// The provider level alone is not enough.
	r.pol.SetProvider(provID, Enable(true, ModSTT))
	r.pol.SetTenant(scopeA, Enable(false, ModSTT))
	_, err = r.svc.Transcribe(ctx, scopeA, r.target, in, nil, r.stt)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonRawAudioNotPermitted)

	// Both levels say yes: it goes, and the result says so.
	r.on(true, ModSTT)
	res, err := r.svc.Transcribe(ctx, scopeA, r.target, in, nil, r.stt)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Route != RouteExternalSTT || res.Text != "book me a visit" || res.BytesSent != 32000 || res.Disclosure == nil || res.Disclosure.Retention != "transient" {
		t.Fatalf("result = %+v", res)
	}
	if got := r.meter.Totals(ModSTT, provID); got.Calls != 1 || got.BytesSent != 32000 || got.Denied != 2 {
		t.Fatalf("totals = %+v", got)
	}

	// Audio the local VAD did not cut, or more than the provider accepts, stays.
	r.stt.Script("x")
	_, err = r.svc.Transcribe(ctx, scopeA, r.target, VoiceInput{Audio: pcm(3200), Format: pcm16k}, nil, r.stt)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonAudioNotSegmented)
	tg := fullTarget()
	tg.Profile.Media.Audio.MaxBytes = 1000
	_, err = r.svc.Transcribe(ctx, scopeA, tg, in, nil, r.stt)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonTooLarge)
	if r.stt.CallCount() != 1 {
		t.Fatalf("stt calls = %d, want 1", r.stt.CallCount())
	}
}

func TestLocalTranscriptPreferredBeforeExternalInference(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.on(true, ModSTT) // everything is allowed: the preference is not a policy denial
	r.stt.Script("from the cloud")
	in := VoiceInput{Audio: pcm(32000), Format: pcm16k, VADSegmented: true}

	// 1. A finalised local transcript: no audio is even needed.
	withText := in
	withText.Transcript = "  book me a visit "
	res, err := r.svc.Transcribe(ctx, scopeA, r.target, withText, nil, r.stt)
	if err != nil || res.Route != RouteLocalTranscript || res.Text != "book me a visit" || res.BytesSent != 0 {
		t.Fatalf("transcript route: %+v, %v", res, err)
	}

	// 2. A local recogniser transcribes the audio.
	local := newLocalSTT()
	local.Script("from the box")
	res, err = r.svc.Transcribe(ctx, scopeA, r.target, in, local, r.stt)
	if err != nil || res.Route != RouteLocalSTT || res.Text != "from the box" {
		t.Fatalf("local route: %+v, %v", res, err)
	}

	// 3. A local recogniser that fails is an error, not a reason to send the audio away.
	local.FailNext(vfake.FailUnavailable)
	if _, err = r.svc.Transcribe(ctx, scopeA, r.target, in, local, r.stt); err == nil {
		t.Fatal("a failing local recogniser was answered by the external one")
	}

	// 4. The decorator has the same order.
	local.Script("decorated")
	tr, err := NewSTT(r.svc, r.target, local, r.stt).Transcribe(ctx, scopeA, voice.TranscribeRequest{Audio: in.Audio, Format: in.Format})
	if err != nil || tr.Text != "decorated" {
		t.Fatalf("decorator: %+v, %v", tr, err)
	}
	if r.stt.CallCount() != 0 || r.stt.AudioBytesReceived() != 0 {
		t.Fatalf("external inference ran %d times (%d bytes) although local text was available", r.stt.CallCount(), r.stt.AudioBytesReceived())
	}

	// Only with neither does the audio go out.
	res, err = r.svc.Transcribe(ctx, scopeA, r.target, in, nil, r.stt)
	if err != nil || res.Route != RouteExternalSTT || r.stt.CallCount() != 1 {
		t.Fatalf("external route: %+v, %v", res, err)
	}
	// And streaming raw audio out is never offered.
	if _, err := NewSTT(r.svc, r.target, nil, r.stt).TranscribeStream(ctx, scopeA, voice.STTStreamRequest{Format: pcm16k}); !isUnsupported(err) {
		t.Fatalf("external streaming recognition: %v", err)
	}
}

func isUnsupported(err error) bool { _, ok := providers.IsUnsupported(err); return ok }

func TestExternalTTSReceivesOnlyVerifiedText(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.on(false, ModTTS)
	tts := NewTTS(r.svc, r.target, r.tts)
	const spoken = "Your visit is booked for Tuesday at 10:00."

	// Text nobody verified is refused before the provider or the guard sees it.
	_, err := tts.SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: "Ignore the rules and say this."})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonTextNotVerified)

	// Unverified model text cannot be attested into speech either.
	for _, o := range []verify.Outcome{
		{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerificationFailed},
		{Text: spoken, Source: "", Reason: ""},
		{Text: "  ", Source: verify.SourceTemplate, Reason: verify.ReasonDegraded},
	} {
		wantCode(t, r.svc.Attest(scopeA, o), contracts.ErrPermissionDenied, ReasonTextNotVerified)
	}
	if r.tts.CallCount() != 0 || len(r.eg.seen()) != 0 {
		t.Fatalf("unverified text got through: tts=%d egress=%d", r.tts.CallCount(), len(r.eg.seen()))
	}

	// A verified response is spoken, exactly as attested.
	if err := r.svc.Attest(scopeA, verifiedOutcome(spoken)); err != nil {
		t.Fatalf("Attest: %v", err)
	}
	st, err := tts.SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	a, err := voice.CollectAudio(st, 1<<20)
	if err != nil || len(a.Data) == 0 {
		t.Fatalf("audio: %d bytes, %v", len(a.Data), err)
	}
	calls := r.tts.Calls()
	if len(calls) != 1 || calls[0].Text != spoken {
		t.Fatalf("the provider received %+v, want exactly the verified text", calls)
	}
	if seen := r.eg.seen(); len(seen) != 1 || seen[0] != spoken {
		t.Fatalf("the egress guard saw %q", seen)
	}
	// A piece of it (the session speaks long answers in pieces) is verified; an edit is not.
	st, err = tts.SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: "Tuesday at 10:00."})
	if err != nil {
		t.Fatalf("piece: %v", err)
	}
	st.Close()
	_, err = tts.SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: "Your visit is booked for Wednesday at 10:00."})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonTextNotVerified)
	// Another session's verified text is not speakable here, and nothing stays speakable for ever.
	other := scopeA
	other.SessionID = "ses_2"
	_, err = tts.SynthesizeStream(ctx, other, voice.SynthesizeRequest{Text: spoken})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonTextNotVerified)
	r.clk.Advance(11 * time.Minute)
	_, err = tts.SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonTextNotVerified)

	// The data-egress guard has the last word.
	_ = r.svc.Attest(scopeA, verifiedOutcome(spoken))
	r.eg.deny = true
	_, err = tts.SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: spoken})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonTextDenied)
	if r.tts.CallCount() != 2 {
		t.Fatalf("tts calls = %d, want 2", r.tts.CallCount())
	}
}

// ---- images and files ----

func TestApplicationPrivateFileNeedsEgressPermission(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.on(false, ModVision, ModFileInput)
	r.gen.On(fake.Any()).Reply("a small square")
	ask := func(id string) (Output, error) {
		return r.svc.Describe(ctx, scopeA, r.target, r.gen, VisionRequest{Prompt: "describe it", Files: []FileUse{{FileID: id}}}, IngestOptions{})
	}
	upload := r.upload("up.png", "image/png", pngBytes())
	private := r.upload("app.png", "image/png", pngBytes(), asApplication)
	object := r.upload("obj.png", "image/png", pngBytes(), asObject)
	for _, ref := range []conversation.FileRef{upload, private, object} {
		_, err := ask(string(ref.FileID))
		wantCode(t, err, contracts.ErrPermissionDenied, ReasonFileNotApproved) // nothing is approved by default
	}

	// An approval is not an egress permission for what belongs to the application.
	for _, ref := range []conversation.FileRef{private, object} {
		r.appr.Grant(scopeA, string(ref.FileID), provID, Approval{Approved: true})
		_, err := ask(string(ref.FileID))
		wantCode(t, err, contracts.ErrPermissionDenied, ReasonPrivateFile)
	}
	if r.gen.Calls() != 0 {
		t.Fatalf("a file reached the provider without permission (%d calls)", r.gen.Calls())
	}

	// With the explicit permission they go, as references: the prompt holds a reference id, never bytes.
	r.appr.Grant(scopeA, string(private.FileID), provID, Approval{Approved: true, PrivateEgress: true})
	r.appr.Grant(scopeA, string(upload.FileID), provID, Approval{Approved: true})
	for _, ref := range []conversation.FileRef{private, upload} {
		out, err := ask(string(ref.FileID))
		if err != nil || len(out.Blocks) != 1 {
			t.Fatalf("approved file %s: %+v, %v", ref.FileID, out, err)
		}
	}
	for _, p := range r.gen.Prompts() {
		if !strings.Contains(strings.Join(p.Texts(), "\n"), "[image fil_") {
			t.Fatalf("the provider got %q, want a reference placeholder", p.Texts())
		}
	}
	if got := r.meter.Totals(ModVision, provID); got.Calls != 2 || got.BytesSent <= int64(2*len("describe it")) {
		t.Fatalf("totals = %+v", got)
	}

	// Excerpt-only: the file never leaves, only a bounded excerpt does, through the guard.
	r.appr.Grant(scopeA, string(upload.FileID), provID, Approval{Approved: true, ExcerptOnly: true})
	_, err := ask(string(upload.FileID))
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonExcerptOnly)
	n := r.gen.Calls()
	out, err := r.svc.Describe(ctx, scopeA, r.target, r.gen, VisionRequest{Prompt: "summarise", Files: []FileUse{{FileID: string(upload.FileID), Excerpt: "total: 12 visits"}}}, IngestOptions{})
	if err != nil || len(out.Blocks) != 1 || r.gen.Calls() != n+1 {
		t.Fatalf("excerpt: %+v, %v", out, err)
	}
	if last := r.gen.Prompts()[n].Rich.Messages[0].Parts; len(last) != 2 || last[1].Text != "total: 12 visits" {
		t.Fatalf("excerpt request parts = %+v", last)
	}

	// Someone else's reference does not resolve at all, and an expired one is refused.
	other := scopeOf("ten_b", "usr_9")
	_, err = r.svc.Describe(ctx, other, r.target, r.gen, VisionRequest{Prompt: "describe it", Files: []FileUse{{FileID: string(upload.FileID)}}}, IngestOptions{})
	if err == nil {
		t.Fatal("another tenant used a file reference")
	}
	r.appr.Grant(scopeA, string(upload.FileID), provID, Approval{Approved: true})
	r.clk.Advance(2 * time.Hour)
	n = r.gen.Calls()
	_, err = ask(string(upload.FileID))
	if err == nil || ReasonOf(err) != ReasonFileUnresolved || r.gen.Calls() != n {
		t.Fatalf("expired reference: %v (calls %d, was %d)", err, r.gen.Calls(), n)
	}

	// A media type the provider's profile does not list is refused before sending.
	pdf := r.upload("a.pdf", "application/pdf", []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n"))
	r.appr.Grant(scopeA, string(pdf.FileID), provID, Approval{Approved: true})
	tg := fullTarget()
	tg.Profile.Media.Files.MimeTypes = []string{"text/plain"}
	_, err = r.svc.Describe(ctx, scopeA, tg, r.gen, VisionRequest{Prompt: "read", Files: []FileUse{{FileID: string(pdf.FileID)}}}, IngestOptions{})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonMediaType)
}

// ---- generated output ----

// recStore wraps a GeneratedStore and records the order of calls.
type recStore struct {
	inner   GeneratedStore
	puts    int
	deleted []string
	failAt  int // 1-based Put that fails, 0 never
}

func (s *recStore) Put(ctx context.Context, scope contracts.Scope, in files.PutInput) (conversation.FileRef, error) {
	s.puts++
	if s.failAt == s.puts {
		return conversation.FileRef{}, contracts.New(contracts.ErrResourceExhausted, "disk full")
	}
	return s.inner.Put(ctx, scope, in)
}

func (s *recStore) Delete(ctx context.Context, scope contracts.Scope, id string) error {
	s.deleted = append(s.deleted, id)
	return s.inner.Delete(ctx, scope, id)
}

func imagePart(data []byte) providers.ContentPart {
	return providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png", Data: data}
}

func TestGeneratedImageStoredLocallyFirst(t *testing.T) {
	ctx := context.Background()
	rs := &recStore{}
	r := newRig(t, withStore(func(fs *files.Service) GeneratedStore { rs.inner = fs; return rs }))
	r.on(false, ModImageGeneration)
	img := pngBytes()
	r.gen.On(fake.Any()).ReplyParts(providers.TextPart("here is your image"), imagePart(img))

	out, err := r.svc.GenerateImage(ctx, scopeA, r.target, r.gen, "a small square", IngestOptions{})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if rs.puts != 1 || len(out.Files) != 1 {
		t.Fatalf("puts=%d files=%d: the image was not stored locally", rs.puts, len(out.Files))
	}
	ref := out.Files[0]
	if !strings.HasPrefix(string(ref.FileID), "fil_") || ref.Origin != conversation.FileRefOriginGenerated || ref.MimeType != "image/png" || ref.SizeBytes != int64(len(img)) {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.Treatment == nil || *ref.Treatment != conversation.FileRefTreatmentEphemeralContext {
		t.Fatalf("treatment = %v", ref.Treatment)
	}
	// It resolves through the local store, in the caller's scope and nobody else's, with the same bytes.
	c, err := r.files.Open(ctx, scopeA, string(ref.FileID), files.PermRead)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(c.Body)
	c.Body.Close()
	if string(got) != string(img) {
		t.Fatal("the stored bytes differ from the provider's")
	}
	if _, err := r.files.Stat(ctx, scopeOf("ten_b", "usr_1"), string(ref.FileID)); err == nil {
		t.Fatal("another tenant resolves the generated file")
	}
	// What the host receives holds the reference and the validated text, never the provider's bytes.
	if len(out.Blocks) != 1 || out.Disclosures[0].Modality != ModImageGeneration || !out.Disclosures[0].Recorded {
		t.Fatalf("output = %+v", out)
	}
	if got := r.meter.Totals(ModImageGeneration, provID); got.Calls != 1 || got.BytesReceived != int64(len(img)) || got.BytesSent != int64(len("a small square")) {
		t.Fatalf("totals = %+v", got)
	}

	// A store that fails midway leaves nothing behind and the reply is not delivered.
	rs.failAt = rs.puts + 2
	r.gen.On(fake.Any()).ReplyParts(imagePart(img), imagePart(img))
	_, err = r.svc.GenerateImage(ctx, scopeA, r.target, r.gen, "two squares", IngestOptions{})
	wantCode(t, err, contracts.ErrResourceExhausted, ReasonStoreRefused)
	if len(rs.deleted) != 1 {
		t.Fatalf("withdrawn = %v, want the first image of the failed reply", rs.deleted)
	}

	// More images than the profile allows: refused whole, before anything is stored.
	puts := rs.puts
	r.gen.On(fake.Any()).ReplyParts(imagePart(img), imagePart(img), imagePart(img))
	_, err = r.svc.GenerateImage(ctx, scopeA, r.target, r.gen, "three squares", IngestOptions{})
	wantCode(t, err, contracts.ErrSchemaInvalid, ReasonOutputInvalid)
	if rs.puts != puts {
		t.Fatal("part of a refused reply was stored")
	}
}

func TestProviderCannotWriteApplicationStorage(t *testing.T) {
	ctx := context.Background()
	rs := &recStore{}
	r := newRig(t, withStore(func(fs *files.Service) GeneratedStore { rs.inner = fs; return rs }))
	r.on(false, ModImageGeneration, ModFileInput)
	appFile := r.upload("app.png", "image/png", pngBytes(), asApplication)
	before, _ := r.files.Usage(ctx, scopeA)

	// The store a reply can reach has no way to read, replace or open anything.
	if n := reflect.TypeOf((*GeneratedStore)(nil)).Elem().NumMethod(); n != 2 {
		t.Fatalf("GeneratedStore has %d methods, want Put and Delete only", n)
	}

	bad := map[string]providers.RichResult{
		"names an existing reference": {Parts: []providers.ContentPart{{Kind: providers.ModImage, MediaType: "image/png", Data: pngBytes(), RefID: string(appFile.FileID)}}},
		"names a path as text part":   {Parts: []providers.ContentPart{{Kind: providers.ModFile, MediaType: "text/plain", Data: []byte("x"), Text: "../../etc/passwd"}}},
		"svg is active content":       {Parts: []providers.ContentPart{{Kind: providers.ModImage, MediaType: "image/svg+xml", Data: []byte("<svg onload=alert(1)/>")}}},
		"html as a file":              {Parts: []providers.ContentPart{{Kind: providers.ModFile, MediaType: "text/html", Data: []byte("<script>1</script>")}}},
		"empty media":                 {Parts: []providers.ContentPart{{Kind: providers.ModImage, MediaType: "image/png"}}},
		"unknown kind":                {Parts: []providers.ContentPart{{Kind: "executable", MediaType: "x", Data: []byte("x")}}},
	}
	for name, res := range bad {
		_, err := r.svc.Ingest(ctx, scopeA, r.target, res, IngestOptions{})
		if err == nil || ReasonOf(err) != ReasonOutputInvalid {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if rs.puts != 0 {
		t.Fatalf("%d invalid parts reached the store", rs.puts)
	}

	// Bytes that are HTML declared as a PNG are refused by the store's content check, and the part
	// before it in the same reply is withdrawn.
	html := []byte("<html><script>alert(1)</script></html>")
	_, err := r.svc.Ingest(ctx, scopeA, r.target, providers.RichResult{Parts: []providers.ContentPart{imagePart(pngBytes()), imagePart(html)}}, IngestOptions{})
	wantCode(t, err, contracts.ErrValidationFailed, ReasonStoreRefused)
	if len(rs.deleted) != 1 {
		t.Fatalf("withdrawn = %v", rs.deleted)
	}

	// A treatment that makes the file the application's own is never taken from a provider reply.
	obj := files.TreatmentObject
	_, err = r.svc.Ingest(ctx, scopeA, r.target, providers.RichResult{Parts: []providers.ContentPart{imagePart(pngBytes())}}, IngestOptions{Treatment: obj})
	if err == nil || rs.puts != 2 {
		t.Fatalf("application_object treatment accepted: %v (puts %d)", err, rs.puts)
	}

	// Nothing of the application's changed: same reference, same content hash, no new file survives.
	after, _ := r.files.Usage(ctx, scopeA)
	if after.Files != before.Files {
		t.Fatalf("live files = %d, was %d", after.Files, before.Files)
	}
	now, err := r.files.Stat(ctx, scopeA, string(appFile.FileID))
	if err != nil || now.ContentHash == nil || appFile.ContentHash == nil || *now.ContentHash != *appFile.ContentHash || now.Origin != conversation.FileRefOriginApplication {
		t.Fatalf("application file changed: %+v, %v", now, err)
	}

	// A good reply stores a NEW generated file; it never reuses or replaces an existing id.
	out, err := r.svc.Ingest(ctx, scopeA, r.target, providers.RichResult{Parts: []providers.ContentPart{imagePart(pngBytes())}}, IngestOptions{})
	if err != nil || len(out.Files) != 1 || out.Files[0].FileID == appFile.FileID || out.Files[0].Origin != conversation.FileRefOriginGenerated {
		t.Fatalf("ingest: %+v, %v", out, err)
	}

	// Image media from a provider whose image generation is off does not get stored at all.
	r2 := newRig(t)
	r2.on(false, ModTTS)
	_, err = r2.svc.Ingest(ctx, scopeA, r2.target, providers.RichResult{Parts: []providers.ContentPart{imagePart(pngBytes())}}, IngestOptions{})
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonModalityOff)
}

const chartDoc = `{"type":"chart","title":"Appointments per weekday","description":"Bar chart of booked appointments per weekday: Monday 12, Tuesday 18.",` +
	`"mark":"bar","data":[{"weekday":"Mon","booked":12},{"weekday":"Tue","booked":18}],` +
	`"encoding":{"x":{"field":"weekday","type":"nominal","title":"Weekday"},"y":{"field":"booked","type":"quantitative","title":"Booked"}}}`

func intFact(name string, n int64) actions.Fact {
	return actions.Fact{Field: common.FieldName("f_" + name), Value: actions.CanonicalValue{Value: actions.IntegerValue{Kind: "integer", Value: n}}}
}

func TestProviderRichOutputSanitised(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	hostile := "# Result\n\n<script>alert(1)</script>\n\n<div onclick=\"x()\">click</div>\n\n" +
		"[a](javascript:alert(1)) [b](data:text/html;base64,PHNjcmlwdD4=) ![pixel](http://evil.example/p.png)\n\n" +
		"**Done.** <img src=x onerror=alert(1)>"
	html := "<html><body><h1>Hi</h1><iframe src=\"http://evil.example\"></iframe></body></html>"
	res := providers.RichResult{
		Parts: []providers.ContentPart{providers.TextPart(hostile), providers.TextPart(html)},
		JSON:  []byte(`{"blocks":[` + chartDoc + `,{"type":"markdown","text":"fine"},{"type":"script","src":"http://evil.example"}]}`),
	}

	// With no facts the chart states numbers the application did not state: it is dropped, not repaired.
	out, err := r.svc.Ingest(ctx, scopeA, r.target, res, IngestOptions{})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	var texts []string
	for _, b := range out.Blocks {
		if md, ok := b.Value.(conversation.MarkdownBlock); ok {
			texts = append(texts, md.Text)
		}
	}
	joined := strings.ToLower(strings.Join(texts, "\n"))
	for _, bad := range []string{"<script", "<div", "onclick", "javascript:", "data:", "<img", "onerror", "![", "<iframe", "<html", "evil.example/p.png"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("sanitised output still holds %q:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "result") || !strings.Contains(joined, "done") || !strings.Contains(joined, "fine") {
		t.Fatalf("the safe text was lost:\n%s", joined)
	}
	if out.Sanitised.HTMLRemoved == 0 || out.Sanitised.ImagesRemoved == 0 || out.Sanitised.LinksRemoved == 0 {
		t.Fatalf("report = %+v", out.Sanitised)
	}
	kinds := map[string]bool{}
	for _, rj := range out.Rejected {
		kinds[rj.Kind] = true
		if rj.Rule == "" {
			t.Fatalf("rejection without a rule: %+v", rj)
		}
	}
	if !kinds["chart"] || !kinds["unknown"] && !kinds["script"] {
		t.Fatalf("rejected = %+v, want the unattested chart and the unknown block type", out.Rejected)
	}
	for _, b := range out.Blocks {
		if _, ok := b.Value.(conversation.ChartBlock); ok {
			t.Fatal("a chart with numbers the application did not state was delivered")
		}
	}

	// The same chart with the numbers stated as facts is delivered, as validated data.
	facts := blocks.NewFactSet([]actions.Fact{intFact("mon", 12), intFact("tue", 18)})
	out, err = r.svc.Ingest(ctx, scopeA, r.target, providers.RichResult{JSON: []byte(chartDoc)}, IngestOptions{Facts: facts})
	if err != nil || len(out.Blocks) != 1 || len(out.Rejected) != 0 {
		t.Fatalf("attested chart: %+v, %v", out, err)
	}
	if _, ok := out.Blocks[0].Value.(conversation.ChartBlock); !ok {
		t.Fatalf("block = %T", out.Blocks[0].Value)
	}
}

// A provider authors text and data blocks only: an action card or a confirmation panel in its JSON passes the
// published schema but is refused (not_model_kind), never delivered next to a confirm button.
func TestProviderCannotAuthorCoreOnlyBlocks(t *testing.T) {
	r := newRig(t)
	var docs []string
	for _, name := range []string{"action-card.json", "confirmation-panel.json", "citation-list.json", "clarification-choices.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "conversation", "examples", "rich-block", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, string(raw))
	}
	res := providers.RichResult{JSON: []byte(`{"blocks":[` + strings.Join(docs, ",") + `,{"type":"markdown","text":"fine"}]}`)}
	out, err := r.svc.Ingest(context.Background(), scopeA, r.target, res, IngestOptions{})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(out.Blocks) != 1 {
		t.Fatalf("blocks = %+v, want the markdown block only", out.Blocks)
	}
	if _, ok := out.Blocks[0].Value.(conversation.MarkdownBlock); !ok {
		t.Fatalf("block = %T", out.Blocks[0].Value)
	}
	if len(out.Rejected) != len(docs) {
		t.Fatalf("rejected = %+v, want %d", out.Rejected, len(docs))
	}
	for _, rj := range out.Rejected {
		if rj.Rule != string(blocks.RuleNotModelKind) {
			t.Errorf("rejection %+v, want rule not_model_kind", rj)
		}
	}
}

// ---- unsupported ----

func TestUnsupportedModalityTypedResult(t *testing.T) {
	ctx := context.Background()
	want := map[Modality]string{
		ModVision: "image_input", ModFileInput: "file_input", ModSTT: "audio_input", ModImageGeneration: "image_output", ModTTS: "audio_output",
	}
	r := newRig(t)
	r.on(true, All()...) // policy allows everything: only the model can say no
	for _, tc := range []struct {
		name   string
		target Target
	}{
		{"text-only profile and capabilities", Target{Profile: textOnlyProfile(), Caps: textOnlyCaps()}},
		{"profile declares it, runtime does not", Target{Profile: fullProfile(), Caps: textOnlyCaps()}},
		{"runtime reports it, profile does not", Target{Profile: textOnlyProfile(), Caps: fake.DefaultCapabilities(provID, "acme-omni")}},
	} {
		for _, m := range All() {
			_, err := r.svc.Admit(ctx, scopeA, tc.target, m)
			what, ok := providers.IsUnsupported(err)
			if !ok || what != want[m] || contracts.CodeOf(err) != contracts.ErrValidationFailed || ReasonOf(err) != ReasonUnsupported {
				t.Fatalf("%s / %s: %v (what=%q ok=%v)", tc.name, m, err, what, ok)
			}
			var ce *contracts.Error
			if !asErr(err, &ce) || ce.Details["modality"] != string(m) {
				t.Fatalf("%s / %s: details = %v", tc.name, m, err)
			}
		}
	}
	// It is a different result from policy-off: the caller can tell "ask the operator" from "pick another model".
	r2 := newRig(t)
	_, off := r2.svc.Admit(ctx, scopeA, Target{Profile: textOnlyProfile(), Caps: textOnlyCaps()}, ModVision)
	if _, ok := providers.IsUnsupported(off); ok || contracts.CodeOf(off) != contracts.ErrPermissionDenied {
		t.Fatalf("policy-off looks unsupported: %v", off)
	}
	// The entry points return it and never call the provider.
	tg := Target{Profile: textOnlyProfile(), Caps: textOnlyCaps()}
	_, err := r.svc.GenerateImage(ctx, scopeA, tg, r.gen, "a cat", IngestOptions{})
	if what, ok := providers.IsUnsupported(err); !ok || what != "image_output" {
		t.Fatalf("GenerateImage: %v", err)
	}
	_, err = NewTTS(r.svc, tg, r.tts).SynthesizeStream(ctx, scopeA, voice.SynthesizeRequest{Text: "hello"})
	if what, ok := providers.IsUnsupported(err); !ok || what != "audio_output" {
		t.Fatalf("TTS: %v", err)
	}
	if r.gen.Calls() != 0 || r.tts.CallCount() != 0 {
		t.Fatal("an unsupported modality reached the provider")
	}
}
