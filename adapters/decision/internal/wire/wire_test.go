package wire

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

func TestKeyNeverPrints(t *testing.T) {
	k := NewKey("sk-very-secret")
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s %q", k, k, k, k, k), slog.AnyValue(k).String()} {
		if strings.Contains(s, "very-secret") {
			t.Fatalf("key printed: %s", s)
		}
	}
	if b, _ := k.MarshalJSON(); strings.Contains(string(b), "very-secret") {
		t.Fatal("key marshalled")
	}
}

func TestResolveKeyRef(t *testing.T) {
	t.Setenv("WIRE_TEST_KEY", "abc123")
	if k, err := ResolveKeyRef("env:WIRE_TEST_KEY"); err != nil || !k.IsSet() || k.reveal() != "abc123" {
		t.Fatalf("env: %v", err)
	}
	if k, err := ResolveKeyRef(""); err != nil || k.IsSet() {
		t.Fatalf("empty ref: %v", err)
	}
	for _, bad := range []string{"literal-key-value", "env:", "env:1BAD", "env:WIRE_TEST_UNSET", "file:"} {
		_, err := ResolveKeyRef(bad)
		if err == nil || (len(bad) > 8 && strings.Contains(err.Error(), "literal-key")) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	f := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(f, []byte("filekey\nsecond line"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := ResolveKeyRef("file:" + f); err != nil || k.reveal() != "filekey" {
		t.Fatalf("file: %v", err)
	}
	if err := keyFileModeOK(0o644, "linux"); err == nil {
		t.Error("a group-readable key file was accepted")
	}
}

func TestReasonOfMirrorsTheEngine(t *testing.T) {
	cases := []struct {
		err  error
		want gen.FallbackReason
	}{
		{contracts.New(contracts.ErrTimeout, "x"), gen.FallbackReasonTimeout},
		{contracts.New(contracts.ErrSchemaInvalid, "x"), gen.FallbackReasonMalformedOutput},
		{contracts.New(contracts.ErrResourceExhausted, "x"), gen.FallbackReasonResourceGate},
		{contracts.New(contracts.ErrScopeViolation, "x"), gen.FallbackReasonTenantMismatch},
		{contracts.Wrap(contracts.ErrUnavailable, ErrUnsupportedRuntime, "x"), gen.FallbackReasonUnsupportedRuntime},
		{contracts.New(contracts.ErrInternal, "x"), gen.FallbackReasonUnavailable},
		{fmt.Errorf("anything else"), gen.FallbackReasonUnavailable},
	}
	for _, c := range cases {
		if got := ReasonOf(gen.DecisionScoreResult{}, c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}
