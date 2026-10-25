// Package config loads AlixAI's configuration.
//
// There is no central config struct. Every module owns its section and
// registers it, usually from init():
//
//	func init() { config.Register("rag", &Config{ChunkSize: 800}) }
//
// The registered value carries the section's defaults. Load then reads the
// YAML file (top-level keys are section names), applies ALIXAI_<SECTION>_<FIELD>
// environment overrides and finally calls Validate() error on every section
// that has one. Anything unknown, malformed or invalid is an error: the
// loader fails closed and never starts a process on a half-understood file.
//
// Load is atomic. It works on deep copies of the sections (maps and pointers
// included, so decoding cannot write through to the registered values) and
// writes them back only when everything succeeded, so a failed Load leaves the
// registered sections exactly as they were (the defaults, or the previous
// successful Load). A successful Load replaces a section's fields wholesale,
// so owners read them through the registered pointer rather than caching a
// map or nested pointer taken earlier.
//
// The core sections server, data_dir and log are always registered.
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// EnvLookup reads one environment variable; os.LookupEnv has this shape.
type EnvLookup func(key string) (value string, ok bool)

var sectionNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type entry struct {
	name     string
	typ      reflect.Type  // the section's struct (or scalar) type
	live     reflect.Value // the registered pointer, written on successful Load
	defaults reflect.Value // pointer to a private copy of the defaults
}

// Registry holds registered sections. Most code uses the package-level
// functions, which operate on Default; tests create their own with NewRegistry.
// A Registry must not be copied.
type Registry struct {
	mu       sync.RWMutex
	entries  []*entry
	byName   map[string]*entry
	envOwner map[string]string // env var -> section that claims it
	core     CoreConfig
}

// Default is the process-wide registry used by Register and Load.
var Default = NewRegistry()

// NewRegistry returns a registry holding only the core sections.
func NewRegistry() *Registry {
	r := &Registry{
		byName:   map[string]*entry{},
		envOwner: map[string]string{},
		core:     coreDefaults(),
	}
	r.Register("data_dir", &r.core.DataDir)
	r.Register("server", &r.core.Server)
	r.Register("log", &r.core.Log)
	return r
}

// Register adds a module-owned section to the Default registry.
func Register(name string, section any) { Default.Register(name, section) }

// Load loads path into the Default registry using the process environment.
func Load(path string) error { return Default.Load(path) }

// Core returns the core sections of the Default registry as of its last
// successful Load.
func Core() CoreConfig { return Default.Core() }

// Register adds a section under a top-level name matching ^[a-z][a-z0-9_]*$.
// section must be a non-nil pointer to a struct or to a scalar (string, bool,
// integer or float) holding the section's defaults. Register panics on a
// programming error: a bad name or type, a duplicate name, or an environment
// variable name already claimed by another section.
func (r *Registry) Register(name string, section any) {
	if !sectionNameRE.MatchString(name) {
		panic(fmt.Sprintf("config: invalid section name %q (want %s)", name, sectionNameRE))
	}
	v := reflect.ValueOf(section)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		panic(fmt.Sprintf("config: section %q must be a non-nil pointer, got %T", name, section))
	}
	typ := v.Type().Elem()
	if !supportedSectionType(typ) {
		panic(fmt.Sprintf("config: section %q has unsupported type %s", name, typ))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		panic(fmt.Sprintf("config: section %q registered twice", name))
	}
	var names []string
	walkEnv(envPrefix+strings.ToUpper(name), reflect.New(typ).Elem(), func(env string, _ reflect.Value) {
		names = append(names, env)
	})
	for _, env := range names {
		if owner, taken := r.envOwner[env]; taken {
			panic(fmt.Sprintf("config: environment variable %s is claimed by both section %q and %q", env, owner, name))
		}
	}

	defaults := cloneOf(v)
	e := &entry{name: name, typ: typ, live: v, defaults: defaults}
	r.entries = append(r.entries, e)
	r.byName[name] = e
	for _, env := range names {
		r.envOwner[env] = name
	}
}

func supportedSectionType(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Struct,
		reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// Core returns a copy of the core sections as of the last successful Load.
func (r *Registry) Core() CoreConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.core
}

// Sections returns the registered section names in sorted order.
func (r *Registry) Sections() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sortedNames()
}

func (r *Registry) sortedNames() []string {
	names := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		names = append(names, e.name)
	}
	sort.Strings(names)
	return names
}

// Load loads path using the process environment. See LoadFrom.
func (r *Registry) Load(path string) error { return r.LoadFrom(path, os.LookupEnv) }

// LoadFrom resets every section to its defaults, decodes the YAML file at path
// into them (path == "" skips the file), applies environment overrides read
// through lookup, validates every section and, only if all of that succeeded,
// publishes the result into the registered pointers. An explicit path that
// cannot be read is an error.
func (r *Registry) LoadFrom(path string, lookup EnvLookup) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	work := make([]reflect.Value, len(r.entries))
	for i, e := range r.entries {
		work[i] = cloneOf(e.defaults)
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := r.decodeYAML(path, data, work); err != nil {
			return err
		}
	}

	if lookup != nil {
		if err := r.applyEnv(work, lookup); err != nil {
			return err
		}
	}

	var errs []error
	for i, e := range r.entries {
		if v, ok := work[i].Interface().(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("config: section %s: %w", e.name, err))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	for i, e := range r.entries {
		e.live.Elem().Set(work[i].Elem())
	}
	return nil
}
