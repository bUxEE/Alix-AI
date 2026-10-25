package audit

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-083: secrets never reach the table, an export or a checkpoint. The row
// is read straight from the database, because the read API is not the only way
// out.
func TestRedactionAtWrite(t *testing.T) {
	// Fixtures are made up on the spot, never real credentials.
	const (
		apiKey   = "live-key-4f9d2c7a1b"
		password = "correct-horse-battery"
		bearer   = "abc123def456ghi789"
		secret   = "s3cr3t-value-xyz"
		custom   = "ssn-123-45-6789"
		pemBody  = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"
		urlPass  = "pa55w0rd-in-url"
		jwtTail  = "c2lnbmF0dXJlLXBhcnQ"
	)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ." + jwtTail
	h := newHarness(t, func(o *Options) { o.RedactKeys = []string{"ssn_number"} })
	s := scope("app-a", "tenant-1", "ws-1", CapabilityExport)

	ev, err := h.log.Append(ctx, s, NewEvent{Type: EventSettingsChanged, Data: map[string]any{
		"api_key":     apiKey,
		"Password":    password,
		"setting":     "greeting",
		"visible":     "plain value",
		"count":       3,
		"nested":      map[string]any{"client_secret": secret, "keep": "kept", "deeper": []any{map[string]any{"access_token": "tok-" + secret}}},
		"note":        "retry with Authorization: Bearer " + bearer + " please",
		"link":        "https://svc:" + urlPass + "@host.example/path",
		"jwt":         jwt,
		"key_text":    "-----BEGIN PRIVATE KEY-----\n" + pemBody + "\n-----END PRIVATE KEY-----",
		"query":       "https://host.example/cb?token=" + bearer + "&page=2",
		"ssn_number":  custom,
		"cookie":      "session=" + secret,
		"credentials": map[string]any{"user": "u", "pass": "p"},
	}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	forbidden := []string{apiKey, password, bearer, secret, custom, pemBody, urlPass, jwtTail}
	check := func(where, text string) {
		t.Helper()
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s contains %q:\n%s", where, f, text)
			}
		}
		if !strings.Contains(text, Redacted) {
			t.Errorf("%s shows no redaction marker:\n%s", where, text)
		}
		for _, keep := range []string{"plain value", "greeting", "kept", `"count":3`} {
			if !strings.Contains(text, keep) {
				t.Errorf("%s lost the harmless value %q:\n%s", where, keep, text)
			}
		}
	}

	check("the returned event", string(ev.Data))

	var raw string
	if err := h.db.QueryRowContext(ctx, `SELECT data FROM audit_events WHERE id = ?`, ev.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	check("the stored row", raw)
	if raw != string(ev.Data) {
		t.Errorf("stored data differs from the returned event:\n%s\n%s", raw, ev.Data)
	}

	_, exported, err := h.exportLines(s, ExportOptions{Types: []EventType{EventSettingsChanged}})
	if err != nil {
		t.Fatal(err)
	}
	check("the export", exported)

	// What is stored is what was hashed: the chain verifies over the redacted form.
	if err := ev.HashValid(); err != nil {
		t.Errorf("hash of redacted event: %v", err)
	}
	h.mustVerify(key1)

	// A secret smuggled into a checkpoint-bearing path is not possible by design
	// (checkpoints hold a head hash only); the whole table is clean too.
	rows, err := h.db.QueryContext(ctx, `SELECT data FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		for _, f := range forbidden {
			if strings.Contains(d, f) {
				t.Errorf("a stored row contains %q: %s", f, d)
			}
		}
	}
}

// A refused payload is refused without echoing its values.
func TestRefusedPayloadDoesNotEchoSecrets(t *testing.T) {
	h := newHarness(t)
	_, err := h.log.Append(ctx, s1, NewEvent{Type: EventSettingsChanged, Data: map[string]any{
		"password": "do-not-echo-me", "ratio": 1.25,
	}})
	if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "do-not-echo-me") {
		t.Errorf("the error repeats a secret value: %v", err)
	}
	if n := h.count(`SELECT COUNT(*) FROM audit_events`); n != 0 {
		t.Errorf("%d events stored", n)
	}
}
