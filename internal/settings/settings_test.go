package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// storedRows counts the stored documents.
func (f *fixture) storedRows() int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), "SELECT count(*) FROM settings_documents").Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// storedDoc returns the stored (schema_id, version, document) of every row, for
// asserting that a refused request changed nothing.
func (f *fixture) storedDump() string {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		"SELECT application_id, tenant_id, workspace_id, level, subject_id, schema_id, version, document FROM settings_documents ORDER BY seq")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var a, t, w, l, s, sc, d string
		var v int64
		if err := rows.Scan(&a, &t, &w, &l, &s, &sc, &v, &d); err != nil {
			f.t.Fatal(err)
		}
		b.WriteString(strings.Join([]string{a, t, w, l, s, sc, d}, "|"))
		b.WriteString(fmt.Sprintf(" v%d", v))
		b.WriteString("\n")
	}
	return b.String()
}

// TestSettingsTypedAndVersioned: a document is an instance of a published schema
// (typed), stored in canonical form with the schema it was validated against, and
// its revision rises by exactly one per change (versioned).
func TestSettingsTypedAndVersioned(t *testing.T) {
	f := newFixture(t)
	sc := ada()

	t.Run("the schema ids are the published ones", func(t *testing.T) {
		for level, want := range map[Level]string{
			LevelUser:       "urn:alixai:schema:settings:user-settings:v1",
			LevelTenant:     "urn:alixai:schema:settings:tenant-settings:v1",
			LevelDeployment: "urn:alixai:schema:settings:deployment-settings:v1",
		} {
			if got := level.SchemaID(); got != want {
				t.Errorf("%s schema = %q, want %q", level, got, want)
			}
		}
		if (Level("planet")).SchemaID() != "" || Level("planet").Valid() {
			t.Error("an unknown level has a schema")
		}
	})

	t.Run("a level with no document is version 0 and empty", func(t *testing.T) {
		for _, level := range Levels() {
			rec := f.get(sc, level)
			if rec.Version != 0 || string(rec.Settings) != "{}" || rec.Schema != level.SchemaID() || rec.UpdatedAt != nil || rec.UpdatedBy != "" {
				t.Errorf("%s: %+v", level, rec)
			}
		}
	})

	t.Run("the first write is version 1 and the document is canonical", func(t *testing.T) {
		// Members in the wrong order, lists unsorted: the stored form is the schema's.
		rec := f.put(sc, LevelUser, 0, `{
			"capabilities": {"disabled": ["items.get", "items.create"]},
			"notifications": {"events": ["source_indexed", "action_ready"], "enabled": true},
			"language": "it",
			"voice": {"speech_rate_pct": 120, "enabled": true}
		}`)
		want := `{"language":"it","voice":{"enabled":true,"speech_rate_pct":120},"notifications":{"enabled":true,"events":["action_ready","source_indexed"]},"capabilities":{"disabled":["items.create","items.get"]}}`
		if string(rec.Settings) != want {
			t.Errorf("stored document\n got %s\nwant %s", rec.Settings, want)
		}
		if rec.Version != 1 || rec.Level != LevelUser || rec.Schema != LevelUser.SchemaID() || rec.UpdatedBy != tUser1 || rec.UpdatedAt == nil || !rec.UpdatedAt.Equal(tNow) {
			t.Errorf("record = %+v", rec)
		}
		// What is read back is what was returned.
		if diff := cmp.Diff(rec, f.get(sc, LevelUser)); diff != "" {
			t.Errorf("Get differs from Put (-put +get):\n%s", diff)
		}
		// And it is an instance of the generated type of the schema.
		var typed gen.UserSettings
		if err := json.Unmarshal(rec.Settings, &typed); err != nil {
			t.Fatal(err)
		}
		if typed.Language == nil || *typed.Language != "it" || typed.Voice == nil || *typed.Voice.SpeechRatePct != 120 {
			t.Errorf("typed document = %+v", typed)
		}
	})

	t.Run("the revision rises by exactly one per change", func(t *testing.T) {
		f.clk.Advance(1e9)
		r2 := f.put(sc, LevelUser, 1, `{"language":"en"}`)
		f.clk.Advance(1e9)
		r3 := f.put(sc, LevelUser, 2, `{"language":"it","streaming":{"unit":"sentence"}}`)
		if r2.Version != 2 || r3.Version != 3 {
			t.Fatalf("versions = %d, %d, want 2, 3", r2.Version, r3.Version)
		}
		if !r3.UpdatedAt.After(*r2.UpdatedAt) {
			t.Errorf("updated_at did not move: %v then %v", r2.UpdatedAt, r3.UpdatedAt)
		}
		// A document replaces the previous one whole: language en is gone.
		if got := string(f.get(sc, LevelUser).Settings); got != `{"language":"it","streaming":{"unit":"sentence"}}` {
			t.Errorf("stored = %s", got)
		}
	})

	t.Run("writing the same document again changes nothing", func(t *testing.T) {
		before := len(f.settingsEvents())
		again := f.put(sc, LevelUser, 3, `{"streaming":{"unit":"sentence"},"language":"it"}`) // same content, other member order
		if again.Version != 3 {
			t.Errorf("an identical write moved the version to %d", again.Version)
		}
		if got := len(f.settingsEvents()); got != before {
			t.Errorf("an identical write recorded %d audit events", got-before)
		}
	})

	t.Run("the row records the schema and the revision", func(t *testing.T) {
		var schemaID string
		var version int64
		if err := f.db.QueryRowContext(context.Background(),
			"SELECT schema_id, version FROM settings_documents WHERE level = 'user' AND subject_id = ?", tUser1).Scan(&schemaID, &version); err != nil {
			t.Fatal(err)
		}
		if schemaID != LevelUser.SchemaID() || version != 3 {
			t.Errorf("row = %s v%d", schemaID, version)
		}
	})

	t.Run("each level has its own document and its own revision", func(t *testing.T) {
		ten := f.put(sc, LevelTenant, 0, `{"default_language":"en","history":{"retention_days":30}}`)
		dep := f.put(sc, LevelDeployment, 0, `{"voice":{"allowed":true}}`)
		if ten.Version != 1 || dep.Version != 1 || ten.Schema != LevelTenant.SchemaID() || dep.Schema != LevelDeployment.SchemaID() {
			t.Errorf("tenant %+v, deployment %+v", ten, dep)
		}
		if f.get(sc, LevelUser).Version != 3 {
			t.Error("writing the tenant and deployment documents moved the user document")
		}
	})

	t.Run("a value of the wrong type is refused and nothing moves", func(t *testing.T) {
		dump := f.storedDump()
		events := len(f.settingsEvents())
		for name, tc := range map[string]struct {
			level Level
			doc   string
			ver   int64
		}{
			"string for an integer":     {LevelUser, `{"voice":{"speech_rate_pct":"120"}}`, 3},
			"float for an integer":      {LevelUser, `{"voice":{"speech_rate_pct":120.5}}`, 3},
			"integer for a boolean":     {LevelUser, `{"voice":{"enabled":1}}`, 3},
			"null for a member":         {LevelUser, `{"language":null}`, 3},
			"unknown enum":              {LevelUser, `{"streaming":{"unit":"paragraph"}}`, 3},
			"out of range":              {LevelUser, `{"accessibility":{"text_scale_pct":900}}`, 3},
			"list for a section":        {LevelUser, `{"voice":["enabled"]}`, 3},
			"tenant retention zero":     {LevelTenant, `{"history":{"retention_days":0}}`, 1},
			"deployment size as string": {LevelDeployment, `{"attachments":{"max_file_mb":"5"}}`, 1},
			"a user member in a tenant": {LevelTenant, `{"voice":{"enabled":true}}`, 1},
			"language not canonical":    {LevelUser, `{"language":"IT"}`, 3},
		} {
			err := f.tryPut(sc, tc.level, tc.ver, tc.doc)
			if err == nil {
				t.Errorf("%s: accepted", name)
				continue
			}
			if code(err) != contracts.ErrValidationFailed {
				t.Errorf("%s: code %s, want validation_failed (%v)", name, code(err), err)
			}
		}
		if got := f.storedDump(); got != dump {
			t.Errorf("refused documents changed the store:\n%s\nbefore:\n%s", got, dump)
		}
		if got := len(f.settingsEvents()); got != events {
			t.Errorf("refused documents recorded %d audit events", got-events)
		}
	})

	t.Run("a stored document of a schema this version does not know fails closed", func(t *testing.T) {
		if _, err := f.db.ExecContext(context.Background(),
			"UPDATE settings_documents SET schema_id = 'urn:alixai:schema:settings:user-settings:v2' WHERE level = 'user'"); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.Get(context.Background(), sc, LevelUser)
		wantCode(t, err, contracts.ErrInternal)
		_, err = f.svc.Effective(context.Background(), sc)
		wantCode(t, err, contracts.ErrInternal)
	})
}

// TestSettingsOptimisticConcurrency: a write succeeds only against the version the
// caller read; the loser gets conflict, the stored document is the winner's and
// nothing of the loser is recorded.
func TestSettingsOptimisticConcurrency(t *testing.T) {
	f := newFixture(t)
	sc := ada()

	t.Run("creating over an existing document, or updating a missing one, conflicts", func(t *testing.T) {
		err := f.tryPut(sc, LevelUser, 1, `{"language":"en"}`) // nothing exists: only 0 creates
		wantCode(t, err, contracts.ErrConflict)
		var ce *contracts.Error
		if asErr(err, &ce); ce.Details["current_version"] != int64(0) {
			t.Errorf("details = %v", ce.Details)
		}
		if f.storedRows() != 0 || len(f.settingsEvents()) != 0 {
			t.Fatal("a conflicting create wrote something")
		}
		f.put(sc, LevelUser, 0, `{"language":"en"}`)
		wantCode(t, f.tryPut(sc, LevelUser, 0, `{"language":"it"}`), contracts.ErrConflict) // already exists
	})

	t.Run("two writers read the same version: the first wins, the second conflicts", func(t *testing.T) {
		readA, readB := f.get(sc, LevelUser), f.get(sc, LevelUser)
		if readA.Version != readB.Version {
			t.Fatal("setup")
		}
		first := f.put(sc, LevelUser, readA.Version, `{"language":"it"}`)
		err := f.tryPut(sc, LevelUser, readB.Version, `{"language":"en","streaming":{"enabled":false}}`)
		wantCode(t, err, contracts.ErrConflict)
		var ce *contracts.Error
		asErr(err, &ce)
		if ce.Details["current_version"] != first.Version {
			t.Errorf("current_version = %v, want %d", ce.Details["current_version"], first.Version)
		}
		if got := f.get(sc, LevelUser); string(got.Settings) != `{"language":"it"}` || got.Version != first.Version {
			t.Errorf("stored = %+v: the loser changed something", got)
		}
		if strings.Contains(err.Error(), "streaming") || strings.Contains(err.Error(), `"it"`) {
			t.Errorf("the conflict repeats a document: %v", err)
		}
		// Re-reading and retrying works.
		retry := f.put(sc, LevelUser, first.Version, `{"language":"en","streaming":{"enabled":false}}`)
		if retry.Version != first.Version+1 {
			t.Errorf("retry version = %d", retry.Version)
		}
	})

	t.Run("a refused conflict records no audit event", func(t *testing.T) {
		before := len(f.settingsEvents())
		wantCode(t, f.tryPut(sc, LevelUser, 99, `{"language":"it"}`), contracts.ErrConflict)
		if got := len(f.settingsEvents()); got != before {
			t.Errorf("a conflict recorded %d events", got-before)
		}
	})

	t.Run("a negative expected version is not a version", func(t *testing.T) {
		wantCode(t, f.tryPut(sc, LevelUser, -1, `{"language":"it"}`), contracts.ErrValidationFailed)
	})

	t.Run("many writers race on one version: exactly one wins", func(t *testing.T) {
		g := newFixture(t)
		base := g.put(ada(), LevelTenant, 0, `{"default_language":"en"}`)
		var won, conflicted, other atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				doc := `{"default_language":"it","history":{"retention_days":` + string(rune('1'+i%9)) + `}}`
				_, err := g.svc.Put(context.Background(), ada(), LevelTenant, PutRequest{ExpectedVersion: base.Version, Settings: json.RawMessage(doc)})
				switch code(err) {
				case "":
					won.Add(1)
				case contracts.ErrConflict:
					conflicted.Add(1)
				default:
					other.Add(1)
					t.Errorf("writer %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		if won.Load() != 1 || conflicted.Load() != 11 || other.Load() != 0 {
			t.Fatalf("won %d, conflicted %d, other %d", won.Load(), conflicted.Load(), other.Load())
		}
		if got := g.get(ada(), LevelTenant); got.Version != base.Version+1 {
			t.Errorf("version = %d, want %d", got.Version, base.Version+1)
		}
		if got := len(g.settingsEvents()); got != 2 { // the base write and the one winner
			t.Errorf("%d audit events, want 2", got)
		}
	})

	t.Run("a write that can not be recorded does not happen", func(t *testing.T) {
		g := newFixture(t)
		g.rec.fail = contracts.New(contracts.ErrUnavailable, "the audit log is down")
		err := g.tryPut(ada(), LevelUser, 0, `{"language":"it"}`)
		wantCode(t, err, contracts.ErrUnavailable)
		if g.storedRows() != 0 {
			t.Fatal("the document was stored although its audit event could not be appended")
		}
		g.rec.fail = nil
		if rec := g.put(ada(), LevelUser, 0, `{"language":"it"}`); rec.Version != 1 {
			t.Errorf("version after recovery = %d", rec.Version)
		}
	})
}
