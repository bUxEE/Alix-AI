package main

import (
	"context"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/apps/convstage"
	"github.com/buxee/alix-ai/internal/cli"
)

// The conversation stage replays each example of each user capability as a scripted turn, through
// the same components `alixai serve` composes, over the adapter of the application under test
// (internal/apps/convstage holds the stage and says what a replay is held to).
//
// This file is what the command contributes: the adapter. It is built by internal/cli's
// BuildPackageAdapter from the installed copy of the package, which is the code path serve and
// `alixai audit run` use, so an MCP, an OpenAPI and a native package are all driven, each through
// the transport its manifest declares; and the disposable tenants come from the provisioning bridge
// of that same transport (the one the integration audit's runtime probes use).
//
// A package this build has no transport for (its manifest declares no operation endpoint, or more
// than one kind) is not_run, never skipped quietly: a stage that did not run proves nothing, and
// not_run fails the run.

// replayExamples is the third stage. It returns the stage, how many examples the package
// declares and how many were replayed (they must be equal for the stage to pass).
func replayExamples(ctx context.Context, e *env) (Stage, int, int) {
	start := time.Now()
	st := Stage{Name: "conversation"}
	declared := 0
	finish := func(items ...Item) (Stage, int, int) {
		st.Items = append(st.Items, items...)
		st.DurationMS = time.Since(start).Milliseconds()
		return st, declared, 0
	}

	installed, hash, err := apppkgLoad(ctx, e.dir)
	if err != nil {
		return finish(Item{ID: "conversation.rig", Name: "conversation rig", Status: StatusFail, Message: err.Error()})
	}
	declared = convstage.Declared(installed)

	built, err := cli.BuildPackageAdapter(ctx, cli.BuildRequest{
		ApplicationID: e.applicationID, Package: installed, ManifestHash: hash, Dir: e.dir,
	})
	switch {
	case err != nil && cli.TransportNoAdapter(err):
		return finish(Item{ID: "conversation.transport", Name: "application transport", Status: StatusNotRun,
			Message: "this build has no adapter for the package's endpoints, so its examples were not replayed: " + err.Error()})
	case err != nil:
		return finish(Item{ID: "conversation.rig", Name: "conversation rig", Status: StatusFail,
			Message: "open the application's adapter: " + err.Error()})
	}
	defer built.Close()

	prov, err := built.Provisioner(ctx)
	if err != nil {
		return finish(Item{ID: "conversation.world", Name: "disposable tenants", Status: StatusFail,
			Message: "the provisioning bridge of the application's transport refused: " + err.Error()})
	}

	res := convstage.Replay(ctx, convstage.Subject{
		ApplicationID: e.applicationID, Package: installed, Adapter: built.Adapter(), Provisioner: prov, ScratchDir: e.dep.root,
	})
	for _, it := range res.Items {
		st.Items = append(st.Items, Item{ID: it.ID, Name: it.Name, Status: Status(it.Status), Message: it.Message, Details: it.Details})
	}
	st.DurationMS = time.Since(start).Milliseconds()
	return st, res.Declared, res.Replayed
}

// apppkgLoad loads the installed copy of the package, as the audit saw it: its endpoints are the
// ones under test. It returns the package and its manifest hash.
func apppkgLoad(ctx context.Context, dir string) (*apppkg.Package, string, error) {
	src, err := apppkg.FromPath(dir)
	if err != nil {
		return nil, "", err
	}
	res, err := apppkg.Load(ctx, src, apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		return nil, "", err
	}
	if res.Package == nil {
		return nil, "", errors.New("the installed copy of the package has critical findings")
	}
	hash := res.ManifestHash
	if hash == "" {
		hash = "installed-copy" // only a key for the provisioning bridge, which needs the same value twice
	}
	return res.Package, hash, nil
}
