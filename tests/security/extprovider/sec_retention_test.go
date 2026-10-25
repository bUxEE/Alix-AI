package extprovidersec

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// SEC-115
func TestSecExtProviderRetentionDisclosedBeforeDataLeaves(t *testing.T) {
	sc := receptionist()
	in := dataegress.DisclosureInput{Role: pv.RoleConversationalController}

	t.Run("the disclosure states the retention, training and residency the profile states, and every send carries it", func(t *testing.T) {
		w := newWorld(t)
		prof := w.profile(w.profA)
		d, err := w.eg.Disclose(ctxb(), sc, prof, in)
		if err != nil {
			t.Fatalf("Disclose: %v", err)
		}
		if !d.ExternalEnabled || d.Terms.Retention != prof.Terms.Retention || d.Terms.TrainingUse != prof.Terms.TrainingUse ||
			d.Terms.Residency != prof.Region.Residency || d.Terms.RetentionDays == nil || *d.Terms.RetentionDays != 30 {
			t.Fatalf("disclosure %+v does not match the profile terms %+v", d.Terms, prof.Terms)
		}
		// What the host shows is what the guard records for each request that goes.
		w.proposeBook()
		w.say(sc, sayBook)
		sends := w.snd.all()
		if len(sends) != 1 {
			t.Fatalf("sends = %d", len(sends))
		}
		if sd := sends[0].Req.Call.Disclosure; sd.Terms.Retention != prof.Terms.Retention || sd.Terms.TrainingUse != prof.Terms.TrainingUse {
			t.Fatalf("the request's disclosure %+v differs from the profile terms", sd.Terms)
		}
	})

	t.Run("a provider that keeps data longer, or may train on it, is disclosed as such: terms are never understated", func(t *testing.T) {
		for name, edit := range map[string]func(*pv.ProviderProfile){
			"indefinite retention": func(p *pv.ProviderProfile) {
				p.Terms.Retention, p.Terms.RetentionDays = pv.RetentionModeIndefinite, nil
			},
			"training allowed": func(p *pv.ProviderProfile) { p.Terms.TrainingUse = pv.TrainingUseByDefault },
			"retention unknown": func(p *pv.ProviderProfile) {
				p.Terms.Retention, p.Terms.RetentionDays, p.Terms.TrainingUse = pv.RetentionModeUnknown, nil, pv.TrainingUseUnknown
			},
		} {
			w := newWorld(t, withProfileEdit(edit))
			prof := w.profile(w.profA)
			d, err := w.eg.Disclose(ctxb(), sc, prof, in)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if d.Terms.Retention != prof.Terms.Retention || d.Terms.TrainingUse != prof.Terms.TrainingUse {
				t.Fatalf("%s: disclosure %+v, profile %+v", name, d.Terms, prof.Terms)
			}
			if d.Terms.Retention == pv.RetentionModeZero {
				t.Fatalf("%s: disclosed as zero retention", name)
			}
		}
	})

	t.Run("when nothing may leave the disclosure lists nothing as leaving", func(t *testing.T) {
		w := newWorld(t, withoutEgressPolicy())
		d, err := w.eg.Disclose(ctxb(), sc, w.profile(w.profA), in)
		if err != nil {
			t.Fatal(err)
		}
		if d.ExternalEnabled || len(d.DataCategories) != 0 || d.RAGExcerptsIncluded {
			t.Fatalf("disclosure %+v: external mode is off, so nothing may be listed as leaving", d)
		}

		w2 := newWorld(t)
		w2.engage()
		d2, err := w2.eg.Disclose(ctxb(), sc, w2.profile(w2.profA), in)
		if err != nil {
			t.Fatal(err)
		}
		if d2.ExternalEnabled || len(d2.DataCategories) != 0 {
			t.Fatalf("disclosure under the kill switch %+v", d2)
		}
	})
}

// ---- the multimodal rig: a real files service, the modality gate and scripted generators ----

var modT0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

const modProv = "acme"

func i64(n int64) *int64 { return &n }

func modScope() contracts.Scope {
	s := storetest.Scope("app_demo", "ten_a", "wsp_1")
	s.PrincipalID, s.SessionID = "usr_1", "ses_1"
	return s
}

// modProfile declares every modality with a stated retention.
func modProfile() pv.ProviderProfile {
	h := func(ret pv.RetentionMode, mimes ...string) *pv.ProviderProfileMediaHandling {
		return &pv.ProviderProfileMediaHandling{MaxBytes: 1 << 20, MimeTypes: mimes, Retention: ret}
	}
	return pv.ProviderProfile{
		ProviderID: modProv, ModelID: "acme-omni",
		Roles: []pv.Role{pv.RoleConversationalController, pv.RoleVision, pv.RoleImageGeneration, pv.RoleSTT, pv.RoleTTS},
		Modalities: pv.ProviderProfileModalities{
			Input:  []pv.Modality{pv.ModalityText, pv.ModalityImage, pv.ModalityFile, pv.ModalityAudio},
			Output: []pv.Modality{pv.ModalityText, pv.ModalityImage, pv.ModalityAudio},
		},
		Media: &pv.ProviderProfileMedia{
			Images: h(pv.RetentionModeLimited, "image/png", "image/jpeg"),
			Files:  h(pv.RetentionModeIndefinite, "application/pdf", "text/plain"),
			Audio:  h(pv.RetentionModeTransient, "audio/wav"),
		},
		Voice:           pv.ProviderProfileVoice{STT: true, TTS: true, AudioRetention: pv.RetentionModeTransient},
		ImageGeneration: pv.ProviderProfileImageGeneration{Supported: true, MaxImagesPerRequest: i64(2), OutputRetention: pv.RetentionModeLimited},
		// External speech to text is reserved by the length of the audio, so it needs a price per input second.
		Pricing: pv.PriceTable{Currency: "USD", Prices: pv.PriceTablePrices{AudioSecondsIn: &pv.PriceTablePrice{PerUnits: 60, MicroUnits: 6000}}},
	}
}

type textEgress struct{}

func (textEgress) AllowText(context.Context, contracts.Scope, extmodal.Target, extmodal.Purpose, string) (extmodal.TextDecision, error) {
	return extmodal.TextDecision{EnvelopeID: pv.EnvelopeID(ids.New("envl")), DecisionID: pv.EgressDecisionID(ids.New("edec"))}, nil
}

// audioEgress allows every utterance: the audio setting of the egress policy is not the subject here.
type audioEgress struct{}

func (audioEgress) AllowAudio(context.Context, contracts.Scope, extmodal.Target, extmodal.Audio) (extmodal.TextDecision, error) {
	return extmodal.TextDecision{EnvelopeID: pv.EnvelopeID(ids.New("envl")), DecisionID: pv.EgressDecisionID(ids.New("edec"))}, nil
}

// admitBudget admits every reservation of an external call.
type admitBudget struct{}

func (admitBudget) Reserve(context.Context, contracts.Scope, providerbudget.Call) (providerbudget.Reservation, error) {
	return providerbudget.Reservation{ID: pv.ReservationID(ids.New("rsv"))}, nil
}

func (admitBudget) Settle(context.Context, contracts.Scope, pv.ReservationID, providerbudget.Actual) (providerbudget.Settlement, error) {
	return providerbudget.Settlement{}, nil
}

func (admitBudget) Release(context.Context, contracts.Scope, pv.ReservationID) error { return nil }

type modRig struct {
	svc   *extmodal.Service
	pol   *extmodal.MemoryPolicy
	appr  *extmodal.MemoryApprovals
	files *files.Service
	gen   *fake.Provider
	stt   *vfake.Provider
	tts   *vfake.Provider
}

func newModRig(t *testing.T) *modRig {
	t.Helper()
	clk := clock.NewFake(modT0)
	fcfg := files.DefaultConfig()
	fcfg.Object = files.RuleConfig{Enabled: true, AllowedMIME: []string{"image/png"}, TTL: time.Hour}
	fs, err := files.New(files.Options{DB: storetest.Open(t, nil), DataDir: t.TempDir(), Clock: clk, Config: &fcfg})
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	t.Cleanup(func() { fs.Close() })
	r := &modRig{
		pol: extmodal.NewMemoryPolicy(), appr: extmodal.NewMemoryApprovals(), files: fs,
		gen: fake.New(fake.WithCapabilities(fake.DefaultCapabilities(modProv, "acme-omni"))), stt: vfake.New(), tts: vfake.New(),
	}
	if r.svc, err = extmodal.New(extmodal.Deps{Policy: r.pol, Egress: textEgress{}, Audio: audioEgress{}, Files: fs, Approvals: r.appr, Store: fs, Budgets: admitBudget{}, Clock: clk}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *modRig) target(edit ...func(*pv.ProviderProfile)) extmodal.Target {
	p := modProfile()
	for _, e := range edit {
		e(&p)
	}
	return extmodal.Target{Profile: p, Caps: fake.DefaultCapabilities(modProv, "acme-omni")}
}

func (r *modRig) on(rawAudio bool, ms ...extmodal.Modality) {
	r.pol.SetProvider(modProv, extmodal.Enable(rawAudio, ms...))
	r.pol.SetTenant(modScope(), extmodal.Enable(rawAudio, ms...))
}

func pngBytes() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

func (r *modRig) upload(t *testing.T, name, mime string, content []byte, opts ...func(*files.PutInput)) conversation.FileRef {
	t.Helper()
	in := files.PutInput{Name: name, DeclaredMIME: mime, Content: bytes.NewReader(content)}
	for _, o := range opts {
		o(&in)
	}
	ref, err := r.files.Put(ctxb(), modScope(), in)
	if err != nil {
		t.Fatalf("Put(%s): %v", name, err)
	}
	return ref
}

func wantReason(t *testing.T, err error, code contracts.ErrCode, reason extmodal.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s/%s", code, reason)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("code = %s, want %s: %v", got, code, err)
	}
	if got := extmodal.ReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q: %v", got, reason, err)
	}
}

// SEC-125
func TestSecExtAudioImageFileRetentionDisclosed(t *testing.T) {
	sc := modScope()
	wantRetention := map[extmodal.Modality]pv.RetentionMode{
		extmodal.ModVision: pv.RetentionModeLimited, extmodal.ModFileInput: pv.RetentionModeIndefinite,
		extmodal.ModSTT: pv.RetentionModeTransient, extmodal.ModTTS: pv.RetentionModeTransient, extmodal.ModImageGeneration: pv.RetentionModeLimited,
	}

	t.Run("every modality is off by default, and on at the provider level alone or the tenant level alone is still off", func(t *testing.T) {
		r := newModRig(t)
		for _, m := range extmodal.All() {
			_, err := r.svc.Admit(ctxb(), sc, r.target(), m)
			wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonModalityOff)
		}
		r.pol.SetProvider(modProv, extmodal.Enable(true, extmodal.All()...))
		for _, m := range extmodal.All() {
			_, err := r.svc.Admit(ctxb(), sc, r.target(), m)
			wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonModalityOff)
		}
		r2 := newModRig(t)
		r2.pol.SetTenant(sc, extmodal.Enable(true, extmodal.All()...))
		for _, m := range extmodal.All() {
			_, err := r2.svc.Admit(ctxb(), sc, r2.target(), m)
			wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonModalityOff)
		}
	})

	t.Run("a stated retention is disclosed exactly, for audio, images, files and generated images", func(t *testing.T) {
		r := newModRig(t)
		r.on(true, extmodal.All()...)
		for _, m := range extmodal.All() {
			adm, err := r.svc.Admit(ctxb(), sc, r.target(), m)
			if err != nil {
				t.Fatalf("%s: %v", m, err)
			}
			d := adm.Disclosure
			if d.Retention != wantRetention[m] || d.Provider != modProv || !d.Recorded || !strings.Contains(d.Text, string(wantRetention[m])) {
				t.Fatalf("%s: disclosure %+v, want retention %s stated in the text", m, d, wantRetention[m])
			}
		}
	})

	t.Run("a retention the profile does not state cannot be disclosed, so the modality stays off and nothing is sent", func(t *testing.T) {
		unknown := map[extmodal.Modality]func(*pv.ProviderProfile){
			extmodal.ModVision:          func(p *pv.ProviderProfile) { p.Media.Images.Retention = pv.RetentionModeUnknown },
			extmodal.ModFileInput:       func(p *pv.ProviderProfile) { p.Media.Files.Retention = "" },
			extmodal.ModSTT:             func(p *pv.ProviderProfile) { p.Voice.AudioRetention = pv.RetentionModeUnknown },
			extmodal.ModTTS:             func(p *pv.ProviderProfile) { p.Voice.AudioRetention = pv.RetentionModeUnknown },
			extmodal.ModImageGeneration: func(p *pv.ProviderProfile) { p.ImageGeneration.OutputRetention = pv.RetentionModeUnknown },
		}
		for m, edit := range unknown {
			r := newModRig(t)
			r.on(true, extmodal.All()...)
			_, err := r.svc.Admit(ctxb(), sc, r.target(edit), m)
			wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonRetentionUndisclosed)
		}
		// The same holds for the calls themselves: the provider is not reached.
		r := newModRig(t)
		r.on(true, extmodal.All()...)
		png := r.upload(t, "p.png", "image/png", pngBytes())
		r.appr.Grant(sc, string(png.FileID), modProv, extmodal.Approval{Approved: true})
		noImages := r.target(unknown[extmodal.ModVision])
		r.gen.On(fake.Any()).Reply("a square")
		_, err := r.svc.Describe(ctxb(), sc, noImages, r.gen, extmodal.VisionRequest{Prompt: "describe", Files: []extmodal.FileUse{{FileID: string(png.FileID)}}}, extmodal.IngestOptions{})
		wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonRetentionUndisclosed)
		_, err = r.svc.GenerateImage(ctxb(), sc, r.target(unknown[extmodal.ModImageGeneration]), r.gen, "a square", extmodal.IngestOptions{})
		wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonRetentionUndisclosed)
		in := extmodal.VoiceInput{Audio: make([]byte, 32000), Format: voice.PCM16Mono(16000), VADSegmented: true}
		r.stt.Script("hello")
		_, err = r.svc.Transcribe(ctxb(), sc, r.target(unknown[extmodal.ModSTT]), in, nil, r.stt)
		wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonRetentionUndisclosed)
		if r.gen.Calls() != 0 || r.stt.CallCount() != 0 || r.stt.AudioBytesReceived() != 0 {
			t.Fatalf("generator calls %d stt calls %d audio bytes %d: data reached a provider whose retention is unknown", r.gen.Calls(), r.stt.CallCount(), r.stt.AudioBytesReceived())
		}
	})

	t.Run("raw audio leaves only with the explicit switch at both levels, only when the local VAD cut it, and the disclosure comes with it", func(t *testing.T) {
		r := newModRig(t)
		in := extmodal.VoiceInput{Audio: make([]byte, 32000), Format: voice.PCM16Mono(16000), VADSegmented: true}
		r.on(false, extmodal.ModSTT)
		r.stt.Script("hello")
		_, err := r.svc.Transcribe(ctxb(), sc, r.target(), in, nil, r.stt)
		wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonRawAudioNotPermitted)
		if r.stt.AudioBytesReceived() != 0 {
			t.Fatal("audio reached the provider without the raw-audio switch")
		}
		r.on(true, extmodal.ModSTT)
		res, err := r.svc.Transcribe(ctxb(), sc, r.target(), in, nil, r.stt)
		if err != nil || res.Disclosure == nil || res.Disclosure.Retention != pv.RetentionModeTransient || res.BytesSent != 32000 {
			t.Fatalf("result %+v err %v: the disclosure must accompany audio that left", res, err)
		}
		in.VADSegmented = false
		_, err = r.svc.Transcribe(ctxb(), sc, r.target(), in, nil, r.stt)
		wantReason(t, err, contracts.ErrPermissionDenied, extmodal.ReasonAudioNotSegmented)
		// A local transcript means no audio leaves at all.
		before := r.stt.AudioBytesReceived()
		got, err := r.svc.Transcribe(ctxb(), sc, r.target(), extmodal.VoiceInput{Transcript: "from the local recogniser", Audio: make([]byte, 32000), Format: voice.PCM16Mono(16000), VADSegmented: true}, nil, r.stt)
		if err != nil || got.Route != extmodal.RouteLocalTranscript || r.stt.AudioBytesReceived() != before {
			t.Fatalf("route %v err %v: a finalised local transcript must keep the audio home", got.Route, err)
		}
	})

	t.Run("a file or image reaches a provider only with an explicit approval, and an application-private one needs the egress permission too", func(t *testing.T) {
		r := newModRig(t)
		r.on(false, extmodal.ModVision, extmodal.ModFileInput)
		r.gen.On(fake.Any()).Reply("a small square")
		ask := func(id string) error {
			_, err := r.svc.Describe(ctxb(), sc, r.target(), r.gen, extmodal.VisionRequest{Prompt: "describe it", Files: []extmodal.FileUse{{FileID: id}}}, extmodal.IngestOptions{})
			return err
		}
		upload := r.upload(t, "up.png", "image/png", pngBytes())
		private := r.upload(t, "app.png", "image/png", pngBytes(), func(in *files.PutInput) { in.Origin = files.OriginApplication })

		for _, ref := range []conversation.FileRef{upload, private} {
			wantReason(t, ask(string(ref.FileID)), contracts.ErrPermissionDenied, extmodal.ReasonFileNotApproved)
		}
		r.appr.Grant(sc, string(private.FileID), modProv, extmodal.Approval{Approved: true})
		wantReason(t, ask(string(private.FileID)), contracts.ErrPermissionDenied, extmodal.ReasonPrivateFile)
		if r.gen.Calls() != 0 {
			t.Fatalf("a file reached the provider without permission (%d calls)", r.gen.Calls())
		}
		// Control: with the permission they go, as references.
		r.appr.Grant(sc, string(private.FileID), modProv, extmodal.Approval{Approved: true, PrivateEgress: true})
		r.appr.Grant(sc, string(upload.FileID), modProv, extmodal.Approval{Approved: true})
		for _, ref := range []conversation.FileRef{private, upload} {
			if err := ask(string(ref.FileID)); err != nil {
				t.Fatalf("approved file %s: %v", ref.FileID, err)
			}
		}
		// Another tenant's file reference does not resolve, approved or not.
		other := storetest.Scope("app_demo", "ten_b", "wsp_1")
		other.PrincipalID, other.SessionID = "usr_9", "ses_9"
		r.pol.SetTenant(other, extmodal.Enable(false, extmodal.ModVision, extmodal.ModFileInput))
		r.appr.Grant(other, string(upload.FileID), modProv, extmodal.Approval{Approved: true})
		calls := r.gen.Calls()
		if _, err := r.svc.Describe(ctxb(), other, r.target(), r.gen, extmodal.VisionRequest{Prompt: "describe it", Files: []extmodal.FileUse{{FileID: string(upload.FileID)}}}, extmodal.IngestOptions{}); err == nil || r.gen.Calls() != calls {
			t.Fatalf("tenant B reached tenant A's file through a provider (err %v)", err)
		}
	})

	t.Run("what a provider produces is stored locally and returned as a reference, never as the provider's bytes", func(t *testing.T) {
		r := newModRig(t)
		r.on(false, extmodal.ModImageGeneration)
		img := pngBytes()
		r.gen.On(fake.Any()).ReplyParts(providers.TextPart("here is your image"), providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png", Data: img})
		out, err := r.svc.GenerateImage(ctxb(), sc, r.target(), r.gen, "a small square", extmodal.IngestOptions{})
		if err != nil {
			t.Fatalf("GenerateImage: %v", err)
		}
		if len(out.Files) != 1 || !strings.HasPrefix(string(out.Files[0].FileID), "fil_") || out.Files[0].Origin != conversation.FileRefOriginGenerated {
			t.Fatalf("files %+v", out.Files)
		}
		if len(out.Disclosures) == 0 || out.Disclosures[0].Retention != pv.RetentionModeLimited || !out.Disclosures[0].Recorded {
			t.Fatalf("disclosures %+v: the host must be able to show what the provider keeps of the prompt and the image", out.Disclosures)
		}
	})
}
