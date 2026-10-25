package httpapi

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxConsecutiveFailures is how many items in a row may fail with the API being
// unavailable or timing out before the scan gives up: the API is down, and
// going on would only repeat the failure for every remaining item.
const maxConsecutiveFailures = 16

// scan is the state of one Discover or DetectDeletions call.
type scan struct {
	s    *session
	ctx  context.Context
	now  time.Time
	prev map[string]source.Record

	cs      source.ChangeSet
	next    map[string]source.Record // live records after this scan
	skipped map[string]source.Reason // listed items found and not accepted
	files   int64
	bytes   int64
	failRun int
}

func (p *Provider) newScan(ctx context.Context, s *session, prev []source.Record) *scan {
	sc := &scan{
		s: s, ctx: ctx, now: p.clk.Now(),
		prev:    make(map[string]source.Record, len(prev)),
		next:    make(map[string]source.Record, len(prev)),
		skipped: map[string]source.Reason{},
	}
	for _, r := range prev {
		sc.prev[r.Key] = r
	}
	return sc
}

// abort returns the diagnostics of a scan that did not complete: counts and
// what was skipped or failed so far, and no change that could be applied.
func (sc *scan) abort(err error) (source.ChangeSet, error) {
	diag := source.ChangeSet{Counts: sc.cs.Counts, Skipped: sc.cs.Skipped, Failed: sc.cs.Failed}
	if cerr := sc.ctx.Err(); cerr != nil {
		return diag, interrupted(cerr)
	}
	return diag, err
}

func (sc *scan) finish() source.ChangeSet {
	cs := sc.cs
	cs.Next = make([]source.Record, 0, len(sc.next))
	for _, r := range sc.next {
		cs.Next = append(cs.Next, r)
	}
	sort.Slice(cs.Next, func(i, j int) bool { return cs.Next[i].Key < cs.Next[j].Key })
	sort.Slice(cs.Added, func(i, j int) bool { return cs.Added[i].Key < cs.Added[j].Key })
	sort.Slice(cs.Modified, func(i, j int) bool { return cs.Modified[i].Key < cs.Modified[j].Key })
	sort.Slice(cs.Deleted, func(i, j int) bool { return cs.Deleted[i].Key < cs.Deleted[j].Key })
	sort.Slice(cs.Skipped, func(i, j int) bool { return cs.Skipped[i].Key < cs.Skipped[j].Key })
	sort.Slice(cs.Failed, func(i, j int) bool { return cs.Failed[i].Key < cs.Failed[j].Key })
	cs.Counts.Tombstoned = len(cs.Deleted)
	return cs
}

func (sc *scan) skip(key string, why source.Reason) {
	sc.skipped[key] = why
	sc.cs.Skipped = append(sc.cs.Skipped, source.Skip{Key: key, Reason: why})
	sc.cs.Counts.Skipped++
}

// fail records an item that could not be read. If it had a previous record the
// record is kept: a failure to read is not a deletion.
func (sc *scan) fail(key string, err error) {
	sc.cs.Failed = append(sc.cs.Failed, source.Failure{Key: key, Err: asContractsError(err, "the item cannot be read")})
	sc.cs.Counts.Failed++
	if r, ok := sc.prev[key]; ok {
		sc.next[key] = r
	}
}

func (sc *scan) tombstone(rec source.Record, why source.Reason) {
	sc.cs.Deleted = append(sc.cs.Deleted, source.TombstoneFor(sc.s.def, rec, why, sc.now))
}

// live accepts an item into the next snapshot and enforces the declared
// limits. A source that holds more than it declared is not truncated: the scan
// stops with contracts.ErrResourceExhausted and applies nothing.
func (sc *scan) live(r source.Record) error {
	sc.next[r.Key] = r
	sc.files++
	sc.bytes += r.Size
	sc.cs.Counts.Bytes = sc.bytes
	lim := sc.s.def.Limits
	if sc.files > lim.MaxFiles {
		return contracts.Newf(contracts.ErrResourceExhausted, "the source holds more than its declared %d files", lim.MaxFiles).
			WithDetails("limit", "max_files")
	}
	if sc.bytes > lim.MaxTotalBytes {
		return contracts.Newf(contracts.ErrResourceExhausted, "the source holds more than its declared %d bytes", lim.MaxTotalBytes).
			WithDetails("limit", "max_total_bytes")
	}
	return nil
}

// noteFailure counts consecutive failures that say the API is down and returns
// the error that ends the scan once there are too many.
func (sc *scan) noteFailure(err error) error {
	switch contracts.CodeOf(err) {
	case contracts.ErrUnavailable, contracts.ErrTimeout:
		sc.failRun++
		if sc.failRun >= maxConsecutiveFailures {
			return unavailable("the API stopped answering during the scan", err)
		}
	default:
		sc.failRun = 0
	}
	return nil
}

// Discover implements source.Provider.
func (p *Provider) Discover(ctx context.Context, scope contracts.Scope, def source.Definition, prev []source.Record) (source.ChangeSet, error) {
	s, err := p.begin(ctx, scope, def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	defer s.close()
	sc := p.newScan(ctx, s, prev)

	// The whole listing first, and only a complete one: an item is gone only
	// if every page was read and it is not in them.
	entries, err := s.listAll(ctx, listBound(def.Limits.MaxFiles))
	if err != nil {
		return sc.abort(err)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	for i := 0; i < len(entries); {
		j := i + 1
		for j < len(entries) && entries[j].key == entries[i].key {
			j++
		}
		dup := j-i > 1
		e := entries[i]
		i = j
		if cerr := ctx.Err(); cerr != nil {
			return sc.abort(cerr)
		}
		if err := sc.entry(e, dup); err != nil {
			return sc.abort(err)
		}
	}
	sc.tombstones()
	p.log.Debug("httpapi scan",
		"source_id", def.SourceID, "listed", len(entries), "added", len(sc.cs.Added), "modified", len(sc.cs.Modified),
		"deleted", len(sc.cs.Deleted), "unchanged", sc.cs.Counts.Unchanged, "failed", sc.cs.Counts.Failed)
	return sc.finish(), nil
}

// untouched is the cheap check that spares a fetch: the listing proves the item
// is the version the record describes if the listed version is a content
// address equal to the record's hash, or if the listed size and modification
// time are exactly the recorded ones and the record is old enough, relative to
// that time, to be trusted (a record observed within the racy window of the
// item's modification time could hide a second write in the same timestamp
// tick).
func (sc *scan) untouched(r source.Record, e listEntry) bool {
	if r.Hash == "" || r.MIME == "" {
		return false
	}
	if isContentAddress(e.version) {
		return e.version == r.Hash
	}
	if r.SeenAt.IsZero() || e.size < 0 || e.modTime.IsZero() {
		return false
	}
	return r.Size == e.size && r.ModTime.Equal(e.modTime) && r.SeenAt.Sub(e.modTime) > sc.s.p.cfg.RacyWindow
}

// entry handles one listed key.
func (sc *scan) entry(e listEntry, dup bool) error {
	s, key := sc.s, e.key
	if !s.m.Eligible(key) {
		return nil // not an item of this source; no count, no trace
	}
	sc.cs.Counts.Discovered++
	if err := s.p.paths.ValidKey(key); err != nil {
		sc.skip(key, source.ReasonUnsafeName)
		return nil
	}
	if dup {
		sc.fail(key, contracts.New(contracts.ErrSchemaInvalid, "the item is listed more than once"))
		return nil
	}
	if e.deleted {
		// A deletion marker, not an item: nothing was found.
		sc.cs.Counts.Discovered--
		return nil
	}
	if e.size > s.def.Limits.MaxFileBytes {
		sc.skip(key, source.ReasonTooLarge)
		return nil
	}

	prev, had := sc.prev[key]
	if had && sc.untouched(prev, e) {
		if !s.def.AcceptsFormat(prev.MIME) {
			sc.skip(key, source.ReasonFormat)
			return nil
		}
		sc.cs.Counts.Unchanged++
		return sc.live(prev)
	}

	res, err := s.fetchListed(sc.ctx, e)
	if res.hashed {
		sc.cs.Counts.Hashed++
	}
	if err != nil {
		if cerr := sc.ctx.Err(); cerr != nil {
			return cerr
		}
		var gone *goneError
		if errors.As(err, &gone) {
			// Listed and then not found: a race, not a verdict. The record is
			// kept; the next listing says whether the item is still there.
			err = contracts.New(contracts.ErrUnavailable, "the item disappeared between the listing and the fetch")
		}
		if fatalForScan(err) {
			return err
		}
		sc.fail(key, err)
		return sc.noteFailure(err)
	}
	sc.failRun = 0
	if res.skip != "" {
		sc.skip(key, res.skip)
		return nil
	}

	v := res.version
	rec := source.RecordOf(key, v, sc.now)
	switch {
	case !had:
		sc.cs.Added = append(sc.cs.Added, source.Item{Key: key, Version: v})
	case prev.Hash != v.Hash:
		sc.cs.Modified = append(sc.cs.Modified, source.Item{Key: key, Version: v})
	default:
		// The same bytes under a new time or size: nothing to index, but the
		// refreshed record lets the next scan take the shortcut.
		sc.cs.Counts.Unchanged++
	}
	return sc.live(rec)
}

// tombstones decides which previous items left the source. The listing was
// complete, so the question per key is only what it said: a deletion marker, an
// item the rules no longer admit, an item that was found and is no longer
// accepted, or an item that is not in it at all. A key whose fetch failed keeps
// its record (fail put it in next).
func (sc *scan) tombstones() {
	keys := make([]string, 0, len(sc.prev))
	for k := range sc.prev {
		if _, live := sc.next[k]; !live {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		rec := sc.prev[key]
		switch why, skipped := sc.skipped[key]; {
		case skipped:
			sc.tombstone(rec, why)
		case sc.s.p.paths.ValidKey(key) != nil:
			sc.tombstone(rec, source.ReasonUnsafeName)
		case !sc.s.m.Eligible(key):
			sc.tombstone(rec, source.ReasonExcluded)
		default:
			// Marked deleted, or absent from a complete listing.
			sc.tombstone(rec, source.ReasonDeleted)
		}
	}
}

// DetectDeletions implements source.Provider. It asks the API whether it answers
// (before and after) and then inspects only the previous keys, one HEAD each.
func (p *Provider) DetectDeletions(ctx context.Context, scope contracts.Scope, def source.Definition, prev []source.Record) (source.ChangeSet, error) {
	s, err := p.begin(ctx, scope, def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	defer s.close()
	sc := p.newScan(ctx, s, prev)

	// A 404 on an item is believed only if the list endpoint answers.
	if _, err := s.listPage(ctx, "", 1); err != nil {
		return sc.abort(err)
	}
	keys := make([]string, 0, len(sc.prev))
	for k := range sc.prev {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if cerr := ctx.Err(); cerr != nil {
			return sc.abort(cerr)
		}
		if err := sc.settleKey(key); err != nil {
			return sc.abort(err)
		}
	}
	// As in Discover: conclude deletions only if the API still answers.
	if _, err := s.listPage(ctx, "", 1); err != nil {
		return sc.abort(err)
	}
	return sc.finish(), nil
}

// settleKey inspects one previous key for DetectDeletions.
func (sc *scan) settleKey(key string) error {
	s := sc.s
	rec := sc.prev[key]
	switch {
	case s.p.paths.ValidKey(key) != nil:
		sc.tombstone(rec, source.ReasonUnsafeName)
		return nil
	case !s.m.Eligible(key):
		sc.tombstone(rec, source.ReasonExcluded)
		return nil
	}
	ir, err := s.headItem(sc.ctx, key)
	if err != nil {
		var gone *goneError
		switch {
		case errors.As(err, &gone):
			sc.tombstone(rec, source.ReasonDeleted)
			return nil
		case sc.ctx.Err() != nil:
			return sc.ctx.Err()
		case fatalForScan(err):
			return err
		}
		sc.fail(key, err)
		return sc.noteFailure(err)
	}
	defer ir.close()
	sc.failRun = 0
	switch {
	case ir.length > s.def.Limits.MaxFileBytes:
		sc.tombstone(rec, source.ReasonTooLarge)
	case !s.def.AcceptsFormat(rec.MIME):
		sc.tombstone(rec, source.ReasonFormat)
	default:
		sc.cs.Counts.Unchanged++
		sc.next[key] = rec
	}
	return nil
}
