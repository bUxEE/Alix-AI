package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/buxee/alix-ai/internal/readiness/scaffold"
	"github.com/buxee/alix-ai/internal/readiness/scan"
)

// `alixai readiness scan DIR --scaffold OUTDIR` (master plan 9.5, ADR 0037 item 10; milestone M8).
//
// The option writes an unsigned, quarantined DRAFT package skeleton from the verified findings of the scan that has just been made:
// see internal/readiness/scaffold for what goes in and what never does. It is advisory work for the application owner: the draft
// is refused by the package loader as unsigned, and it is never installed, signed or activated by this command.
//
// How it is wired. cmd_readiness.go registers "readiness scan" and is not edited by this task, so this file wraps the registered command
// from its own init (Go runs a package's init functions in file-name order, and "cmd_readiness.go" sorts before this file):
// without -scaffold the original command runs untouched; with it, this file runs the same steps through the same helpers and then
// writes the draft. The usage text of the original does not list -scaffold; the next change to cmd_readiness.go should fold this in.
//
// What it writes where. The scan still writes only to the data directory (and -out). The draft goes into OUTDIR only, which must not
// exist and must not be inside the scanned directory; both are checked before the scan is made.

const rdyScaffoldScanUsage = "alixai readiness scan DIR [-config FILE] [-app ID] [-lang en|it] [-format md|json|both] [-no-model] [-model ID] [-out FILE] [-scaffold OUTDIR]"

// rdyScaffoldValueFlags are the flags of "readiness scan" that take a value: the token after one is never a flag.
var rdyScaffoldValueFlags = map[string]bool{"config": true, "app": true, "lang": true, "format": true, "model": true, "out": true, "scaffold": true}

func init() {
	Default.mu.Lock()
	defer Default.mu.Unlock()
	orig, ok := Default.cmds["readiness scan"]
	if !ok {
		panic("cli: readiness scaffold: the readiness scan command is not registered yet (cmd_readiness.go must initialise first)")
	}
	orig.Summary += " (-scaffold OUTDIR also writes an unsigned draft package)"
	run := orig.Run
	orig.Run = func(ctx context.Context, env *Env, args []string) error {
		switch {
		case rdyScaffoldWanted(args):
			return rdyScaffoldRunScan(ctx, env, args)
		case rdyScaffoldHelp(args):
			err := run(ctx, env, args)
			fmt.Fprintln(env.Stdout, "  -scaffold string\n    \talso write an unsigned, quarantined DRAFT package skeleton from the verified findings into this directory, which must not exist")
			return err
		}
		return run(ctx, env, args)
	}
	Default.cmds["readiness scan"] = orig
}

func rdyScaffoldFlagName(tok string) (name string, hasValue bool, ok bool) {
	if !strings.HasPrefix(tok, "-") || tok == "-" || tok == "--" {
		return "", false, false
	}
	n := strings.TrimPrefix(strings.TrimPrefix(tok, "-"), "-")
	n, _, hasValue = strings.Cut(n, "=")
	return n, hasValue, true
}

// rdyScaffoldWanted reports whether -scaffold (or --scaffold) is among the flags, reading values the way the flag package does.
func rdyScaffoldWanted(args []string) bool {
	for i := 0; i < len(args); i++ {
		name, hasValue, ok := rdyScaffoldFlagName(args[i])
		if !ok {
			continue
		}
		if name == "scaffold" {
			return true
		}
		if rdyScaffoldValueFlags[name] && !hasValue {
			i++
		}
	}
	return false
}

func rdyScaffoldHelp(args []string) bool {
	for i := 0; i < len(args); i++ {
		name, hasValue, ok := rdyScaffoldFlagName(args[i])
		if !ok {
			continue
		}
		if name == "h" || name == "help" {
			return true
		}
		if rdyScaffoldValueFlags[name] && !hasValue {
			i++
		}
	}
	return false
}

func rdyScaffoldRunScan(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("readiness scan")
	var f rdyCLIFlags
	rdyCLICommon(env, fs, &f, "md, json or both")
	rdyCLIModelFlags(fs, &f)
	fs.StringVar(&f.out, "out", "", "also write the report to this one file (owner-only); with -format both it receives the JSON")
	outDir := fs.String("scaffold", "", "also write an unsigned, quarantined DRAFT package skeleton from the verified findings into this directory, which must not exist")
	pos, err := rdyCLIParse(env, fs, rdyScaffoldScanUsage, args, 1)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return Usagef("need exactly one directory (usage: %s)", rdyScaffoldScanUsage)
	}
	if strings.TrimSpace(*outDir) == "" {
		return Usagef("-scaffold needs the directory to write the draft to (usage: %s)", rdyScaffoldScanUsage)
	}
	lang, err := f.check("md", "json", "both")
	if err != nil {
		return err
	}
	if err := rdyCLICheckOut(f.out); err != nil {
		return err
	}
	dir, err := rdyCLIAbsDir(pos[0])
	if err != nil {
		return err
	}
	// Refuse before any work: an existing OUTDIR, a missing parent, or a place inside the scanned directory.
	if err := scaffold.ScaffoldCheck(*outDir, dir); err != nil {
		return Usagef("-scaffold: %v", err)
	}
	key, err := f.key(dir)
	if err != nil {
		return err
	}
	d, err := rdyCLIOpen(ctx, env, f.config, true)
	if err != nil {
		return err
	}
	defer d.close()
	setup, err := rdyCLIModelSetup(ctx, env, &f)
	if err != nil {
		return err
	}
	res, err := scan.ScanDir(dir, scan.ScanOptions{})
	if err != nil {
		return fmt.Errorf("the scan failed: %w", err)
	}
	run, err := rdyCLIScanStore(ctx, env, d, key, &f, dir, res, setup)
	if err != nil {
		return fmt.Errorf("the scan failed: %w", err)
	}
	rdyCLIModelNotes(env, run)
	fmt.Fprintf(env.Stderr, "alixai: note: stored report %s under %s\n", run.Doc.ReportID, key)

	// The draft is made from the stored report (post-verification) and the same scan's inventory; the scaffold re-reads and re-hashes
	// every cited file itself, through a handle confined to the scanned directory.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("the report is stored, but the draft was not written: the scanned directory cannot be opened: %w", err)
	}
	defer root.Close()
	made, err := scaffold.ScaffoldWrite(*outDir, scaffold.ScaffoldInput{
		Report: run.Doc, Inventory: res.Inventory, Source: root.FS(), ScannedRoot: dir, ApplicationID: f.app,
	})
	if err != nil {
		return fmt.Errorf("the report is stored, but the draft was not written: %w", err)
	}
	fmt.Fprintf(env.Stderr, "alixai: note: wrote an UNSIGNED DRAFT package to %s (%d files, %d capability stub(s), %d RAG source stub(s)); "+
		"it is refused by the package loader until the application owner completes and signs it: see DRAFT-TODO.md\n",
		made.OutDir, len(made.Files), len(made.Capabilities), len(made.RAGSources))
	if made.FindingsDropped > 0 {
		fmt.Fprintf(env.Stderr, "alixai: note: %d finding(s) were not used: only found or partial findings whose evidence still holds become part of a draft\n", made.FindingsDropped)
	}
	return rdyCLIEmitReport(env, run.Doc, run.Dropped, lang, f.format, f.out)
}
