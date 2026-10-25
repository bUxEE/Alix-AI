package stream

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/coder/websocket"
)

// maxClientMessage is the largest message a client may send. The transport is
// server to client only, so no client message is legitimate; the limit exists so
// that an oversized frame is refused before it is buffered.
const maxClientMessage = 256

// WebSocket close codes (RFC 6455 section 7.4.2 and the application range).
const (
	// CloseDone is a normal closure: the done event was sent.
	CloseDone = websocket.StatusNormalClosure // 1000
	// CloseSlowConsumer asks the client to reconnect with its cursor.
	CloseSlowConsumer = websocket.StatusTryAgainLater // 1013
	// CloseAborted says the turn was abandoned before it finished.
	CloseAborted = websocket.StatusInternalError // 1011
)

// WebSocketHandler serves the events of one turn over a WebSocket: one text
// message per event, each exactly the TurnEvent JSON that an SSE data line
// carries. The transport is server to client only; a message from the client is
// a protocol violation and closes the connection (1008). The resume cursor is
// the "cursor" query parameter (a browser cannot set Last-Event-ID on a
// WebSocket; the header is read as well, for other clients), with the same
// meaning as for SSE: the seq of the last event received. The stream ends with
// a normal closure (1000, "done") after the done event; closure 1013
// ("slow_consumer") asks the client to reconnect with its cursor; any other end
// is to be resumed the same way.
//
// Everything that can be refused before the upgrade is refused with an HTTP
// status, exactly as for SSE (403, 400, 410, 429): the client gets a status,
// not a closed socket. A browser's Origin must be the request's own host or one
// of Options.AllowedOrigins.
func (h *Hub) WebSocketHandler(o Options) http.Handler {
	o = o.withDefaults()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, turn := h.attach(w, r, o)
		if sub == nil {
			return
		}
		defer sub.Close()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:    o.Subprotocols,
			OriginPatterns:  o.AllowedOrigins,
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			// Accept has already written the refusal (a bad Origin is 403).
			h.cfg.Logger.Info("websocket upgrade refused", "turn_id", turn.id)
			return
		}
		c.SetReadLimit(maxClientMessage)
		if sub.Complete() {
			_ = c.Close(CloseDone, "complete")
			return
		}
		// CloseRead keeps reading so that pings, pongs and close frames are
		// handled, and closes the connection if the client sends data.
		ctx := c.CloseRead(r.Context())
		h.wsLoop(ctx, c, sub, turn, o)
	})
}

func (h *Hub) wsLoop(ctx context.Context, c *websocket.Conn, sub *Subscription, turn *Turn, o Options) {
	for {
		nctx, cancel := context.WithTimeout(ctx, o.Heartbeat)
		f, err := sub.Next(nctx)
		cancel()
		switch {
		case err == nil:
			wctx, wcancel := context.WithTimeout(ctx, o.WriteTimeout)
			err := c.Write(wctx, websocket.MessageText, f.Data)
			wcancel()
			if err != nil {
				return // the library has closed the connection
			}
		case ctx.Err() != nil:
			_ = c.CloseNow()
			return
		case errors.Is(err, context.DeadlineExceeded):
			pctx, pcancel := context.WithTimeout(ctx, o.WriteTimeout)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case errors.Is(err, io.EOF):
			_ = c.Close(CloseDone, "done")
			return
		case errors.Is(err, ErrSlowConsumer):
			h.cfg.Logger.Info("stream subscriber cut off", "turn_id", turn.id, "reason", string(ReasonSlowConsumer))
			_ = c.Close(CloseSlowConsumer, string(ReasonSlowConsumer))
			return
		default:
			h.cfg.Logger.Info("stream ended without done", "turn_id", turn.id, "reason", string(ReasonOf(err)))
			_ = c.Close(CloseAborted, string(ReasonAborted))
			return
		}
	}
}
