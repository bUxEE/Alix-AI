package models

import (
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// Registry is a loaded, fully validated set of entries. It is immutable after
// loading: accessors return copies of the top-level values, and the slices
// inside an Entry must be treated as read-only.
type Registry struct {
	entries []Entry // sorted by id
	byID    map[string]int
}

// LoadOption tunes LoadFS and LoadDir.
type LoadOption func(*loadConfig)

type loadConfig struct {
	evidence fs.FS
}

// WithEvidenceFS makes the loader check that every evidence_path and
// results_path an entry names exists in fsys (rooted at the repository root).
// Without it only the shape of the path is checked, which is the right
// behaviour for a binary that carries the registry but not the docs tree.
func WithEvidenceFS(fsys fs.FS) LoadOption {
	return func(c *loadConfig) { c.evidence = fsys }
}

// LoadDir loads every *.yaml file directly inside dir as one registry.
func LoadDir(dir string, opts ...LoadOption) (*Registry, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("models: registry directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("models: registry directory %q is not a directory", dir)
	}
	return LoadFS(os.DirFS(dir), opts...)
}

// LoadFS loads every *.yaml file at the root of fsys as one registry. It reads
// everything before judging anything and returns either a complete, valid
// Registry or a *LoadError listing every violation; there is no partial load.
//
// File rules: one entry per file; the file is named <id>.yaml; a .yml file or
// a subdirectory is refused rather than ignored (a typo must not silently drop
// a model from the registry); other files, such as a README, are ignored.
func LoadFS(fsys fs.FS, opts ...LoadOption) (*Registry, error) {
	var cfg loadConfig
	for _, o := range opts {
		o(&cfg)
	}

	des, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("models: read registry: %w", err)
	}
	sort.Slice(des, func(i, j int) bool { return des[i].Name() < des[j].Name() })

	var (
		vs    []Violation
		items []loaded
	)
	for _, de := range des {
		name := de.Name()
		switch {
		case de.IsDir():
			vs = append(vs, Violation{File: name, Rule: RuleFile, Detail: "subdirectories are not part of the registry"})
			continue
		case strings.HasSuffix(name, ".yml"):
			vs = append(vs, Violation{File: name, Rule: RuleFile, Detail: "use the .yaml extension"})
			continue
		case !strings.HasSuffix(name, ".yaml"):
			continue
		}
		if !de.Type().IsRegular() {
			vs = append(vs, Violation{File: name, Rule: RuleFile, Detail: "not a regular file"})
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			vs = append(vs, Violation{File: name, Rule: RuleFile, Detail: err.Error()})
			continue
		}
		e, fileVs := ParseEntry(name, data)
		if len(fileVs) > 0 {
			vs = append(vs, fileVs...)
			continue
		}
		if want := strings.TrimSuffix(name, ".yaml"); e.ID != want {
			vs = append(vs, Violation{File: name, Entry: e.ID, Rule: RuleFile, Field: "id",
				Detail: fmt.Sprintf("id %q must match the file name %q", e.ID, want)})
			continue
		}
		if cfg.evidence != nil {
			vs = append(vs, checkEvidence(&e, name, cfg.evidence)...)
		}
		items = append(items, loaded{file: name, entry: e})
	}

	if len(items) == 0 && len(vs) == 0 {
		vs = append(vs, Violation{Rule: RuleEmpty, Detail: "the registry directory holds no .yaml entries"})
	}
	// Cross-entry rules run on everything that parsed, so a problem in one
	// file does not hide a conflict among the others.
	vs = append(vs, checkRegistry(items)...)
	if len(vs) > 0 {
		return nil, newLoadError(vs)
	}

	r := &Registry{entries: make([]Entry, len(items)), byID: make(map[string]int, len(items))}
	for i, it := range items {
		r.entries[i] = it.entry
	}
	sort.Slice(r.entries, func(i, j int) bool { return r.entries[i].ID < r.entries[j].ID })
	for i, e := range r.entries {
		r.byID[e.ID] = i
	}
	return r, nil
}

// Len is the number of entries.
func (r *Registry) Len() int { return len(r.entries) }

// Entries returns every entry, sorted by id.
func (r *Registry) Entries() []Entry {
	return append([]Entry(nil), r.entries...)
}

// Get returns the entry with the given id.
func (r *Registry) Get(id string) (Entry, bool) {
	i, ok := r.byID[id]
	if !ok {
		return Entry{}, false
	}
	return r.entries[i], true
}

// DefaultFor returns the entry marked as the default for (role, profile). The
// loader guarantees there is at most one. ok is false when none is declared:
// callers must treat that as "no model", never pick one themselves.
func (r *Registry) DefaultFor(role Role, profile Profile) (Entry, bool) {
	want := Default{Role: role, Profile: profile}
	for _, e := range r.entries {
		for _, d := range e.DefaultFor {
			if d == want {
				return e, true
			}
		}
	}
	return Entry{}, false
}

// FallbackChain returns the entries to try after id, nearest first: id's
// fallback, that entry's fallback, and so on. The loader has already refused
// unknown ids and cycles, so the chain always ends.
func (r *Registry) FallbackChain(id string) ([]Entry, error) {
	cur, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("models: unknown model %q", id)
	}
	var chain []Entry
	for cur.Fallback.ModelID != nil && len(chain) <= len(r.entries) {
		next, ok := r.Get(*cur.Fallback.ModelID)
		if !ok {
			return nil, fmt.Errorf("models: %q names unknown fallback %q", cur.ID, *cur.Fallback.ModelID)
		}
		chain = append(chain, next)
		cur = next
	}
	return chain, nil
}
