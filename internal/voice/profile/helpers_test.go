package profile_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// bound is how long a test waits for something that must happen; quiet is how
// long it watches for something that must not. A flake in a quiet wait is a
// false pass, never a false failure.
const (
	bound = 5 * time.Second
	quiet = 60 * time.Millisecond
)

var (
	packEN = profile.Pack{ID: "en", Language: "en", Version: "1", Roles: []providers.Role{providers.RoleSTT, providers.RoleTTS, providers.RoleVAD}}
	packIT = profile.Pack{ID: "it", Language: "it-IT", Version: "1", Roles: []providers.Role{providers.RoleSTT, providers.RoleTTS}}
	packDE = profile.Pack{ID: "de", Language: "de", Version: "1", Roles: []providers.Role{providers.RoleSTT}}
)

func scope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_1",
		ApplicationID:    "app_clinic",
		TenantID:         "ten_1",
		WorkspaceID:      "ws_main",
		PrincipalID:      "usr_1",
		Roles:            []string{"receptionist"},
		CapabilityScopes: []string{"booking.create"},
		SessionID:        "ses_1",
	}
}

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s, want %s (%v)", got, code, err)
	}
}

// recLoader is a PackLoader that records what is loaded, how much at once and
// how many Activate/Deactivate calls overlap, and can fail or block on demand.
type recLoader struct {
	loads, unloads         atomic.Int32
	loaded, maxLoaded      atomic.Int32
	inflight, maxInflight  atomic.Int32
	mu                     sync.Mutex
	failLoad, failUnload   map[string]error
	block                  chan struct{} // while non-nil, Load waits on it
	entered                chan string   // receives the pack id when Load starts
	resident               map[string]bool
	residentTwiceViolation atomic.Bool
}

func newRecLoader() *recLoader {
	return &recLoader{failLoad: map[string]error{}, failUnload: map[string]error{}, resident: map[string]bool{}}
}

func bump(cur, max *atomic.Int32) {
	n := cur.Add(1)
	for {
		m := max.Load()
		if n <= m || max.CompareAndSwap(m, n) {
			return
		}
	}
}

func (l *recLoader) Load(ctx context.Context, p profile.Pack) error {
	bump(&l.inflight, &l.maxInflight)
	defer l.inflight.Add(-1)
	l.mu.Lock()
	block, entered, fail := l.block, l.entered, l.failLoad[p.ID]
	l.mu.Unlock()
	if entered != nil {
		entered <- p.ID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail != nil {
		return fail
	}
	l.mu.Lock()
	if len(l.resident) > 0 {
		l.residentTwiceViolation.Store(true)
	}
	l.resident[p.ID] = true
	l.mu.Unlock()
	l.loads.Add(1)
	bump(&l.loaded, &l.maxLoaded)
	return nil
}

func (l *recLoader) Unload(_ context.Context, p profile.Pack) error {
	bump(&l.inflight, &l.maxInflight)
	defer l.inflight.Add(-1)
	l.mu.Lock()
	fail := l.failUnload[p.ID]
	l.mu.Unlock()
	if fail != nil {
		return fail
	}
	l.mu.Lock()
	delete(l.resident, p.ID)
	l.mu.Unlock()
	l.unloads.Add(1)
	l.loaded.Add(-1)
	return nil
}

func (l *recLoader) set(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f()
}

// rig is a complete voice deployment around the fake runtime.
type rig struct {
	catalog  *profile.MemoryCatalog
	loader   *recLoader
	rt       *vfake.Provider
	reg      *providers.Registry
	factory  atomic.Int32
	released atomic.Int32
}

func newRig(packs ...profile.Pack) *rig {
	return &rig{
		catalog: profile.NewMemoryCatalog(packs...),
		loader:  newRecLoader(),
		rt:      vfake.New(),
		reg:     providers.NewRegistry(nil),
	}
}

func (r *rig) connect(context.Context) (profile.Providers, error) {
	r.factory.Add(1)
	return profile.Providers{
		STT: r.rt, TTS: r.rt, VAD: r.rt,
		Close: func() error { r.released.Add(1); return nil },
	}, nil
}

func (r *rig) deps(cfg profile.Config) profile.Deps {
	return profile.Deps{Config: cfg, Catalog: r.catalog, Loader: r.loader, Factory: r.connect, Registry: r.reg}
}

func (r *rig) registered() []providers.Role {
	var out []providers.Role
	for _, role := range voice.Roles() {
		if r.reg.Has(role) {
			out = append(out, role)
		}
	}
	return out
}

var errBusy = errors.New("model is busy")
