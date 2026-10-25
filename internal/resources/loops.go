package resources

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// LoopBudget bounds one agent or tool loop (master plan 17.2: "no unbounded
// agent loops"). Both limits are always set: Config.MaxLoopSteps iterations and
// Config.MaxLoopWall of elapsed time, whichever comes first. Use Run so the
// loop cannot be written without its bound, or call Step before each iteration
// of a loop of your own. A LoopBudget is for one loop and one goroutine.
type LoopBudget struct {
	kind     string
	max      int
	steps    int
	clk      clock.Clock
	deadline int64 // unix nanoseconds
}

// NewLoop starts a loop budget. kind names the loop in the error ("dialogue",
// "tool"), never user content.
func (g *Governor) NewLoop(kind string) *LoopBudget {
	return newLoopBudget(kind, g.cfg.MaxLoopSteps, g.cfg.MaxLoopWall, g.clk)
}

func newLoopBudget(kind string, steps int, wall time.Duration, clk clock.Clock) *LoopBudget {
	return &LoopBudget{kind: kind, max: steps, clk: clk, deadline: clk.Now().Add(wall).UnixNano()}
}

// Steps is how many iterations have been admitted.
func (l *LoopBudget) Steps() int { return l.steps }

// Step admits one more iteration, or returns resource_exhausted when the step
// limit is used up or the wall-clock limit has passed.
func (l *LoopBudget) Step() error {
	if l.steps >= l.max {
		return contracts.Newf(contracts.ErrResourceExhausted, "%s loop stopped after %d steps (limit %d)", l.kind, l.steps, l.max).
			WithDetails("reason", "loop_steps")
	}
	if l.clk.Now().UnixNano() >= l.deadline {
		return contracts.Newf(contracts.ErrResourceExhausted, "%s loop stopped after %d steps: wall-clock limit reached", l.kind, l.steps).
			WithDetails("reason", "loop_wall")
	}
	l.steps++
	return nil
}

// Run calls fn once per admitted step until fn reports done, returns an
// error, ctx ends, or the budget runs out (resource_exhausted). It makes an
// unbounded loop impossible to write: a model that keeps asking for another
// tool call stops at the limit.
func (l *LoopBudget) Run(ctx context.Context, fn func(step int) (done bool, err error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.Step(); err != nil {
			return err
		}
		done, err := fn(l.steps)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}
