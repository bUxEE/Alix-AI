package profile_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func newPacks(t *testing.T, r *rig) *profile.Packs {
	t.Helper()
	k, err := profile.NewPacks(r.catalog, r.loader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func activeID(k *profile.Packs) string {
	p, ok := k.Active()
	if !ok {
		return ""
	}
	return p.ID
}

// TestLanguagePackActivateOneAtATime checks the activation rule of master plan
// 13 and 17.2: packs are activated explicitly, one at a time, and a second pack
// is never loaded while another is.
func TestLanguagePackActivateOneAtATime(t *testing.T) {
	ctx := context.Background()

	t.Run("nothing is active until one is activated", func(t *testing.T) {
		r := newRig(packEN, packIT, packDE)
		k := newPacks(t, r)
		if id := activeID(k); id != "" {
			t.Fatalf("a pack %q is active before any Activate", id)
		}
		if r.loader.loads.Load() != 0 {
			t.Fatal("constructing the registry loaded a pack")
		}
		installed, err := k.Installed(ctx)
		if err != nil || len(installed) != 3 || installed[0].ID != "de" || installed[1].ID != "en" || installed[2].ID != "it" {
			t.Fatalf("Installed = %+v, %v (want de, en, it)", installed, err)
		}
		if r.loader.loads.Load() != 0 {
			t.Fatal("listing the installed packs loaded one")
		}
	})

	t.Run("a second pack is refused while one is active, in either order", func(t *testing.T) {
		r := newRig(packEN, packIT, packDE)
		k := newPacks(t, r)
		if _, err := k.Activate(ctx, "en"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"it", "de"} {
			_, err := k.Activate(ctx, id)
			wantCode(t, err, contracts.ErrConflict)
			var ce *contracts.Error
			if !errors.As(err, &ce) || ce.Details["active"] != "en" || ce.Details["requested"] != id {
				t.Fatalf("conflict details = %+v", ce)
			}
		}
		if activeID(k) != "en" || r.loader.loads.Load() != 1 {
			t.Fatalf("active %q, loads %d: the refused activations must change nothing", activeID(k), r.loader.loads.Load())
		}
		// Activating the active pack again is a no-op, not a second load.
		if p, err := k.Activate(ctx, "en"); err != nil || p.ID != "en" {
			t.Fatalf("re-activate = %+v, %v", p, err)
		}
		if r.loader.loads.Load() != 1 {
			t.Fatalf("re-activating loaded the pack again (%d loads)", r.loader.loads.Load())
		}
	})

	t.Run("switching packs is deactivate then activate and never holds two", func(t *testing.T) {
		r := newRig(packEN, packIT, packDE)
		k := newPacks(t, r)
		for _, id := range []string{"en", "it", "de", "en"} {
			if _, err := k.Activate(ctx, id); err != nil {
				t.Fatalf("Activate(%s): %v", id, err)
			}
			if got := activeID(k); got != id {
				t.Fatalf("active = %q, want %q", got, id)
			}
			if err := k.Deactivate(ctx); err != nil {
				t.Fatalf("Deactivate: %v", err)
			}
			if activeID(k) != "" || r.loader.loaded.Load() != 0 {
				t.Fatalf("after Deactivate: active %q, loaded %d", activeID(k), r.loader.loaded.Load())
			}
		}
		if r.loader.maxLoaded.Load() != 1 || r.loader.residentTwiceViolation.Load() {
			t.Fatalf("max loaded = %d: two packs were resident at once", r.loader.maxLoaded.Load())
		}
		if err := k.Deactivate(ctx); err != nil {
			t.Fatalf("Deactivate with nothing active = %v, want nil", err)
		}
	})

	t.Run("unknown, uninstalled and malformed ids never load", func(t *testing.T) {
		r := newRig(packEN)
		k := newPacks(t, r)
		for _, id := range []string{"fr", "", "EN", "en ", "../en", "en/it", "e", "a-", "-a"} {
			_, err := k.Activate(ctx, id)
			wantCode(t, err, contracts.ErrValidationFailed)
		}
		if r.loader.loads.Load() != 0 || activeID(k) != "" {
			t.Fatal("a bad id loaded or activated something")
		}
	})

	t.Run("a pack that fails to load leaves nothing active and keeps its code", func(t *testing.T) {
		r := newRig(packEN, packDE)
		r.loader.set(func() {
			r.loader.failLoad["de"] = contracts.New(contracts.ErrResourceExhausted, "model does not fit")
		})
		k := newPacks(t, r)
		_, err := k.Activate(ctx, "de")
		wantCode(t, err, contracts.ErrResourceExhausted)
		if activeID(k) != "" || r.loader.loaded.Load() != 0 {
			t.Fatalf("active %q, loaded %d after a failed load", activeID(k), r.loader.loaded.Load())
		}
		// An untyped loader failure is unavailable, never success-looking.
		r.loader.set(func() { r.loader.failLoad["de"] = errBusy })
		_, err = k.Activate(ctx, "de")
		wantCode(t, err, contracts.ErrUnavailable)
		if !errors.Is(err, errBusy) {
			t.Fatalf("the cause is lost: %v", err)
		}
		// And the registry still works afterwards.
		if _, err := k.Activate(ctx, "en"); err != nil {
			t.Fatalf("Activate(en) after a failure: %v", err)
		}
	})

	t.Run("a pack that fails to unload stays active so a second cannot load beside it", func(t *testing.T) {
		r := newRig(packEN, packIT)
		k := newPacks(t, r)
		if _, err := k.Activate(ctx, "en"); err != nil {
			t.Fatal(err)
		}
		r.loader.set(func() { r.loader.failUnload["en"] = errBusy })
		wantCode(t, k.Deactivate(ctx), contracts.ErrUnavailable)
		if activeID(k) != "en" {
			t.Fatalf("active = %q after a failed unload, want en", activeID(k))
		}
		_, err := k.Activate(ctx, "it")
		wantCode(t, err, contracts.ErrConflict)
		r.loader.set(func() { delete(r.loader.failUnload, "en") })
		if err := k.Deactivate(ctx); err != nil {
			t.Fatalf("retry Deactivate: %v", err)
		}
		if _, err := k.Activate(ctx, "it"); err != nil {
			t.Fatal(err)
		}
		if r.loader.maxLoaded.Load() != 1 || r.loader.residentTwiceViolation.Load() {
			t.Fatalf("max loaded = %d", r.loader.maxLoaded.Load())
		}
	})

	t.Run("concurrent activations are serialised: one load, the rest refused or no-ops", func(t *testing.T) {
		r := newRig(packEN, packIT, packDE)
		k := newPacks(t, r)
		release := make(chan struct{})
		entered := make(chan string, 16)
		r.loader.set(func() { r.loader.block, r.loader.entered = release, entered })

		const n = 12
		ids := []string{"en", "it", "de"}
		type result struct {
			id  string
			err error
		}
		results := make(chan result, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				_, err := k.Activate(ctx, id)
				results <- result{id, err}
			}(ids[i%len(ids)])
		}

		var winner string
		select { // exactly one Load is in flight
		case winner = <-entered:
		case <-time.After(bound):
			t.Fatal("no Activate reached the loader")
		}
		select {
		case id := <-entered:
			t.Fatalf("a second Load (%s) started while %s was still loading", id, winner)
		case r := <-results:
			t.Fatalf("Activate(%s) returned %v while the first load was still in flight", r.id, r.err)
		case <-time.After(quiet):
		}

		// A caller that gives up while waiting for its turn is released.
		cctx, cancel := context.WithCancel(ctx)
		waiting := make(chan error, 1)
		go func() { _, err := k.Activate(cctx, "en"); waiting <- err }()
		cancel()
		select {
		case err := <-waiting:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a cancelled waiter = %v, want context.Canceled", err)
			}
		case <-time.After(bound):
			t.Fatal("a cancelled Activate stayed blocked behind the running load")
		}

		close(release)
		wg.Wait()
		close(results)
		ok, refused := 0, 0
		for res := range results {
			switch {
			case res.err == nil:
				ok++
				if res.id != winner {
					t.Errorf("Activate(%s) succeeded but %s won", res.id, winner)
				}
			case contracts.CodeOf(res.err) == contracts.ErrConflict:
				refused++
				if res.id == winner {
					t.Errorf("Activate(%s) was refused although it is the active pack", res.id)
				}
			default:
				t.Errorf("Activate(%s) = %v", res.id, res.err)
			}
		}
		if ok+refused != n || ok != n/len(ids) {
			t.Fatalf("ok %d, refused %d, want %d no-op successes", ok, refused, n/len(ids))
		}
		if got := r.loader.loads.Load(); got != 1 {
			t.Fatalf("%d loads for %d concurrent activations", got, n)
		}
		if r.loader.maxInflight.Load() != 1 {
			t.Fatalf("%d loader calls overlapped", r.loader.maxInflight.Load())
		}
		if r.loader.maxLoaded.Load() != 1 || r.loader.residentTwiceViolation.Load() {
			t.Fatalf("max loaded = %d", r.loader.maxLoaded.Load())
		}
		if activeID(k) != winner {
			t.Fatalf("active = %q, want %q", activeID(k), winner)
		}
	})
}

func TestPackValidation(t *testing.T) {
	stt := []providers.Role{providers.RoleSTT}
	cases := []struct {
		name string
		p    profile.Pack
		ok   bool
	}{
		{"valid", profile.Pack{ID: "it-fast", Language: "it-IT", Roles: stt}, true},
		{"all roles", profile.Pack{ID: "en", Language: "en", Roles: []providers.Role{providers.RoleSTT, providers.RoleTTS, providers.RoleVAD}}, true},
		{"empty id", profile.Pack{Language: "en", Roles: stt}, false},
		{"upper-case id", profile.Pack{ID: "EN", Language: "en", Roles: stt}, false},
		{"path-like id", profile.Pack{ID: "../en", Language: "en", Roles: stt}, false},
		{"overlong id", profile.Pack{ID: strings.Repeat("a", 60), Language: "en", Roles: stt}, false},
		{"no language", profile.Pack{ID: "en", Roles: stt}, false},
		{"bad language", profile.Pack{ID: "en", Language: "english", Roles: stt}, false},
		{"no roles", profile.Pack{ID: "en", Language: "en"}, false},
		{"text role", profile.Pack{ID: "en", Language: "en", Roles: []providers.Role{providers.RoleController}}, false},
		{"unknown role", profile.Pack{ID: "en", Language: "en", Roles: []providers.Role{"mic"}}, false},
		{"duplicate role", profile.Pack{ID: "en", Language: "en", Roles: []providers.Role{providers.RoleSTT, providers.RoleSTT}}, false},
	}
	for _, tc := range cases {
		err := tc.p.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		} else if err != nil {
			wantCode(t, err, contracts.ErrSchemaInvalid)
		}
	}
	if !packEN.Provides(providers.RoleVAD) || packIT.Provides(providers.RoleVAD) {
		t.Fatal("Provides disagrees with the role list")
	}
}

func TestNewPacksRequiresBothDependencies(t *testing.T) {
	r := newRig()
	if _, err := profile.NewPacks(nil, r.loader); err == nil {
		t.Fatal("a nil catalog must be rejected")
	}
	if _, err := profile.NewPacks(r.catalog, nil); err == nil {
		t.Fatal("a nil loader must be rejected")
	}
	k, err := profile.NewPacks(r.catalog, profile.NopLoader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Activate(context.Background(), "en"); err == nil {
		t.Fatal("activating from an empty catalog must fail")
	}
}
