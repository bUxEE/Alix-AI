package secretssec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/store"
)

func mkdir(p string) error { return os.MkdirAll(p, 0o700) }

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v)
}

var httpClient = &http.Client{Timeout: probeTimeout}

// call sends one request to the stack and returns the status code.
func (s *stack) call(method, path string, headers map[string]string, body string) int {
	s.t.Helper()
	req, err := http.NewRequest(method, s.base+path, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// SEC-082: an end-to-end run seeded with canary secrets leaves no canary in any log line, panic
// message or log file. The stack is the real one (store, audit, policy, actions, identity, the
// gateway on a loopback port), every logger is internal/log's, and the
// canaries (random per run) go down every path that touches a secret: context tokens in
// Authorization (valid, replayed, expired, garbage), a cookie, a password-like and two
// operator-configured fields in request bodies, nested and listed values, an API key resolved from a
// secret reference (env: and file:) and logged, a credential echoed by the application's
// adapter, an audit event written with sensitive fields, and a recovered panic.
//
// The scan reads the process's log sink, the log file and the audit events. Controls: the same canaries
// written without the redaction layer ARE found by the same scanner (so it can see a leak), the paths that
// log request data do emit their canaries in the unredacted copy of the logs (so redaction is what removed
// them), and the scanner fails when it reads nothing.
func TestSecLogInspectionNoCanary(t *testing.T) {
	c := canaries{}
	plant := func(family string) string {
		v := newCanary(t)
		c[family] = v
		return v
	}
	ctx := context.Background()

	// Keys that come from secret references, as in production: one from the environment, one from a file.
	refEnv, refFile := plant("secret reference value (env:)"), plant("secret reference value (file:)")
	t.Setenv("ALIXAI_SEC_CANARY_KEY", refEnv)
	refPath := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(refPath, []byte(refFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newStack(t)
	p, err := s.co.Prepare(ctx, scopeOfAlice(), actions.PrepareInput{Session: scopeOfAlice(), CapabilityID: capCreate, Fields: map[string]any{"name": "widget"}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	confirmation := p.Confirmation.Reveal()
	c["confirmation token"] = confirmation
	prep := string(p.Action.PreparationID)

	// 1. Context tokens in Authorization, through the identity middleware and the real route.
	good := s.token()
	c["context token (accepted)"] = good
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	cookie := plant("cookie")
	with := func(h map[string]string) map[string]string {
		h["Cookie"] = "sid=" + cookie + "; theme=dark"
		return h
	}
	statuses := map[string]int{}
	statuses["accepted token"] = s.call("GET", "/v1/canary/action/"+prep, with(bearer(good)), "")
	statuses["replayed token"] = s.call("GET", "/v1/canary/action/"+prep, with(bearer(good)), "")
	expired := s.issuer.Draft().Times(-30*time.Minute, -30*time.Minute, -20*time.Minute).Sign()
	c["context token (expired)"] = expired
	statuses["expired token"] = s.call("GET", "/v1/canary/action/"+prep, with(bearer(expired)), "")
	wrongAud := s.token(func(d *identitytest.Draft) { d.Set(identity.ClaimAudience, "dep_other") })
	c["context token (wrong audience)"] = wrongAud
	statuses["wrong audience"] = s.call("GET", "/v1/canary/action/"+prep, with(bearer(wrongAud)), "")
	garbage := plant("garbage bearer value")
	statuses["garbage bearer"] = s.call("GET", "/v1/canary/action/"+prep, with(bearer(garbage)), "")
	statuses["basic credentials"] = s.call("GET", "/v1/canary/action/"+prep, map[string]string{"Authorization": "Basic " + plant("basic credentials")}, "")

	// 2. Request bodies: a password-like and two configured fields, to the real strict endpoint.
	password, ssn, note := plant("password field"), plant("configured field ssn"), plant("configured field patient_note")
	body := fmt.Sprintf(`{"token":%q,"password":%q,"ssn":%q,"patient_note":%q}`, confirmation, password, ssn, note)
	statuses["body with sensitive fields"] = s.call("POST", "/v1/canary/action/"+prep+"/confirm", with(bearer(s.token())), body)

	// 3. The application's adapter answers the commit with a credential, which the real confirm route carries.
	adapterBearer, adapterKey := plant("adapter error: bearer"), plant("adapter error: api_key")
	s.host.setEcho("Authorization: Bearer " + adapterBearer + " api_key=" + adapterKey)
	statuses["confirm, host echoes a credential"] = s.call("POST", "/v1/canary/action/"+prep+"/confirm", with(bearer(s.token())),
		fmt.Sprintf(`{"token":%q}`, confirmation))

	// 4. Application code that logs what it receives (the route the product does not have).
	echoBearer, nested, listed := plant("echo route: Authorization header"), plant("nested api_key"), plant("listed cookie")
	textBearer, textPassword, urlPassword := plant("bearer inside free text"), plant("password= inside free text"), plant("password inside a URL")
	marker := "visible-marker-" + newCanary(t) // not a secret: it must still be logged
	echoBody, _ := json.Marshal(map[string]any{
		"password":     password,
		"ssn":          ssn,
		"patient_note": note,
		"profile":      map[string]any{"credentials": map[string]any{"api_key": nested}, "name": marker},
		"items":        []any{map[string]any{"cookie": listed}, map[string]any{"n": 1}},
		"text":         "retry with Authorization: Bearer " + textBearer,
		"free":         "connecting with password=" + textPassword,
		"dsn":          "postgres://svc:" + urlPassword + "@db.internal:5432/app",
	})
	statuses["canary echo route"] = s.call("POST", "/canary/echo", map[string]string{
		"Authorization": "Bearer " + echoBearer, "Cookie": "sid=" + cookie,
	}, string(echoBody))

	// 5. A recovered panic whose message carries credentials.
	panicBearer, panicPassword := plant("panic message: bearer"), plant("panic message: password=")
	msg := "upstream said Authorization: Bearer " + panicBearer + " and password=" + panicPassword
	statuses["handler panic"] = s.call("GET", "/canary/panic?m="+url.QueryEscape(msg), nil, "")

	// 6. Secret references resolved to their values, which a careless caller then logs (the adapter's own
	// Secret type, which keeps its key out of every rendering, is covered by the adapter's tests).
	resolve := func(ref string) string {
		switch {
		case strings.HasPrefix(ref, "env:"):
			return os.Getenv(strings.TrimPrefix(ref, "env:"))
		case strings.HasPrefix(ref, "file:"):
			b, err := os.ReadFile(strings.TrimPrefix(ref, "file:"))
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(b))
		}
		t.Fatalf("not a reference: %q", ref)
		return ""
	}
	for _, ref := range []string{"env:ALIXAI_SEC_CANARY_KEY", "file:" + refPath} {
		v := resolve(ref)
		if v == "" {
			t.Fatalf("the reference %q resolved to nothing", ref)
		}
		s.log.Info("secret reference resolved", "ref", ref, "api_key", v, "secret_value", v, "detail", "resolved "+ref+" with api_key="+v)
	}
	// A key pasted where a reference belongs, in the error that refuses it.
	pasted := plant("key pasted instead of a reference")
	s.log.Warn("secret reference refused", "err", fmt.Errorf("the reference must start with env: or file: (got api_key=%s)", pasted))

	// 7. An audit event written with sensitive fields.
	auditPassword, auditSSN := plant("audit event: password"), plant("audit event: configured ssn")
	if _, err := s.audit.Append(ctx, scopeOfAlice(), audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{
		"n": 1, "password": auditPassword, "ssn": auditSSN, "changed": "profile",
	}}); err != nil {
		t.Fatalf("audit append: %v", err)
	}

	// --- the exercise checks: the run went down the paths it claims ---
	if got := statuses["accepted token"]; got == 0 || got >= 500 {
		t.Errorf("accepted token: status %d", got)
	}
	for _, name := range []string{"replayed token", "expired token", "wrong audience", "garbage bearer", "basic credentials"} {
		if statuses[name] != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401: the identity path was not exercised", name, statuses[name])
		}
	}
	if statuses["body with sensitive fields"] != http.StatusBadRequest {
		t.Errorf("body with sensitive fields: status %d, want 400", statuses["body with sensitive fields"])
	}
	if statuses["canary echo route"] != http.StatusNoContent || statuses["handler panic"] != http.StatusInternalServerError {
		t.Errorf("echo route %d, panic %d", statuses["canary echo route"], statuses["handler panic"])
	}
	if s.host.calls == 0 {
		t.Error("the application's Commit was never called: the adapter-error path was not exercised")
	}
	prod := s.logs.prod.String()
	for _, want := range []string{
		"context token accepted", "context token refused", "handler panic", "canary route received a request",
		"secret reference resolved", "secret reference refused", marker,
	} {
		if !strings.Contains(prod, want) {
			t.Errorf("the production log lacks %q: the run did not reach that path\n%s", want, tail(prod))
		}
	}

	// --- the control: redaction is what kept these out, so the unredacted copy of the logs holds them ---
	mustReachRaw := []string{
		"password field", "configured field ssn", "configured field patient_note", "nested api_key", "listed cookie",
		"bearer inside free text", "password= inside free text", "password inside a URL", "cookie",
		"echo route: Authorization header", "panic message: bearer", "panic message: password=",
		"secret reference value (env:)", "secret reference value (file:)", "key pasted instead of a reference",
	}
	rawLeaks, err := scanSources(c, s.logs.raw.source("unredacted logs"))
	if err != nil {
		t.Fatal(err)
	}
	reached := map[string]bool{}
	for _, l := range rawLeaks {
		reached[l.Family] = true
	}
	for _, f := range mustReachRaw {
		if !reached[f] {
			t.Errorf("the canary %q is not in the unredacted copy of the logs: the path that should carry it did not, so the test proves nothing about it", f)
		}
	}

	// --- the scan of what an operator keeps: the log sink, the log file, the audit events ---
	s.stop() // flush and close the store before its audit events are read
	auditText := readAuditEvents(t, s)
	srcs := []source{s.logs.prod.source("log sink"), {name: "audit events", data: []byte(auditText)}}
	files, err := readLogFiles(s.logs.dir)
	if err != nil {
		t.Fatal(err)
	}
	srcs = append(srcs, files...)
	leaks, err := scanSources(c, srcs...)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leaks {
		t.Errorf("LEAK: %s", l)
	}
	if len(files) == 0 || len(files[0].data) == 0 {
		t.Error("the log file is empty: the file sink was not exercised")
	}
	// The audit events were read: the sensitive fields are in them as the marker, the harmless one as itself.
	if !strings.Contains(auditText, "profile") || !strings.Contains(auditText, alog.Redacted) {
		t.Errorf("the audit events do not hold the redacted event that was written: %s", tail(auditText))
	}

	// --- the control: a deliberate unredacted write of every canary is found by the same scan ---
	t.Run("control: an unredacted write of the canaries is found", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "planted")
		if err := mkdir(dir); err != nil {
			t.Fatal(err)
		}
		planted := newSink(t, filepath.Join(dir, "alixai.log"))
		raw := slog.New(slog.NewJSONHandler(planted, &slog.HandlerOptions{Level: slog.LevelDebug}))
		families := make([]string, 0, len(c))
		for f := range c {
			families = append(families, f)
		}
		sort.Strings(families)
		for _, f := range families {
			raw.Info("deliberately unredacted", "detail", c[f])
		}
		inSink, err := scanSources(c, planted.source("planted sink"))
		if err != nil {
			t.Fatal(err)
		}
		inFile, err := scanLogPath(c, dir)
		if err != nil {
			t.Fatal(err)
		}
		for name, found := range map[string][]leak{"sink": inSink, "file": inFile} {
			seen := map[string]bool{}
			for _, l := range found {
				if !l.Piece {
					seen[l.Family] = true
				}
			}
			for _, f := range families {
				if !seen[f] {
					t.Errorf("the planted canary %q was not found in the %s", f, name)
				}
			}
		}
	})

	t.Run("the production redaction helper, on every sensitive key and nested value", func(t *testing.T) { redactionHelper(t) })
}

// tail is the end of a log, for failure messages.
func tail(s string) string {
	if len(s) > 1500 {
		return "..." + s[len(s)-1500:]
	}
	return s
}

// readAuditEvents is the audit events of the stack as text, read from its store, which is reopened
// because the stack's own handle is closed.
func readAuditEvents(t *testing.T, s *stack) string {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(s.dataDir, store.DefaultFileName), Migrations: store.NewRegistry()})
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `SELECT event_type, data FROM audit_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, data string
		if err := rows.Scan(&typ, &data); err != nil {
			t.Fatal(err)
		}
		b.WriteString(typ + " " + data + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// redactionHelper runs the production redaction layer on log keys that contain each of the
// redactor's substrings, in the spellings logs use, and on values nested in maps, slices, groups,
// structs and errors; and on the keys an operator configured. The value under a sensitive key never
// appears; the control is the same value under a key that is not sensitive, which does.
func redactionHelper(t *testing.T) {
	type creds struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Nested   struct {
			Cookie string `json:"cookie"`
		} `json:"nested"`
	}
	sensitive := []string{
		"token", "access_token", "accessToken", "refresh-token", "id_token", "csrf_token_value",
		"secret", "client_secret", "SECRET", "secretKey", "shared.secret",
		"password", "Password", "db_password", "passwd", "new-password",
		"authorization", "Authorization", "proxy-authorization", "authorization_header",
		"apikey", "api_key", "API-KEY", "X-Api-Key", "apiKey", "openai_api_key",
		"cookie", "Cookie", "Set-Cookie", "session_cookie",
		"privatekey", "private_key", "PrivateKey", "tls.private-key",
		"credential", "credentials", "db_credentials", "Credential",
		redactKeyA, redactKeyB, "SSN", "Patient-Note",
	}
	for _, key := range sensitive {
		for form, mk := range map[string]func(v string) any{
			"string":         func(v string) any { return v },
			"map":            func(v string) any { return map[string]any{"a": v, "b": []any{v}} },
			"slice":          func(v string) any { return []string{v, v} },
			"struct":         func(v string) any { c := creds{User: "u", Password: v}; c.Nested.Cookie = v; return c },
			"error":          func(v string) any { return fmt.Errorf("failed: %s", v) },
			"pointer-to-map": func(v string) any { return &map[string]any{"deep": map[string]any{"deeper": v}} },
		} {
			v := newCanary(t)
			var buf bytes.Buffer
			l := alog.NewWithWriter(logCfg(), &buf)
			l.Info("redaction probe", key, mk(v))
			l.Info("redaction probe in a group", slog.Group(key, slog.String("inner", v), slog.Any("map", map[string]any{"k": v})))
			l.With(key, v).Info("redaction probe via With")
			if got, err := scanSources(canaries{key + "/" + form: v}, source{name: "log", data: buf.Bytes()}); err != nil || len(got) != 0 {
				t.Errorf("key %q (%s): the value reached the log: %v %v\n%s", key, form, got, err, buf.String())
			}
		}
	}
	// A sensitive key nested in a value under a key that is not sensitive.
	v := newCanary(t)
	var buf bytes.Buffer
	alog.NewWithWriter(logCfg(), &buf).Info("nested", "request", map[string]any{
		"headers": map[string]any{"Authorization": "Bearer " + v, "X-Request-Id": "r1"},
		"body":    []any{map[string]any{"user": map[string]any{"password": v}}},
		"ssn":     v,
	})
	if got, err := scanSources(canaries{"nested": v}, source{name: "log", data: buf.Bytes()}); err != nil || len(got) != 0 {
		t.Errorf("nested sensitive keys leaked: %v %v\n%s", got, err, buf.String())
	}
	// Control: a value under a key that is not sensitive is the operator's to log and is kept, so the scan above
	// can see a value when the redaction does not apply.
	plain := newCanary(t)
	buf.Reset()
	alog.NewWithWriter(logCfg(), &buf).Info("probe", "request_id", plain, "profile", map[string]any{"display_name": plain})
	got, err := scanSources(canaries{"plain": plain}, source{name: "log", data: buf.Bytes()})
	if err != nil || len(got) == 0 || strings.Count(buf.String(), plain) != 2 {
		t.Errorf("the value under non-sensitive keys was not found in the log (%v, %v): the scanner or the probe is blind\n%s", got, err, buf.String())
	}
	// Sensitive keys are matched as substrings, so a key that merely contains one is redacted too.
	for _, key := range []string{"mytokenvalue", "xpasswordx", "no_secret_here", "cookiejar", "credentialsfile"} {
		v := newCanary(t)
		buf.Reset()
		alog.NewWithWriter(logCfg(), &buf).Info("probe", key, v)
		if got, _ := scanSources(canaries{key: v}, source{name: "log", data: buf.Bytes()}); len(got) != 0 {
			t.Errorf("key %q contains a sensitive word and its value was logged: %v", key, got)
		}
	}
}
