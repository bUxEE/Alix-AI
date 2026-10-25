//go:build !windows && !linux && !darwin

package confine

import (
	"errors"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// Apply fails closed: on a platform with no supported confinement the worker
// does not parse. AlixAI's supported platforms are Linux, Windows and macOS.
func Apply(o Options) (wire.Sandbox, error) {
	return wire.Sandbox{}, errors.New("no sandbox is implemented for this operating system")
}
