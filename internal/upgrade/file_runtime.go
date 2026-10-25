package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// FileRuntime is a Runtime that keeps the runtime set in a file: the desired
// state that the deployment (compose files, Quadlet units, the supervisor of
// M5-07) renders the LocalAI image and backends from. Activate records the set;
// Observe returns what is recorded. It is DECLARED state, not a probe of a
// running process: a composition root that can ask LocalAI what it runs wraps
// or replaces it with an implementation that does.
//
// With no file, Observe returns the empty set, which the upgrade treats as "no
// runtime recorded".
type FileRuntime struct {
	// Path is the JSON file, mode 0600, written atomically.
	Path string
}

// Observe implements Runtime.
func (f FileRuntime) Observe(context.Context) (RuntimeSet, error) {
	raw, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return RuntimeSet{}, nil
	}
	if err != nil {
		return RuntimeSet{}, contracts.Wrap(contracts.ErrUnavailable, err, "the recorded runtime cannot be read")
	}
	var rs RuntimeSet
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rs); err != nil {
		return RuntimeSet{}, contracts.Wrap(contracts.ErrInternal, err, "the recorded runtime is malformed")
	}
	return rs, nil
}

// Activate implements Runtime.
func (f FileRuntime) Activate(_ context.Context, rs RuntimeSet) error {
	if rs.Runtime == "" { // "no runtime recorded"
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return contracts.Wrap(contracts.ErrUnavailable, err, "the runtime record cannot be removed")
		}
		return nil
	}
	raw, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "the runtime set cannot be encoded")
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "the runtime record cannot be written")
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "the runtime record cannot be written")
	}
	if err := os.Rename(tmp, f.Path); err != nil {
		_ = os.Remove(tmp)
		return contracts.Wrap(contracts.ErrUnavailable, err, "the runtime record cannot be written")
	}
	return nil
}

var _ Runtime = FileRuntime{}

// RuntimeFile is where the upgrade directory of dataDir keeps its FileRuntime.
func RuntimeFile(dataDir string) string { return filepath.Join(dataDir, dirName, "runtime.json") }
