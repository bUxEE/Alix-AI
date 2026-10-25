package main

import (
	"fmt"
	"os"
)

const usage = `usage: voicebench <command> [flags]

  fetch-fleurs   download the pinned FLEURS clips and write their manifest
  run            measure one candidate in one phase (-phase stt|vad|tts) against a running LocalAI
  check          validate .voice results and re-derive every aggregate from their records
  verdict        apply the pre-registered gates of docs/evidence/m4-voice/protocol.md
  companion      write the bench-envelope companion of a vad result (accepted by "bench check")
  coresident     the Lite co-residency sequence: controller, embedder and voice workers together

Run a command with -h for its flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "fetch-fleurs":
		err = fetchFleursCommand(args)
	case "run":
		err = runCommand(args)
	case "check":
		err = checkCommand(args)
	case "verdict":
		err = verdictCommand(args)
	case "companion":
		err = companionCommand(args)
	case "coresident":
		err = coresidentCommand(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "voicebench: %v\n", err)
		os.Exit(1)
	}
}
