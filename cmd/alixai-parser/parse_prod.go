//go:build !parsertest

package main

import (
	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// parse is the production dispatcher: a declared format or nothing. The test
// build (tag parsertest) adds fault-injection "formats"; this one has none, and
// a test asserts that.
func parse(req wire.Request, body []byte) (*formats.Output, error) {
	if !req.Format.Valid() {
		return nil, &formats.Error{Reason: wire.ReasonBadRequest, Message: "unsupported format"}
	}
	return formats.Parse(req.Format, body, req.Limits)
}
