// Package spike holds the M0-13 live runtime spike: tests that need a running LocalAI container, downloaded models and a
// built alixai binary. Everything that does is behind the `live` build tag; the one hermetic test here keeps the spike's
// model definitions in step with the model registry.
//
// Run order (the commands are in docs/evidence/m0-spike/README.md):
//
//	stack.sh up lite            start LocalAI with the spike overrides (tests/live/spike/localai)
//	go build -o bin/alixai ./cmd/alixai
//	ALIXAI_LOCALAI_URL=http://127.0.0.1:8080 ALIXAI_LOCALAI_KEY=... \
//	ALIXAI_SPIKE_BIN=bin/alixai ALIXAI_SPIKE_CONTAINER=alixai-localai \
//	  go test -tags live -count=1 -v ./tests/live/spike/
//
// The tests drive the alixai binary (`alixai serve`) and plain HTTP to the runtime. They never import adapters/localai:
// only the adapter and the composition roots may (docs/architecture/module-map.md, rule 2).
package spike
