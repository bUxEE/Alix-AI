package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/buxee/alix-ai/internal/release"
)

var (
	sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestRE    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// findModuleRoot walks up from start to the directory that holds go.mod.
func findModuleRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && st.Mode().IsRegular() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod in %s or any parent directory; run from the repository or pass -C", start)
		}
		dir = parent
	}
}

// moduleRootFlag resolves the -C flag: the given directory, or the module root above the working directory.
func moduleRootFlag(c string) (string, error) {
	if c != "" {
		return findModuleRoot(c)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return findModuleRoot(wd)
}

// writeFileAtomic writes data to path through a temporary file in the same directory, so a reader never sees a
// half-written file. Release output is public content, so the mode is 0644.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".release-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// writeOutput writes data to path, or to stdout when path is empty.
func writeOutput(path string, data []byte, stdout io.Writer) error {
	if path == "" {
		_, err := stdout.Write(data)
		return err
	}
	return writeFileAtomic(path, data)
}

// requireFlag returns a usage error when a required string flag is empty.
func requireFlag(name, value string) error {
	if value == "" {
		return usagef("-%s is required", name)
	}
	return nil
}

var errNoGo = errors.New("the go command was not found on PATH or in GOROOT")

// marshalIndent is the file form of a JSON document: indented, no HTML escaping, one final newline.
func marshalIndent(v any) ([]byte, error) { return release.MarshalIndent(v) }
