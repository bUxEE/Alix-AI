package aguigold

import (
	"context"
	"net/http"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// rigEventsIntakes stands in for the application-event intake this rig does not run: the gateway refuses a route area that is half wired
// (v1_events needs EventsIntakes and the feed next to the ApplicationService the rig provides), and this rig does not exercise events.
type rigEventsIntakes struct{}

var _ gateway.EventsIntakes = rigEventsIntakes{}

func (rigEventsIntakes) EventsHandler(context.Context, string) (http.Handler, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "this rig runs no events intake")
}
