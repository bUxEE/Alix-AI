package modes

import (
	"context"
	"sync"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Mode is the product mode of additive plan section 2.
type Mode = pv.DeploymentMode

// The three modes. Local is the default.
const (
	ModeLocal    = pv.DeploymentModeLocal
	ModeHybrid   = pv.DeploymentModeHybrid
	ModeExternal = pv.DeploymentModeExternal
)

// Level is where a mode is set.
type Level string

// The levels, widest first.
const (
	LevelDeployment  Level = "deployment"
	LevelApplication Level = "application"
	LevelTenant      Level = "tenant"
)

// Values of Resolved.DecidedBy besides a Level.
const (
	DecidedByDefault    = "default"
	DecidedByKillSwitch = "kill_switch"
)

// permissiveness orders the modes by how much may leave: local 0, hybrid 1, external 2. An unknown
// value is local: the most restrictive.
func permissiveness(m Mode) int {
	switch m {
	case ModeHybrid:
		return 1
	case ModeExternal:
		return 2
	}
	return 0
}

func normalize(m Mode) Mode {
	if m.Valid() {
		return m
	}
	return ModeLocal
}

// ModeSource holds the configured mode of each level. A level with no setting reports false.
type ModeSource interface {
	Mode(ctx context.Context, level Level, scope contracts.Scope) (Mode, bool, error)
}

// KillSwitch reads the persistent global external-disable switch. *dataegress.Service implements it.
type KillSwitch interface {
	KillSwitch(ctx context.Context, deploymentID string) (dataegress.KillSwitchState, error)
}

var _ KillSwitch = (*dataegress.Service)(nil)

// MemoryModes is an in-memory ModeSource, for tests and for the composition root until the mode
// settings are persisted. It is safe for concurrent use.
type MemoryModes struct {
	mu sync.RWMutex
	m  map[string]Mode
}

// NewMemoryModes returns an empty source: every level is unset, so every scope resolves to Local.
func NewMemoryModes() *MemoryModes { return &MemoryModes{m: map[string]Mode{}} }

func modeKey(level Level, s contracts.Scope) (string, bool) {
	switch level {
	case LevelDeployment:
		return "d/" + s.DeploymentID, true
	case LevelApplication:
		return "a/" + s.DeploymentID + "/" + s.ApplicationID, true
	case LevelTenant:
		return "t/" + s.DeploymentID + "/" + s.ApplicationID + "/" + s.TenantID, true
	}
	return "", false
}

// Set records the mode of the level the scope names. The caller (an operator path) has already been
// authorised; this type checks only that the level and mode are valid.
func (s *MemoryModes) Set(level Level, scope contracts.Scope, m Mode) error {
	k, ok := modeKey(level, scope)
	if !ok || !m.Valid() {
		return contracts.New(contracts.ErrValidationFailed, "modes: not a valid level or mode")
	}
	s.mu.Lock()
	s.m[k] = m
	s.mu.Unlock()
	return nil
}

// Mode implements ModeSource.
func (s *MemoryModes) Mode(_ context.Context, level Level, scope contracts.Scope) (Mode, bool, error) {
	k, ok := modeKey(level, scope)
	if !ok {
		return "", false, contracts.New(contracts.ErrValidationFailed, "modes: not a valid level")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, found := s.m[k]
	return m, found, nil
}

// Resolved is the effective mode of a scope and how it came about.
type Resolved struct {
	// Mode is the effective mode: the most restrictive level, or Local under the kill switch.
	Mode Mode
	// Configured is what the levels say, before the kill switch.
	Configured Mode
	// DecidedBy is the level that set the effective mode, DecidedByDefault or DecidedByKillSwitch.
	DecidedBy string
	// KillSwitch is true when the global disable is engaged or could not be read.
	KillSwitch bool
	// Levels holds the setting of each level that has one (an invalid value shows as local).
	Levels map[Level]Mode
	// Unreadable names a level whose setting could not be read (that level counted as local).
	Unreadable []Level
}

// Resolver resolves the effective mode of a scope.
type Resolver struct {
	src ModeSource
	ks  KillSwitch
}

// NewResolver returns a Resolver. Both parts are required: without a kill switch reader the
// resolver could not honour an emergency disable, so it refuses to exist.
func NewResolver(src ModeSource, ks KillSwitch) (*Resolver, error) {
	if src == nil || ks == nil {
		return nil, contracts.New(contracts.ErrInternal, "modes: a mode source and a kill switch reader are required")
	}
	return &Resolver{src: src, ks: ks}, nil
}

// killed reports whether the global disable is engaged. A failed read counts as engaged.
func (r *Resolver) killed(ctx context.Context, deploymentID string) bool {
	st, err := r.ks.KillSwitch(ctx, deploymentID)
	return err != nil || st.Engaged
}

// Resolve returns the effective mode. It fails only for an invalid scope; a setting that cannot be
// read counts as Local, so nothing here can open external processing by failing.
func (r *Resolver) Resolve(ctx context.Context, scope contracts.Scope) (Resolved, error) {
	if err := scope.Validate(); err != nil {
		return Resolved{}, err
	}
	res := Resolved{Levels: map[Level]Mode{}}
	eff, decided := ModeLocal, DecidedByDefault
	for i, lv := range []Level{LevelDeployment, LevelApplication, LevelTenant} {
		m, set, err := r.src.Mode(ctx, lv, scope)
		if err != nil {
			res.Unreadable = append(res.Unreadable, lv)
			m, set = ModeLocal, true
		}
		if set {
			m = normalize(m)
			res.Levels[lv] = m
		} else {
			m = ModeLocal // an unset level is Local, as an unset egress level is disabled
		}
		if i == 0 || permissiveness(m) < permissiveness(eff) {
			eff = m
			decided = DecidedByDefault
			if set {
				decided = string(lv)
			}
		}
	}
	res.Configured, res.Mode, res.DecidedBy = eff, eff, decided
	if r.killed(ctx, scope.DeploymentID) {
		res.KillSwitch, res.Mode, res.DecidedBy = true, ModeLocal, DecidedByKillSwitch
	}
	return res, nil
}
