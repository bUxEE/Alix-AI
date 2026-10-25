package extmodal

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

func scopeOf(tenant, principal string) contracts.Scope {
	s := storetest.Scope("app_demo", tenant, "wsp_1")
	s.PrincipalID, s.SessionID = principal, "ses_1"
	return s
}

var scopeA = scopeOf("ten_a", "usr_1")

const provID = "acme"

func i64(n int64) *int64 { return &n }

// fullProfile declares every modality with a stated retention.
func fullProfile() pv.ProviderProfile {
	h := func(ret pv.RetentionMode, mimes ...string) *pv.ProviderProfileMediaHandling {
		return &pv.ProviderProfileMediaHandling{MaxBytes: 1 << 20, MimeTypes: mimes, Retention: ret}
	}
	price := func(per, micro int64) *pv.PriceTablePrice {
		return &pv.PriceTablePrice{PerUnits: per, MicroUnits: pv.MicroUnits(micro)}
	}
	return pv.ProviderProfile{
		ProfileID: "pprf_01J00000000000000000000ACM", ProviderID: provID, ModelID: "acme-omni", ModelVersion: "2026-10-01",
		Limits: pv.ProviderProfileLimits{MaxContextToks: 128000, MaxOutputToks: 1024},
		Pricing: pv.PriceTable{Currency: "USD", Prices: pv.PriceTablePrices{
			ToksIn: price(1000, 1000), ToksOut: price(1000, 4000), ImagesIn: price(1, 5000), ImagesOut: price(1, 40000),
			AudioSecondsIn: price(60, 6000),
		}},
		Roles: []pv.Role{pv.RoleConversationalController, pv.RoleVision, pv.RoleImageGeneration, pv.RoleSTT, pv.RoleTTS},
		Modalities: pv.ProviderProfileModalities{
			Input:  []pv.Modality{pv.ModalityText, pv.ModalityImage, pv.ModalityFile, pv.ModalityAudio},
			Output: []pv.Modality{pv.ModalityText, pv.ModalityImage, pv.ModalityAudio},
		},
		Media: &pv.ProviderProfileMedia{
			Images: h(pv.RetentionModeLimited, "image/png", "image/jpeg"),
			Files:  h(pv.RetentionModeLimited, "application/pdf", "text/plain"),
			Audio:  h(pv.RetentionModeTransient, "audio/wav"),
		},
		Voice:           pv.ProviderProfileVoice{STT: true, TTS: true, AudioRetention: pv.RetentionModeTransient},
		ImageGeneration: pv.ProviderProfileImageGeneration{Supported: true, MaxImagesPerRequest: i64(2), OutputRetention: pv.RetentionModeLimited},
	}
}

// textOnlyProfile has the shape of the first (X-10) profile: text in and out, no media block.
func textOnlyProfile() pv.ProviderProfile {
	return pv.ProviderProfile{
		ProviderID: provID, ModelID: "acme-text",
		Roles:      []pv.Role{pv.RoleConversationalController},
		Modalities: pv.ProviderProfileModalities{Input: []pv.Modality{pv.ModalityText}, Output: []pv.Modality{pv.ModalityText}},
		Voice:      pv.ProviderProfileVoice{AudioRetention: pv.RetentionModeUnknown},
		ImageGeneration: pv.ProviderProfileImageGeneration{
			OutputRetention: pv.RetentionModeUnknown,
		},
	}
}

func textOnlyCaps() providers.ModelCapabilities {
	return providers.ModelCapabilities{
		Provider: provID, Model: "acme-text",
		InputModalities: []providers.Modality{providers.ModText}, OutputModalities: []providers.Modality{providers.ModText},
		Streaming: true, StructuredOutput: providers.StructuredStrict, MaxContextToks: 1000, MaxOutputToks: 100,
		Metering: providers.MeteringPerResponse,
	}
}

func fullTarget() Target {
	return Target{Profile: fullProfile(), Caps: fake.DefaultCapabilities(provID, "acme-omni")}
}

// recEgress records the text that reaches the data-egress seam.
type recEgress struct {
	mu        sync.Mutex
	texts     []string
	decisions []TextDecision
	deny      bool
}

func (e *recEgress) AllowText(_ context.Context, _ contracts.Scope, _ Target, _ Purpose, text string) (TextDecision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, text)
	if e.deny {
		return TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "guard says no")
	}
	d := TextDecision{EnvelopeID: pv.EnvelopeID(ids.New("envl")), DecisionID: pv.EgressDecisionID(ids.New("edec"))}
	e.decisions = append(e.decisions, d)
	return d, nil
}

// recAudio records the audio that reaches the data-egress seam (never its bytes: the seam has none).
type recAudio struct {
	mu        sync.Mutex
	seen      []Audio
	decisions []TextDecision
	deny      bool
}

func (e *recAudio) AllowAudio(_ context.Context, _ contracts.Scope, _ Target, a Audio) (TextDecision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, a)
	if e.deny {
		return TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "guard says no")
	}
	d := TextDecision{EnvelopeID: pv.EnvelopeID(ids.New("envl")), DecisionID: pv.EgressDecisionID(ids.New("edec"))}
	e.decisions = append(e.decisions, d)
	return d, nil
}

func (e *recAudio) calls() ([]Audio, []TextDecision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Audio(nil), e.seen...), append([]TextDecision(nil), e.decisions...)
}

func (e *recEgress) decided() []TextDecision {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]TextDecision(nil), e.decisions...)
}

// recBudget records the reservations, settlements and releases of external calls. It admits every call unless
// refuse is set.
type recBudget struct {
	mu       sync.Mutex
	refuse   error
	reserved []providerbudget.Call
	settled  map[pv.ReservationID]providerbudget.Actual
	released []pv.ReservationID
}

func newRecBudget() *recBudget {
	return &recBudget{settled: map[pv.ReservationID]providerbudget.Actual{}}
}

func (b *recBudget) Reserve(_ context.Context, _ contracts.Scope, call providerbudget.Call) (providerbudget.Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refuse != nil {
		return providerbudget.Reservation{}, b.refuse
	}
	b.reserved = append(b.reserved, call)
	return providerbudget.Reservation{ID: pv.ReservationID(ids.New("rsv")), Units: call.Units}, nil
}

func (b *recBudget) Settle(_ context.Context, _ contracts.Scope, id pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settled[id] = a
	return providerbudget.Settlement{}, nil
}

func (b *recBudget) Release(_ context.Context, _ contracts.Scope, id pv.ReservationID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.released = append(b.released, id)
	return nil
}

// calls returns the reservations, the outcomes of the settled ones and the number released.
func (b *recBudget) calls() ([]providerbudget.Call, []providerbudget.Actual, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []providerbudget.Actual
	for _, a := range b.settled {
		out = append(out, a)
	}
	return append([]providerbudget.Call(nil), b.reserved...), out, len(b.released)
}

func (e *recEgress) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.texts...)
}

// rig is a Service with every modality's collaborators.
type rig struct {
	t      *testing.T
	svc    *Service
	clk    *clock.Fake
	pol    *MemoryPolicy
	appr   *MemoryApprovals
	eg     *recEgress
	aeg    *recAudio
	files  *files.Service
	store  GeneratedStore
	budget *recBudget
	meter  *Meter
	gen    *fake.Provider
	stt    *vfake.Provider
	tts    *vfake.Provider
	target Target
}

type rigOpt func(*rig, *Deps)

func withStore(s func(*files.Service) GeneratedStore) rigOpt {
	return func(r *rig, d *Deps) { r.store = s(r.files); d.Store = r.store }
}

// newRig enables nothing: tests call on().
func newRig(t *testing.T, opts ...rigOpt) *rig {
	t.Helper()
	clk := clock.NewFake(t0)
	fcfg := files.DefaultConfig()
	fcfg.Object = files.RuleConfig{Enabled: true, AllowedMIME: []string{"image/png"}, TTL: time.Hour}
	fs, err := files.New(files.Options{DB: storetest.Open(t, nil), DataDir: t.TempDir(), Clock: clk, Config: &fcfg})
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	t.Cleanup(func() { fs.Close() })
	r := &rig{
		t: t, clk: clk, pol: NewMemoryPolicy(), appr: NewMemoryApprovals(), eg: &recEgress{}, aeg: &recAudio{}, files: fs, meter: NewMeter(),
		gen: fake.New(fake.WithCapabilities(fake.DefaultCapabilities(provID, "acme-omni"))),
		stt: vfake.New(), tts: vfake.New(), target: fullTarget(), budget: newRecBudget(),
	}
	d := Deps{Policy: r.pol, Egress: r.eg, Audio: r.aeg, Files: fs, Approvals: r.appr, Store: fs, Budgets: r.budget, Clock: clk, Observer: r.meter}
	r.store = fs
	for _, o := range opts {
		o(r, &d)
	}
	svc, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.svc = svc
	return r
}

// on turns modalities on at both levels.
func (r *rig) on(rawAudio bool, ms ...Modality) {
	r.pol.SetProvider(provID, Enable(rawAudio, ms...))
	r.pol.SetTenant(scopeA, Enable(rawAudio, ms...))
}

func pngBytes() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

func (r *rig) upload(name, mime string, content []byte, opts ...func(*files.PutInput)) conversation.FileRef {
	r.t.Helper()
	in := files.PutInput{Name: name, DeclaredMIME: mime, Content: bytes.NewReader(content)}
	for _, o := range opts {
		o(&in)
	}
	ref, err := r.files.Put(context.Background(), scopeA, in)
	if err != nil {
		r.t.Fatalf("Put(%s): %v", name, err)
	}
	return ref
}

func asApplication(in *files.PutInput) { in.Origin = files.OriginApplication }

func asObject(in *files.PutInput) { in.Treatment = files.TreatmentObject }

func wantCode(t *testing.T, err error, code contracts.ErrCode, reason Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s/%s", code, reason)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("code = %s, want %s: %v", got, code, err)
	}
	if got := ReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q: %v", got, reason, err)
	}
}

func verifiedOutcome(text string) verify.Outcome {
	return verify.Outcome{Text: text, Source: verify.SourceModel, Reason: verify.ReasonVerified}
}

func pcm(n int) []byte { return make([]byte, n) }

var pcm16k = voice.PCM16Mono(16000)

func richText(s string) providers.RichResult {
	return providers.RichResult{Parts: []providers.ContentPart{providers.TextPart(s)}}
}

func newLocalSTT() *vfake.Provider { return vfake.New(vfake.WithName("local-stt")) }

func asErr(err error, target **contracts.Error) bool { return errors.As(err, target) }
