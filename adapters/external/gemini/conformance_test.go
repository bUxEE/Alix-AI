package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

// ringKeys adapts the suite's key ring to the adapter's key source.
type ringKeys struct{ ring *suite.KeyRing }

func (k ringKeys) Key(_ context.Context, scope contracts.Scope, _ string) (credentials.Secret, error) {
	v, err := k.ring.Resolve(scope)
	if err != nil {
		return "", err
	}
	return credentials.Secret(v), nil
}

// lockedBuffer is a log sink the adapter and the suite can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func shippedProfile(t testing.TB) pv.ProviderProfile {
	t.Helper()
	var p pv.ProviderProfile
	if err := json.Unmarshal(ProfileJSON(), &p); err != nil {
		t.Fatalf("the shipped profile does not decode: %v", err)
	}
	return p
}

// fixtureSubject returns a subject over the recorded fixtures: the real adapter
// built the way the composition root builds it (egress dialer, route guard), a
// real loopback HTTP server, a placeholder credential.
func fixtureSubject(t testing.TB, profile pv.ProviderProfile) (suite.Subject, func()) {
	t.Helper()
	up, err := newFakeUpstream()
	if err != nil {
		t.Fatal(err)
	}
	dest, err := egress.DestinationFromURL(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(ProviderName, al); err != nil {
		t.Fatal(err)
	}
	ring := suite.NewKeyRing()
	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := Config{
		BaseURL: up.URL, CredentialRef: "cred_01CONFORMANCEREFERENCE", Model: string(profile.ModelID), // TEST-ONLY opaque reference id, not a key
		MaxOutputToks: int(profile.Limits.MaxOutputToks), MaxContextToks: int(profile.Limits.MaxContextToks),
		RequestTimeout: 20 * time.Second, StreamTimeout: 20 * time.Second,
	}
	c, err := New(cfg, reg.Dialer(ProviderName), ringKeys{ring}, WithLogger(log),
		WithSleeper(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		up.Close()
		t.Fatal(err)
	}
	return suite.Subject{
		Name: "gemini (fixtures)", Generator: c, Caps: c, Runtime: c, Profile: profile, Upstream: up, Keys: ring,
		Logs: logs.String,
	}, up.Close
}

// TestConformanceGeminiFixtures: the adapter passes every check of the
// provider conformance suite against the recorded fixtures, over a real loopback
// HTTP server, and the run produces a valid conformance report.
func TestConformanceGeminiFixtures(t *testing.T) {
	profile := shippedProfile(t)
	t.Run("each check", func(t *testing.T) {
		suite.RunProviderSuite(t, func(t *testing.T) suite.Subject {
			s, closeFn := fixtureSubject(t, profile)
			t.Cleanup(closeFn)
			return s
		})
	})
	t.Run("report", func(t *testing.T) {
		start := time.Now()
		rows := suite.Run(context.Background(), func() (suite.Subject, func(), error) {
			s, closeFn := fixtureSubject(t, profile)
			return s, closeFn, nil
		})
		if len(rows) != 30 {
			t.Fatalf("%d checks, want 30", len(rows))
		}
		for _, r := range rows {
			want := suite.Passed
			if r.Group == "multimodal" && (r.Name == "image_input_output" || r.Name == "file_input_output" || r.Name == "audio_input_output") {
				want = suite.NotApplicable
			}
			if r.Result != want {
				t.Errorf("%s/%s: %s %s (%s), want %s", r.Group, r.Name, r.Result, r.Reason, r.Detail, want)
			}
		}
		rep, raw, err := suite.BuildReport(suite.ReportInput{Rows: rows, Profile: profile, Mode: suite.ModeFixtures, Started: start, Completed: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Outcome != pv.ConformanceReportOutcomePassed || rep.Provider.ProviderID != "gemini" {
			t.Fatalf("report %s for %s", rep.Outcome, rep.Provider.ProviderID)
		}
		if strings.Contains(string(raw), "fixture-credential") {
			t.Fatal("the report holds a credential")
		}
	})
}

// TestShippedProfileBillingNotice: the starting profile says that Gemini API
// access is tied to a Google Cloud project's quota and billing and that a
// consumer subscription is not API access, and it validates against the schema.
func TestShippedProfileBillingNotice(t *testing.T) {
	p := shippedProfile(t)
	if p.BillingNotice.SubscriptionGrantsAPIAccess {
		t.Fatal("the billing notice claims a subscription grants API access")
	}
	text := strings.ToLower(p.BillingNotice.Text)
	for _, want := range []string{"google cloud project", "quota", "billing", "subscription", "does not include api access"} {
		if !strings.Contains(text, want) {
			t.Errorf("billing notice lacks %q: %q", want, p.BillingNotice.Text)
		}
	}
	if err := suite.ValidateAgainst(suite.SchemaProviderProfile, ProfileJSON()); err != nil {
		t.Fatal(err)
	}
}
