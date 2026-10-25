package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// The harness is a host application for the length of a run: every request
// to the stack needs a fresh single-use context token (ADR 0013), signed with
// the key the application registered (`alixai identity add-issuer-key`). The
// minter writes exactly the profile the verifier accepts: header alg, typ and
// kid, the closed claim set, a fresh jti per token and a short lifetime.
//
// The key comes from a file the operator names. There is no default key and no
// fallback to the repository's test-only publisher key: a run that is not given
// a key does not start (the operator may pass tools/devkeys/test-only-publisher.key.pem
// explicitly to a stack that trusts it).

// tokenLifetime is short: a token is used at once and never again.
const tokenLifetime = 2 * time.Minute

// TokenIdentity is who the harness speaks as.
type TokenIdentity struct {
	Issuer      string
	Audience    string // the deployment id
	KeyID       string
	Application string
	Tenant      string
	Workspace   string
	Principal   string
	Roles       []string
	Scopes      []string
}

// Minter mints context tokens for one identity and one session.
type Minter struct {
	key     ed25519.PrivateKey
	id      TokenIdentity
	session string
	now     func() time.Time
}

// NewMinter validates the identity (as the verifier will) and returns a Minter.
func NewMinter(key ed25519.PrivateKey, id TokenIdentity, session string) (*Minter, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("the issuer key is not an Ed25519 private key")
	}
	if id.Issuer == "" || id.Audience == "" || id.KeyID == "" {
		return nil, errors.New("the token issuer, audience (the deployment id) and key id are required")
	}
	probe := contracts.Scope{
		DeploymentID: id.Audience, ApplicationID: id.Application, TenantID: id.Tenant, WorkspaceID: id.Workspace,
		PrincipalID: id.Principal, Roles: id.Roles, CapabilityScopes: append([]string{"x"}, id.Scopes...), SessionID: session,
	}
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("the token identity is not a valid scope: %w", err)
	}
	return &Minter{key: key, id: id, session: session, now: time.Now}, nil
}

// LoadIssuerKey reads an Ed25519 private key from a PKCS#8 PEM file. Text
// before the PEM block (the test key file carries a warning there) is ignored.
func LoadIssuerKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("--issuer-key is required: the harness mints context tokens and has no default key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: no PKCS#8 PRIVATE KEY block", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ed, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: the key is not Ed25519", path)
	}
	return ed, nil
}

var tokenEncoding = base64.RawURLEncoding

// Mint returns a fresh token. extra capability scopes are added to the
// identity's own (the queue probe adds ops.queue.read to its token only).
func (m *Minter) Mint(extra ...string) (string, error) {
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", err
	}
	now := m.now()
	scopes := slices.Clone(m.id.Scopes)
	for _, s := range extra {
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	header := map[string]any{
		identity.HeaderAlgorithm: identity.Algorithm,
		identity.HeaderType:      identity.TokenType,
		identity.HeaderKeyID:     m.id.KeyID,
	}
	claims := map[string]any{
		identity.ClaimIssuer:           m.id.Issuer,
		identity.ClaimAudience:         m.id.Audience,
		identity.ClaimSubject:          m.id.Principal,
		identity.ClaimIssuedAt:         now.Unix(),
		identity.ClaimNotBefore:        now.Unix(),
		identity.ClaimExpiry:           now.Add(tokenLifetime).Unix(),
		identity.ClaimTokenID:          tokenEncoding.EncodeToString(jti[:]),
		identity.ClaimApplication:      m.id.Application,
		identity.ClaimTenant:           m.id.Tenant,
		identity.ClaimWorkspace:        m.id.Workspace,
		identity.ClaimRoles:            m.id.Roles,
		identity.ClaimCapabilityScopes: scopes,
		identity.ClaimSession:          m.session,
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := tokenEncoding.EncodeToString(h) + "." + tokenEncoding.EncodeToString(p)
	return input + "." + tokenEncoding.EncodeToString(ed25519.Sign(m.key, []byte(input))), nil
}

// Provider is an sdkgo.TokenProvider: a fresh token for every request.
func (m *Minter) Provider(extra ...string) sdkgo.TokenProvider {
	return func(context.Context, sdkgo.TokenRequest) (string, error) { return m.Mint(extra...) }
}

// newSessionID makes the session id of a run: the core never mints one, the host does.
func newSessionID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ses_soak_" + hex.EncodeToString(b[:]), nil
}
