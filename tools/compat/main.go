package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usageText = `usage: compat [check|freeze|compare] [flags]

  check    [-root DIR] [-version v1] [-json] [-quiet]
           compare the working tree with schemas/_frozen/<version>; exit 1 on a breaking change or a missing freeze
  freeze   [-root DIR] [-version v1]
           write the current contracts as the frozen set; refuses when the change from an existing frozen set is breaking
  compare  [-json] [-quiet] OLD_FROZEN_DIR NEW_FROZEN_DIR
           classify the change between two frozen trees (for CI: the base branch against the proposed one)

exit status: 0 no breaking change; 1 breaking change, missing or unreadable freeze; 2 usage error`

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	cmd := "check"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("compat "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	version := fs.String("version", "v1", "frozen contract version")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	quiet := fs.Bool("quiet", false, "print only breaking changes")
	fs.Usage = func() { fmt.Fprintln(stderr, usageText) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch cmd {
	case "check":
		repo, err := findRepo(*root)
		if err != nil {
			return fail(stderr, err)
		}
		frozen, err := LoadFrozen(filepath.Join(repo, filepath.FromSlash(frozenRoot), *version))
		if err != nil {
			return fail(stderr, err)
		}
		current, err := LoadCurrent(repo)
		if err != nil {
			return fail(stderr, err)
		}
		rep := Compare(frozen, current)
		return finish(stdout, rep, *asJSON, *quiet, *version, true)
	case "freeze":
		repo, err := findRepo(*root)
		if err != nil {
			return fail(stderr, err)
		}
		dir := filepath.Join(repo, filepath.FromSlash(frozenRoot), *version)
		current, err := LoadCurrent(repo)
		if err != nil {
			return fail(stderr, err)
		}
		if frozen, err := LoadFrozen(dir); err == nil {
			rep := Compare(frozen, current)
			if rep.Worst() == Breaking {
				for _, c := range rep.Changes {
					if c.Severity == Breaking {
						fmt.Fprintln(stderr, c)
					}
				}
				fmt.Fprintf(stderr, "compat: refusing to freeze, %d breaking change(s) against %s; v1 contracts never change incompatibly (cut v2 instead)\n", rep.Count(Breaking), *version)
				return 1
			}
		} else if _, statErr := os.Stat(dir); statErr == nil {
			return fail(stderr, fmt.Errorf("the existing frozen set is unreadable, not overwriting it: %w", err))
		}
		if err := WriteFrozen(dir, current); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "compat: froze %d schemas and the OpenAPI document into %s\n", len(current.Schemas), filepath.ToSlash(dir))
		return 0
	case "compare":
		if fs.NArg() != 2 {
			fmt.Fprintln(stderr, usageText)
			return 2
		}
		oldSet, err := LoadFrozen(fs.Arg(0))
		if err != nil {
			return fail(stderr, err)
		}
		newSet, err := LoadFrozen(fs.Arg(1))
		if err != nil {
			return fail(stderr, err)
		}
		return finish(stdout, Compare(oldSet, newSet), *asJSON, *quiet, "the old tree", false)
	default:
		fmt.Fprintln(stderr, usageText)
		return 2
	}
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "compat: %v\n", err)
	return 1
}

// findRepo walks up from dir to the directory holding go.mod and schemas/.
func findRepo(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for p := abs; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
			if st, err := os.Stat(filepath.Join(p, "schemas")); err == nil && st.IsDir() {
				return p, nil
			}
		}
		if filepath.Dir(p) == p {
			return "", fmt.Errorf("no repository root (go.mod and schemas/) at or above %s", abs)
		}
	}
}

func finish(stdout io.Writer, rep Report, asJSON, quiet bool, against string, hint bool) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if rep.Changes == nil {
			rep.Changes = []Change{}
		}
		_ = enc.Encode(rep)
	} else {
		for _, c := range rep.Changes {
			if quiet && c.Severity != Breaking {
				continue
			}
			fmt.Fprintln(stdout, c)
		}
		fmt.Fprintf(stdout, "compat: %d breaking, %d additive, %d docs-only against %s\n",
			rep.Count(Breaking), rep.Count(Additive), rep.Count(DocsOnly), against)
		if hint && rep.Count(Additive) > 0 {
			fmt.Fprintln(stdout, "compat: additive changes are allowed (minor release); `go run ./tools/compat freeze` records them in the frozen set")
		}
	}
	if rep.Worst() == Breaking {
		return 1
	}
	return 0
}
