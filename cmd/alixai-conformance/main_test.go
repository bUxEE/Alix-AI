package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// The conformance run against the real clinic-booking reference application: the clinic is
// started as a subprocess (node, test support mode, a loopback port it picks), its signed
// package is validated, audited and replayed, and what the tool says is held to what the
// application itself recorded.
func TestConformanceAgainstClinicBooking(t *testing.T) {
	clinic := startClinic(t)
	before := clinic.stats(t)

	res := runConformance(t,
		"-endpoint", "clinic-mcp="+clinic.mcpURL,
		"-trust-key", devkeys.KeyID+"="+devkeys.PublicKeyBase64(), "-allow-test-keys",
		clinicPackage)
	if res.code != exitPass || res.report.Result != "pass" {
		t.Fatalf("the clinic reference application does not conform:\n%s", describe(res))
	}
	if res.stdout != "" {
		t.Errorf("with -json FILE nothing goes to standard output, got %q", res.stdout)
	}
	if got := stageNames(res.report); !cmp.Equal(got, []string{"package", "audit", "conversation"}) {
		t.Fatalf("stages = %v", got)
	}
	if p := res.report.Package; p.ApplicationID != "clinic-booking" || p.Authenticity != "verified" || p.SignedBy != devkeys.KeyID || p.ManifestHash == "" {
		t.Errorf("package = %+v: the original's signature is checked against the key given", p)
	}

	// stage 1: the package loads, its signature is the TEST-ONLY publisher's, nothing is critical
	pkg := res.stage(t, "package")
	if it, ok := pkg.item("package.load"); !ok || it.Status != StatusPass {
		t.Errorf("package.load = %+v", it)
	}
	if it, _ := pkg.item("package.authenticity"); it.Status != StatusPass {
		t.Errorf("package.authenticity = %+v", it)
	}

	// stage 2: the audit reached the application. A section that passes cannot have been
	// skipped: deterministic results, safe execution and failure handling are the probes
	// that call the application.
	audit := res.stage(t, "audit")
	if it, ok := audit.item("audit.outcome"); !ok || it.Status == StatusFail || it.Details["transport"] != "in_use" {
		t.Errorf("audit.outcome = %+v: the audit must have inspected the application's adapter", it)
	}
	sections := 0
	for _, it := range audit.Items {
		if !strings.HasPrefix(it.ID, "audit.section.") {
			continue
		}
		sections++
		// the clinic's retrieval path cannot be hosted by the audit of this build (its RAG
		// source ships inside the package): those sections are unverified and say so, and
		// nothing may fail
		if it.Status != StatusPass && it.Status != StatusUnverified {
			t.Errorf("%s = %s: %s", it.Name, it.Status, it.Message)
		}
	}
	if sections != 10 {
		t.Errorf("the audit reports %d sections, want the ten of master plan 4.5", sections)
	}
	for _, n := range []string{"2", "8", "9"} {
		if it, _ := audit.item("audit.section." + n); it.Status != StatusPass {
			t.Errorf("section %s = %+v: the probes against the application must have passed", n, it)
		}
	}

	// stage 3: every declared example was replayed, none failed, and the replays reached the
	// acts the capabilities are for
	conv := res.stage(t, "conversation")
	if s := res.report.Summary; s.ExamplesDeclared != 22 || s.Examples != 22 {
		t.Errorf("examples replayed/declared = %d/%d, want 22/22 (6, 4, 4, 4, 4 over the five capabilities)", s.Examples, s.ExamplesDeclared)
	}
	if conv.count(StatusFail) != 0 || conv.count(StatusNotRun) != 0 {
		t.Errorf("the conversation stage has failures: %s", describe(res))
	}
	for id, act := range map[string]string{
		"conversation.example.appointments.cancel.en-cancel-by-reference": "request_confirmation",
		"conversation.example.appointments.book.en-book-next-available":   "request_confirmation",
		// the examples that name a date (2026-10-14) stop being bookable when that day has passed,
		// which the application refuses with a declared validation_failed: the one that asks for
		// "the next 7 days" is relative to now
		"conversation.example.availability.list.en-next-week-with-doctor": "present_result",
		"conversation.example.patients.find.en-find-by-name":              "present_result",
	} {
		it, ok := conv.item(id)
		if !ok || it.Status != StatusPass || it.Details["act"] != act {
			t.Errorf("%s = %+v, want a pass that reached %s", id, it, act)
		}
	}
	for _, c := range []string{"appointments.book", "appointments.cancel", "availability.list", "patients.find"} {
		if it, _ := conv.item("conversation.capability." + c); it.Status != StatusPass {
			t.Errorf("capability %s = %+v", c, it)
		}
	}
	if it, ok := conv.item("conversation.cleanup"); !ok || it.Status != StatusPass {
		t.Errorf("the disposable tenants were not removed: %+v", it)
	}

	// the application's own books: the replays prepared actions, and every disposable tenant
	// the run made is gone
	after := clinic.stats(t)
	if after.prepares() <= before.prepares() {
		t.Errorf("the application was never asked to prepare anything: %v", after.Calls)
	}
	if diff := cmp.Diff(before.Totals, after.Totals); diff != "" {
		t.Errorf("the run left records in the application (-before +after):\n%s", diff)
	}

	// the JUnit file says what the JSON says
	checkJUnitAgrees(t, res)
	if res.junit.Failures != 0 || res.junit.Tests != res.report.Summary.Items {
		t.Errorf("junit: tests %d failures %d, want %d and 0", res.junit.Tests, res.junit.Failures, res.report.Summary.Items)
	}
	if !strings.Contains(res.stderr, "PASS") {
		t.Errorf("the summary on standard error does not say PASS:\n%s", res.stderr)
	}
}

// -strict makes warnings and unverified items fail the run, in the JSON and in the JUnit.
func TestConformanceStrictFailsWhatItWarnsAbout(t *testing.T) {
	clinic := startClinic(t)
	res := runConformance(t, "-strict",
		"-endpoint", "clinic-mcp="+clinic.mcpURL,
		"-trust-key", devkeys.KeyID+"="+devkeys.PublicKeyBase64(), "-allow-test-keys",
		clinicPackage)
	if res.code != exitFail || res.report.Result != "fail" || !res.report.Strict {
		t.Fatalf("a strict run of a package with warnings must fail:\n%s", describe(res))
	}
	checkJUnitAgrees(t, res)
	if res.junit.Failures == 0 || res.junit.Skipped != 0 {
		t.Errorf("strict: junit failures %d skipped %d: unverified items fail instead of skipping", res.junit.Failures, res.junit.Skipped)
	}
}

// The tool says what is wrong, in both formats, and stops looking at what it cannot trust.
func TestConformanceReportsFailures(t *testing.T) {
	t.Run("a package changed after it was signed fails and the later stages do not run", func(t *testing.T) {
		dir := t.TempDir()
		if err := copyPackage(clinicPackage, dir); err != nil {
			t.Fatal(err)
		}
		doc := filepath.Join(dir, "documentation", "policies", "booking-rules.md")
		f, err := os.OpenFile(doc, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("\nA bookable slot may now be taken twice.\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		res := runConformance(t, dir)
		if res.code != exitFail || res.report.Result != "fail" {
			t.Fatalf("a tampered package must fail:\n%s", describe(res))
		}
		pkg := res.stage(t, "package")
		if pkg.Status != StatusFail {
			t.Errorf("package stage = %s, want fail", pkg.Status)
		}
		if it, _ := pkg.item("package.load"); it.Status != StatusFail || !strings.Contains(it.Message, "critical") {
			t.Errorf("package.load = %+v", it)
		}
		for _, name := range []string{"audit", "conversation"} {
			st := res.stage(t, name)
			if st.Status != StatusNotRun || st.count(StatusPass) != 0 || len(st.Items) == 0 {
				t.Errorf("%s stage = %s with %d item(s): a package that failed its first stage is not looked at further", name, st.Status, len(st.Items))
			}
		}
		if res.report.Summary.Examples != 0 {
			t.Errorf("examples replayed = %d, want 0", res.report.Summary.Examples)
		}
		checkJUnitAgrees(t, res)
		if res.junit.Failures < 3 {
			t.Errorf("junit failures = %d: the failed package item and the two stages that did not run are failures", res.junit.Failures)
		}
	})

	t.Run("an application that is not running fails the audit and the conversation, with no example passing", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close() // nothing listens there now

		res := runConformance(t, "-endpoint", "clinic-mcp=http://"+addr+"/mcp", clinicPackage)
		if res.code != exitFail || res.report.Result != "fail" {
			t.Fatalf("an application that cannot be reached must fail:\n%s", describe(res))
		}
		if st := res.stage(t, "package"); st.Status == StatusFail || st.Status == StatusNotRun {
			t.Errorf("the package itself is fine, stage = %s", st.Status)
		}
		audit := res.stage(t, "audit")
		if audit.Status != StatusFail {
			t.Errorf("audit stage = %s, want fail: %s", audit.Status, describe(res))
		}
		conv := res.stage(t, "conversation")
		if conv.Status != StatusFail {
			t.Errorf("conversation stage = %s, want fail: %s", conv.Status, describe(res))
		}
		if conv.count(StatusPass) != 0 || res.report.Summary.Examples != 0 {
			t.Errorf("conversation: %d passing item(s), %d example(s) replayed: with the application down nothing can pass", conv.count(StatusPass), res.report.Summary.Examples)
		}
		if res.report.Summary.ExamplesDeclared != 22 {
			t.Errorf("examples declared = %d: the package still declares them", res.report.Summary.ExamplesDeclared)
		}
		checkJUnitAgrees(t, res)
		if res.junit.Failures == 0 {
			t.Error("junit has no failure")
		}
	})

	t.Run("a package none of whose examples reaches a capability demonstrates nothing and fails", func(t *testing.T) {
		clinic := startClinic(t)
		dir := t.TempDir()
		if err := copyPackage(clinicPackage, dir); err != nil {
			t.Fatal(err)
		}
		// every utterance of every example becomes words that no title or term of the package
		// uses, so the router offers the model nothing and no capability can be shown reachable
		files, err := filepath.Glob(filepath.Join(dir, "capabilities", "*.yaml"))
		if err != nil || len(files) != 5 {
			t.Fatalf("capabilities = %v, %v", files, err)
		}
		utterance := regexp.MustCompile(`(?m)^(\s+utterance: ).*$`)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, utterance.ReplaceAll(b, []byte("${1}zzqx plmb vrtk")), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now().UTC().Add(-time.Hour)
		if _, err := trust.SignPackageDir(dir, trust.SignOptions{
			PackageIdentity: trust.PackageIdentity{ApplicationID: "clinic-booking", PackageVersion: "1.0.0"},
			Keys:            []trust.SigningKey{devkeys.SigningKey()}, Now: now, ExpiresAt: now.AddDate(1, 0, 0),
		}); err != nil {
			t.Fatal(err)
		}

		res := runConformance(t, "-endpoint", "clinic-mcp="+clinic.mcpURL,
			"-trust-key", devkeys.KeyID+"="+devkeys.PublicKeyBase64(), "-allow-test-keys", dir)
		if res.code != exitFail || res.report.Result != "fail" {
			t.Fatalf("a package whose examples reach nothing must fail: %s", describe(res))
		}
		conv := res.stage(t, "conversation")
		if it, ok := conv.item("conversation.demonstrated"); !ok || it.Status != StatusFail {
			t.Errorf("conversation.demonstrated = %+v", it)
		}
		if s := res.report.Summary; s.Examples != 22 || s.ExamplesDeclared != 22 {
			t.Errorf("examples replayed/declared = %d/%d: every example is replayed, they just reach nothing", s.Examples, s.ExamplesDeclared)
		}
		for _, c := range []string{"appointments.book", "appointments.cancel", "availability.list", "patients.find", "patients.update_contact"} {
			if it, _ := conv.item("conversation.capability." + c); it.Status != StatusUnverified {
				t.Errorf("capability %s = %+v, want unverified", c, it)
			}
		}
		for _, it := range conv.Items {
			if strings.HasPrefix(it.ID, "conversation.example.") && it.Status != StatusWarn {
				t.Errorf("%s = %s: an utterance nothing routes is a warning on the example, and no more", it.ID, it.Status)
			}
		}
		checkJUnitAgrees(t, res)
	})

	t.Run("the command line is checked", func(t *testing.T) {
		for _, c := range []struct {
			name string
			args []string
		}{
			{"no package", nil},
			{"two packages", []string{"a", "b"}},
			{"an endpoint without a url", []string{"-endpoint", "clinic-mcp", "p"}},
			{"an endpoint given twice", []string{"-endpoint", "a=http://127.0.0.1:1/", "-endpoint", "a=http://127.0.0.1:2/", "p"}},
			{"both reports on standard output", []string{"-json", "-", "-junit", "-", "p"}},
			{"an unknown flag", []string{"-nonsense", "p"}},
			{"a timeout that is not positive", []string{"-timeout", "0s", "p"}},
		} {
			var out, errb bytes.Buffer
			if code := run(context.Background(), c.args, &out, &errb); code != exitUsage {
				t.Errorf("%s: exit %d, want %d (%s)", c.name, code, exitUsage, errb.String())
			}
			if out.Len() != 0 {
				t.Errorf("%s: a usage error wrote to standard output: %q", c.name, out.String())
			}
		}
	})

	t.Run("a package directory that does not exist fails", func(t *testing.T) {
		res := runConformance(t, filepath.Join(t.TempDir(), "missing"))
		if res.code != exitFail || res.stage(t, "package").Status != StatusFail || res.stage(t, "audit").Status != StatusNotRun {
			t.Fatalf("%s", describe(res))
		}
	})
}

func stageNames(r Report) []string {
	var out []string
	for _, s := range r.Stages {
		out = append(out, s.Name)
	}
	return out
}

// checkJUnitAgrees holds the JUnit file to the JSON report: a suite per stage, a case per
// item, and the failures and skips the run's own rule gives.
func checkJUnitAgrees(t *testing.T, res conformRun) {
	t.Helper()
	if len(res.junit.Suites) != len(res.report.Stages) {
		t.Fatalf("junit has %d suites, the report %d stages", len(res.junit.Suites), len(res.report.Stages))
	}
	total, failures := 0, 0
	for i, st := range res.report.Stages {
		suite := res.junit.Suites[i]
		wantFail, wantSkip := 0, 0
		for _, it := range st.Items {
			switch {
			case it.Status.failing(res.report.Strict):
				wantFail++
			case it.Status == StatusUnverified:
				wantSkip++
			}
		}
		if suite.Name != st.Name || suite.Tests != len(st.Items) || len(suite.Cases) != len(st.Items) || suite.Failures != wantFail || suite.Skipped != wantSkip {
			t.Errorf("junit suite %s: tests %d failures %d skipped %d; the report says %d item(s), %d failing, %d unverified",
				suite.Name, suite.Tests, suite.Failures, suite.Skipped, len(st.Items), wantFail, wantSkip)
		}
		for j, c := range suite.Cases {
			if c.Name != st.Items[j].Name {
				t.Errorf("junit case %d of %s is %q, the report has %q", j, st.Name, c.Name, st.Items[j].Name)
			}
			if (c.Failure != nil) != st.Items[j].Status.failing(res.report.Strict) {
				t.Errorf("junit case %q: failure=%v but the item is %s", c.Name, c.Failure != nil, st.Items[j].Status)
			}
		}
		total += suite.Tests
		failures += suite.Failures
	}
	if res.junit.Tests != total || res.junit.Failures != failures {
		t.Errorf("junit totals: tests %d failures %d, the suites add up to %d and %d", res.junit.Tests, res.junit.Failures, total, failures)
	}
	if got := (res.code == exitPass); got != (res.junit.Failures == 0) {
		t.Errorf("exit status %d and %d junit failure(s) disagree", res.code, res.junit.Failures)
	}
}
