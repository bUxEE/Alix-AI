package cli

import (
	"context"
	"fmt"

	"github.com/buxee/alix-ai/internal/version"
)

func init() {
	Register(Command{
		Name:    "version",
		Summary: "print the build version",
		Run:     runVersion,
	})
}

func runVersion(_ context.Context, env *Env, args []string) error {
	fs := env.FlagSet("version")
	if err := env.ParseFlags(fs, "alixai version", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	_, err := fmt.Fprintln(env.Stdout, version.String())
	return err
}
