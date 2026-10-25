// Command fakellm is the stand-in LocalAI the supervisor tests spawn. The tests
// build it with the Go toolchain. It listens on nothing and calls nothing: it
// records its start in a state directory, signals readiness with a ready-<pid> file, and
// then either runs until it is stopped or exits with status 3 on cue.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	state := flag.String("state", "", "state directory (required)")
	crashRuns := flag.Int("crash-runs", 0, "runs numbered up to this crash")
	crashAfter := flag.Duration("crash-after", 30*time.Millisecond, "a crashing run exits this long after start")
	crashOnFile := flag.String("crash-on-file", "", "a crashing run exits once this file exists, instead of on a timer")
	readyAfter := flag.Duration("ready-after", 10*time.Millisecond, "time to readiness")
	dumpEnv := flag.String("dump-env", "", "write the names of the environment variables to this file")
	maxLife := flag.Duration("max-life", 2*time.Minute, "exit by itself after this long, so a leaked process cannot linger")
	hold := flag.Duration("hold", 0, "only sleep this long and exit: a backend process that keeps the output it inherited open")
	spawnHolder := flag.Bool("spawn-holder", false, "start a -hold process that inherits this one's output, then exit with status 3")
	flag.Parse()
	if *hold > 0 {
		time.Sleep(*hold)
		return
	}
	if *spawnHolder {
		self, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		c := exec.Command(self, "-hold", "60s")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if c.Start() != nil {
			os.Exit(2)
		}
		os.Exit(3)
	}
	if *state == "" {
		fmt.Fprintln(os.Stderr, "fakellm: -state is required")
		os.Exit(2)
	}

	starts := filepath.Join(*state, "starts")
	f, err := os.OpenFile(starts, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close()
	b, _ := os.ReadFile(starts)
	run := strings.Count(string(b), "\n")

	ready := filepath.Join(*state, fmt.Sprintf("ready-%d", os.Getpid()))
	if *dumpEnv != "" {
		var names []string
		for _, kv := range os.Environ() {
			names = append(names, strings.SplitN(kv, "=", 2)[0])
		}
		// Written aside and renamed, so a reader that sees the file sees all of it.
		if err := os.WriteFile(*dumpEnv+".tmp", []byte(strings.Join(names, "\n")), 0o600); err == nil {
			_ = os.Rename(*dumpEnv+".tmp", *dumpEnv)
		}
	}

	time.Sleep(*readyAfter)
	_ = os.WriteFile(ready, []byte("ok"), 0o600)

	crashing := run <= *crashRuns
	die := func() { os.Exit(3) }
	var timer <-chan time.Time
	if crashing && *crashOnFile == "" {
		timer = time.After(*crashAfter)
	}
	life := time.After(*maxLife)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-timer:
			die()
		case <-tick.C:
			if crashing && *crashOnFile != "" {
				if _, err := os.Stat(*crashOnFile); err == nil {
					die()
				}
			}
		case <-life:
			os.Exit(0)
		}
	}
}
