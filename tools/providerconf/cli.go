package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

// liveEnv must be "1" for live mode, on top of the --live flag.
const liveEnv = "ALIXAI_PROVIDERCONF_LIVE"

var (
	errNoLiveBuild = errors.New("live mode needs a binary built with -tags live; this one was not")
	errNoLiveEnv   = errors.New("live mode needs " + liveEnv + "=1 in the environment as well as --live")
)

// resolveMode decides fixtures or live. Live needs the flag, the build tag and
// the environment variable; the environment variable alone changes nothing.
// Nothing is constructed here: a refusal happens before any client exists.
func resolveMode(live bool, env func(string) string) (string, error) {
	if !live {
		return suite.ModeFixtures, nil
	}
	if !liveBuild {
		return "", errNoLiveBuild
	}
	if env(liveEnv) != "1" {
		return "", errNoLiveEnv
	}
	return suite.ModeLive, nil
}

// subjectFns build the subject of one check; tests replace them.
var (
	fixtureSubjectFn = func(provider string, profile pv.ProviderProfile) (suite.Subject, func(), error) {
		switch provider {
		case providerOpenAI:
			return openaiFixtureSubject(profile)
		case providerAnthropic:
			return anthropicFixtureSubject(profile)
		}
		return suite.Subject{}, nil, fmt.Errorf("unknown provider %q (known: %s, %s)", provider, providerOpenAI, providerAnthropic)
	}
	liveSubjectFn = liveSubject
)

func cli(args []string, stdout, stderr io.Writer, env func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: providerconf run [--provider openai|anthropic] [--live] [--profile file] [--out file] | providerconf check <report.json>")
		return 2
	}
	switch args[0] {
	case "run":
		return runCmd(args[1:], stdout, stderr, env)
	case "check":
		return checkCmd(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "providerconf: unknown command %q\n", args[0])
	return 2
}

func checkCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: providerconf check <report.json>")
		return 2
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "providerconf: %v\n", err)
		return 1
	}
	if err := suite.ValidateAgainst(suite.SchemaConformanceReport, raw); err != nil {
		fmt.Fprintf(stderr, "providerconf: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "the report is valid")
	return 0
}

func runCmd(args []string, stdout, stderr io.Writer, env func(string) string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	provider := fs.String("provider", providerOpenAI, "the provider adapter to run the suite against")
	live := fs.Bool("live", false, "run against the real service (also needs "+liveEnv+"=1 and a binary built with -tags live)")
	profilePath := fs.String("profile", "", "a provider profile JSON file (default: the embedded fixture profile)")
	out := fs.String("out", "", "write the report here (default: standard output)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	mode, err := resolveMode(*live, env)
	if err != nil {
		fmt.Fprintf(stderr, "providerconf: %v\n", err)
		return 2
	}

	var profile pv.ProviderProfile
	if *profilePath != "" {
		raw, err := os.ReadFile(*profilePath)
		if err == nil {
			err = json.Unmarshal(raw, &profile)
		}
		if err != nil {
			fmt.Fprintf(stderr, "providerconf: cannot read the profile: %v\n", err)
			return 1
		}
	} else {
		profile, err = fixtureProfileFor(*provider)
		if err != nil {
			fmt.Fprintf(stderr, "providerconf: %v\n", err)
			return 1
		}
	}
	if string(profile.ProviderID) != *provider {
		fmt.Fprintf(stderr, "providerconf: the profile is for provider %q, not %q\n", profile.ProviderID, *provider)
		return 2
	}

	mk := func() (suite.Subject, func(), error) {
		if mode == suite.ModeLive {
			return liveSubjectFn(*provider, profile, env)
		}
		return fixtureSubjectFn(*provider, profile)
	}
	started := time.Now().UTC()
	rows := suite.Run(context.Background(), mk)
	completed := time.Now().UTC()
	for _, r := range rows {
		line := fmt.Sprintf("%-14s %-42s %s", r.Group, r.Name, r.Result)
		if r.Result == suite.Failed {
			line += "  " + r.Reason + " (" + r.Detail + ")"
		}
		fmt.Fprintln(stderr, line)
	}
	_, raw, err := suite.BuildReport(suite.ReportInput{Rows: rows, Profile: profile, Mode: mode, Started: started, Completed: completed})
	if err != nil {
		fmt.Fprintf(stderr, "providerconf: the report is not valid, nothing was written: %v\n", err)
		return 1
	}
	if *out != "" {
		if err := os.WriteFile(*out, raw, 0o644); err != nil {
			fmt.Fprintf(stderr, "providerconf: %v\n", err)
			return 1
		}
	} else if _, err := stdout.Write(raw); err != nil {
		return 1
	}
	for _, r := range rows {
		if r.Result == suite.Failed {
			fmt.Fprintln(stderr, "providerconf: NOT QUALIFIED, the report records the failed checks")
			return 1
		}
	}
	fmt.Fprintf(stderr, "providerconf: all %d checks passed or not applicable (%s)\n", len(rows), mode)
	return 0
}
