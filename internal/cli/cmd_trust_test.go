package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/tools/devkeys"
)

func TestTrustAddListRevoke(t *testing.T) {
	e := newOpsEnv(t, "")
	_, pubFile, pub := e.genKey("publisher-2026")

	var added keyOut
	decode(t, e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", "publisher-2026", "-public-key-file", pubFile,
		"-label", "Example Libraries release key", "-operator", "root").stdout, &added)
	if added.Status != "active" || added.KeyID != "publisher-2026" || added.Fingerprint != trust.Fingerprint(pub) || added.TestOnly {
		t.Fatalf("added = %+v", added)
	}
	if added.PublicKey != trust.EncodePublicKey(pub) {
		t.Errorf("public key = %q", added.PublicKey)
	}

	// the same key id, or the same key material, cannot be registered twice
	if res := e.run("trust", "add-key", "-app", "demo-library", "-key-id", "publisher-2026", "-public-key", trust.EncodePublicKey(pub)); res.code != ExitError {
		t.Errorf("a key id is registered once: exit %d", res.code)
	}
	if res := e.run("trust", "add-key", "-app", "demo-library", "-key-id", "another-id", "-public-key", trust.EncodePublicKey(pub)); res.code != ExitError {
		t.Errorf("key material is registered once per application: exit %d", res.code)
	}

	var keys []keyOut
	decode(t, e.mustRun("trust", "list", "-app", "demo-library").stdout, &keys)
	if len(keys) != 1 || keys[0].KeyID != "publisher-2026" || keys[0].Status != "active" || keys[0].Label == "" {
		t.Fatalf("list = %+v", keys)
	}
	var none []keyOut
	decode(t, e.mustRun("trust", "list", "-app", "other-app").stdout, &none)
	if len(none) != 0 {
		t.Errorf("keys are per application: %+v", none)
	}

	// another application registered the same key material
	e.mustRun("trust", "add-key", "-app", "other-app", "-key-id", "shared", "-public-key", trust.EncodePublicKey(pub))

	var revoked keyOut
	res := e.mustRun("trust", "revoke-key", "-app", "demo-library", "-key-id", "publisher-2026", "-reason", "laptop stolen", "-operator", "root")
	decode(t, res.stdout, &revoked)
	if revoked.Status != "revoked" || revoked.RevokedReason != "laptop stolen" || revoked.RevokedAt == nil {
		t.Fatalf("revoked = %+v", revoked)
	}
	// the material is revoked for the other application too: it is listed so, and a running
	// server is told to reload both applications
	if n := strings.Count(res.stderr, "alixai: note:"); n != 2 {
		t.Errorf("the revocation told the server about %d applications, want 2:\n%s", n, res.stderr)
	}
	decode(t, e.mustRun("trust", "list", "-app", "other-app").stdout, &keys)
	if len(keys) != 1 || keys[0].Status != "revoked" {
		t.Errorf("the other application's copy of the revoked material = %+v", keys)
	}
	// revoked key material can never come back under another name
	if res := e.run("trust", "add-key", "-app", "demo-library", "-key-id", "renamed", "-public-key", trust.EncodePublicKey(pub)); res.code != ExitError {
		t.Errorf("a revoked key cannot be registered again: exit %d", res.code)
	}
	if res := e.run("trust", "revoke-key", "-app", "demo-library", "-key-id", "never-registered", "-reason", "x"); res.code != ExitError {
		t.Errorf("an unknown key cannot be revoked: exit %d", res.code)
	}

	events := e.eventTypes("demo-library")
	if count(events, "settings.changed") != 2 { // the add and the revoke; refused requests leave no record
		t.Errorf("trust changes are in the audit log once each: %v", events)
	}
}

func TestTrustRefusesTestOnlyKeysWithoutTheDeveloperFlag(t *testing.T) {
	e := newOpsEnv(t, "")
	for _, b64 := range []string{devkeys.PublicKeyBase64(), trust.SchemaExamplePublicKeyB64} {
		res := e.run("trust", "add-key", "-app", "demo-library", "-key-id", "published", "-public-key", b64)
		if res.code != ExitUsage || !strings.Contains(res.stderr, "TEST-ONLY") {
			t.Fatalf("exit %d\n%s", res.code, res.stderr)
		}
	}
	var keys []keyOut
	decode(t, e.mustRun("trust", "list", "-app", "demo-library").stdout, &keys)
	if len(keys) != 0 {
		t.Fatalf("a refused key must not be registered: %+v", keys)
	}
	var added keyOut
	decode(t, e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys").stdout, &added)
	if !added.TestOnly {
		t.Errorf("the listing marks a published TEST-ONLY key: %+v", added)
	}
	decode(t, e.mustRun("trust", "list", "-app", "demo-library").stdout, &keys)
	if len(keys) != 1 || !keys[0].TestOnly {
		t.Errorf("list = %+v", keys)
	}
}

func TestTrustUsageErrors(t *testing.T) {
	e := newOpsEnv(t, "")
	_, pubFile, pub := e.genKey("k")
	b64 := trust.EncodePublicKey(pub)
	for name, args := range map[string][]string{
		"no app":                {"trust", "add-key", "-key-id", "k", "-public-key", b64},
		"no key id":             {"trust", "add-key", "-app", "a", "-public-key", b64},
		"no key":                {"trust", "add-key", "-app", "a", "-key-id", "k"},
		"two keys":              {"trust", "add-key", "-app", "a", "-key-id", "k", "-public-key", b64, "-public-key-file", pubFile},
		"garbled key":           {"trust", "add-key", "-app", "a", "-key-id", "k", "-public-key", "not base64!"},
		"short key":             {"trust", "add-key", "-app", "a", "-key-id", "k", "-public-key", "AAAA"},
		"stray argument":        {"trust", "add-key", "-app", "a", "-key-id", "k", "-public-key", b64, "extra"},
		"bad operator":          {"trust", "add-key", "-app", "a", "-key-id", "k", "-public-key", b64, "-operator", "no good"},
		"revoke without reason": {"trust", "revoke-key", "-app", "a", "-key-id", "k"},
		"list without app":      {"trust", "list"},
	} {
		if res := e.run(args...); res.code != ExitUsage {
			t.Errorf("%s: exit %d, want usage\n%s", name, res.code, res.stderr)
		}
	}
	if res := e.run("trust", "add-key", "-app", "a", "-key-id", "k", "-public-key-file", filepath.Join(e.dir, "missing.pub")); res.code != ExitError {
		t.Errorf("a missing key file is an error: exit %d", res.code)
	}
}
