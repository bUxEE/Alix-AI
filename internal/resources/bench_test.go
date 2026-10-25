package resources

import (
	"testing"

	"github.com/buxee/alix-ai/internal/jobs"
)

func BenchmarkAdmitRelease(b *testing.B) {
	e := newEnv(b)
	req := Request{Class: jobs.ClassTurn, Bytes: mib}
	b.ReportAllocs()
	for b.Loop() {
		tk, err := e.gov.Admit(bg, req)
		if err != nil {
			b.Fatal(err)
		}
		tk.Release()
	}
}

func BenchmarkPolicyPick(b *testing.B) {
	e := newEnv(b)
	pol := e.gov.Policy(nil)
	ready := make([]jobs.Candidate, 16)
	for i := range ready {
		ready[i] = jobs.Candidate{ID: "j", Class: jobs.ClassIngest, Type: "ingest.parse"}
	}
	b.ReportAllocs()
	for b.Loop() {
		if pol.Pick(ready) != 0 {
			b.Fatal("held")
		}
	}
}

func BenchmarkPolicyPickHeld(b *testing.B) {
	e := newEnv(b)
	pol := e.gov.Policy(nil)
	release := e.gov.BeginInteractive(jobs.ClassTurn)
	defer release()
	ready := make([]jobs.Candidate, 16)
	for i := range ready {
		ready[i] = jobs.Candidate{ID: "j", Class: jobs.ClassIngest, Type: "ingest.parse"}
	}
	b.ReportAllocs()
	for b.Loop() {
		if pol.Pick(ready) != -1 {
			b.Fatal("not held")
		}
	}
}

func BenchmarkRefresh(b *testing.B) {
	e := newEnv(b)
	e.rt.set(500*mib, LoadedModel{ID: "chat", Bytes: 500 * mib}, LoadedModel{ID: "embed", Bytes: 100 * mib})
	b.ReportAllocs()
	for b.Loop() {
		e.gov.Refresh(bg)
	}
}

func BenchmarkNewGovernor(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(Options{Config: Config{MemoryBudgetBytes: gib}}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOSMemoryProbe(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := (OSMemoryProbe{}).ProcessRSS(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOSDiskProbe(b *testing.B) {
	dir := b.TempDir()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := (OSDiskProbe{}).FreeBytes(dir); err != nil {
			b.Fatal(err)
		}
	}
}
