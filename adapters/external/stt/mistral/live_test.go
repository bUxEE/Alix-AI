//go:build live

package mistral

import "testing"

// Live calls against a real Mistral key are human-gated. Set Mistral_API_KEY and
// run: go test -tags live ./adapters/external/stt/Mistral -run Live
func TestLiveSkippedByDefault(t *testing.T) {
	t.Skip("live Mistral STT: set credentials and remove this skip deliberately")
}
