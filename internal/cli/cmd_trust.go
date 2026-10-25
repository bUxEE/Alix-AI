package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/trust"
)

func init() {
	Register(Command{
		Name:    "trust add-key",
		Summary: "register a publisher's public key for an application in the trust store",
		Run:     runTrustAddKey,
	})
	Register(Command{
		Name:    "trust revoke-key",
		Summary: "revoke a registered key permanently: nothing it signed verifies again",
		Run:     runTrustRevokeKey,
	})
	Register(Command{
		Name:    "trust list",
		Summary: "list the keys registered for an application, as JSON",
		Run:     runTrustList,
	})
}

type keyOut struct {
	ApplicationID string `json:"application_id"`
	KeyID         string `json:"key_id"`
	Status        string `json:"status"`
	// Purpose is what a platform key may sign (release, audit-checkpoint).
	Purpose     string `json:"purpose,omitempty"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	Label       string `json:"label,omitempty"`
	// TestOnly marks a published TEST-ONLY key, which anyone can sign with.
	TestOnly      bool       `json:"test_only,omitempty"`
	RegisteredAt  time.Time  `json:"registered_at"`
	RetiredAt     *time.Time `json:"retired_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedReason string     `json:"revoked_reason,omitempty"`
}

func keyOf(k trust.Key) keyOut {
	out := keyOut{
		ApplicationID: k.ApplicationID, KeyID: k.KeyID, Status: string(k.Status), Purpose: string(k.Purpose), Fingerprint: trust.Fingerprint(k.PublicKey),
		PublicKey: trust.EncodePublicKey(k.PublicKey), Label: k.Label, TestOnly: trust.IsTestOnlyKey(k.PublicKey),
		RegisteredAt: k.RegisteredAt, RevokedReason: k.RevokedReason,
	}
	if !k.RetiredAt.IsZero() {
		t := k.RetiredAt
		out.RetiredAt = &t
	}
	if !k.RevokedAt.IsZero() {
		t := k.RevokedAt
		out.RevokedAt = &t
	}
	return out
}

// recordTrustChange writes the change to the audit log: on the application's
// chain, or the platform's for the platform application. A change that cannot
// be recorded is reported, not hidden.
func recordTrustChange(ctx context.Context, s *stack, app, operator string, data map[string]any) error {
	loc := audit.Platform()
	if app != trust.PlatformApplicationID {
		var err error
		if loc, err = audit.ForApplication(app, operator); err != nil {
			return err
		}
	}
	data["setting"] = "trust.publisher_key"
	_, err := s.log.AppendAt(ctx, loc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: data})
	return err
}

const addKeyUsage = "alixai trust add-key [-config FILE] -app ID -key-id KEYID (-public-key BASE64 | -public-key-file FILE) [-purpose release|audit-checkpoint] [-label TEXT] [-allow-test-keys] [-operator ID]"

// runTrustAddKey registers a publisher's Ed25519 public key (the base64 of the
// raw 32 bytes, as 'alixai package keygen' writes in its .pub file) as trusted
// for one application. Packages of that application signed with the key then
// verify. It never registers the published TEST-ONLY keys unless
// -allow-test-keys is given, which is for development deployments only: anyone
// can sign with them. The platform's own report-signing key is registered the
// same way, for the application alixai.platform.
//
// -purpose binds a platform key to one kind of artifact (H1): "release" for a
// key that signs release manifests, "audit-checkpoint" for one that signs audit
// checkpoints and tombstones. Release and checkpoint verification accept only a
// key of their purpose (and the built-in release key). Given for a key that is
// already registered with the same material and no purpose, it classifies that
// key instead of failing: how a key registered before purposes existed is
// given one. A purpose is never changed once set.
func runTrustAddKey(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("trust add-key")
	path := env.ConfigFlag(fs)
	app := fs.String("app", "", "the application the key is trusted for (required)")
	keyID := fs.String("key-id", "", "the key's ID, as packages name it in integrity.publisher_key_ids (required)")
	pubB64 := fs.String("public-key", "", "the public key: base64 of the raw 32 bytes")
	pubFile := fs.String("public-key-file", "", "a file holding the public key as base64 (the .pub file 'alixai package keygen' writes)")
	label := fs.String("label", "", "a note for operators, at most 256 bytes")
	purposeFlag := fs.String("purpose", "", "what the platform key may sign: release or audit-checkpoint (alixai.platform only)")
	allowTest := fs.Bool("allow-test-keys", false, "accept a published TEST-ONLY key (development deployments only: anyone can sign with it)")
	operator := fs.String("operator", "", "who registers the key, recorded in the audit log")
	if err := env.ParseFlags(fs, addKeyUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), addKeyUsage)
	}
	if *app == "" || *keyID == "" {
		return Usagef("-app and -key-id are required (usage: %s)", addKeyUsage)
	}
	if (*pubB64 == "") == (*pubFile == "") {
		return Usagef("give exactly one of -public-key and -public-key-file")
	}
	if *operator != "" && !operatorIDRE.MatchString(*operator) {
		return Usagef("-operator %q is not an operator id (1 to 128 letters, digits and ._@:-)", *operator)
	}
	purpose, err := trust.ParseKeyPurpose(*purposeFlag)
	if err != nil {
		return Usagef("-purpose: %v", err)
	}
	if purpose != trust.PurposeNone && *app != trust.PlatformApplicationID {
		return Usagef("-purpose is for keys of %s only", trust.PlatformApplicationID)
	}
	enc := *pubB64
	if *pubFile != "" {
		b, err := os.ReadFile(*pubFile)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", *pubFile, err)
		}
		enc = strings.TrimSpace(string(b))
	}
	pub, err := trust.DecodePublicKey(enc)
	if err != nil {
		return Usagef("the public key is not the base64 of 32 raw bytes: %v", err)
	}
	if trust.IsTestOnlyKey(pub) && !*allowTest {
		return Usagef("this is a published TEST-ONLY key that anyone can sign with; pass -allow-test-keys to register it in a development deployment")
	}
	s, err := openStack(ctx, env, *path, *allowTest)
	if err != nil {
		return err
	}
	defer s.close()
	action := "add"
	key, err := s.trustStore.RegisterKey(ctx, *app, *keyID, ed25519.PublicKey(pub), *label, trust.WithPurpose(purpose))
	if errors.Is(err, trust.ErrKeyConflict) && purpose != trust.PurposeNone {
		// The same key, registered before purposes existed: classify it.
		stored, lerr := s.trustStore.LookupKey(ctx, *app, *keyID)
		if lerr == nil && stored.PublicKey.Equal(ed25519.PublicKey(pub)) && stored.Purpose == trust.PurposeNone {
			if err = s.trustStore.SetKeyPurpose(ctx, *app, *keyID, purpose); err == nil {
				action = "set_purpose"
				key, err = s.trustStore.LookupKey(ctx, *app, *keyID)
			}
		}
	}
	if err != nil {
		return err
	}
	data := map[string]any{
		"action": action, "application": key.ApplicationID, "key_id": key.KeyID, "fingerprint": trust.Fingerprint(key.PublicKey),
	}
	if key.Purpose != trust.PurposeNone {
		data["purpose"] = string(key.Purpose)
	}
	if err := recordTrustChange(ctx, s, key.ApplicationID, *operator, data); err != nil {
		return fmt.Errorf("the key is registered but the audit log could not record it: %w", err)
	}
	return emitJSON(env.Stdout, keyOf(key))
}

const revokeKeyUsage = "alixai trust revoke-key [-config FILE] -app ID -key-id KEYID -reason TEXT [-operator ID]"

// runTrustRevokeKey revokes a key permanently. Nothing it signed verifies
// again, whenever the signature claims to have been made: packages installed
// from it no longer open (a running service notices within its cache lifetime,
// at most 30 seconds), and the revoked key material can never be registered
// again under another name. Use it when a key is compromised; a key that is
// merely replaced is rotated.
func runTrustRevokeKey(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("trust revoke-key")
	path := env.ConfigFlag(fs)
	app := fs.String("app", "", "the application the key is registered for (required)")
	keyID := fs.String("key-id", "", "the key to revoke (required)")
	reason := fs.String("reason", "", "why the key is revoked, at most 512 bytes (required)")
	operator := fs.String("operator", "", "who revokes the key, recorded in the audit log")
	if err := env.ParseFlags(fs, revokeKeyUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), revokeKeyUsage)
	}
	if *app == "" || *keyID == "" || strings.TrimSpace(*reason) == "" {
		return Usagef("-app, -key-id and -reason are required (usage: %s)", revokeKeyUsage)
	}
	if *operator != "" && !operatorIDRE.MatchString(*operator) {
		return Usagef("-operator %q is not an operator id (1 to 128 letters, digits and ._@:-)", *operator)
	}
	s, err := openStack(ctx, env, *path, false)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.trustStore.RevokeKey(ctx, *app, *keyID, *reason); err != nil {
		return err
	}
	if err := recordTrustChange(ctx, s, *app, *operator, map[string]any{
		"action": "revoke", "application": *app, "key_id": *keyID, "reason": *reason,
	}); err != nil {
		return fmt.Errorf("the key is revoked but the audit log could not record it: %w", err)
	}
	k, err := s.trustStore.LookupKey(ctx, *app, *keyID)
	if err != nil {
		return err
	}
	if err := emitJSON(env.Stdout, keyOf(k)); err != nil {
		return err
	}
	// A running server keeps a verified package for a short while; tell it now so a revoked
	// key stops serving what it signed at once instead of when that cache expires. With no
	// server running this is a no-op; a reachable server that does not confirm fails the command.
	_, err = transportsNotifyRefresh(ctx, env, s.dataDir, *app)
	// The material is revoked for every application that registered it (trust.LookupKey),
	// so each of them reloads too.
	others, oerr := s.trustStore.ApplicationsSharingKey(ctx, *app, *keyID)
	if oerr != nil {
		return errors.Join(err, oerr)
	}
	for _, other := range others {
		if _, nerr := transportsNotifyRefresh(ctx, env, s.dataDir, other); err == nil {
			err = nerr
		}
	}
	return err
}

const trustListUsage = "alixai trust list [-config FILE] -app ID"

// runTrustList prints the keys registered for an application, oldest first,
// with their status and fingerprint. Keys that are the published TEST-ONLY keys
// are marked.
func runTrustList(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("trust list")
	path := env.ConfigFlag(fs)
	app := fs.String("app", "", "the application whose keys are listed (required); use alixai.platform for the report-signing keys")
	if err := env.ParseFlags(fs, trustListUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *app == "" {
		return Usagef("-app is required and nothing else is accepted (usage: %s)", trustListUsage)
	}
	s, err := openStack(ctx, env, *path, false)
	if err != nil {
		return err
	}
	defer s.close()
	keys, err := s.trustStore.ListKeys(ctx, *app)
	if err != nil {
		return err
	}
	out := make([]keyOut, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyOf(k))
	}
	return emitJSON(env.Stdout, out)
}
