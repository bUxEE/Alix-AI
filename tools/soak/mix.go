package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"
)

// The load mix: what the harness does to the stack while it samples it. The
// default (tests/performance/soak/mix.json) is one interactive session doing
// booking and RAG turns at a human pace, plus continuous re-ingestion of the
// clinic policies source. Both halves can be switched off or reshaped in the
// mix file; nothing about the mix is in code.

// Mix is the load of one run.
type Mix struct {
	ID          string         `json:"id"`
	Description string         `json:"description,omitempty"`
	Interactive InteractiveMix `json:"interactive"`
	Ingestion   IngestionMix   `json:"ingestion"`
}

// InteractiveMix is the conversation half: one session, one turn at a time,
// the steps in order and round again, a pause after each (the think time of a
// person).
type InteractiveMix struct {
	Enabled bool   `json:"enabled"`
	ThinkMS int64  `json:"think_ms"`
	Steps   []Step `json:"steps"`
}

// Ingestion operations.
const (
	IngestRebuild = "rebuild"
	IngestSync    = "sync"
)

// IngestionMix is the background half: one source, re-ingested over and over.
// rebuild forces a replacement index generation (the load: every chunk is
// parsed, embedded and indexed again); sync only reconciles with the folder and
// does nothing when nothing changed.
type IngestionMix struct {
	Enabled bool `json:"enabled"`
	// Source is the name the application package declares the source under.
	Source string `json:"source"`
	Op     string `json:"op"`
	// PauseMS is the wait between the end of one re-ingestion and the next.
	PauseMS int64 `json:"pause_ms"`
	// SettleMS is how long a finished-looking source must stay finished before the
	// re-ingestion counts as done: a rebuild is queued, and the source may still
	// read "ready" (the old generation) for a moment after the request.
	SettleMS int64 `json:"settle_ms"`
	// TimeoutMS bounds one re-ingestion. Hitting it is a failed ingestion.
	TimeoutMS int64 `json:"timeout_ms"`
}

// Think is the pause between two turns.
func (m InteractiveMix) Think() time.Duration { return time.Duration(m.ThinkMS) * time.Millisecond }

// Pause is the pause between two re-ingestions.
func (m IngestionMix) Pause() time.Duration { return time.Duration(m.PauseMS) * time.Millisecond }

// Settle is the settle time of a re-ingestion.
func (m IngestionMix) Settle() time.Duration { return time.Duration(m.SettleMS) * time.Millisecond }

// Timeout is the bound of a re-ingestion.
func (m IngestionMix) Timeout() time.Duration { return time.Duration(m.TimeoutMS) * time.Millisecond }

var (
	idPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	sourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

const (
	maxMixBytes  = 1 << 20
	maxStepText  = 2000
	maxMixSteps  = 500
	classBooking = "booking"
	classRAG     = "rag"
)

// Validate checks the mix. It fails closed: a mix that would put no load on
// the stack is an error, not an easy pass.
func (m Mix) Validate() error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("mix id %q is not a lower-case id", m.ID)
	}
	if !m.Interactive.Enabled && !m.Ingestion.Enabled {
		return errors.New("the mix switches both the interactive session and the ingestion off: it puts no load on the stack")
	}
	if m.Interactive.Enabled {
		in := m.Interactive
		if in.ThinkMS < 0 {
			return errors.New("interactive.think_ms must not be negative")
		}
		if len(in.Steps) == 0 || len(in.Steps) > maxMixSteps {
			return fmt.Errorf("interactive.steps needs 1 to %d steps", maxMixSteps)
		}
		seen := map[string]bool{}
		for i, s := range in.Steps {
			switch {
			case !idPattern.MatchString(s.ID) || len(s.ID) > 48:
				return fmt.Errorf("step %d: id %q is not a lower-case id of at most 48 characters", i, s.ID)
			case seen[s.ID]:
				return fmt.Errorf("step %q appears twice", s.ID)
			case s.Class != classBooking && s.Class != classRAG:
				return fmt.Errorf("step %q: class %q is not booking or rag", s.ID, s.Class)
			case s.Language != "en" && s.Language != "it":
				return fmt.Errorf("step %q: language %q is not en or it", s.ID, s.Language)
			case s.Text == "" || !utf8.ValidString(s.Text) || utf8.RuneCountInString(s.Text) > maxStepText:
				return fmt.Errorf("step %q: the text is empty, not UTF-8 or longer than %d characters", s.ID, maxStepText)
			}
			seen[s.ID] = true
		}
	}
	if m.Ingestion.Enabled {
		g := m.Ingestion
		switch {
		case !sourcePattern.MatchString(g.Source):
			return fmt.Errorf("ingestion.source %q is not a source declaration name", g.Source)
		case g.Op != IngestRebuild && g.Op != IngestSync:
			return fmt.Errorf("ingestion.op %q is not rebuild or sync", g.Op)
		case g.PauseMS < 0 || g.SettleMS < 0:
			return errors.New("ingestion.pause_ms and settle_ms must not be negative")
		case g.TimeoutMS <= 0:
			return errors.New("ingestion.timeout_ms must be positive: a re-ingestion that never ends is a failed one")
		}
	}
	return nil
}

// LoadMix reads and validates a mix file and returns it with the SHA-256 of
// its bytes, which the result records.
func LoadMix(path string) (Mix, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return Mix{}, "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMixBytes+1))
	if err != nil {
		return Mix{}, "", err
	}
	if len(data) > maxMixBytes {
		return Mix{}, "", fmt.Errorf("%s is larger than %d bytes", path, maxMixBytes)
	}
	var m Mix
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Mix{}, "", fmt.Errorf("%s: %w", path, err)
	}
	if dec.More() {
		return Mix{}, "", fmt.Errorf("%s: more than one JSON document", path)
	}
	if err := m.Validate(); err != nil {
		return Mix{}, "", fmt.Errorf("%s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return m, hex.EncodeToString(sum[:]), nil
}

// defaultMixRel is where the shipped mix lives, beside go.mod.
var defaultMixRel = filepath.Join("tests", "performance", "soak", "mix.json")

// FindDefaultMix looks for the shipped mix from dir upwards, the way the other
// tools find schemas/ and tests/performance.
func FindDefaultMix(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			p := filepath.Join(dir, defaultMixRel)
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("the repository at %s has no %s: pass --mix", dir, filepath.ToSlash(defaultMixRel))
			}
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory, so the default mix cannot be found: pass --mix")
		}
		dir = parent
	}
}
