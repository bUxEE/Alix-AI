package fs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// scan is the state of one Discover call.
type scan struct {
	p    *Provider
	ctx  context.Context
	def  source.Definition
	m    *source.Matcher
	h    *rootHandle
	now  time.Time
	prev map[string]source.Record

	cs         source.ChangeSet
	next       map[string]source.Record // live records after this scan
	skipped    map[string]source.Reason // previous-or-not keys found and not accepted
	failedDirs []string                 // directories whose listing failed
	files      int64
	bytes      int64
}

// limitReached aborts the walk when the source holds more than it declared.
type limitReached struct{ err *contracts.Error }

func (l limitReached) Error() string { return l.err.Error() }

// rootGone aborts the walk when the root itself cannot be read.
type rootGone struct{ cause error }

func (r rootGone) Error() string { return r.cause.Error() }

// Discover implements source.Provider.
func (p *Provider) Discover(ctx context.Context, scope contracts.Scope, def source.Definition, prev []source.Record) (source.ChangeSet, error) {
	if err := p.begin(ctx, scope, def); err != nil {
		return source.ChangeSet{}, err
	}
	m, err := p.matcher(def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	h, err := p.openRoot(def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	defer h.close()

	s := &scan{
		p: p, ctx: ctx, def: def, m: m, h: h, now: p.clk.Now(),
		prev:    make(map[string]source.Record, len(prev)),
		next:    make(map[string]source.Record, len(prev)),
		skipped: map[string]source.Reason{},
	}
	for _, r := range prev {
		s.prev[r.Key] = r
	}

	fsys := h.r.FS()
	if p.wrapFS != nil {
		fsys = p.wrapFS(fsys)
	}
	werr := iofs.WalkDir(fsys, ".", s.visit)
	if werr != nil {
		return s.abort(werr)
	}

	// Deleting needs a reachable root. The handle opened at the start can
	// outlive the directory it names (an unmounted share, a removed folder),
	// and lookups through it would then say "missing" for everything. Reopen
	// the root by its path before concluding that anything is gone.
	again, err := p.openRoot(def)
	if err != nil {
		return s.abort(err)
	}
	again.close()

	if err := s.tombstones(); err != nil {
		return s.abort(err)
	}
	return s.finish(), nil
}

// abort returns the diagnostics of a scan that did not complete: counts and
// what was skipped or failed so far, and no change that could be applied.
func (s *scan) abort(err error) (source.ChangeSet, error) {
	diag := source.ChangeSet{Counts: s.cs.Counts, Skipped: s.cs.Skipped, Failed: s.cs.Failed}
	var lim limitReached
	var gone rootGone
	switch {
	case errors.As(err, &lim):
		return diag, lim.err
	case errors.As(err, &gone):
		return diag, unavailable("the root became unreadable during the scan", gone.cause)
	case s.ctx.Err() != nil:
		return diag, interrupted(s.ctx.Err())
	}
	return diag, err
}

func (s *scan) finish() source.ChangeSet {
	cs := s.cs
	cs.Next = make([]source.Record, 0, len(s.next))
	for _, r := range s.next {
		cs.Next = append(cs.Next, r)
	}
	sort.Slice(cs.Next, func(i, j int) bool { return cs.Next[i].Key < cs.Next[j].Key })
	sortKeys(&cs)
	cs.Counts.Tombstoned = len(cs.Deleted)
	return cs
}

func sortKeys(cs *source.ChangeSet) {
	sort.Slice(cs.Added, func(i, j int) bool { return cs.Added[i].Key < cs.Added[j].Key })
	sort.Slice(cs.Modified, func(i, j int) bool { return cs.Modified[i].Key < cs.Modified[j].Key })
	sort.Slice(cs.Deleted, func(i, j int) bool { return cs.Deleted[i].Key < cs.Deleted[j].Key })
	sort.Slice(cs.Skipped, func(i, j int) bool { return cs.Skipped[i].Key < cs.Skipped[j].Key })
	sort.Slice(cs.Failed, func(i, j int) bool { return cs.Failed[i].Key < cs.Failed[j].Key })
}

func (s *scan) visit(key string, d iofs.DirEntry, err error) error {
	if cerr := s.ctx.Err(); cerr != nil {
		return cerr
	}
	if err != nil {
		if key == "." {
			return rootGone{cause: err}
		}
		// A directory whose listing failed. What is below it is unknown, not
		// gone: it keeps its records.
		s.failedDirs = append(s.failedDirs, key)
		s.fail(key, err)
		return nil
	}
	if key == "." {
		return nil
	}
	if d.IsDir() {
		if s.p.paths.ValidKey(key) != nil {
			s.cs.Skipped = append(s.cs.Skipped, source.Skip{Key: key, Reason: source.ReasonUnsafeName})
			s.cs.Counts.Skipped++
			return iofs.SkipDir
		}
		if s.m.PruneDir(key) {
			return iofs.SkipDir
		}
		return nil
	}
	return s.file(key, d)
}

func (s *scan) skip(key string, why source.Reason) {
	s.skipped[key] = why
	s.cs.Skipped = append(s.cs.Skipped, source.Skip{Key: key, Reason: why})
	s.cs.Counts.Skipped++
}

// fail records an item or directory that could not be read. If it had a
// previous record the record is kept: a read error is not a deletion.
func (s *scan) fail(key string, err error) {
	ce := asContractsError(key, err)
	s.cs.Failed = append(s.cs.Failed, source.Failure{Key: key, Err: ce})
	s.cs.Counts.Failed++
	if r, ok := s.prev[key]; ok {
		s.next[key] = r
	}
}

func asContractsError(key string, err error) *contracts.Error {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, fmt.Sprintf("%q cannot be read", key))
}

func (s *scan) file(key string, d iofs.DirEntry) error {
	if !s.m.Eligible(key) {
		return nil // not an item of this source; no count, no trace
	}
	s.cs.Counts.Discovered++
	if err := s.p.paths.ValidKey(key); err != nil {
		s.skip(key, source.ReasonUnsafeName)
		return nil
	}
	switch t := d.Type(); {
	case t&iofs.ModeSymlink != 0:
		s.skip(key, s.h.linkReason(s.p.paths, key))
		return nil
	case !t.IsRegular():
		s.skip(key, source.ReasonIrregular)
		return nil
	}
	info, err := d.Info()
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			s.cs.Counts.Discovered-- // vanished between listing and stat: not seen
			return nil
		}
		s.fail(key, err)
		return nil
	}
	if info.Size() > s.def.Limits.MaxFileBytes {
		s.skip(key, source.ReasonTooLarge)
		return nil
	}

	prev, had := s.prev[key]
	if had && s.p.untouched(prev, info) {
		if !s.def.AcceptsFormat(prev.MIME) {
			s.skip(key, source.ReasonFormat)
			return nil
		}
		s.cs.Counts.Unchanged++
		return s.live(prev)
	}

	f, err := s.p.openFile(s.h, key)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			s.cs.Counts.Discovered--
			return nil
		}
		s.fail(key, itemError(key, err))
		return nil
	}
	res, err := s.p.readItem(s.ctx, f, key, s.def)
	_ = f.Close()
	if res.hashed {
		s.cs.Counts.Hashed++
	}
	if err != nil {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		s.fail(key, err)
		return nil
	}
	if res.skip != "" {
		s.skip(key, res.skip)
		return nil
	}

	v := res.version
	rec := source.RecordOf(key, v, s.now)
	switch {
	case !had:
		s.cs.Added = append(s.cs.Added, source.Item{Key: key, Version: v})
	case prev.Hash != v.Hash:
		s.cs.Modified = append(s.cs.Modified, source.Item{Key: key, Version: v})
	default:
		// Same bytes under a new time or size: nothing to index, but the
		// refreshed record lets the next scan take the shortcut.
		s.cs.Counts.Unchanged++
	}
	return s.live(rec)
}

// live accepts an item into the next snapshot and enforces the declared
// limits. A source that holds more than it declared is not truncated: the scan
// stops with contracts.ErrResourceExhausted and applies nothing.
func (s *scan) live(r source.Record) error {
	s.next[r.Key] = r
	s.files++
	s.bytes += r.Size
	s.cs.Counts.Bytes = s.bytes
	if s.files > s.def.Limits.MaxFiles {
		return limitReached{contracts.Newf(contracts.ErrResourceExhausted,
			"the source holds more than its declared %d files", s.def.Limits.MaxFiles).WithDetails("limit", "max_files")}
	}
	if s.bytes > s.def.Limits.MaxTotalBytes {
		return limitReached{contracts.Newf(contracts.ErrResourceExhausted,
			"the source holds more than its declared %d bytes", s.def.Limits.MaxTotalBytes).WithDetails("limit", "max_total_bytes")}
	}
	return nil
}

// untouched is the size-and-time shortcut: the item is the version the record
// describes if size and modification time are exactly the recorded ones and
// the record is old enough, relative to the file's time, to be trusted (a
// record observed within the racy window of the file's modification time could
// hide a second write in the same timestamp tick).
func (p *Provider) untouched(r source.Record, info iofs.FileInfo) bool {
	if r.Hash == "" || r.MIME == "" || r.SeenAt.IsZero() {
		return false
	}
	mt := info.ModTime()
	return r.Size == info.Size() && r.ModTime.Equal(mt) && r.SeenAt.Sub(mt) > p.racy
}

// tombstones decides which previous items left the source. The walk is over and
// the root was reached again, so the question per key is only what the walk
// found: an item that was found and is no longer admitted, one that the rules
// no longer reach, or one whose place was inspected and is empty. Anything the
// walk could not see (below a directory that failed to list, or that fails to
// be inspected) keeps its record.
func (s *scan) tombstones() error {
	keys := make([]string, 0, len(s.prev))
	for k := range s.prev {
		if _, live := s.next[k]; !live {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		rec := s.prev[key]
		if why, ok := s.skipped[key]; ok {
			s.tombstone(rec, why)
			continue
		}
		if s.p.paths.ValidKey(key) != nil {
			s.tombstone(rec, source.ReasonUnsafeName)
			continue
		}
		if !s.m.Eligible(key) {
			s.tombstone(rec, source.ReasonExcluded)
			continue
		}
		if s.underFailedDir(key) {
			s.next[key] = rec
			continue
		}
		if err := s.p.settle(s, rec); err != nil {
			return err
		}
	}
	return nil
}

func (s *scan) underFailedDir(key string) bool {
	for _, d := range s.failedDirs {
		if len(key) > len(d) && key[:len(d)] == d && key[len(d)] == '/' {
			return true
		}
	}
	return false
}

func (s *scan) tombstone(rec source.Record, why source.Reason) {
	s.cs.Deleted = append(s.cs.Deleted, source.TombstoneFor(s.def, rec, why, s.now))
}

// settle inspects the place of a previous item the walk did not report and
// either tombstones it or keeps it.
func (p *Provider) settle(s *scan, rec source.Record) error {
	_, kind, at, err := s.h.lstatKey(rec.Key)
	if err != nil {
		s.fail(rec.Key, err)
		return nil
	}
	switch kind {
	case kindMissing:
		s.tombstone(rec, source.ReasonDeleted)
	case kindSymlink:
		s.tombstone(rec, s.h.linkReason(p.paths, at))
	case kindIrregular, kindDir:
		s.tombstone(rec, source.ReasonIrregular)
	default:
		// There, regular, and eligible by the rules, yet the walk did not
		// report it under this key. On a case-insensitive filesystem that
		// means it was renamed (the walk reported the on-disk spelling);
		// otherwise it appeared after the walk passed. Keep the record in the
		// second case; the next scan sees it.
		if canon, err := p.canonicalKey(s.h, rec.Key); err == nil && canon != rec.Key {
			s.tombstone(rec, source.ReasonDeleted)
			return nil
		}
		s.next[rec.Key] = rec
	}
	return nil
}

// DetectDeletions implements source.Provider.
func (p *Provider) DetectDeletions(ctx context.Context, scope contracts.Scope, def source.Definition, prev []source.Record) (source.ChangeSet, error) {
	if err := p.begin(ctx, scope, def); err != nil {
		return source.ChangeSet{}, err
	}
	m, err := p.matcher(def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	h, err := p.openRoot(def)
	if err != nil {
		return source.ChangeSet{}, err
	}
	defer h.close()

	s := &scan{
		p: p, ctx: ctx, def: def, m: m, h: h, now: p.clk.Now(),
		prev:    make(map[string]source.Record, len(prev)),
		next:    make(map[string]source.Record, len(prev)),
		skipped: map[string]source.Reason{},
	}
	keys := make([]string, 0, len(prev))
	for _, r := range prev {
		s.prev[r.Key] = r
		keys = append(keys, r.Key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return s.abort(err)
		}
		rec := s.prev[key]
		switch {
		case p.paths.ValidKey(key) != nil:
			s.tombstone(rec, source.ReasonUnsafeName)
			continue
		case !m.Eligible(key):
			s.tombstone(rec, source.ReasonExcluded)
			continue
		}
		info, kind, at, err := h.lstatKey(key)
		switch {
		case err != nil:
			s.fail(key, err)
		case kind == kindMissing:
			s.tombstone(rec, source.ReasonDeleted)
		case kind == kindSymlink:
			s.tombstone(rec, h.linkReason(p.paths, at))
		case kind != kindRegular:
			s.tombstone(rec, source.ReasonIrregular)
		case info.Size() > def.Limits.MaxFileBytes:
			s.tombstone(rec, source.ReasonTooLarge)
		case !def.AcceptsFormat(rec.MIME):
			s.tombstone(rec, source.ReasonFormat)
		default:
			s.cs.Counts.Unchanged++
			s.next[key] = rec
		}
	}
	// As in Discover: conclude deletions only if the root is still reachable.
	again, err := p.openRoot(def)
	if err != nil {
		return s.abort(err)
	}
	again.close()
	return s.finish(), nil
}
