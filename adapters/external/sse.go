// Package external holds the helpers shared by the hosted-provider adapters
// under adapters/external/<provider> (ADR-0022): a bounded server-sent-events
// reader and a bounded retry policy. It has no provider knowledge, makes no
// network call and holds no credential. Provider packages import it and
// nothing else of each other.
package external

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// ErrEventTooLarge is returned when one server-sent event exceeds the limit.
var ErrEventTooLarge = errors.New("external: server-sent event exceeds the size limit")

// SSEEvent is one dispatched server-sent event.
type SSEEvent struct {
	// Event is the "event:" field ("" when absent).
	Event string
	// Data is the "data:" lines joined with a newline.
	Data string
}

// SSEReader parses the text/event-stream format (WHATWG HTML section 9.2):
// lines ending in LF, CR or CRLF, comment lines starting with ":", the fields
// event and data, and a blank line that dispatches. Fields it does not use (id,
// retry) are ignored. An event that is cut off before its blank line is
// dropped, never delivered half-parsed.
type SSEReader struct {
	br       *bufio.Reader
	maxEvent int
	// skipLF is set after a line that ended in CR: an LF that comes next is the rest of a CRLF, not a line.
	skipLF bool
}

// NewSSEReader reads from r. maxEvent bounds the size of one event (all its
// lines together); a larger one is ErrEventTooLarge.
func NewSSEReader(r io.Reader, maxEvent int) *SSEReader {
	if maxEvent <= 0 {
		maxEvent = 1 << 20
	}
	return &SSEReader{br: bufio.NewReaderSize(r, 16<<10), maxEvent: maxEvent}
}

// readLine returns the next line without its terminator (LF, CR or CRLF), with a size bound. A CR ends the line
// at once and an LF right after it is skipped by the next call, so a live stream is never held waiting for the
// byte that follows a CR.
func (s *SSEReader) readLine(budget int) (string, error) {
	var line []byte
	for {
		b, err := s.br.ReadByte()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return string(line), nil
			}
			return "", err
		}
		if s.skipLF {
			s.skipLF = false
			if b == '\n' {
				continue
			}
		}
		switch b {
		case '\n':
			return string(line), nil
		case '\r':
			s.skipLF = true
			return string(line), nil
		}
		if len(line) >= budget {
			return "", ErrEventTooLarge
		}
		line = append(line, b)
	}
}

// Next returns the next event. It returns io.EOF at a clean end of stream and
// any other error from the underlying reader unchanged.
func (s *SSEReader) Next() (SSEEvent, error) {
	var (
		ev       SSEEvent
		data     []string
		have     bool
		consumed int
	)
	for {
		line, err := s.readLine(s.maxEvent - consumed)
		if err != nil {
			return SSEEvent{}, err
		}
		consumed += len(line) + 1
		if consumed > s.maxEvent {
			return SSEEvent{}, ErrEventTooLarge
		}
		if line == "" {
			if !have {
				continue
			}
			ev.Data = strings.Join(data, "\n")
			return ev, nil
		}
		if line[0] == ':' {
			continue
		}
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			ev.Event, have = value, true
		case "data":
			data, have = append(data, value), true
		}
	}
}
