// Command release is the AlixAI release tool (M5-03, master plan 16.3 and 23).
//
//	go run ./tools/release reproducible-check   build twice in clean directories and compare hashes
//	go run ./tools/release sbom binary|image    CycloneDX SBOM of a binary or of the service image
//	go run ./tools/release manifest             unsigned release manifest (artifacts, images, backends, models, licences)
//	go run ./tools/release sign                 add an Ed25519 signature (internal/trust) to a manifest
//	go run ./tools/release verify               check a manifest's signature and, optionally, the files it lists
//
// Everything works offline once the Go module cache is filled: builds run with GOPROXY=off, the SBOM schemas are
// embedded, and the model registry is the one embedded in the binary. The tool opens no network connection, sends
// no telemetry and runs no program other than the Go toolchain (`go env`, `go build`).
//
// The signing key is never produced or stored here. CI builds the unsigned manifest; the key holder signs it
// offline (docs/operations/release.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// usageError is a mistake in how the tool was called: exit status 2, not 1.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

var commands = []command{
	{"reproducible-check", "build a package twice in clean directories and compare the hashes", cmdReproducibleCheck},
	{"sbom", "write a CycloneDX SBOM: `sbom binary` or `sbom image`", cmdSBOM},
	{"manifest", "write the unsigned release manifest", cmdManifest},
	{"sign", "add a signature to a release manifest (key held offline)", cmdSign},
	{"verify", "verify a release manifest's signature and the files it lists", cmdVerify},
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: release <command> [flags]")
	fmt.Fprintln(w)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-20s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, "\nRun a command with -h for its flags. See docs/operations/release.md.")
}

// run executes one invocation and returns the process exit status: 0 success, 1 the check or the operation failed,
// 2 the tool was called wrongly.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return 0
	}
	for _, c := range commands {
		if c.name != args[0] {
			continue
		}
		err := c.run(ctx, args[1:], stdout, stderr)
		var ue usageError
		switch {
		case err == nil:
			return 0
		case errors.Is(err, flag.ErrHelp):
			return 0
		case errors.As(err, &ue):
			fmt.Fprintf(stderr, "release %s: %v\n", c.name, err)
			return 2
		default:
			fmt.Fprintf(stderr, "release %s: %v\n", c.name, err)
			return 1
		}
	}
	fmt.Fprintf(stderr, "release: unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

// newFlagSet returns a flag set that reports errors to stderr and does not exit.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("release "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses args and turns a flag error into a usage error.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usagef("%v", err)
	}
	return nil
}

// multiFlag is a flag that may be given more than once.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
