// Package log builds AlixAI's structured logger: log/slog with a JSON handler
// and a redaction layer, so that secrets never reach a log line (master plan
// §16.2). Redaction is part of the handler, not a convention: code that logs
// through a logger from New cannot bypass it.
//
// Two things are redacted. Values whose key looks sensitive (token, secret,
// password, authorization, api_key, cookie and close relatives, plus the keys
// listed in the log.redact_keys config) are replaced whatever their type, and
// that includes anything inside a group or map under such a key. And strings
// are scrubbed for bearer tokens, JWTs, credential headers, key=value pairs,
// URL userinfo and PEM private keys, wherever they appear: the message,
// attribute values, error text and nested values.
package log

import (
	"io"
	"log/slog"
	"os"

	"github.com/buxee/alix-ai/internal/config"
)

// New returns a JSON logger writing to stderr, configured from cfg.
func New(cfg config.LogConfig) *slog.Logger {
	return NewWithWriter(cfg, os.Stderr)
}

// NewWithWriter is New with an explicit destination.
func NewWithWriter(cfg config.LogConfig, w io.Writer) *slog.Logger {
	return slog.New(NewHandler(cfg, w))
}

// NewHandler returns the redacting JSON handler. An unparseable cfg.Level falls
// back to info; config validation rejects it long before logging starts.
func NewHandler(cfg config.LogConfig, w io.Writer) slog.Handler {
	level, err := cfg.SlogLevel()
	if err != nil {
		level = slog.LevelInfo
	}
	red := NewRedactor(cfg.RedactKeys)
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: red.replaceAttr,
	})
}
