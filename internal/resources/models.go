package resources

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/providers"
)

// modelState is what the governor knows about one loaded model.
type modelState struct {
	loadedAt time.Time // the latest load the governor knows of
	lastUse  time.Time
	inUse    int
	bytes    uint64
	role     providers.Role
	reported bool // present in the latest runtime report
}

// observeModelsLocked merges a runtime report into the model table: new
// models appear (a model seen again after this governor unloaded it counts a
// reload), models missing from the report disappear. Caller holds g.mu.
func (g *Governor) observeModelsLocked(report []LoadedModel, now time.Time) {
	for _, ms := range g.models {
		ms.reported = false
	}
	for _, lm := range report {
		ms, ok := g.models[lm.ID]
		if !ok {
			ms = &modelState{}
			g.models[lm.ID] = ms
		}
		if ms.loadedAt.IsZero() {
			ms.loadedAt = now
			if !lm.LoadedAt.IsZero() {
				ms.loadedAt = lm.LoadedAt
			}
		} else if lm.LoadedAt.After(ms.loadedAt) {
			ms.loadedAt = lm.LoadedAt
		}
		if lm.LastUsed.After(ms.lastUse) {
			ms.lastUse = lm.LastUsed
		}
		if ms.lastUse.IsZero() {
			ms.lastUse = ms.loadedAt
		}
		ms.bytes, ms.role, ms.reported = lm.Bytes, lm.Role, true
		if _, was := g.unloaded[lm.ID]; was {
			delete(g.unloaded, lm.ID)
			g.stats.Reloads++
		}
	}
	for id, ms := range g.models {
		if !ms.reported && ms.inUse == 0 {
			delete(g.models, id)
		}
	}
}

// Touch records that a model was just used, which restarts its idle clock.
// Call it when a request to the model starts or ends (UseModel does both).
func (g *Governor) Touch(modelID string) {
	now := g.clk.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.touchLocked(modelID, now)
}

func (g *Governor) touchLocked(modelID string, now time.Time) *modelState {
	ms, ok := g.models[modelID]
	if !ok {
		// First sight is a use: the model is, or is about to be, loaded now.
		ms = &modelState{loadedAt: now}
		g.models[modelID] = ms
	}
	ms.lastUse = now
	return ms
}

// UseModel marks a model in use for the duration of a request and returns the
// release. A model in use is never unloaded. The release restarts the idle
// clock.
func (g *Governor) UseModel(modelID string) (release func()) {
	now := g.clk.Now()
	g.mu.Lock()
	g.touchLocked(modelID, now).inUse++
	g.mu.Unlock()
	var done bool
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if done {
			return
		}
		done = true
		end := g.clk.Now()
		if ms, ok := g.models[modelID]; ok {
			if ms.inUse > 0 {
				ms.inUse--
			}
			ms.lastUse = end
		}
	}
}

// idleCandidate is a model the policy would unload.
type idleCandidate struct {
	id      string
	lastUse time.Time
}

// Sweep unloads, through the provider's Unloader, every model that is idle for
// at least Config.IdleUnloadAfter AND has been loaded for at least
// Config.MinWarm AND is neither pinned nor in use. It works from the latest
// Refresh (a runtime report older than Config.StaleAfter, or none, unloads
// nothing: the governor does not guess what is loaded), unloads the longest
// idle first and at most Config.MaxUnloadsPerSweep per call. It does I/O and
// must not run inside a queue transaction. A model that a request starts
// using at the very moment it is unloaded simply loads again (counted in
// Stats.Reloads). The errors of failed unloads are joined; the sweep goes on
// past them.
func (g *Governor) Sweep(ctx context.Context) ([]string, error) {
	if g.cfg.IdleUnloadDisabled || g.unloader == nil || g.reporter == nil {
		return nil, nil
	}
	now := g.clk.Now()
	g.mu.Lock()
	if !g.fresh(g.runtime, now) {
		g.mu.Unlock()
		return nil, nil
	}
	var cands []idleCandidate
	for id, ms := range g.models {
		switch {
		case !ms.reported, ms.inUse > 0, g.pinned[id]:
		case now.Sub(ms.lastUse) < g.cfg.IdleUnloadAfter:
		case now.Sub(ms.loadedAt) < g.cfg.MinWarm:
		default:
			cands = append(cands, idleCandidate{id: id, lastUse: ms.lastUse})
		}
	}
	g.mu.Unlock()

	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].lastUse.Equal(cands[j].lastUse) {
			return cands[i].lastUse.Before(cands[j].lastUse)
		}
		return cands[i].id < cands[j].id
	})
	if len(cands) > g.cfg.MaxUnloadsPerSweep {
		cands = cands[:g.cfg.MaxUnloadsPerSweep]
	}

	var done []string
	var errs []error
	for _, c := range cands {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		err := g.unloader.Unload(ctx, c.id)
		g.mu.Lock()
		if err != nil {
			g.stats.UnloadErrors++
		} else {
			g.stats.Unloads++
			g.unloaded[c.id] = g.clk.Now()
			delete(g.models, c.id)
		}
		g.mu.Unlock()
		if err != nil {
			g.log.Warn("resource governor: model unload failed", "model", c.id, "err", err.Error())
			errs = append(errs, err)
			continue
		}
		g.log.Info("resource governor: idle model unloaded", "model", c.id, "idle_ms", now.Sub(c.lastUse).Milliseconds())
		done = append(done, c.id)
	}
	return done, errors.Join(errs...)
}

// ModelLoad is a request to load a model into the runtime.
type ModelLoad struct {
	ID   string
	Role providers.Role
	// Bytes is the memory the model will hold once loaded.
	Bytes uint64
}

// AdmitModelLoad is the resource gate for loading a model. The conversational
// controller may use the whole budget; every other role (router, reranker,
// embedder, a scorer) is held to the background share, so an optional worker
// can never take the reserve the controller needs. The gate rejects with a
// typed error when the model does not fit and never unloads anything to make
// room: evicting the controller or another resident model is the operator's
// decision, not an admission side effect. A load is judged as live work (it
// is rejected, never queued, and does not wait for live turns: the turn may
// need that very model). Release the Ticket once the runtime reports the model
// loaded (its memory then shows in the runtime reading).
func (g *Governor) AdmitModelLoad(_ context.Context, ml ModelLoad) (*Ticket, error) {
	g.ensureFresh()
	limit := g.backgroundLimit()
	if ml.Role == providers.RoleController {
		limit = g.cfg.MemoryBudgetBytes
	}
	return g.admit(Request{Class: jobs.ClassTurn, Bytes: ml.Bytes, Label: "model_load"}, limit)
}
