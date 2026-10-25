package identity_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var bg = context.Background()

// wantReject fails t unless err is a typed refusal with the given code and
// reason. Asserting the reason is what proves a test exercised the check it is
// named for, not some earlier one.
func wantReject(t *testing.T, err error, code contracts.ErrCode, reason identity.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("token accepted; want %s (%s)", code, reason)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *contracts.Error", err)
	}
	if ce.Code != code {
		t.Fatalf("code = %s, want %s (error: %v)", ce.Code, code, err)
	}
	if got := identity.ReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q (error: %v)", got, reason, err)
	}
}

// wantScopeLevels fails t unless err has the code and names exactly these levels.
func wantScopeLevels(t *testing.T, err error, code contracts.ErrCode, levels ...string) {
	t.Helper()
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
	got, _ := ce.Details["levels"].([]string)
	if strings.Join(got, ",") != strings.Join(levels, ",") {
		t.Fatalf("levels = %v, want %v", got, levels)
	}
}

// replayRows reports how many nonces the cache holds.
func replayRows(t *testing.T, env *identitytest.Env) int {
	t.Helper()
	n, err := env.Replay.Len(bg)
	if err != nil {
		t.Fatalf("replay Len: %v", err)
	}
	return n
}

// segments splits a compact token.
func segments(t *testing.T, token string) (h, p, s string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	return parts[0], parts[1], parts[2]
}

// withPayload returns token with its payload rewritten by edit and the
// original header and signature kept: a tampered token.
func withPayload(t *testing.T, token string, edit func(map[string]any)) string {
	t.Helper()
	h, p, s := segments(t, token)
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return h + "." + base64.RawURLEncoding.EncodeToString(out) + "." + s
}

const minute = time.Minute

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
