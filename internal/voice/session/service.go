package session

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ID prefixes of the voice session (schemas/voice/shared: three lowercase
// letters, then the ULID).
const (
	prefixVoiceSession = "vss"
	prefixUtterance    = "utt"
	prefixPlayback     = "pbk"
)

// Defaults of Deps.
const (
	// DefaultMaxSessions is how many voice connections one process serves at a
	// time. Speech models are the heaviest thing the core runs: work beyond it
	// is refused (resource_exhausted, retryable) rather than queued without
	// bound (master plan 17.2).
	DefaultMaxSessions = 4
	// DefaultMaxSessionsPerTenant and DefaultMaxSessionsPerPrincipal are one
	// tenant's and one user's share of MaxSessions (a tenant is an application
	// and a tenant id, a user is a principal of that tenant), so one tenant or
	// one user cannot hold every slot for MaxDuration while the others are
	// refused.
	DefaultMaxSessionsPerTenant    = 2
	DefaultMaxSessionsPerPrincipal = 1
	// DefaultMaxDuration is how long one voice connection lives at most, and
	// the bound on a connection whose credential has no expiry the handler
	// knows. A longer conversation reconnects with a fresh context token.
	DefaultMaxDuration = 15 * time.Minute
)

// TurnRunner runs one turn of the normal pipeline (master plan 11.1) for a
// finalised transcript. It takes text and nothing else: the voice path cannot
// produce a confirmation, a rejection or a choice, because the interface has
// no way to carry one. *dialogue.Engine is wrapped by EngineTurns; the gateway
// wraps it again to publish the turn into the stream hub (routes_v1_voice.go).
type TurnRunner interface {
	// RunTextTurn runs text as the user's message on the session the scope
	// names, in language ("" lets the core choose). An error with a Result
	// that has a TurnID means the turn ran but could not be reported whole.
	RunTextTurn(ctx context.Context, scope contracts.Scope, text, language string) (dialogue.Result, error)
}

type engineTurns struct{ e *dialogue.Engine }

func (t engineTurns) RunTextTurn(ctx context.Context, scope contracts.Scope, text, language string) (dialogue.Result, error) {
	return t.e.Turn(ctx, scope, dialogue.TurnInput{Text: text, Language: language})
}

// EngineTurns is the TurnRunner of a dialogue engine, with no further effect.
func EngineTurns(e *dialogue.Engine) TurnRunner { return engineTurns{e: e} }

// RetainedAudio is the raw audio of one finished utterance, handed to the
// AudioRetainer of a tenant whose policy retains it. PCM is never serialised.
type RetainedAudio struct {
	UtteranceID string
	Format      voice.AudioFormat
	PCM         []byte `json:"-"`
}

// AudioRetainer keeps raw audio for a tenant whose AudioPolicy says to. It is
// never called for any other tenant, and a deployment with no retainer cannot
// open a voice session for a tenant that retains (it fails closed). Where the
// audio goes, for how long and how it is protected is the retainer's business.
type AudioRetainer interface {
	Retain(ctx context.Context, scope contracts.Scope, audio RetainedAudio) error
}

// SpeakerHint is what an optional speaker-recognition component believes about
// who spoke. It is a convenience for the host's interface and never an
// identity: it is never consulted for a permission, a scope or a confirmation.
type SpeakerHint struct {
	Label      string
	Confidence float64
}

// SpeakerRecognizer is the optional speaker-recognition component (master plan
// 13: "may improve convenience but cannot replace application authorization").
// The session reports its hint beside the transcript and uses it for nothing
// else: the scope an utterance runs under is the one the verified context token
// gave the connection, whatever the hint says.
type SpeakerRecognizer interface {
	Recognize(ctx context.Context, scope contracts.Scope, format voice.AudioFormat, pcm []byte) (SpeakerHint, error)
}

// Deps is what a Service is built from.
type Deps struct {
	// Providers is where the gated voice roles are registered (what
	// profile.Load fills). The roles are looked up when a session opens, so a
	// deployment without voice has no session to open (unavailable, with the
	// section 18 effect of the missing role).
	Providers *providers.Registry
	// Packs is the language-pack registry, when the deployment has one. With it
	// a session opens only while a pack that provides speech recognition is
	// active, and every stream is cancelled when the pack changes under it.
	Packs profile.PackRegistry
	// Policy decides per tenant whether raw audio is retained; nil reads the
	// voice_session configuration section (ConfigPolicy), which retains nothing
	// by default.
	Policy AudioPolicy
	// Retainer keeps the audio of a tenant whose policy retains it.
	Retainer AudioRetainer
	// Speaker is the optional speaker-recognition component.
	Speaker SpeakerRecognizer
	// Clock and IDs default to the real clock and a generator over it.
	Clock clock.Clock
	IDs   *ids.Generator
	// Logger receives ids, sizes and codes: never audio, a transcript or a
	// confirmation secret.
	Logger *slog.Logger
	// Limits caps what one capture, one submission and one speech may cost the
	// runtime; the zero value takes voice.DefaultLimits.
	Limits voice.Limits
	// MaxSessions and MaxDuration default to DefaultMaxSessions and
	// DefaultMaxDuration.
	MaxSessions int
	MaxDuration time.Duration
	// MaxSessionsPerTenant and MaxSessionsPerPrincipal default to
	// DefaultMaxSessionsPerTenant and DefaultMaxSessionsPerPrincipal, and are
	// never more than MaxSessions and MaxSessionsPerTenant respectively.
	MaxSessionsPerTenant    int
	MaxSessionsPerPrincipal int
}

// Options is what one voice session is opened with.
type Options struct {
	// Turns runs the finalised transcripts; required.
	Turns TurnRunner
	// Language is the user's BCP 47 tag: the recognition hint of a capture that
	// names none, and the language of the reply when the core supports it. ""
	// leaves both to the core.
	Language string
	// NoSpeech turns speech off: responses arrive as text only.
	NoSpeech bool
	// Deadline is when the credential the connection was opened with expires:
	// the session ends then, so a scope never outlives its token. Zero means
	// MaxDuration.
	Deadline time.Time
	// Logger is the request-scoped logger; nil uses Deps.Logger.
	Logger *slog.Logger
}

// Service opens voice sessions. It owns no goroutine of its own and holds no
// speech model: a text-only deployment never constructs one, and a deployment
// with voice pays nothing until a session opens.
type Service struct {
	d   Deps
	ids *ids.Generator

	mu     sync.Mutex
	closed bool
	live   map[*Session]struct{}
	byKey  map[string]*Session
}

// New returns a Service over d.
func New(d Deps) (*Service, error) {
	if d.Providers == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "voice session: the service needs the provider registry the voice profile fills")
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Policy == nil {
		d.Policy = ConfigPolicy{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Limits == (voice.Limits{}) {
		d.Limits = voice.DefaultLimits()
	}
	if err := d.Limits.Validate(); err != nil {
		return nil, err
	}
	if d.MaxSessions <= 0 {
		d.MaxSessions = DefaultMaxSessions
	}
	if d.MaxSessionsPerTenant <= 0 {
		d.MaxSessionsPerTenant = DefaultMaxSessionsPerTenant
	}
	d.MaxSessionsPerTenant = min(d.MaxSessionsPerTenant, d.MaxSessions)
	if d.MaxSessionsPerPrincipal <= 0 {
		d.MaxSessionsPerPrincipal = DefaultMaxSessionsPerPrincipal
	}
	d.MaxSessionsPerPrincipal = min(d.MaxSessionsPerPrincipal, d.MaxSessionsPerTenant)
	if d.MaxDuration <= 0 {
		d.MaxDuration = DefaultMaxDuration
	}
	g := d.IDs
	if g == nil {
		g = ids.NewGenerator(d.Clock)
	}
	return &Service{d: d, ids: g, live: map[*Session]struct{}{}, byKey: map[string]*Session{}}, nil
}

func unavailable(role providers.Role, why string) error {
	return contracts.New(contracts.ErrUnavailable, "voice: "+why).
		WithDetails("role", role.String()).
		WithDetails("effect", role.Effect())
}

// Open prepares a voice session on the conversation session scope names. It
// refuses, before anything is allocated beyond the registration (the gateway
// turns each refusal into an HTTP status, ahead of the WebSocket upgrade):
//
//   - a scope that does not validate (no default scope exists);
//   - no speech-recognition role, or no active language pack that provides it
//     (unavailable, effect text_input_only: the user types, text keeps working);
//   - a tenant that retains raw audio when no retainer is configured, or a
//     policy that cannot answer (fail closed);
//   - more sessions than MaxSessions, than MaxSessionsPerTenant for the
//     scope's tenant or than MaxSessionsPerPrincipal for its principal
//     (resource_exhausted, retryable; details.limit says which).
//
// A speech-synthesis role that is missing is not a refusal: the session opens
// with speech off and every response arrives as verified text (effect
// verified_text_only). Opening a second session for the conversation session
// closes the first (a reconnect takes over).
//
// The session does nothing until Serve is called; a caller that does not serve
// it must Close it.
func (s *Service) Open(ctx context.Context, scope contracts.Scope, o Options) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if o.Turns == nil {
		return nil, contracts.New(contracts.ErrInternal, "voice session: no turn runner was supplied")
	}
	if o.Language != "" && !voice.ValidLanguage(o.Language) {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "voice: language %q is not a language tag", o.Language)
	}
	life := s.d.MaxDuration
	if !o.Deadline.IsZero() {
		left := o.Deadline.Sub(s.d.Clock.Now())
		if left <= 0 {
			return nil, contracts.New(contracts.ErrExpired, "the credential the voice session was opened with has expired")
		}
		life = min(life, left)
	}

	stt, err := voice.LookupSTT(s.d.Providers)
	if err != nil {
		return nil, err
	}
	var packID string
	packSpeech := true
	if s.d.Packs != nil {
		pack, ok := s.d.Packs.Active()
		switch {
		case !ok:
			return nil, unavailable(providers.RoleSTT, "no language pack is active")
		case !pack.Provides(providers.RoleSTT):
			return nil, unavailable(providers.RoleSTT, "the active language pack does not provide speech recognition")
		}
		packID, packSpeech = pack.ID, pack.Provides(providers.RoleTTS)
	}
	var tts voice.TTS
	if !o.NoSpeech && packSpeech {
		// A missing synthesis role leaves speech off; it never fails the session.
		if t, err := voice.LookupTTS(s.d.Providers); err == nil {
			tts = t
		}
	}

	retain, err := s.d.Policy.RetainRawAudio(ctx, scope)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "voice: the audio retention policy could not be read")
	}
	if retain && s.d.Retainer == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "voice: this tenant retains raw audio and no audio store is configured")
	}

	log := o.Logger
	if log == nil {
		log = s.d.Logger
	}
	sess := &Session{
		svc: s, id: s.ids.New(prefixVoiceSession), scope: scope.Clone(), opts: o,
		stt: stt, tts: tts, retain: retain, packID: packID, life: life,
		log: log, key: scopeKey(scope),
	}
	sess.log = log.With("voice_session_id", sess.id)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, contracts.New(contracts.ErrUnavailable, "voice: the service is shutting down")
	}
	prev := s.byKey[sess.key]
	if err := s.admitLocked(scope, prev); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.live[sess] = struct{}{}
	s.byKey[sess.key] = sess
	s.mu.Unlock()
	if prev != nil {
		prev.Close(ReasonReplaced)
	}
	return sess, nil
}

// admitLocked refuses a session over one of the three caps. The session prev,
// which the new one replaces, does not count against any of them: a reconnect
// is not a new connection. Sessions are few (MaxSessions), so they are counted
// afresh rather than kept in counters that could drift.
func (s *Service) admitLocked(scope contracts.Scope, prev *Session) error {
	var total, tenant, principal int
	for l := range s.live {
		if l == prev {
			continue
		}
		total++
		if l.scope.ApplicationID == scope.ApplicationID && l.scope.TenantID == scope.TenantID {
			tenant++
			if l.scope.PrincipalID == scope.PrincipalID {
				principal++
			}
		}
	}
	refuse := func(limit string, n int, msg string) error {
		return contracts.Newf(contracts.ErrResourceExhausted, msg, n).WithDetails("limit", limit)
	}
	switch {
	case total >= s.d.MaxSessions:
		return refuse("deployment", s.d.MaxSessions, "voice: %d voice sessions are open; try again shortly")
	case tenant >= s.d.MaxSessionsPerTenant:
		return refuse("tenant", s.d.MaxSessionsPerTenant, "voice: this tenant has %d voice sessions open; try again shortly")
	case principal >= s.d.MaxSessionsPerPrincipal:
		return refuse("principal", s.d.MaxSessionsPerPrincipal, "voice: this user has %d voice sessions open; close one or try again shortly")
	}
	return nil
}

func scopeKey(s contracts.Scope) string {
	return strings.Join([]string{s.ApplicationID, s.TenantID, s.WorkspaceID, s.SessionID}, "\x00")
}

func (s *Service) deregister(sess *Session) {
	s.mu.Lock()
	delete(s.live, sess)
	if s.byKey[sess.key] == sess {
		delete(s.byKey, sess.key)
	}
	s.mu.Unlock()
}

// Live returns how many voice sessions are open.
func (s *Service) Live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// CancelLiveStreams cancels the capture and the playback of every open session
// (reason pack_changed). The sessions stay open and text keeps working. The
// profile's gate is checked when a stream opens and a stream already open keeps
// running after the pack is deactivated or the profile closes, so whoever takes
// the pack away calls this first: Packs does it for Deactivate, and the
// composition root calls it before profile.Close.
func (s *Service) CancelLiveStreams() {
	s.mu.Lock()
	live := make([]*Session, 0, len(s.live))
	for sess := range s.live {
		live = append(live, sess)
	}
	s.mu.Unlock()
	for _, sess := range live {
		sess.cancelStreams(reasonPackChanged)
	}
}

// Close refuses new sessions and closes the open ones (ReasonServerClosing).
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	live := make([]*Session, 0, len(s.live))
	for sess := range s.live {
		live = append(live, sess)
	}
	s.mu.Unlock()
	for _, sess := range live {
		sess.Close(ReasonServerClosing)
	}
}

// Packs returns the language-pack registry the operator must use so that the
// session service learns of a change: its Deactivate cancels the live streams
// first and then delegates. It is nil when Deps.Packs was not set.
func (s *Service) Packs() profile.PackRegistry {
	if s.d.Packs == nil {
		return nil
	}
	return packGuard{svc: s, inner: s.d.Packs}
}

type packGuard struct {
	svc   *Service
	inner profile.PackRegistry
}

func (g packGuard) Installed(ctx context.Context) ([]profile.Pack, error) {
	return g.inner.Installed(ctx)
}
func (g packGuard) Active() (profile.Pack, bool) { return g.inner.Active() }
func (g packGuard) Activate(ctx context.Context, id string) (profile.Pack, error) {
	return g.inner.Activate(ctx, id)
}
func (g packGuard) Deactivate(ctx context.Context) error {
	g.svc.CancelLiveStreams()
	return g.inner.Deactivate(ctx)
}
