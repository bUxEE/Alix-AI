// Command alixai is the single AlixAI service binary. All behaviour lives in
// internal/cli; this file only hands over the process.
package main

import (
	"os"

	"github.com/buxee/alix-ai/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
