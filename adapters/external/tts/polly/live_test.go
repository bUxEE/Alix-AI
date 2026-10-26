//go:build live

package polly

import "testing"

// Live calls against real AWS credentials are human-gated. Set the credential
// as ACCESS_KEY_ID:SECRET_ACCESS_KEY (or …:SESSION_TOKEN) and run:
// go test -tags live ./adapters/external/tts/polly -run Live
func TestLiveSkippedByDefault(t *testing.T) {
	t.Skip("live Amazon Polly TTS: set credentials and remove this skip deliberately")
}
