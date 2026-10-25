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
	source := "built-in defaults + environment"
	if *path != "" {
		source = *path + " + environment"
	}
	_, err := fmt.Fprintf(env.Stdout, "config ok: %s (sections: %s)\n", source, strings.Join(env.Config.Sections(), ", "))
	return err
}
