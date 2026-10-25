package identity_test

import (
	"context"
	"os"
	"testing"

	"github.com/buxee/alix-ai/internal/identity/identitytest"
)

// BenchmarkVerifySignatureAndClaims measures the cryptographic and parsing cost
// of one verification with the replay cache stubbed out (ADR 0013, owed
// measurement 3).
func BenchmarkVerifySignatureAndClaims(b *testing.B) {
	v, iss, _ := fuzzRig(b)
	token := iss.Token()
	b.SetBytes(int64(len(token)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := v.Verify(context.Background(), token); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerifyWithSQLiteReplay is the full path: signature, claims and the
// single-use nonce written to SQLite. Each iteration verifies a distinct token.
func BenchmarkVerifyWithSQLiteReplay(b *testing.B) {
	env := identitytest.NewEnv(b)
	tokens := make([]string, b.N)
	for i := range tokens {
		tokens[i] = env.Issuer.Token()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for _, tok := range tokens {
		if _, err := env.Verifier.Verify(context.Background(), tok); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(len(tokens[0])), "token_bytes")
	if st, err := os.Stat(env.DB.Path()); err == nil && b.N > 0 {
		// The main file alone; the WAL holds recent pages until checkpoint.
		b.ReportMetric(float64(st.Size())/float64(b.N), "db_bytes/row")
	}
}

// BenchmarkMintToken is the host-side cost of minting (sign and encode), for
// the SDK documentation.
func BenchmarkMintToken(b *testing.B) {
	env := identitytest.NewEnv(b)
	d := env.Issuer.Draft()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = d.Sign()
	}
}
