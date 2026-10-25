// Package cli is the command line of the single alixai binary and a composition
// root: it may wire any internal package together.
//
// The root knows no subcommand. Every subcommand lives in its own file
// cmd_<name>.go and registers itself from init():
//
//	func init() {
//		Register(Command{Name: "config check", Summary: "...", Run: runConfigCheck})
//	}
//
// so later tasks add files and never edit this one. A name is one or more
// lower-case words ("version", "config check"); the dispatcher runs the
// command with the longest name that prefixes the arguments. The standard
// library flag package parses each command's flags; there is no framework.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/buxee/alix-ai/internal/config"
)

// Process exit codes.
const (
	ExitOK    = 0
	ExitError = 1 // the command ran and failed
	ExitUsage = 2 // the command line was wrong
)

// ErrUsage marks an error as a mistake in the command line (exit code 2).
var ErrUsage = errors.New("usage")

// Usagef returns an error that makes the command exit with ExitUsage.
func Usagef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, a...))
}

// Env is everything a command may touch outside its arguments, so tests can
// substitute all of it. Nil fields take the process defaults.
type Env struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv config.EnvLookup
	Config    *config.Registry
}

func (e *Env) withDefaults() *Env {
	c := Env{}
	if e != nil {
		c = *e
	}
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
	if c.LookupEnv == nil {
		c.LookupEnv = os.LookupEnv
	}
	if c.Config == nil {
		c.Config = config.Default
	}
	return &c
}

// Command is one subcommand.
type Command struct {
	// Name is one or more lower-case words, e.g. "version" or "config check".
	Name string
	// Summary is the one-line description shown by help.
	Summary string
	// Run executes the command with the arguments after its name. A returned
	// error makes the exit code ExitError, or ExitUsage if it wraps ErrUsage;
	// flag.ErrHelp means help was printed and is a success.
	Run func(ctx context.Context, env *Env, args []string) error
}

// FlagSet returns a flag set for a command: errors are returned, never
// printed or fatal. Parse it with Env.ParseFlags.
func (e *Env) FlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// ParseFlags parses args. On -h it prints usage plus the flag defaults to
// stdout and returns flag.ErrHelp; any other problem is a usage error.
func (e *Env) ParseFlags(fs *flag.FlagSet, usage string, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(e.Stdout, "Usage: %s\n", usage)
			fs.SetOutput(e.Stdout)
			fs.PrintDefaults()
			return flag.ErrHelp
		}
		return Usagef("%v (try -h)", err)
	}
	return nil
}

// ConfigFlag registers the standard -config flag. Its default is
// $ALIXAI_CONFIG; empty means "no file, built-in defaults plus environment".
func (e *Env) ConfigFlag(fs *flag.FlagSet) *string {
	def, _ := e.LookupEnv("ALIXAI_CONFIG")
	return fs.String("config", def, "path to the YAML config file (default $ALIXAI_CONFIG; empty: built-in defaults)")
}

var commandNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*(?: [a-z][a-z0-9-]*)*$`)

// Root dispatches argument lists to registered commands.
type Root struct {
	program string

	mu   sync.RWMutex
	cmds map[string]Command
}

// NewRoot returns an empty dispatcher. program is the name shown in help.
func NewRoot(program string) *Root {
	return &Root{program: program, cmds: map[string]Command{}}
}

// Default is the dispatcher the cmd_*.go files register on.
var Default = NewRoot("alixai")

// Register adds c to the Default dispatcher.
func Register(c Command) { Default.Register(c) }

// Run executes args (without the program name) on the Default dispatcher and
// returns the process exit code.
func Run(ctx context.Context, args []string, env *Env) int {
	return Default.Run(ctx, args, env)
}

// Main runs the process: real arguments, real streams, cancelled on interrupt
// or termination. It returns the exit code.
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, os.Args[1:], nil)
}

// Register adds a command. It panics on a malformed or duplicate name or a nil
// Run, which are programming errors caught the first time the binary starts.
func (r *Root) Register(c Command) {
	if !commandNameRE.MatchString(c.Name) {
		panic(fmt.Sprintf("cli: invalid command name %q", c.Name))
	}
	if c.Run == nil {
		panic(fmt.Sprintf("cli: command %q has no Run", c.Name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.cmds[c.Name]; dup {
		panic(fmt.Sprintf("cli: command %q registered twice", c.Name))
	}
	r.cmds[c.Name] = c
}

// Names returns the registered command names, sorted.
func (r *Root) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.cmds))
	for n := range r.cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Run executes args and returns the exit code: ExitOK, ExitError or ExitUsage.
func (r *Root) Run(ctx context.Context, args []string, env *Env) int {
	env = env.withDefaults()

	if len(args) == 0 {
		r.usage(env.Stderr)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		r.usage(env.Stdout)
		return ExitOK
	}

	cmd, rest, ok := r.match(args)
	if !ok {
		if group := r.group(args[0]); len(group) > 0 {
			fmt.Fprintf(env.Stderr, "%s: %q needs a subcommand:\n", r.program, args[0])
			r.listCommands(env.Stderr, group)
			return ExitUsage
		}
		fmt.Fprintf(env.Stderr, "%s: unknown command %q\n\n", r.program, args[0])
		r.usage(env.Stderr)
		return ExitUsage
	}

	err := cmd.Run(ctx, env, rest)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return ExitOK
	case errors.Is(err, ErrUsage):
		fmt.Fprintf(env.Stderr, "%s: %s: %s\n", r.program, cmd.Name, strings.TrimPrefix(err.Error(), ErrUsage.Error()+": "))
		return ExitUsage
	default:
		fmt.Fprintf(env.Stderr, "%s: %v\n", r.program, err)
		return ExitError
	}
}

// match finds the command with the longest name that prefixes args.
func (r *Root) match(args []string) (cmd Command, rest []string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for n := len(args); n >= 1; n-- {
		if c, found := r.cmds[strings.Join(args[:n], " ")]; found {
			return c, args[n:], true
		}
	}
	return Command{}, nil, false
}

// group returns the commands that live under the first word word.
func (r *Root) group(word string) []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Command
	for name, c := range r.cmds {
		if strings.HasPrefix(name, word+" ") {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Root) usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s <command> [flags]\n\nCommands:\n", r.program)
	r.mu.RLock()
	cmds := make([]Command, 0, len(r.cmds))
	for _, c := range r.cmds {
		cmds = append(cmds, c)
	}
	r.mu.RUnlock()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	r.listCommands(w, cmds)
	fmt.Fprintf(w, "\nRun '%s <command> -h' for a command's flags.\n", r.program)
}

func (r *Root) listCommands(w io.Writer, cmds []Command) {
	width := 0
	for _, c := range cmds {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.Name, c.Summary)
	}
}
