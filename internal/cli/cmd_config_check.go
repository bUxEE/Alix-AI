package cli

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	Register(Command{
		Name:    "config check",
		Summary: "load and validate the configuration, then exit",
		Run:     runConfigCheck,
	})
}

// configCheckAfterLoad runs after a successful Load (sibling-section checks that
// Validate cannot see while Load still works on copies). Nil-safe default.
var configCheckAfterLoad = func() error { return nil }

// configCheckExtraLines, when set, appends operator lines after "config ok"
// (layer locality from the external build).
var configCheckExtraLines = func() []string { return nil }

// runConfigCheck performs exactly what startup does with the configuration:
// the YAML file (unknown keys rejected), the ALIXAI_* environment overrides
// and every section's Validate. It prints "config ok" or the first problem;
// it never prints values, because sections may hold secrets.
func runConfigCheck(_ context.Context, env *Env, args []string) error {
	fs := env.FlagSet("config check")
	path := env.ConfigFlag(fs)
	if err := env.ParseFlags(fs, "alixai config check [-config file.yaml]", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}

	if err := env.Config.LoadFrom(*path, env.LookupEnv); err != nil {
		return err
	}
	if err := configCheckAfterLoad(); err != nil {
		return err
	}
	source := "built-in defaults + environment"
	if *path != "" {
		source = *path + " + environment"
	}
	if _, err := fmt.Fprintf(env.Stdout, "config ok: %s (sections: %s)\n", source, strings.Join(env.Config.Sections(), ", ")); err != nil {
		return err
	}
	for _, line := range configCheckExtraLines() {
		if _, err := fmt.Fprintln(env.Stdout, line); err != nil {
			return err
		}
	}
	return nil
}
