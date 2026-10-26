//go:build noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/providers/voice"
)

func dictationExternalLayer() (bool, string) { return false, "" }

func dictationExternalOnlySTT() voice.STT { return nil }

func dictationPrepareExternal(ctx context.Context) context.Context { return ctx }
