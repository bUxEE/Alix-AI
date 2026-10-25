package main

import (
	"os"

	"github.com/buxee/alix-ai/benchmarks/repro/internal/repro"
)

func main() { os.Exit(repro.RunCompare(os.Args[1:], os.Stdout, os.Stderr)) }
