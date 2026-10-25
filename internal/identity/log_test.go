package identity_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	alixlog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-082: a context token, or any part of one, never reaches a log line, an
// error message or a response body.
//
// The scenario drives every path that logs (acceptance, each kind of refusal,
// a failing replay cache and key store, key revocation) through the middleware
// and the verifier, against two sinks. The first is a plain slog JSON handler
// that scrubs nothing, so a pass proves the code never emits the token rather
// than relying on internal/log's redaction; the second is that redacting
// handler, as production configures it.
func TestTokenNeverLogged(t *testing.T) {
	var raw, redacted bytes.Buffer
	sinks := map[string]struct {
		logger *slog.Logger
		buf    *bytes.Buffer
	}{
		"plain handler":     {slog.New(slog.NewJSONHandler(&raw, &slog.HandlerOptions{Level: slog.LevelDebug})), &raw},
		"redacting handler": {alixlog.NewWithWriter(config.LogConfig{Level: "debug"}, &redacted), &redacted},
	}

	for name, sink := range sinks {
		t.Run(name, func(t *testing.T) {
			logger, buf := sink.logger, sink.buf

			clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
			db := storetest.Open(t, nil)
			iss := identitytest.NewIssuer(t, identitytest.IssuerConfig{Clock: clk})
			keys := identity.NewKeyRegistry(logger)
			if err := keys.Register(iss.Key()); err != nil {
				t.Fatal(err)
			}
			verifier, err := identity.NewVerifier(identity.Options{
				DeploymentID: identitytest.DefaultDeployment,
				Keys:         keys,
				Replay:       identity.NewSQLiteReplayCache(db, clk, logger),
				Clock:        clk,
				Logger:       logger,
			})
			if err != nil {
				t.Fatal(err)
			}
			failing, err := identity.NewVerifier(identity.Options{
				DeploymentID: identitytest.DefaultDeployment,
				Keys:         keys,
				Replay:       failingReplay{},
				Clock:        clk,
				Logger:       logger,
			})
			if err != nil {
				t.Fatal(err)
			}
			broken, err := identity.NewVerifier(identity.Options{
				DeploymentID: identitytest.DefaultDeployment,
				Keys:         brokenKeys{},
				Replay:       failingReplay{},
				Clock:        clk,
				Logger:       logger,
			})
			if err != nil {
				t.Fatal(err)
			}

			good := iss.Token()
			type attempt struct {
				token string
				auth  identity.Authenticator
			}
			attempts := []attempt{
				{good, verifier},       // accepted
				{good, verifier},       // replayed
				{iss.Token(), failing}, // replay cache down
				{iss.Token(), broken},  // key store down
				{withPayload(t, iss.Token(), func(m map[string]any) { m[identity.ClaimTenant] = "ten_other" }), verifier},
				{iss.Draft().Times(-6*minute, -6*minute, -minute).Sign(), verifier},
				{iss.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimAudience, "dep_other") }), verifier},
				{iss.Token(func(d *identitytest.Draft) { d.Drop(identity.ClaimWorkspace) }), verifier},
				{iss.Token(func(d *identitytest.Draft) { d.SetHeader(identity.HeaderKeyID, "unknown-key") }), verifier},
				{iss.Token(func(d *identitytest.Draft) { d.SetHeader(identity.HeaderAlgorithm, "none") }), verifier},
				{good[:len(good)-6], verifier}, // truncated
			}

			var secrets []string
			var outputs []string // every error text and response body the callers can see
			for _, a := range attempts {
				h, p, s := segments(t, a.token)
				secrets = append(secrets, a.token, h, p, s, "Bearer "+a.token)

				// Through the middleware, which logs.
				rec := do(identity.Middleware(a.auth, identity.WithLogger(logger))(&probe{}),
					newReq("GET", "/v1/x", http.Header{"Authorization": {"Bearer " + a.token}}, ""))
				outputs = append(outputs, rec.Body.String())

				// Directly, which returns the error.
				if v, ok := a.auth.(*identity.Verifier); ok {
					if _, err := v.Verify(bg, a.token); err != nil {
						outputs = append(outputs, err.Error())
						var ce *contracts.Error
						if errors.As(err, &ce) {
							outputs = append(outputs, ce.Message)
						}
					}
				}
			}
			keys.Revoke(iss.KeyID)

			out := buf.String()
			// The assertions are not vacuous: the paths that log did log.
			for _, want := range []string{
				"context token accepted", "context token refused", "replay cache failed",
				"issuer key lookup failed", "issuer key revoked",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("log lacks %q, so the scenario did not exercise that path:\n%s", want, out)
				}
			}
			if n := strings.Count(out, "context token refused"); n < len(attempts)-1 {
				t.Fatalf("only %d refusals logged for %d refused attempts", n, len(attempts)-1)
			}

			for _, s := range secrets {
				if len(s) < 16 {
					t.Fatalf("test bug: secret %q too short to be meaningful", s)
				}
				if strings.Contains(out, s) {
					t.Fatalf("a token or token segment appears in the log:\n%s", out)
				}
				for _, o := range outputs {
					if strings.Contains(o, s) {
						t.Fatalf("a token or token segment appears in an error or response: %s", o)
					}
				}
			}
			if strings.Contains(out, "eyJ") || strings.Contains(out, "Bearer ") {
				t.Fatalf("log contains something shaped like a bearer credential:\n%s", out)
			}
		})
	}
}
