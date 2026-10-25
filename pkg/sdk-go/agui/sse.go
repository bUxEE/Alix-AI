package agui

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// MaxFrameBytes bounds one frame (and one unterminated line) the reader accepts: the gateway never sends an event over
// 256 KiB, so a server that never ends a frame is refused instead of buffered.
const MaxFrameBytes = 1 << 20

// ErrFrameTooLarge is returned by the reader for a frame over MaxFrameBytes.
var ErrFrameTooLarge = errors.New("agui: a frame of the event stream exceeds the size limit")

// Frame is one server-sent event as the AG-UI stream uses it: `data:` is an AG-UI event on one line and `id:` (when the
// frame has one) is the resume cursor. The id belongs to the frame that carried it, it is not the sticky "last event id"
// of the HTML standard: a frame without one has none.
type Frame struct {
	// ID is the `id:` field, valid only when HasID.
	ID    string
	HasID bool
	// Event is the `event:` field, empty when the frame has none (AG-UI does not use it).
	Event string
	// Data is the `data:` field; a frame with several data lines holds them joined by LF.
	Data []byte
}

// Encoder writes frames in the framing of the AG-UI SSE stream: an optional `id: <n>` line, then `data: <event>`, then a
// blank line, flushed after each frame. It writes nothing else (no retry, no comment, no event name unless the frame has one).
type Encoder struct {
	w     io.Writer
	flush func() error
}

// NewEncoder returns an encoder on w. When w is an http.ResponseWriter (or has Flush) each frame is flushed as written.
func NewEncoder(w io.Writer) *Encoder {
	e := &Encoder{w: w}
	switch v := w.(type) {
	case http.ResponseWriter:
		rc := http.NewResponseController(v)
		e.flush = func() error {
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
			return nil
		}
	case interface{ Flush() error }:
		e.flush = v.Flush
	case http.Flusher:
		e.flush = func() error { v.Flush(); return nil }
	}
	return e
}

// WriteFrame writes one frame. A frame whose id or event name contains a line break or a NUL, or whose data contains a CR,
// cannot be framed without changing what it means, so it is an error and nothing is written.
func (e *Encoder) WriteFrame(f Frame) error {
	if strings.ContainsAny(f.ID, "\r\n\x00") || strings.ContainsAny(f.Event, "\r\n") || bytes.IndexByte(f.Data, '\r') >= 0 {
		return errors.New("agui: the frame cannot be framed (line break in a field)")
	}
	if !utf8.Valid(f.Data) {
		return errors.New("agui: the frame is not valid UTF-8")
	}
	var buf bytes.Buffer
	if f.Event != "" && f.Event != "message" {
		buf.WriteString("event: " + f.Event + "\n")
	}
	if f.HasID {
		buf.WriteString("id: " + f.ID + "\n")
	}
	for _, line := range bytes.Split(f.Data, []byte{'\n'}) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	if _, err := e.w.Write(buf.Bytes()); err != nil {
		return err
	}
	if e.flush != nil {
		return e.flush()
	}
	return nil
}

// WriteEvent marshals an event and writes it as a frame; id is the resume cursor, "" for none.
func (e *Encoder) WriteEvent(id string, ev Event) error {
	data, err := MarshalEvent(ev)
	if err != nil {
		return err
	}
	return e.WriteFrame(Frame{ID: id, HasID: id != "", Data: data})
}

// FrameReader reads the frames of an event stream, to the "Event stream interpretation" rules of the HTML standard: lines
// end in CRLF, LF or CR, `:` lines are comments, `field: value` takes one optional space, data lines join with LF and a frame
// is dispatched by a blank line. A frame cut off before its blank line is never returned, so a connection that dies
// mid-frame cannot deliver half an event.
type FrameReader struct {
	scan  *bufio.Scanner
	cur   Frame
	lines [][]byte
	size  int
}

// NewFrameReader returns a reader on r.
func NewFrameReader(r io.Reader) *FrameReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxFrameBytes)
	sc.Split(scanLines)
	return &FrameReader{scan: sc}
}

func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' {
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if !atEOF {
				return 0, nil, nil // the CR may be the first half of CRLF: wait for more
			}
		}
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (r *FrameReader) reset() {
	r.cur, r.lines, r.size = Frame{}, nil, 0
}

// Next returns the next frame, io.EOF at the clean end of the stream, or the read error.
func (r *FrameReader) Next() (Frame, error) {
	for r.scan.Scan() {
		line := r.scan.Bytes()
		if len(line) == 0 {
			if len(r.lines) == 0 { // comments, or only `event:` or `id:`: nothing to dispatch
				r.reset()
				continue
			}
			f := r.cur
			f.Data = bytes.Join(r.lines, []byte("\n"))
			r.reset()
			if !utf8.Valid(f.Data) {
				return Frame{}, errors.New("agui: the event stream is not valid UTF-8")
			}
			return f, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value := string(line), ""
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = string(line[:i]), strings.TrimPrefix(string(line[i+1:]), " ")
		}
		switch field {
		case "event":
			r.cur.Event = value
		case "data":
			r.size += len(value) + 1
			if r.size > MaxFrameBytes {
				return Frame{}, ErrFrameTooLarge
			}
			r.lines = append(r.lines, []byte(value))
		case "id":
			if !strings.ContainsRune(value, 0) {
				r.cur.ID, r.cur.HasID = value, true
			}
		}
	}
	if err := r.scan.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Frame{}, ErrFrameTooLarge
		}
		return Frame{}, err
	}
	return Frame{}, io.EOF // an unfinished frame at the end is dropped
}
