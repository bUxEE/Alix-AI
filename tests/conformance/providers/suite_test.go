package providersuite

import (
	"context"
	"testing"
)

func fakeFactory(o FakeOptions) func() (Subject, func(), error) {
	return func() (Subject, func(), error) { return NewFake(o).Subject(), nil, nil }
}

func rowFor(rows []Row, name string) (Row, bool) {
	for _, r := range rows {
		if r.Name == name {
			return r, true
		}
	}
	return Row{}, false
}

// TestConformanceFakeExternalProvider: the reference fake external provider
// passes every check, with no media declared and with all three declared, so a
// failure in the tests below is caused by the defect and nothing else.
func TestConformanceFakeExternalProvider(t *testing.T) {
	t.Run("text only", func(t *testing.T) {
		rows := Run(context.Background(), fakeFactory(FakeOptions{}))
		if len(rows) != 30 {
			t.Fatalf("%d checks, want 30", len(rows))
		}
		for _, r := range rows {
			want := Passed
			if r.Group == "multimodal" && (r.Name == "image_input_output" || r.Name == "file_input_output" || r.Name == "audio_input_output") {
				want = NotApplicable
			}
			if r.Result != want {
				t.Errorf("%s/%s: %s %s (%s), want %s", r.Group, r.Name, r.Result, r.Reason, r.Detail, want)
			}
		}
	})
	t.Run("all modalities", func(t *testing.T) {
		RunProviderSuite(t, func(t *testing.T) Subject {
			return NewFake(FakeOptions{Image: true, File: true, Audio: true}).Subject()
		})
		rows := Run(context.Background(), fakeFactory(FakeOptions{Image: true, File: true, Audio: true}))
		for _, n := range []string{"image_input_output", "file_input_output", "audio_input_output"} {
			if r, _ := rowFor(rows, n); r.Result != Passed {
				t.Errorf("%s = %s, want passed when declared", n, r.Result)
			}
		}
	})
}

// TestSuiteFailsEachBrokenProvider: one deliberately broken provider per
// check, each of which must fail its own check with its own reason code.
func TestSuiteFailsEachBrokenProvider(t *testing.T) {
	all := BrokenProviders()
	checks := Checks()
	if len(all) != len(checks) {
		t.Fatalf("%d broken providers for %d checks", len(all), len(checks))
	}
	for i, b := range all {
		if checks[i].Name != b.Check {
			t.Fatalf("broken provider %d is for %s, the check at that position is %s", i, b.Check, checks[i].Name)
		}
		t.Run(b.Check, func(t *testing.T) {
			c := checks[i]
			out := runOne(context.Background(), c, fakeFactory(b.Options))
			if out.Result != Failed {
				t.Fatalf("%s passed a provider that is broken like this: %s", b.Check, b.What)
			}
			if out.Reason != b.Reason {
				t.Fatalf("%s failed with reason %q (%s), want %q", b.Check, out.Reason, out.Detail, b.Reason)
			}
		})
	}
}
