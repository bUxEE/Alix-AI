package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// packageOutput is what `alixai package validate` prints, as far as the run reads it.
type packageOutput struct {
	OK             bool                `json:"ok"`
	ApplicationID  string              `json:"application_id"`
	PackageVersion string              `json:"package_version"`
	ManifestHash   string              `json:"manifest_hash"`
	Authenticity   string              `json:"authenticity"`
	SignedBy       string              `json:"signed_by"`
	Critical       int                 `json:"critical"`
	Warnings       int                 `json:"warnings"`
	Findings       []contracts.Finding `json:"findings"`
}

// validatePackage is the first stage: the loader's schemas and cross-file checks, the
// package's signature and integrity digests. It is `alixai package validate`, run
// in-process, so the verdict is the one an operator installing the package would get.
func validatePackage(ctx context.Context, dir, coreVersion string, trustKeys []string, allowTestKeys bool) (Stage, packageOutput) {
	start := time.Now()
	st := Stage{Name: "package"}
	args := []string{"package", "validate"}
	if coreVersion != "" {
		args = append(args, "-core-version", coreVersion)
	}
	for _, k := range trustKeys {
		args = append(args, "-trust-key", k)
	}
	if allowTestKeys {
		args = append(args, "-allow-test-keys")
	}
	args = append(args, dir)
	// no configuration: validating reads a package and writes nothing
	res := runCLI(ctx, "", args...)
	var out packageOutput
	// the report is printed whatever the exit status; output that is not the report is an
	// error of this stage, never a pass
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		st.Status = StatusFail
		st.Message = "the package could not be validated: " + firstNonEmpty(res.Stderr, res.Stdout, err.Error())
		st.Items = []Item{{ID: "package.load", Name: "package loads and validates", Status: StatusFail, Message: st.Message}}
		st.DurationMS = time.Since(start).Milliseconds()
		return st, out
	}

	load := Item{ID: "package.load", Name: "package loads and validates", Status: StatusPass,
		Details: map[string]any{"critical": out.Critical, "warnings": out.Warnings}}
	if !out.OK || out.Critical > 0 {
		load.Status = StatusFail
		load.Message = fmt.Sprintf("the package has %d critical finding(s)", out.Critical)
	}
	st.Items = append(st.Items, load)

	auth := Item{ID: "package.authenticity", Name: "package signature", Details: map[string]any{"authenticity": out.Authenticity}}
	switch apppkg.Authenticity(out.Authenticity) {
	case apppkg.AuthVerified:
		auth.Status = StatusPass
		if out.SignedBy != "" {
			auth.Details["signed_by"] = out.SignedBy
		}
	case apppkg.AuthIntegrityOnly:
		auth.Status = StatusWarn
		auth.Message = "the package digests match its signature manifest, but nobody vouched for it: pass -trust-key KEYID=PUBLICKEY to establish who signed it"
	default:
		auth.Status = StatusFail
		auth.Message = "the package is unsigned, was changed after it was signed, or is not signed by an acceptable key"
	}
	st.Items = append(st.Items, auth)

	for _, f := range out.Findings {
		var status Status
		switch f.Severity {
		case contracts.SeverityCritical:
			status = StatusFail
		case contracts.SeverityWarning:
			status = StatusWarn
		default:
			continue // information is facts, not checks
		}
		st.Items = append(st.Items, findingItem("package", f, status))
	}
	st.DurationMS = time.Since(start).Milliseconds()
	return st, out
}

// findingItem turns a finding into an item of a stage.
func findingItem(stage string, f contracts.Finding, status Status) Item {
	id := stage + ".finding." + f.CheckID
	name := f.CheckID
	d := map[string]any{"severity": string(f.Severity)}
	if f.Capability != "" {
		id += "." + f.Capability
		name += " (" + f.Capability + ")"
		d["capability"] = f.Capability
	}
	if f.Path != "" {
		d["path"] = f.Path
	}
	if f.Remediation != "" {
		d["remediation"] = f.Remediation
	}
	return Item{ID: id, Name: name, Status: status, Message: strings.TrimSpace(f.Message), Details: d}
}
