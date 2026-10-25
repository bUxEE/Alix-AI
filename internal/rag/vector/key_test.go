package vector

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func mustKey(t *testing.T, scope contracts.Scope, model, input string) InputKey {
	t.Helper()
	k, err := NewInputKey(scope, model, input)
	if err != nil {
		t.Fatalf("NewInputKey: %v", err)
	}
	return k
}

// The key is the SHA-256 of the length-prefixed fields; this recomputes it
// independently so a change to the recipe (which would invalidate every stored
// vector's reuse) cannot pass unnoticed.
func TestInputKeyRecipeIsStable(t *testing.T) {
	scope := storetest.Scope("app", "tenA", "ws1")
	h := sha256.New()
	for _, f := range []string{"alixai/vector/input-key/v1", "app", "tenA", "ws1", "granite-embedding-97m", "Guide > Refunds\nYes, within 30 days."} {
		h.Write(binary.AppendUvarint(nil, uint64(len(f))))
		h.Write([]byte(f))
	}
	var want InputKey
	copy(want[:], h.Sum(nil))
	if got := mustKey(t, scope, "granite-embedding-97m", "Guide > Refunds\nYes, within 30 days."); got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
	if len(want.String()) != 64 {
		t.Fatalf("hex form is %d characters", len(want.String()))
	}
}

// SEC-023: scope is part of the key, so the same content in another application,
// tenant or workspace has an unrelated key and cannot be found by it.
func TestSecInputKeyIncludesScopeModelAndInput(t *testing.T) {
	base := storetest.Scope("app", "tenA", "ws1")
	k := mustKey(t, base, "m", "text")
	differs := map[string]InputKey{
		"another application": mustKey(t, storetest.Scope("appX", "tenA", "ws1"), "m", "text"),
		"another tenant":      mustKey(t, storetest.Scope("app", "tenB", "ws1"), "m", "text"),
		"another workspace":   mustKey(t, storetest.Scope("app", "tenA", "ws2"), "m", "text"),
		"another model":       mustKey(t, base, "m2", "text"),
		"another input":       mustKey(t, base, "m", "text2"),
	}
	for name, other := range differs {
		if other == k {
			t.Errorf("%s: same key as the base scope: identical content would be reusable across that boundary", name)
		}
	}
	if mustKey(t, base, "m", "text") != k {
		t.Error("the key is not deterministic")
	}
	// The principal and session are not part of the key: a different user of the
	// same workspace shares the workspace's embeddings.
	other := base
	other.PrincipalID, other.SessionID = "usr_other", "ses_other"
	if mustKey(t, other, "m", "text") != k {
		t.Error("the key depends on the principal or session; reuse inside a workspace would never hit")
	}
}

// Fields are length-prefixed, so moving a character across a boundary cannot
// produce the same key.
func TestInputKeyFieldsCannotCollide(t *testing.T) {
	type tc struct{ app, ten, ws, model, input string }
	cases := []tc{
		{"a|b", "c", "d", "m", "x"}, {"a", "b|c", "d", "m", "x"}, {"a", "b", "c|d", "m", "x"},
		{"a:b", "c", "d", "m", "x"}, {"a", "b:c", "d", "m", "x"},
		{"ab", "c", "d", "m", "x"}, {"a", "bc", "d", "m", "x"},
		{"a", "b", "c", "m1", "x"}, {"a", "b", "c", "m", "1x"},
		{"a", "b", "c", "m\x01", "x"}, {"a", "b", "c", "m", "\x01x"},
	}
	seen := map[InputKey]tc{}
	for _, c := range cases {
		scope := storetest.Scope(c.app, c.ten, c.ws)
		k, err := NewInputKey(scope, c.model, c.input)
		if err != nil {
			// A control character in the model id is refused outright, which
			// is also a way of not colliding.
			continue
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("%+v and %+v produce the same key", prev, c)
		}
		seen[k] = c
	}
	if len(seen) < 9 {
		t.Fatalf("only %d of the cases produced keys", len(seen))
	}
}

func TestInputKeyRefusesBadArguments(t *testing.T) {
	if _, err := NewInputKey(contracts.Scope{}, "m", "x"); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("no scope: %v", err)
	}
	if _, err := NewInputKey(storetest.Scope("a", "b", "c"), "", "x"); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("no model: %v", err)
	}
	if !(InputKey{}).IsZero() {
		t.Fatal("the zero key is not zero")
	}
}
