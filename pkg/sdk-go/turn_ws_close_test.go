package sdkgo_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// A WebSocket the server closes with 1011 (the turn was abandoned) or 1008 (a protocol violation: the client never sends) ends
// the stream with a typed error that is not resumed, as in the TypeScript SDK; 1013 (slow consumer) is a lost connection that is.
func TestWebSocketCloseCodesThatEndTheStream(t *testing.T) {
	cases := []struct {
		code    websocket.StatusCode
		errCode contracts.ErrCode
		reason  string
		opens   int64
	}{
		{websocket.StatusInternalError, contracts.ErrUnavailable, "aborted", 1},
		{websocket.StatusPolicyViolation, contracts.ErrInternal, "protocol", 1},
		{websocket.StatusTryAgainLater, contracts.ErrUncertainOutcome, "stream_ended_early", 3},
	}
	for _, c := range cases {
		t.Run(c.code.String(), func(t *testing.T) {
			var opens atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/sessions/ses-1/turns/trn_x/events" {
					http.NotFound(w, r)
					return
				}
				opens.Add(1)
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{sdkgo.WebSocketSubprotocol}})
				if err != nil {
					return
				}
				_ = conn.Close(c.code, "closed by the test")
			}))
			t.Cleanup(srv.Close)
			client, err := sdkgo.NewClient(sdkgo.ClientOptions{
				BaseURL:         srv.URL,
				GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) { return "ctx.token", nil },
				MaxResumes:      2,
				ResumeBackoff:   time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := client.StreamTurnEvents(context.Background(), "ses-1", "trn_x", sdkgo.WithCursor(0), sdkgo.WithWebSocket())
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if stream.Next() {
				t.Fatalf("an event arrived: %+v", stream.Event())
			}
			var ce *contracts.Error
			if !errors.As(stream.Err(), &ce) || ce.Code != c.errCode || ce.Details["reason"] != c.reason {
				t.Fatalf("err = %#v, want %s with reason %s", stream.Err(), c.errCode, c.reason)
			}
			if c.code != websocket.StatusTryAgainLater && sdkgo.IsRetryable(stream.Err()) {
				t.Error("a close that ends the stream is reported as retryable")
			}
			if got := opens.Load(); got != c.opens {
				t.Errorf("the events route was opened %d times, want %d", got, c.opens)
			}
		})
	}
}
