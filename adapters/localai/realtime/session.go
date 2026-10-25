package realtime

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	pv "github.com/buxee/alix-ai/internal/providers/voice"
)

// Client event types the adapter sends (LocalAI v4.10.0 realtime.go). The
// adapter never sends conversation.item.create, response.create or any
// tool-result event: with nothing to answer, the runtime has nothing to say.
const (
	evSessionUpdate = "session.update"
	evAudioAppend   = "input_audio_buffer.append"
	evAudioCommit   = "input_audio_buffer.commit"
	evAudioClear    = "input_audio_buffer.clear"
)

// Server event types the adapter reads.
const (
	evSessionCreated = "session.created"
	evSessionUpdated = "session.updated"
	evError          = "error"
	evSpeechStarted  = "input_audio_buffer.speech_started"
	evCommitted      = "input_audio_buffer.committed"
	evCleared        = "input_audio_buffer.cleared"
	evTransDelta     = "conversation.item.input_audio_transcription.delta"
	evTransCompleted = "conversation.item.input_audio_transcription.completed"
	evTransFailed    = "conversation.item.input_audio_transcription.failed"
	evItemAdded      = "conversation.item.added"
	evItemDone       = "conversation.item.done"
	evItemCreated    = "conversation.item.created"
)

// sessionUpdate builds the one session.update the adapter sends, before any
// audio. It switches the session to a transcription session: LocalAI's
// transcription-only session has no answering stage at all (the runtime
// routes it to updateTransSession and never calls generateResponse for it), so
// the model cannot answer or call tools by construction. The rest is defence in
// depth for a runtime that ignores the session type:
//
//   - tool_choice is "none" and the tool list is empty;
//   - turn detection is either null (the client commits) or server VAD with
//     create_response and interrupt_response off, so a detected turn becomes a
//     transcript and nothing else;
//   - the client later treats any response, function-call or MCP event as a
//     violation and closes (see sideDoor).
func sessionUpdate(model, language string, f pv.AudioFormat, serverVAD bool, silenceMs int) ([]byte, error) {
	var turnDetection any // nil marshals to null: the client commits
	if serverVAD {
		turnDetection = map[string]any{
			"type":                "server_vad",
			"create_response":     false,
			"interrupt_response":  false,
			"silence_duration_ms": silenceMs,
		}
	}
	transcription := map[string]any{"model": model}
	if language != "" {
		transcription["language"] = language
	}
	return json.Marshal(map[string]any{
		"type": evSessionUpdate,
		"session": map[string]any{
			"type":        "transcription",
			"tools":       []any{},
			"tool_choice": "none",
			"audio": map[string]any{
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcm", "rate": f.SampleRate},
					"transcription":  transcription,
					"turn_detection": turnDetection,
				},
			},
		},
	})
}

func appendEvent(audio []byte) ([]byte, error) {
	return json.Marshal(map[string]any{"type": evAudioAppend, "audio": base64.StdEncoding.EncodeToString(audio)})
}

func bareEvent(t string) []byte { return []byte(`{"type":"` + t + `"}`) }

// serverEvent is the subset of a server event the adapter reads.
type serverEvent struct {
	Type       string      `json:"type"`
	ItemID     string      `json:"item_id"`
	Delta      string      `json:"delta"`
	Transcript string      `json:"transcript"`
	Error      *errorEvent `json:"error"`
	Item       *struct {
		Type string `json:"type"`
		Role string `json:"role"`
	} `json:"item"`
	Session *struct {
		Type string `json:"type"`
	} `json:"session"`
}

func parseServerEvent(data []byte) (serverEvent, error) {
	var ev serverEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return serverEvent{}, schemaInvalid("the runtime sent a malformed event")
	}
	if ev.Type == "" {
		return serverEvent{}, schemaInvalid("the runtime sent an event with no type")
	}
	return ev, nil
}

// isSideDoor reports whether ev is something only an answering session emits:
// a response, a function or MCP call, or an assistant or tool item. The
// transcription path of the runtime emits only user items.
func isSideDoor(ev serverEvent) bool {
	t := ev.Type
	if strings.HasPrefix(t, "response.") || strings.HasPrefix(t, "mcp_") || strings.Contains(t, "function_call") || strings.Contains(t, "mcp_call") {
		return true
	}
	switch t {
	case evItemAdded, evItemDone, evItemCreated:
		if ev.Item != nil {
			switch ev.Item.Type {
			case "message", "":
				return ev.Item.Role != "" && ev.Item.Role != "user"
			}
			return true
		}
	}
	return false
}
