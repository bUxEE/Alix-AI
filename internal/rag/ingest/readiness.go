package ingest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// reprobeAfter is how long a failed readiness probe stands before a parse job
// tries the worker again.
const reprobeAfter = time.Minute

// Readiness is the answer of Service.Start: whether the parser worker started
// and what confinement it got. Operators see it (the readiness check of the
// service reports it), because the sandbox level decides what "the parser has no
// network" rests on.
type Readiness struct {
	// Ready is true when a worker started and reported its confinement.
	Ready bool
	// Level is parse.LevelL2 (the worker confined itself: on Linux Landlock for
	// the filesystem and seccomp for sockets and program execution) or
	// parse.LevelL1 (process isolation and resource limits only). Empty when the
	// probe failed.
	Level string
	// Sandbox is the worker's report, including the mechanisms that could not be
	// installed (Missing).
	Sandbox parse.SandboxReport
	// Platform is the host's GOOS/GOARCH.
	Platform string
	// ProbeTime is how long the worker took to start and report. StartupTimeout
	// is the limit it had.
	ProbeTime      time.Duration
	StartupTimeout time.Duration
	// CheckedAt is when the probe ran.
	CheckedAt time.Time
	// Warnings are what an operator should act on or know.
	Warnings []string
	// Err is why the probe failed, "" when Ready.
	Err string
}

// Start checks the parser worker once, at service start (master plan 3 rule 14:
// a sandbox that cannot be verified means nothing is parsed). It starts a
// worker, reads the confinement it reports and ends it, without parsing a
// document, bounded by Config.Parser.ProbeTimeout.
//
// The first start of a freshly built executable can be slow where an antivirus
// scans it: 26.5 s under Avast on Windows against the parser's own 30 s startup
// limit (ADR 0014, M2-04). The startup limit is therefore configurable
// (Config.Parser.StartupTimeout, default 90 s) and this probe pays the first
// start before any job does. On Windows, exclude the worker executable from
// antivirus scanning.
//
// A failed probe is an error and leaves the service not ready: parse jobs then
// fail closed (retryable) instead of running unconfined.
func (s *Service) Start(ctx context.Context) (Readiness, error) {
	rd := s.probe(ctx)
	if !rd.Ready {
		return rd, contracts.New(contracts.ErrUnavailable, "ingest: the parser worker is not ready: "+rd.Err)
	}
	for _, w := range rd.Warnings {
		s.log.Warn("parser readiness", "level", rd.Level, "note", w)
	}
	s.log.Info("parser worker ready", "level", rd.Level, "platform", rd.Platform, "probe_ms", rd.ProbeTime.Milliseconds())
	return rd, nil
}

// Readiness returns the last probe's result, and false when none ran.
func (s *Service) Readiness() (Readiness, bool) {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if s.ready == nil {
		return Readiness{}, false
	}
	return *s.ready, true
}

func (s *Service) probe(ctx context.Context) Readiness {
	pctx, cancel := context.WithTimeout(ctx, s.cfg.Parser.ProbeTimeout)
	defer cancel()
	begin := s.clk.Now()
	report, err := s.parser.Probe(pctx)
	rd := Readiness{
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		StartupTimeout: s.cfg.Parser.StartupTimeout,
		CheckedAt:      begin,
		ProbeTime:      s.clk.Now().Sub(begin),
	}
	if err != nil {
		rd.Err = safeMessage(err)
	} else {
		rd.Ready, rd.Level, rd.Sandbox = true, report.Level, report
		rd.Warnings = readinessWarnings(rd)
	}
	s.readyMu.Lock()
	s.ready = &rd
	s.readyMu.Unlock()
	return rd
}

// safeMessage is an error's message without the cause chain's host paths: the
// typed message only.
func safeMessage(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		return string(ce.Code) + ": " + ce.Message
	}
	return "probe failed"
}

func readinessWarnings(rd Readiness) []string {
	var w []string
	switch rd.Level {
	case parse.LevelL2:
		// Nothing to warn about: the worker confined itself.
	default:
		w = append(w, "the parser worker runs at sandbox level L1 (process isolation and resource limits only); "+
			"'no network' for the parser rests on the import-graph test (TestParserHasNoNetwork), not on an operating-system guarantee")
	}
	if len(rd.Sandbox.Missing) > 0 {
		w = append(w, "confinement mechanisms that could not be installed: "+strings.Join(rd.Sandbox.Missing, "; "))
	}
	switch runtime.GOOS {
	case "windows":
		w = append(w, "Windows: no restricted token or AppContainer is applied (L1 is the most the worker gets); "+
			"exclude the alixai-parser executable from antivirus scanning, because the first start of a freshly built worker took 26.5 s under Avast")
	case "darwin":
		w = append(w, "macOS: no Seatbelt wrapper is applied, and the sandbox guarantees on macOS are untested")
	case "linux":
		if runtime.GOARCH == "arm64" {
			w = append(w, "linux/arm64: the sandbox guarantees are untested on this architecture")
		}
	}
	if rd.StartupTimeout > 0 && rd.ProbeTime > rd.StartupTimeout/2 {
		w = append(w, fmt.Sprintf("the worker took %s to start against a limit of %s: exclude the executable from antivirus scanning or raise parser.startup_timeout",
			rd.ProbeTime.Round(time.Millisecond), rd.StartupTimeout))
	}
	w = append(w, "one worker process is started per document (no pool): about 37 ms on Linux and 70 to 112 ms on Windows per document, "+
		"so parse throughput is bounded by parser.max_concurrent and the class 5 worker count")
	return w
}

// parserReady fails closed when the last probe failed; it probes again at most
// once per minute so a worker that was fixed is picked up without a restart.
func (s *Service) parserReady(ctx context.Context) error {
	s.readyMu.Lock()
	rd := s.ready
	s.readyMu.Unlock()
	switch {
	case rd == nil || rd.Ready:
		return nil
	case s.clk.Now().Sub(rd.CheckedAt) < reprobeAfter:
		return contracts.New(contracts.ErrUnavailable, "ingest: the parser worker is not ready: "+rd.Err)
	}
	if again := s.probe(ctx); !again.Ready {
		return contracts.New(contracts.ErrUnavailable, "ingest: the parser worker is not ready: "+again.Err)
	}
	return nil
}
