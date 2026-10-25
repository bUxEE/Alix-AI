package upgrade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// L2: a release is applied only by its own binary. Binary 1.0.0 applying the
// signed manifest of 1.4.0 would record 1.4.0 while running its own
// migrations, and then refuse 1.2.0 as a downgrade.
func TestReleaseIsAppliedOnlyByItsBinary(t *testing.T) {
	t.Run("another version is refused before anything changes", func(t *testing.T) {
		f := newFixture(t)
		events, head := f.auditBefore(t)
		u := f.upgrader(regV2(t, ""), binary("1.0.0"))
		res, err := u.Apply(ctx, Request{Manifest: f.release("1.4.0", []string{"v3.6.0"}, "b2000"), Runtime: &newSet})
		if !errors.Is(err, ErrRefused) || res.Outcome != OutcomeRefused || contracts.CodeOf(err) != contracts.ErrConflict ||
			!strings.Contains(err.Error(), "not the release of this binary") {
			t.Fatalf("binary 1.0.0 applying release 1.4.0: %+v, %v", res, err)
		}
		if st, err := u.LoadState(); err != nil || st.ReleaseVersion != "" {
			t.Errorf("the refused release was recorded: %+v, %v", st, err)
		}
		f.assertPreUpgradeState(t, events, head)
	})

	t.Run("the same version, spelt with a v, applies", func(t *testing.T) {
		f := newFixture(t)
		u := f.upgrader(regV2(t, ""), binary("v2.0.0"))
		if res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet}); err != nil || res.Outcome != OutcomeApplied {
			t.Fatalf("binary v2.0.0 applying release 2.0.0: %+v, %v", res, err)
		}
	})

	t.Run("an unstamped binary listed among the release's binaries applies", func(t *testing.T) {
		f := newFixture(t)
		exe := filepath.Join(t.TempDir(), "alixai")
		// The fixture lists "binary <version>" as the release's binary.
		if err := os.WriteFile(exe, []byte("binary 2.0.0"), 0o600); err != nil {
			t.Fatal(err)
		}
		u := f.upgrader(regV2(t, ""), binary("dev"), func(c *Config) { c.Executable = exe })
		if res, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet}); err != nil || res.Outcome != OutcomeApplied {
			t.Fatalf("a dev build whose file the release lists: %+v, %v", res, err)
		}
	})

	t.Run("an unstamped binary the release does not list is refused", func(t *testing.T) {
		f := newFixture(t)
		exe := filepath.Join(t.TempDir(), "alixai")
		if err := os.WriteFile(exe, []byte("binary 9.9.9"), 0o600); err != nil {
			t.Fatal(err)
		}
		u := f.upgrader(regV2(t, ""), binary("dev"), func(c *Config) { c.Executable = exe })
		if _, err := u.Apply(ctx, Request{Manifest: f.good(), Runtime: &newSet}); !errors.Is(err, ErrRefused) {
			t.Fatalf("a dev build the release does not list: %v", err)
		}
	})
}
