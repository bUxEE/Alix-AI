package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func init() {
	Register(Command{
		Name:    "package install",
		Summary: "install an application package (directory or .tar.gz) into quarantine; it serves nothing until an audit activates it",
		Run:     runPackageInstall,
	})
}

const installUsage = "alixai package install [-config FILE] [-core-version V] [-operator ID] PACKAGE"

type installOut struct {
	// Created is false when the very same content was already installed: the
	// stored record is shown as it is.
	Created        bool                `json:"created"`
	ApplicationID  string              `json:"application_id"`
	PackageVersion string              `json:"package_version"`
	ManifestHash   string              `json:"manifest_hash"`
	State          string              `json:"state"`
	SignedBy       string              `json:"signed_by"`
	Path           string              `json:"path"`
	Files          int                 `json:"files"`
	Bytes          int64               `json:"bytes"`
	Findings       []contracts.Finding `json:"findings"`
	// Next says what happens next: nothing is served until an audit activates
	// the package.
	Next string `json:"next"`
}

// runPackageInstall verifies a package against the deployment's trust store and
// installs it, quarantined, below <data_dir>/packages/<application>/<hash>. A
// package that is unsigned, tampered, signed by a key the trust store does not
// trust (register one with 'alixai trust add-key') or that has any critical
// finding is refused: nothing is written, the findings are printed and the exit
// status is 1. A package that is installed exposes nothing: only a clear audit
// (alixai audit run) activates it.
func runPackageInstall(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("package install")
	path := env.ConfigFlag(fs)
	core := fs.String("core-version", "", "AlixAI core version the package's compatibility range is checked against (default: this binary's version; a development build uses "+apppkg.DevCoreVersion+")")
	operator := fs.String("operator", "", "who installs the package, recorded in the audit log")
	if err := env.ParseFlags(fs, installUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one package directory or .tar.gz (usage: %s)", installUsage)
	}
	if *operator != "" && !operatorIDRE.MatchString(*operator) {
		return Usagef("-operator %q is not an operator id (1 to 128 letters, digits and ._@:-)", *operator)
	}
	src, err := apppkg.FromPath(fs.Arg(0))
	if err != nil {
		return err
	}
	s, err := openStack(ctx, env, *path, false)
	if err != nil {
		return err
	}
	defer s.close()
	inst, err := s.installer(*core)
	if err != nil {
		return err
	}

	res, err := inst.Install(ctx, src)
	var refused *apppkg.RefusedError
	if errors.As(err, &refused) {
		if jerr := emitJSON(env.Stdout, map[string]any{"installed": false, "reason": refused.Reason, "findings": refused.Findings}); jerr != nil {
			return jerr
		}
		return err
	}
	if err != nil {
		return err
	}
	rec := res.Record
	if res.Created {
		// The package is already installed and quarantined; if the record of it
		// cannot be written, say so rather than let an install go unrecorded.
		loc, lerr := audit.ForApplication(rec.ApplicationID, *operator)
		if lerr == nil {
			_, lerr = s.log.AppendAt(ctx, loc, audit.NewEvent{
				Type: audit.EventPackageInstalled,
				Data: map[string]any{
					"manifest_hash": rec.ManifestHash, "package_version": rec.PackageVersion, "signer_key_id": rec.SignerKeyID,
					"state": string(rec.State), "warnings": rec.Warnings,
				},
			})
		}
		if lerr != nil {
			return fmt.Errorf("the package is installed (quarantined) but the audit log could not record it: %w", lerr)
		}
	}
	if res.Findings == nil {
		res.Findings = []contracts.Finding{}
	}
	return emitJSON(env.Stdout, installOut{
		Created: res.Created, ApplicationID: rec.ApplicationID, PackageVersion: rec.PackageVersion, ManifestHash: rec.ManifestHash,
		State: string(rec.State), SignedBy: rec.SignerKeyID, Path: rec.Path, Files: rec.Files, Bytes: rec.Bytes,
		Findings: res.Findings,
		Next:     fmt.Sprintf("alixai audit run %s", rec.ApplicationID),
	})
}
