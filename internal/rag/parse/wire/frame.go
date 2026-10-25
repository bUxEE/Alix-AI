package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FrameType is the one-byte tag of a frame.
type FrameType byte

// The frame types. Direction is fixed: a host never receives a Request and a
// worker never receives a Result, and either side refuses a frame going the
// wrong way.
const (
	// FrameInit is the host's first frame (JSON Init): the process limits the
	// worker applies to itself. The worker reads nothing before it.
	FrameInit FrameType = 'I'
	// FrameHello is the worker's answer to Init (JSON Hello): who it is and what
	// confinement it installed on itself.
	FrameHello FrameType = 'H'
	// FrameRequest is the host's JSON Request, sent only after the host has
	// verified the Hello.
	FrameRequest FrameType = 'Q'
	// FrameBody is the raw bytes of the document, exactly Request.Size long.
	FrameBody FrameType = 'B'
	// FrameResult is the worker's JSON Result header (blocks, pages, stats).
	FrameResult FrameType = 'R'
	// FrameText is the extracted text, raw UTF-8, sent after FrameResult.
	FrameText FrameType = 'T'
	// FrameError is a JSON Failure: the worker understood the request and
	// refused or failed it. A worker that dies sends nothing.
	FrameError FrameType = 'E'
)

// HeaderSize is the size of a frame header: a type byte and a big-endian
// uint32 payload length.
const HeaderSize = 5

// MaxJSONFrame bounds a JSON frame (Hello, Request, Result, Failure). A
// result with a hundred thousand table rows is a few megabytes of JSON.
const MaxJSONFrame = 32 << 20

// ErrFrame is wrapped by every framing failure: a bad type, a length over the
// reader's cap, a truncated payload.
var ErrFrame = errors.New("wire: bad frame")

// ErrOversize is wrapped (with ErrFrame) when a frame declares more bytes than
// the reader allows: a peer breaking the protocol, as opposed to one that died
// mid-frame.
var ErrOversize = errors.New("wire: frame over the size limit")

// WriteFrame writes one frame. The payload is written as given, so a caller
// with a very large payload is not made to copy it.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return fmt.Errorf("%w: payload of %d bytes", ErrFrame, len(payload))
	}
	var h [HeaderSize]byte
	h[0] = byte(t)
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadHeader reads one frame header and checks the length against max before
// anything is allocated. The reader is untrusted on both sides: a hostile
// worker must not be able to make the host allocate by declaring a length.
func ReadHeader(r io.Reader, max int64) (FrameType, int64, error) {
	var h [HeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, 0, err
	}
	n := int64(binary.BigEndian.Uint32(h[1:]))
	if n > max {
		return 0, 0, fmt.Errorf("%w: %w: %q frame declares %d bytes, over the limit of %d", ErrFrame, ErrOversize, h[0], n, max)
	}
	return FrameType(h[0]), n, nil
}

// ReadPayload reads exactly n payload bytes. It grows its buffer as bytes
// arrive instead of trusting n for one big allocation, so a peer that
// declares a large frame and sends nothing costs nothing.
func ReadPayload(r io.Reader, n int64) ([]byte, error) {
	const step = 1 << 20
	if n <= step {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("%w: truncated payload: %w", ErrFrame, noEOF(err))
		}
		return buf, nil
	}
	buf := make([]byte, 0, step)
	for int64(len(buf)) < n {
		want := n - int64(len(buf))
		if want > step {
			want = step
		}
		old := len(buf)
		buf = append(buf, make([]byte, want)...)
		if _, err := io.ReadFull(r, buf[old:]); err != nil {
			return nil, fmt.Errorf("%w: truncated payload: %w", ErrFrame, noEOF(err))
		}
	}
	return buf, nil
}

func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ReadFrame reads one whole frame whose payload is at most max bytes.
func ReadFrame(r io.Reader, max int64) (FrameType, []byte, error) {
	t, n, err := ReadHeader(r, max)
	if err != nil {
		return 0, nil, err
	}
	p, err := ReadPayload(r, n)
	if err != nil {
		return 0, nil, err
	}
	return t, p, nil
}
