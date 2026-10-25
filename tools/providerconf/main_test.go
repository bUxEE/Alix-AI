package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

func noEnv(string) string { return "" }

// TestProviderConfRunWithFixtures: the first adapter (OpenAI) passes the suite
// against the recorded fixtures, over a real loopback HTTP server, and the
// report validates against the schema, decodes into the typed report and says
// what a text-only provider should.
func TestProviderConfRunWithFixtures(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "report.json")
	var stdout, stderr bytes.Buffer
	code := cli([]string{"run", "--out", out}, &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr.String())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := suite.ValidateAgainst(suite.SchemaConformanceReport, raw); err != nil {
		t.Fatalf("the report does not validate: %v", err)
	}
	var rep pv.ConformanceReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("the report does not decode: %v", err)
	}
	if rep.Outcome != pv.ConformanceReportOutcomePassed {
		t.Fatalf("outcome = %s:\n%s", rep.Outcome, stderr.String())
	}
	if rep.Suite.Mode != pv.ConformanceReportSuiteModeFixtures || rep.Provider.ProviderID != "openai" || rep.Provider.ModelID != "gpt-5-mini" {
		t.Fatalf("suite/provider = %+v %+v", rep.Suite, rep.Provider)
	}
	m := rep.Groups.Multimodal
	if rep.Declared.Image || rep.Declared.File || rep.Declared.Audio ||
		m.ImageInputOutput.Result != pv.ConformanceReportConditionalCheckResultResultNotApplicable ||
		m.FileInputOutput.Result != pv.ConformanceReportConditionalCheckResultResultNotApplicable ||
		m.AudioInputOutput.Result != pv.ConformanceReportConditionalCheckResultResultNotApplicable {
		t.Fatalf("the text-only provider declares %+v, image/file/audio = %s/%s/%s", rep.Declared, m.ImageInputOutput.Result, m.FileInputOutput.Result, m.AudioInputOutput.Result)
	}
	if strings.Contains(string(raw), "fixture-credential") {
		t.Fatal("the report holds a credential")
	}

	t.Run("check validates a written report and refuses a broken one", func(t *testing.T) {
		var so, se bytes.Buffer
		if c := cli([]string{"check", out}, &so, &se, noEnv); c != 0 {
			t.Fatalf("check exit %d: %s", c, se.String())
		}
		bad := filepath.Join(dir, "bad.json")
		_ = os.WriteFile(bad, []byte(strings.Replace(string(raw), `"outcome": "passed"`, `"outcome": "maybe"`, 1)), 0o644)
		if c := cli([]string{"check", bad}, &so, &se, noEnv); c == 0 {
			t.Fatal("check accepted an invalid report")
		}
	})
}

// TestProviderConfRunsTheAnthropicSubject: the Anthropic adapter passes the same suite against its recorded fixtures
// (the testdata of the adapter, embedded here), the report validates and names the Anthropic profile, and a profile of
// another provider is refused.
func TestProviderConfRunsTheAnthropicSubject(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	if code := cli([]string{"run", "--provider", "anthropic", "--out", out}, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr.String())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := suite.ValidateAgainst(suite.SchemaConformanceReport, raw); err != nil {
		t.Fatalf("the report does not validate: %v", err)
	}
	var rep pv.ConformanceReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != pv.ConformanceReportOutcomePassed || rep.Suite.Mode != pv.ConformanceReportSuiteModeFixtures || rep.Provider.ProviderID != "anthropic" {
		t.Fatalf("outcome %s, suite %+v, provider %+v", rep.Outcome, rep.Suite, rep.Provider)
	}
	if strings.Contains(string(raw), "fixture-credential") {
		t.Fatal("the report holds a credential")
	}
	profile, err := anthropicFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("each check", func(t *testing.T) {
		suite.RunProviderSuite(t, func(t *testing.T) suite.Subject {
			s, cleanup, err := anthropicFixtureSubject(profile)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			return s
		})
	})
	t.Run("a profile of another provider is refused", func(t *testing.T) {
		var so, se bytes.Buffer
		if code := cli([]string{"run", "--provider", "openai", "--profile", writeProfile(t, profile)}, &so, &se, noEnv); code != 2 {
			t.Fatalf("exit %d, want 2: %s", code, se.String())
		}
	})
}

func writeProfile(t *testing.T, p pv.ProviderProfile) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLiveModeRequiresExplicitFlagAndEnv: live mode needs --live, the live
// build tag and ALIXAI_PROVIDERCONF_LIVE=1; the environment variable alone
// leaves the tool in fixture mode. Every refusal happens before a subject (a
// client, a socket) is built.
func TestLiveModeRequiresExplicitFlagAndEnv(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	liveOn := map[string]string{liveEnv: "1", "ALIXAI_LIVE_OPENAI_KEY": "placeholder-not-a-key"}

	t.Run("the environment variable alone stays in fixture mode", func(t *testing.T) {
		mode, err := resolveMode(false, env(liveOn))
		if err != nil || mode != suite.ModeFixtures {
			t.Fatalf("mode = %q, err = %v", mode, err)
		}
	})
	t.Run("--live without the environment variable is refused", func(t *testing.T) {
		if _, err := resolveMode(true, env(nil)); err == nil {
			t.Fatal("live mode was allowed without the environment variable")
		}
	})
	t.Run("--live with the environment variable needs the live build", func(t *testing.T) {
		mode, err := resolveMode(true, env(liveOn))
		if liveBuild {
			if err != nil || mode != suite.ModeLive {
				t.Fatalf("mode = %q, err = %v", mode, err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), "-tags live") {
			t.Fatalf("a binary without the live tag allowed live mode: %q, %v", mode, err)
		}
	})
	t.Run("a refused live run builds no subject and exits 2", func(t *testing.T) {
		oldF, oldL := fixtureSubjectFn, liveSubjectFn
		defer func() { fixtureSubjectFn, liveSubjectFn = oldF, oldL }()
		built := 0
		fixtureSubjectFn = func(string, pv.ProviderProfile) (suite.Subject, func(), error) {
			built++
			return suite.Subject{}, nil, os.ErrInvalid
		}
		liveSubjectFn = func(string, pv.ProviderProfile, func(string) string) (suite.Subject, func(), error) {
			built++
			return suite.Subject{}, nil, os.ErrInvalid
		}
		var so, se bytes.Buffer
		// no environment variable: refused in every build
		if code := cli([]string{"run", "--live"}, &so, &se, noEnv); code != 2 {
			t.Fatalf("exit %d, want 2: %s", code, se.String())
		}
		if built != 0 || so.Len() != 0 {
			t.Fatalf("a refused live run built %d subjects and wrote %d bytes", built, so.Len())
		}
		if !liveBuild {
			// the flag and the variable, but no live build: still refused
			if code := cli([]string{"run", "--live"}, &so, &se, env(liveOn)); code != 2 || built != 0 {
				t.Fatalf("exit %d, built %d", code, built)
			}
		}
	})
}

// TestOpenAIAdapterPassesEachCheck runs the adapter through the reusable
// RunProviderSuite entry point, so a regression names the check it breaks.
func TestOpenAIAdapterPassesEachCheck(t *testing.T) {
	profile, err := openaiFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	suite.RunProviderSuite(t, func(t *testing.T) suite.Subject {
		s, cleanup, err := openaiFixtureSubject(profile)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		return s
	})
}

type activeCreds struct{}

func (activeCreds) CredentialStatus(context.Context, contracts.Scope, pv.CredentialRefID) (pv.CredentialRefStatus, error) {
	return pv.CredentialRefStatusActive, nil
}

// TestReportIsRecordedByTheRegistry: the report the tool writes for a
// registered profile is accepted by the registry's RecordConformance, so it is
// recordable as last_conformance and, with the rest of the qualification steps,
// qualifies the profile.
func TestReportIsRecordedByTheRegistry(t *testing.T) {
	now := time.Now().UTC()
	reg, err := providerreg.New(storetest.Open(t, nil), providerreg.Config{Clock: clock.NewFake(now), Credentials: activeCreds{}})
	if err != nil {
		t.Fatal(err)
	}
	scope := storetest.Scope("app_conf", "ten_a", "wsp_a")
	draft, err := openaiFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	draft.LastPolicyReview = now.Format("2006-01-02")
	// the registry mints these itself
	draft.ProfileID, draft.Lifecycle, draft.CreatedAt, draft.UpdatedAt = "", "", "", ""
	draft.CredRef, draft.BillingNoticeAck, draft.LastConformance = nil, nil, nil
	registered, err := reg.Register(context.Background(), scope, draft)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	dir := t.TempDir()
	prof, _ := json.Marshal(registered)
	profPath, out := filepath.Join(dir, "profile.json"), filepath.Join(dir, "report.json")
	if err := os.WriteFile(profPath, prof, 0o644); err != nil {
		t.Fatal(err)
	}
	var so, se bytes.Buffer
	if code := cli([]string{"run", "--profile", profPath, "--out", out}, &so, &se, noEnv); code != 0 {
		t.Fatalf("exit %d:\n%s", code, se.String())
	}
	raw, _ := os.ReadFile(out)
	var rep pv.ConformanceReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	got, err := reg.RecordConformance(context.Background(), scope, registered.ProfileID, rep)
	if err != nil {
		t.Fatalf("RecordConformance refused the report: %v", err)
	}
	if got.LastConformance == nil || got.LastConformance.ReportID != rep.ReportID {
		t.Fatal("the report was not recorded as last_conformance")
	}
}
