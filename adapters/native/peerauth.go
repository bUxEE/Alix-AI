package native

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// PeerSession is what mutual authentication established about a peer: the
// application it proved to be (it held that application's key) and under which
// key id. It is made by whoever ran the handshake, never taken from a request.
type PeerSession struct {
	ApplicationID string
	KeyID         string
	// Issuer is the issuer value the application package declares for its
	// local_ipc context exchange.
	Issuer string
}

// PeerAuthenticator is the identity.Authenticator of mutually authenticated local
// IPC (ADR 0013, the alternative to context tokens of master plan 10.1). Where the
// token Verifier turns a signed credential into a Scope, this turns an
// authenticated peer session and the context claims it asserts into one, through
// the same constructor (identity.NewScope), so the HTTP middleware and every other
// consumer see one shape whichever transport was used.
//
// The credential is a single-use, short-lived ticket: Admit records the claims of
// an authenticated peer and returns it, Authenticate redeems it once. The ticket
// is not a bearer secret worth stealing (it expires in seconds and works once),
// and the claims never travel in it. The deployment level of the Scope is
// configuration, never something a peer asserts.
type PeerAuthenticator struct {
	deployment string
	clk        clock.Clock
	ttl        time.Duration

	mu      sync.Mutex
	pending map[string]peerTicket
}

type peerTicket struct {
	scope   contracts.Scope
	issuer  string
	keyID   string
	issued  time.Time
	expires time.Time
}

var _ identity.Authenticator = (*PeerAuthenticator)(nil)

// maxPendingTickets bounds the tickets waiting to be redeemed.
const maxPendingTickets = 4096

// NewPeerAuthenticator returns an authenticator for one deployment. ttl is how
// long a ticket can wait to be redeemed; 0 means 30 seconds, and nothing longer
// than a minute is accepted.
func NewPeerAuthenticator(deploymentID string, clk clock.Clock, ttl time.Duration) (*PeerAuthenticator, error) {
	if deploymentID == "" {
		return nil, errors.New("native: a peer authenticator needs the deployment ID")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if ttl > time.Minute {
		return nil, errors.New("native: a peer ticket cannot live longer than a minute")
	}
	return &PeerAuthenticator{deployment: deploymentID, clk: clk, ttl: ttl, pending: map[string]peerTicket{}}, nil
}

func peerRefusal(code contracts.ErrCode, reason identity.Reason) *contracts.Error {
	return contracts.New(code, "peer context rejected").WithDetails(identity.DetailReason, string(reason))
}

// Admit records the context a peer, already authenticated by the handshake,
// asserts, and returns the credential that redeems it. The claims become a Scope
// through identity.NewScope now, so a context that is incomplete or malformed is
// refused here (scope_missing, scope_violation) and no ticket exists for it. A
// peer authenticated as one application cannot assert another: that is a
// scope_violation.
func (p *PeerAuthenticator) Admit(peer PeerSession, claims identity.ContextClaims) (string, error) {
	if peer.ApplicationID == "" || peer.KeyID == "" || peer.Issuer == "" {
		return "", peerRefusal(contracts.ErrUnauthenticated, identity.ReasonNoCredential)
	}
	if claims.ApplicationID != peer.ApplicationID {
		return "", contracts.New(contracts.ErrScopeViolation, "the peer is not authenticated for this application").
			WithDetails("levels", []string{string(contracts.LevelApplication)})
	}
	scope, err := identity.NewScope(p.deployment, claims)
	if err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", contracts.Wrap(contracts.ErrInternal, err, "cannot generate a peer ticket")
	}
	credential := hex.EncodeToString(raw[:])
	now := p.clk.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	for k, t := range p.pending {
		if !now.Before(t.expires) {
			delete(p.pending, k)
		}
	}
	if len(p.pending) >= maxPendingTickets {
		return "", contracts.New(contracts.ErrResourceExhausted, "too many peer tickets are waiting to be redeemed")
	}
	p.pending[ticketKey(credential)] = peerTicket{scope: scope, issuer: peer.Issuer, keyID: peer.KeyID, issued: now, expires: now.Add(p.ttl)}
	return credential, nil
}

func ticketKey(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

// Authenticate implements identity.Authenticator. A ticket is redeemed once: a
// second presentation is replayed, an old one expired, an unknown one malformed,
// all unauthenticated.
func (p *PeerAuthenticator) Authenticate(_ context.Context, credential string) (*identity.Verified, error) {
	if len(credential) != 64 {
		return nil, peerRefusal(contracts.ErrUnauthenticated, identity.ReasonMalformed)
	}
	key := ticketKey(credential)
	p.mu.Lock()
	t, ok := p.pending[key]
	delete(p.pending, key)
	p.mu.Unlock()
	if !ok {
		return nil, peerRefusal(contracts.ErrUnauthenticated, identity.ReasonReplayed)
	}
	if !p.clk.Now().Before(t.expires) {
		return nil, peerRefusal(contracts.ErrUnauthenticated, identity.ReasonExpired)
	}
	return &identity.Verified{
		Scope: t.scope, Issuer: t.issuer, KeyID: t.keyID, TokenID: key[:32], IssuedAt: t.issued, ExpiresAt: t.expires,
	}, nil
}
