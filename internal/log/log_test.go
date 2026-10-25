package log

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// fakeSecret returns a fresh random value. Tests never embed a literal secret.
func fakeSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

type secretValuer struct{ v string }

func (s secretValuer) LogValue() slog.Value { return slog.StringValue(s.v) }

// emit logs one record (through any route) that carries secret, plus two
// harmless attributes that must survive untouched.
type emit func(t *testing.T, l *slog.Logger, secret string)

func TestRedactSecrets(t *testing.T) {
	const visibleUser = "u-123"
	const visibleStatus = 200

	cases := []struct {
		name string
		emit emit
	}{
		{"key token", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "token", s) }},
		{"key access_token", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "access_token", s) }},
		{"key password", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "password", s) }},
		{"key PassWord mixed case", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "PassWord", s) }},
		{"key api_key", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "api_key", s) }},
		{"key apiKey", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "apiKey", s) }},
		{"key X-Api-Key", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "X-Api-Key", s) }},
		{"key client_secret", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "client_secret", s) }},
		{"key Authorization", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "Authorization", "Bearer "+s) }},
		{"key cookie", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "cookie", "sid="+s) }},
		{"key set-cookie", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "Set-Cookie", "sid="+s+"; HttpOnly") }},
		{"sensitive key with a number value", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", slog.Int("password", 918273645))
		}},
		{"sensitive key with a bytes value", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "token", []byte(s))
		}},
		{"message carries a bearer token", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("upstream rejected Authorization: Bearer " + s)
		}},
		{"message carries a bare bearer token", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("sent bearer " + s + " to upstream")
		}},
		{"bearer inside a string value", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "detail", "retrying with Bearer "+s+" after 3s")
		}},
		{"authorization header line in a value", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "request_dump", "GET /x HTTP/1.1\r\nHost: h\r\nAuthorization: Basic "+s+"\r\nAccept: */*")
		}},
		{"token in a URL query", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "url", "https://h.example/path?x=1&token="+s+"&y=2")
		}},
		{"api_key in a URL query", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "url", "https://h.example/path?api_key="+s)
		}},
		{"password in URL userinfo", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "dsn", "postgres://alix:"+s+"@db.internal:5432/alix")
		}},
		{"JSON text with a secret member", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "body", `{"user":"u","client_secret":"`+s+`","n":1}`)
		}},
		{"JWT in a value", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "session", "jwt=eyJhbGciOiJIUzI1NiJ9."+s+".c2ln")
		}},
		{"PEM private key", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "blob", "-----BEGIN PRIVATE KEY-----\n"+s+"\n-----END PRIVATE KEY-----")
		}},
		{"error with a bearer token", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "err", errors.New("upstream said: Bearer "+s))
		}},
		{"wrapped error with a URL secret", func(t *testing.T, l *slog.Logger, s string) {
			l.Error("m", "error", fmt.Errorf("fetch https://h.example/?api_key=%s: %w", s, errors.New("timeout")))
		}},
		{"logger.With secret attribute", func(t *testing.T, l *slog.Logger, s string) {
			l.With("token", s).Info("m")
		}},
		{"logger.With bearer string", func(t *testing.T, l *slog.Logger, s string) {
			l.With("hdr", "Bearer "+s).Info("m")
		}},
		{"group member", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", slog.Group("auth", slog.String("password", s)))
		}},
		{"sensitive group name", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", slog.Group("credentials", slog.String("value", s), slog.Int("n", 7)))
		}},
		{"WithGroup sensitive name", func(t *testing.T, l *slog.Logger, s string) {
			l.WithGroup("secrets").Info("m", "value", s)
		}},
		{"nested groups", func(t *testing.T, l *slog.Logger, s string) {
			l.WithGroup("req").Info("m", slog.Group("hdr", slog.String("Authorization", "Bearer "+s)))
		}},
		{"http.Header", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "headers", http.Header{"Authorization": {"Bearer " + s}, "Accept": {"*/*"}})
		}},
		{"nested map", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "cfg", map[string]any{"db": map[string]any{"password": s, "host": "h"}})
		}},
		{"struct with json tags", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "login", struct {
				User     string `json:"user"`
				Password string `json:"password"`
			}{"ada", s})
		}},
		{"slice of strings with a bearer", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "hdrs", []string{"x", "Bearer " + s})
		}},
		{"LogValuer under a sensitive key", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "token", secretValuer{s})
		}},
		{"LogValuer yielding a bearer string", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "detail", secretValuer{"Bearer " + s})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := NewWithWriter(config.LogConfig{Level: "debug"}, &buf)
			secret := fakeSecret(t)

			tc.emit(t, l, secret)
			l.Info("tail", "user_id", visibleUser, "status", visibleStatus)

			out := buf.String()
			if strings.Contains(out, secret) {
				t.Fatalf("secret leaked into the log:\n%s", out)
			}
			if strings.Contains(out, "918273645") {
				t.Fatalf("numeric secret leaked into the log:\n%s", out)
			}
			if !strings.Contains(out, Redacted) {
				t.Fatalf("nothing was redacted:\n%s", out)
			}

			// Every line stays valid JSON and the harmless attributes survive.
			var last map[string]any
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				last = nil
				if err := json.Unmarshal([]byte(line), &last); err != nil {
					t.Fatalf("log line is not JSON: %v\n%s", err, line)
				}
			}
			if last["msg"] != "tail" || last["user_id"] != visibleUser || last["status"] != float64(visibleStatus) {
				t.Fatalf("harmless attributes were damaged: %v", last)
			}
		})
	}
}

func TestRedactConfiguredFields(t *testing.T) {
	cfg := config.LogConfig{Level: "info", RedactKeys: []string{"ssn", "Card-Number", "  dob  "}}

	cases := []struct {
		name string
		emit emit
	}{
		{"exact key", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "ssn", s) }},
		{"upper-case key", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "SSN", s) }},
		{"hyphenated entry matches underscored key", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "card_number", s) }},
		{"camel-case key", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "cardNumber", s) }},
		{"trimmed entry", func(t *testing.T, l *slog.Logger, s string) { l.Info("m", "dob", s) }},
		{"logger.With", func(t *testing.T, l *slog.Logger, s string) { l.With("ssn", s).Info("m") }},
		{"group member", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", slog.Group("patient", slog.String("ssn", s), slog.String("name", "Ada")))
		}},
		{"configured group name", func(t *testing.T, l *slog.Logger, s string) {
			l.WithGroup("dob").Info("m", "value", s)
		}},
		{"map", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "patient", map[string]string{"ssn": s, "name": "Ada"})
		}},
		{"struct with json tag", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "patient", struct {
				SSN  string `json:"ssn"`
				Name string `json:"name"`
			}{s, "Ada"})
		}},
		{"non-string value", func(t *testing.T, l *slog.Logger, s string) {
			l.Info("m", "ssn", []string{s, s})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secret := fakeSecret(t)

			var buf bytes.Buffer
			tc.emit(t, NewWithWriter(cfg, &buf), secret)
			NewWithWriter(cfg, &buf).Info("tail", "user_id", "u-1", "name", "Ada", "ssn_note", "kept", "dobby", "kept")

			out := buf.String()
			if strings.Contains(out, secret) {
				t.Fatalf("configured field leaked:\n%s", out)
			}
			if !strings.Contains(out, Redacted) {
				t.Fatalf("nothing was redacted:\n%s", out)
			}
			// Configured keys are exact (modulo case, "_" and "-"), not substrings.
			for _, want := range []string{`"user_id":"u-1"`, `"name":"Ada"`, `"ssn_note":"kept"`, `"dobby":"kept"`} {
				if !strings.Contains(out, want) {
					t.Errorf("harmless attribute %s missing or damaged:\n%s", want, out)
				}
			}
		})
	}

	t.Run("without the configuration the same keys are logged (the config is what redacts them)", func(t *testing.T) {
		var buf bytes.Buffer
		secret := fakeSecret(t)
		NewWithWriter(config.LogConfig{Level: "info"}, &buf).Info("m", "ssn", secret, "dob", "1990-01-01")
		if !strings.Contains(buf.String(), secret) || strings.Contains(buf.String(), Redacted) {
			t.Fatalf("control run unexpectedly redacted:\n%s", buf.String())
		}
	})
}

func TestLoggerLevelAndFormat(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriter(config.LogConfig{Level: "WARN"}, &buf)
	l.Debug("d")
	l.Info("i")
	l.Warn("w", "n", 1)
	l.Error("e")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("level warn kept %d lines, want 2:\n%s", len(lines), buf.String())
	}
	var rec struct {
		Time  string  `json:"time"`
		Level string  `json:"level"`
		Msg   string  `json:"msg"`
		N     float64 `json:"n"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Level != "WARN" || rec.Msg != "w" || rec.N != 1 {
		t.Fatalf("record = %+v", rec)
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil || ts.Location() != time.UTC {
		t.Fatalf("time %q is not UTC RFC3339: %v", rec.Time, err)
	}

	// An unusable level falls back to info instead of silencing or crashing.
	buf.Reset()
	bad := NewWithWriter(config.LogConfig{Level: "loud"}, &buf)
	bad.Debug("d")
	bad.Info("i")
	if !strings.Contains(buf.String(), `"msg":"i"`) || strings.Contains(buf.String(), `"msg":"d"`) {
		t.Fatalf("fallback level is not info:\n%s", buf.String())
	}
}

func TestLargeIntegersSurviveRedactionWalk(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriter(config.LogConfig{Level: "info"}, &buf)
	l.Info("m", "ids", map[string]int64{"big": 9007199254740993})
	if !strings.Contains(buf.String(), "9007199254740993") {
		t.Fatalf("int64 above 2^53 was rounded:\n%s", buf.String())
	}
}
