package diag

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	alog "github.com/buxee/alix-ai/internal/log"
)

// Bounds on what the ring keeps. They hold a bundle to a size a person can
// attach to an issue and keep one noisy loop from filling memory.
const (
	// DefaultRecentErrors is the ring size when none is asked for.
	DefaultRecentErrors = 100
	// MaxRecentErrors is the largest ring.
	MaxRecentErrors = 1000

	maxEntryMessage = 512
	maxEntryAttrs   = 16
	maxEntryValue   = 256
	// maxRedactInput bounds the text the redaction patterns run over.
	maxRedactInput = 4096
)

// ErrorEntry is one warning or error as the bundle shows it. It is redacted
// when it is recorded: the ring never holds a raw value.
type ErrorEntry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// ErrorRing keeps the most recent warnings and errors the process logged, for
// the diagnostics bundle. It is bounded, holds redacted text only and is safe for
// concurrent use.
type ErrorRing struct {
	red *alog.Redactor

	mu   sync.Mutex
	buf  []ErrorEntry
	next int
	full bool
}

// NewErrorRing returns a ring that keeps the last capacity entries (clamped to
// 1..MaxRecentErrors; zero or less is DefaultRecentErrors). red is the redactor
// applied on the way in; nil is the built-in rules.
func NewErrorRing(capacity int, red *alog.Redactor) *ErrorRing {
	switch {
	case capacity <= 0:
		capacity = DefaultRecentErrors
	case capacity > MaxRecentErrors:
		capacity = MaxRecentErrors
	}
	if red == nil {
		red = alog.NewRedactor(nil)
	}
	return &ErrorRing{red: red, buf: make([]ErrorEntry, capacity)}
}

// Recent returns the entries, oldest first. The result is a copy.
func (r *ErrorRing) Recent() []ErrorEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]ErrorEntry, 0, n)
	start := 0
	if r.full {
		start = r.next
	}
	for i := 0; i < n; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

func (r *ErrorRing) add(e ErrorEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next++
	if r.next == len(r.buf) {
		r.next, r.full = 0, true
	}
}

// Wrap returns a slog.Handler that passes every record to next unchanged and
// also records the warnings and errors in the ring, redacted. The composition
// root wraps the handler of the logger it builds from internal/log:
//
//	logger := slog.New(ring.Wrap(logHandler))
func (r *ErrorRing) Wrap(next slog.Handler) slog.Handler {
	return &ringHandler{ring: r, next: next}
}

type ringHandler struct {
	ring   *ErrorRing
	next   slog.Handler
	attrs  []slog.Attr // flattened, already prefixed with the groups open when they were added
	groups []string
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || h.next.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		h.ring.add(h.entry(rec))
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), prefixAttrs(h.groups, attrs)...)
	return &c
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

func prefixAttrs(groups []string, attrs []slog.Attr) []slog.Attr {
	if len(groups) == 0 {
		return attrs
	}
	prefix := strings.Join(groups, ".") + "."
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		a.Key = prefix + a.Key
		out[i] = a
	}
	return out
}

// entry redacts and bounds one record.
func (h *ringHandler) entry(rec slog.Record) ErrorEntry {
	red := h.ring.red
	e := ErrorEntry{Time: rec.Time.UTC(), Level: rec.Level.String(), Message: clip(red.String(clip(rec.Message, maxRedactInput)), maxEntryMessage)}
	put := func(a slog.Attr) {
		for _, f := range flatten("", a) {
			if len(e.Attrs) >= maxEntryAttrs {
				return
			}
			if e.Attrs == nil {
				e.Attrs = map[string]any{}
			}
			e.Attrs[f.Key] = h.ring.value(f)
		}
	}
	for _, a := range h.attrs {
		put(a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		put(prefixAttrs(h.groups, []slog.Attr{a})[0])
		return true
	})
	return e
}

// flatten expands a group attribute into dotted keys.
func flatten(prefix string, a slog.Attr) []slog.Attr {
	a.Value = a.Value.Resolve()
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if a.Value.Kind() != slog.KindGroup {
		a.Key = key
		return []slog.Attr{a}
	}
	var out []slog.Attr
	for _, g := range a.Value.Group() {
		out = append(out, flatten(key, g)...)
	}
	return out
}

// value turns one attribute into redacted, bounded data. A key that looks
// sensitive (the last dotted segment, or any group above it) never keeps its
// value.
func (r *ErrorRing) value(a slog.Attr) any {
	for _, seg := range strings.Split(a.Key, ".") {
		if r.red.SensitiveKey(seg) {
			return alog.Redacted
		}
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return clip(r.red.String(clip(a.Value.String(), maxRedactInput)), maxEntryValue)
	case slog.KindAny:
		switch x := a.Value.Any().(type) {
		case nil:
			return nil
		case error:
			return clip(r.red.String(clip(x.Error(), maxRedactInput)), maxEntryValue)
		default:
			v, _ := redactValue(r.red, x)
			if s, ok := v.(string); ok {
				return clip(s, maxEntryValue)
			}
			return v
		}
	case slog.KindInt64:
		return a.Value.Int64()
	case slog.KindUint64:
		return a.Value.Uint64()
	case slog.KindFloat64:
		return a.Value.Float64()
	case slog.KindBool:
		return a.Value.Bool()
	case slog.KindDuration:
		return a.Value.Duration().String()
	case slog.KindTime:
		return a.Value.Time().UTC().Format(time.RFC3339Nano)
	}
	return clip(r.red.String(clip(a.Value.String(), maxRedactInput)), maxEntryValue)
}

// clip cuts s to at most n bytes without splitting a rune.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}
