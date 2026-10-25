package stream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// SSEHandler serves the events of one turn as server-sent events. The request
// is a GET that names the turn (by default the "turn_id" path value); the
// events are the same frames a resumed reader gets:
//
//	id: <seq>
//	event: <type>
//	data: <the TurnEvent JSON, one line>
//
// The id is the event's seq, which is the resume cursor. A client that loses
// the connection reconnects with the last id it saw in the Last-Event-ID header
// (an EventSource does this by itself) or in the "cursor" query parameter, and
// receives the events after it: each exactly once, in order. The stream ends
// after the done event. A connection that ends without one is to be resumed.
// When the cursor is already at the end of a finished turn the answer is 204
// (the SSE signal to stop reconnecting). A cursor behind the retained window is
// 410, one ahead of the stream or malformed is 400, a turn that is not visible
// in the caller's scope is 403, whoever's it is.
//
// The handler reads from the hub only: it never runs a turn, so a resume never
// repeats one. The confirmation secret of a prepared action is not in any event
// and is not sent here: it is shown once, to the client that made the turn, by
// the turn's own response, and a resume does not repeat it.
func (h *Hub) SSEHandler(o Options) http.Handler {
	o = o.withDefaults()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, turn := h.attach(w, r, o)
		if sub == nil {
			return
		}
		defer sub.Close()
		hd := w.Header()
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Content-Type-Options", "nosniff")
		if sub.Complete() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		hd.Set("Content-Type", "text/event-stream")
		hd.Set("X-Accel-Buffering", "no")
		rc := http.NewResponseController(w)
		write := func(b []byte) error {
			_ = rc.SetWriteDeadline(time.Now().Add(o.WriteTimeout))
			if _, err := w.Write(b); err != nil {
				return err
			}
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
			return nil
		}
		w.WriteHeader(http.StatusOK)
		retry := o.RetryAfter.Milliseconds()
		if err := write([]byte("retry: " + strconv.FormatInt(retry, 10) + "\n\n")); err != nil {
			return
		}
		h.sseLoop(r.Context(), sub, turn, o, write)
	})
}

func (h *Hub) sseLoop(ctx context.Context, sub *Subscription, turn *Turn, o Options, write func([]byte) error) {
	for {
		wctx, cancel := context.WithTimeout(ctx, o.Heartbeat)
		f, err := sub.Next(wctx)
		cancel()
		switch {
		case err == nil:
			if write(sseFrame(f)) != nil {
				return
			}
		case ctx.Err() != nil:
			return
		case errors.Is(err, context.DeadlineExceeded):
			if write([]byte(": keep-alive\n\n")) != nil {
				return
			}
		case errors.Is(err, io.EOF):
			return
		case errors.Is(err, ErrSlowConsumer):
			h.cfg.Logger.Info("stream subscriber cut off", "turn_id", turn.id, "reason", string(ReasonSlowConsumer))
			_ = write([]byte(": slow consumer, reconnect with Last-Event-ID\n\n"))
			return
		default:
			h.cfg.Logger.Info("stream ended without done", "turn_id", turn.id, "reason", string(ReasonOf(err)))
			_ = write([]byte(": the turn was abandoned\n\n"))
			return
		}
	}
}

// sseFrame renders a frame as one SSE event. The type is one of the schema's
// event names and the data one line of JSON (both are checked when the event is
// published), so neither can inject a field or a second event.
func sseFrame(f Frame) []byte {
	b := make([]byte, 0, len(f.Data)+len(f.Type)+32)
	b = append(b, "id: "...)
	b = strconv.AppendInt(b, f.Seq, 10)
	b = append(b, "\nevent: "...)
	b = append(b, f.Type...)
	b = append(b, "\ndata: "...)
	b = append(b, f.Data...)
	return append(b, '\n', '\n')
}
