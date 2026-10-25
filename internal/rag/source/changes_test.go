package source

import (
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestHealthReachable(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if !(Health{Availability: Available, CheckedAt: now}).Reachable() {
		t.Error("a checked, available health is reachable")
	}
	cases := map[string]Health{
		"zero":                        {},
		"unavailable":                 {Availability: Unavailable, Err: contracts.New(contracts.ErrUnavailable, "x")},
		"unavailable but time set":    {Availability: Unavailable, CheckedAt: now, Err: contracts.New(contracts.ErrUnavailable, "x")},
		"available but never checked": {Availability: Available},
		"available with an error":     {Availability: Available, CheckedAt: now, Err: contracts.New(contracts.ErrUnavailable, "x")},
		"removed":                     {Availability: conversation.CitationAvailabilityRemoved, CheckedAt: now},
	}
	for name, h := range cases {
		if h.Reachable() {
			t.Errorf("%s: reported reachable", name)
		}
	}
}

func TestTombstonesForRemoval(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	prev := []Record{
		{Key: "b.md", Size: 2, Hash: "sha256:bb", MIME: MIMEMarkdown},
		{Key: "a.md", Size: 1, Hash: "sha256:aa", MIME: MIMEMarkdown},
	}
	purge, err := FromDeclaration(baseDecl(), baseBinding())
	if err != nil {
		t.Fatal(err)
	}
	purge.Deletion = Deletion{PurgeAfter: 48 * time.Hour, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedPurge}
	got := TombstonesForRemoval(purge, prev, now)
	if len(got) != 2 || got[0].Key != "a.md" || got[1].Key != "b.md" {
		t.Fatalf("tombstones = %+v", got)
	}
	for _, ts := range got {
		if ts.Reason != ReasonSourceRemoved || !ts.At.Equal(now) || !ts.PurgeAfter.Equal(now.Add(48*time.Hour)) {
			t.Errorf("purge tombstone = %+v", ts)
		}
	}
	if got[0].Last.Hash != "sha256:aa" || got[0].Last.Size != 1 {
		t.Errorf("a tombstone keeps the last version: %+v", got[0].Last)
	}

	disable := purge
	disable.Deletion.OnSourceRemoved = ap.RAGSourceDeletionOnSourceRemovedDisable
	for _, ts := range TombstonesForRemoval(disable, prev, now) {
		if !ts.PurgeAfter.IsZero() {
			t.Errorf("a disabled source is never purged automatically: %+v", ts)
		}
	}
	if len(TombstonesForRemoval(purge, nil, now)) != 0 {
		t.Error("no records, no tombstones")
	}

	zero := purge
	zero.Deletion.PurgeAfter = 0
	if ts := TombstonesForRemoval(zero, prev, now); !ts[0].PurgeAfter.Equal(now) {
		t.Errorf("a zero purge window purges immediately: %+v", ts[0])
	}
}

func TestRecordRoundTrip(t *testing.T) {
	seen := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	v := Version{Hash: "sha256:aa", Size: 3, ModTime: seen.Add(-time.Hour), MIME: MIMEText}
	r := RecordOf("a.txt", v, seen)
	if r.Version() != v || r.Key != "a.txt" || !r.SeenAt.Equal(seen) {
		t.Errorf("record = %+v", r)
	}
	if (ChangeSet{}).Changed() {
		t.Error("an empty change set changes nothing")
	}
	if !(ChangeSet{Deleted: []Tombstone{{}}}).Changed() {
		t.Error("a tombstone is a change")
	}
}
