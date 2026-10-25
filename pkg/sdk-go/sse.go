package sdkgo

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// A small server-sent-events reader, written to the "Event stream interpretation" rules of the HTML standard:
// lines end in CRLF, LF or CR, `:` lines are comments, `field: value` takes one optional space, data lines
// join with LF and an event is dispatched by a blank line. An event cut off before its blank line is never
// dispatched, so a connection that dies mid-frame cannot deliver half an event.

// maxEventBytes bounds one event (or one unterminated line): a server that never ends a frame is refused.
const maxEventBytes = 8 << 20

var errEventTooLarge = errors.New("an event of the stream exceeds the size limit")

// frame is one unit of an event stream: for server-sent events its event name and id as well, for a
// WebSocket only the data.
type frame struct {
	event string
	id    string
	hasID bool
	data  []byte
	// framed is true for server-sent events, whose `event:` and `id:` must agree with the event they carry.
	framed bool
}

// frameSource yields the frames of one connection. next returns io.EOF at the clean end of the connection;
// any other error is a read failure. close releases the connection and is safe to call twice.
type frameSource interface {
	next() (frame, error)
	close()
}

type sseReader struct {
	body  io.ReadCloser
	scan  *bufio.Scanner
	event string
	data  [][]byte
	size  int
	id    string
	hasID bool
}

func newSSEReader(body io.ReadCloser) *sseReader {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	sc.Split(scanSSELines)
	return &sseReader{body: body, scan: sc}
}

// scanSSELines splits on CRLF, LF or a lone CR.
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
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

func (r *sseReader) reset() {
	r.event, r.data, r.size, r.id, r.hasID = "", nil, 0, "", false
}

func (r *sseReader) next() (frame, error) {
	for r.scan.Scan() {
		line := r.scan.Bytes()
		if len(line) == 0 {
			if len(r.data) == 0 { // comments or only `event:`: nothing to dispatch
				r.reset()
				continue
			}
			name := r.event
			if name == "" {
				name = "message"
			}
			data := bytes.Join(r.data, []byte("\n"))
			f := frame{event: name, id: r.id, hasID: r.hasID, data: data, framed: true}
			r.reset()
			if !utf8.Valid(data) {
				return frame{}, errors.New("the event stream is not valid UTF-8")
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
			r.event = value
		case "data":
			r.size += len(value) + 1
			if r.size > maxEventBytes {
				return frame{}, errEventTooLarge
			}
			r.data = append(r.data, []byte(value))
		case "id":
			if !strings.ContainsRune(value, 0) {
				r.id, r.hasID = value, true
			}
		} // retry and unknown fields are ignored: the resume pause is the client's own
	}
	if err := r.scan.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return frame{}, errEventTooLarge
		}
		return frame{}, err
	}
	return frame{}, io.EOF // an unfinished event at the end is dropped
}

func (r *sseReader) close() { _ = r.body.Close() }
