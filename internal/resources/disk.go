package resources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// DiskLevel is how much free space a watched volume has left.
type DiskLevel uint8

const (
	// DiskOK: ingestion may write.
	DiskOK DiskLevel = iota
	// DiskPressure: free space is below the pause floor; ingestion pauses.
	DiskPressure
	// DiskCritical: free space is below the transactional reserve; what is
	// left belongs to the store (commits, audit, sessions).
	DiskCritical
)

// String is the level's stable name.
func (l DiskLevel) String() string {
	switch l {
	case DiskOK:
		return "ok"
	case DiskPressure:
		return "pressure"
	case DiskCritical:
		return "critical"
	}
	return "unknown"
}

// DiskOptions is everything NewDiskGuard needs.
type DiskOptions struct {
	// Config supplies the floors; zero fields take the defaults.
	Config Config
	// Paths are the volumes to watch: the data directory (database, index,
	// blobs) and any other directory ingestion writes to. At least one.
	Paths []string
	// Probe reads free space. Nil means OSDiskProbe.
	Probe DiskProbe
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// Logger receives level changes. Nil discards.
	Logger *slog.Logger
}

// DiskGuard is the disk-pressure detector (master plan 18: "pause ingestion
// before interactive or transactional storage is endangered"). It satisfies
// internal/rag/ingest.DiskPressure structurally: install it as
// ingest.Deps.Pressure or hand it to ingest.NewPause.
//
// Each volume has three floors: below the pause floor ingestion pauses, it
// resumes only at the (higher) resume floor, and below the reserve the volume
// is critical. Pressure on any watched volume pauses ingestion. A volume whose
// free space cannot be read counts as under pressure (the answer is unknown
// and ingestion fails closed rather than fill a disk it could not measure).
// Readings are reused for the probe interval. Safe for concurrent use.
type DiskGuard struct {
	cfg   Config
	paths []string
	probe DiskProbe
	clk   clock.Clock
	log   *slog.Logger

	mu     sync.Mutex
	at     time.Time
	known  bool
	levels map[string]DiskLevel
	free   map[string]uint64
	err    error
}

// NewDiskGuard validates the options and builds the guard.
func NewDiskGuard(opts DiskOptions) (*DiskGuard, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	if len(opts.Paths) == 0 {
		return nil, errors.New("resources: DiskGuard needs at least one path to watch")
	}
	d := &DiskGuard{
		cfg:    opts.Config.WithDefaults(),
		paths:  append([]string(nil), opts.Paths...),
		probe:  opts.Probe,
		clk:    opts.Clock,
		log:    opts.Logger,
		levels: map[string]DiskLevel{},
		free:   map[string]uint64{},
	}
	if d.probe == nil {
		d.probe = OSDiskProbe{}
	}
	if d.clk == nil {
		d.clk = clock.Real{}
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	return d, nil
}

// next is the hysteresis rule for one volume.
func (d *DiskGuard) next(prev DiskLevel, free uint64) DiskLevel {
	switch {
	case free < d.cfg.DiskReserveBytes:
		return DiskCritical
	case free < d.cfg.DiskPauseFreeBytes:
		return DiskPressure
	case prev != DiskOK && free < d.cfg.DiskResumeFreeBytes:
		return DiskPressure // not yet resumed
	}
	return DiskOK
}

// refreshLocked re-reads every volume when the reading is older than the probe
// interval. Caller holds d.mu. The probe is a local statfs, so holding the
// lock across it is the simple, correct choice (readers share one reading).
func (d *DiskGuard) refreshLocked() {
	now := d.clk.Now()
	if d.known && !now.Before(d.at) && now.Sub(d.at) < d.cfg.ProbeInterval {
		return
	}
	var errs []error
	for _, p := range d.paths {
		free, err := d.probe.FreeBytes(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("free space of %q: %w", p, err))
			continue
		}
		prev := d.levels[p]
		lvl := d.next(prev, free)
		if lvl != prev || !d.known {
			if lvl != DiskOK {
				d.log.Warn("disk pressure", "path", p, "level", lvl.String(), "free", free)
			} else if prev != DiskOK {
				d.log.Info("disk pressure cleared", "path", p, "free", free)
			}
		}
		d.levels[p], d.free[p] = lvl, free
	}
	d.at, d.known, d.err = now, true, errors.Join(errs...)
}

// UnderPressure reports whether ingestion must stop writing now. An error
// means a volume could not be measured; the answer is then true as well (the
// caller treats an error as pressure).
func (d *DiskGuard) UnderPressure(_ context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshLocked()
	if d.err != nil {
		return true, contracts.Wrap(contracts.ErrUnavailable, d.err, "resource governor: disk free space unknown")
	}
	for _, l := range d.levels {
		if l != DiskOK {
			return true, nil
		}
	}
	return false, nil
}

// Level is the worst level over the watched volumes. An unreadable volume
// reports DiskCritical (nothing is known about what is left).
func (d *DiskGuard) Level() DiskLevel {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshLocked()
	if d.err != nil {
		return DiskCritical
	}
	worst := DiskOK
	for _, l := range d.levels {
		worst = max(worst, l)
	}
	return worst
}

// Invalidate drops the cached reading, so the next question asks the probe.
func (d *DiskGuard) Invalidate() {
	d.mu.Lock()
	d.known = false
	d.mu.Unlock()
}
