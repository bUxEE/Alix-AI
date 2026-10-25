package parse

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/parsetest"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// TestParseFootprint measures what parsing costs, per format: the worker's
// peak memory as the host sees it, parse throughput inside the worker, the
// end-to-end time a caller waits (process start included), and what starting a
// process for every document costs against reusing one worker for many. The
// figures go in docs/efficiency/M2-04.md.
//
// A plain `go test` runs a quick version (one 100 KiB document per format, one
// run each, a couple of seconds on Linux, more on Windows where every process
// start is slow) so that every format is exercised through a real worker and
// the memory ceiling is checked. The measurement itself is switched on with
// ALIXAI_PROBE_PARSE, a comma-separated list:
//
//	full     100 KiB and 1 MiB documents, three runs each, the idle worker and the
//	         process-per-document comparison (about 40 s)
//	large    everything in full plus an 8 MiB document per format (about 1 minute)
//	reps=N   N runs of each measurement (default 3 under full, 1 otherwise)
//
//	ALIXAI_PROBE_PARSE=full go test -run TestParseFootprint -v ./internal/rag/parse/
//	ALIXAI_PROBE_PARSE=large,reps=5 go test -run TestParseFootprint -v ./internal/rag/parse/
//
// The assertions are ceilings that hold on any host (memory stays inside the
// budget, every parse succeeds); the figures are logged, never asserted,
// because they depend on the machine.
func TestParseFootprint(t *testing.T) {
	sizes, reps, full, large, repsFlag := []int{100 << 10}, 1, false, false, 0
	for _, opt := range strings.Split(os.Getenv("ALIXAI_PROBE_PARSE"), ",") {
		switch {
		case opt == "full":
			full = true
		case opt == "large":
			full, large = true, true
		case strings.HasPrefix(opt, "reps="):
			if n, err := strconv.Atoi(strings.TrimPrefix(opt, "reps=")); err == nil && n > 0 && n <= 50 {
				repsFlag = n
			}
		}
	}
	if full {
		sizes, reps = []int{100 << 10, 1 << 20}, 3
	}
	if large {
		sizes = append(sizes, 8<<20)
	}
	if repsFlag > 0 {
		reps = repsFlag
	}
	cfg := func(c *Config) {
		c.MaxOutputBytes = 32 << 20
		c.MaxInputBytes = 64 << 20
		c.MaxBlocks = 2_000_000
	}
	r := newTestRunner(t, cfg)

	rep, err := r.Probe(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("environment: %s, %d CPUs, %s; sandbox %s host=%v worker=%v", rep.Platform, runtime.NumCPU(), runtime.Version(), rep.Level, rep.Host, rep.Worker)

	// An idle worker: started, confined, handshake done, nothing parsed.
	{
		var peaks, sets []int64
		var spawns []time.Duration
		for i := 0; i < reps; i++ {
			t0 := time.Now()
			w, err := r.start(ctx(t))
			if err != nil {
				t.Fatal(err)
			}
			spawn := time.Since(t0)
			p, ws, kind := w.conf.memory()
			w.finish()
			peaks, sets, spawns = append(peaks, p), append(sets, ws), append(spawns, spawn)
			if i == 0 {
				t.Logf("idle worker: peak %s %.1f MiB, working set %.1f MiB", kind, mib(p), mib(ws))
			}
		}
		t.Logf("idle worker over %d starts: spawn to verified sandbox median %s (min %s, max %s); peak median %.1f MiB, working set median %.1f MiB",
			reps, median(spawns), slices.Min(spawns), slices.Max(spawns), mib(medianInt(peaks)), mib(medianInt(sets)))
	}

	t.Logf("| format | input | text out | blocks | worker parse | parse MB/s (input) | host and transfer | end to end | peak %s | working set |", peakName())
	t.Logf("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, f := range []wire.Format{wire.FormatTXT, wire.FormatMD, wire.FormatHTML, wire.FormatCSV, wire.FormatDOCX, wire.FormatPDF} {
		for _, size := range sizes {
			doc := footprintDoc(t, f, size)
			var parse, total, hostCost []time.Duration
			var peak, set int64
			var last *Result
			for i := 0; i < reps; i++ {
				res, err := r.Parse(ctx(t), f, doc)
				if err != nil {
					t.Fatalf("%s %d bytes: %v", f, len(doc), err)
				}
				parse = append(parse, time.Duration(res.Stats.ParseNanos))
				total = append(total, res.Stats.Total)
				// What the host and the pipes add: sending the document, reading and
				// validating the result. Start-up (Spawn) and the worker's parse are
				// the other two parts of the end-to-end time.
				hostCost = append(hostCost, res.Stats.Total-res.Stats.Spawn-time.Duration(res.Stats.ParseNanos))
				peak = max(peak, res.Stats.PeakMemoryBytes)
				set = max(set, res.Stats.PeakWorkingSetBytes)
				last = res
			}
			if peak > r.cfg.MemoryBytes {
				t.Errorf("%s %d bytes: peak %d is over the budget %d", f, len(doc), peak, r.cfg.MemoryBytes)
			}
			pm := median(parse)
			mbps := float64(len(doc)) / (1 << 20) / pm.Seconds()
			t.Logf("| %s | %s | %s | %d | %s | %.1f | %s | %s | %.0f MiB | %.0f MiB |",
				f, size2(len(doc)), size2(len(last.Text)), len(last.Blocks), pm.Round(time.Millisecond), mbps,
				median(hostCost).Round(time.Millisecond), median(total).Round(time.Millisecond), mib(peak), mib(set))
		}
	}

	if !full {
		return
	}
	// One process per document against one worker for many.
	const n = 30
	small := footprintDoc(t, wire.FormatMD, 4<<10)
	t0 := time.Now()
	var spawn []time.Duration
	for i := 0; i < n; i++ {
		res, err := r.Parse(ctx(t), wire.FormatMD, small)
		if err != nil {
			t.Fatal(err)
		}
		spawn = append(spawn, res.Stats.Spawn)
	}
	fresh := time.Since(t0)

	w, err := r.start(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	t0 = time.Now()
	for i := 0; i < n; i++ {
		if _, err := w.run(ctx(t), string(wire.FormatMD), small); err != nil {
			t.Fatal(err)
		}
	}
	reused := time.Since(t0)
	w.finish()
	t.Logf("%d documents of %s: a process each %s (%s per document, spawn median %s); one worker for all %s (%s per document)",
		n, size2(len(small)), fresh.Round(time.Millisecond), (fresh / n).Round(time.Microsecond), median(spawn).Round(time.Microsecond),
		reused.Round(time.Millisecond), (reused / n).Round(time.Microsecond))
}

func peakName() string {
	if runtime.GOOS == "windows" {
		return "commit"
	}
	return "RSS"
}

func mib(b int64) float64 { return float64(b) / (1 << 20) }

func size2(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
}

func median(d []time.Duration) time.Duration {
	c := slices.Clone(d)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

func medianInt(d []int64) int64 {
	c := slices.Clone(d)
	slices.Sort(c)
	return c[len(c)/2]
}

// footprintDoc builds a document of the format whose extracted text is about
// size bytes (the PDF and DOCX inputs are a little different in size because of
// compression and markup). Deterministic prose, no hidden text.
func footprintDoc(t *testing.T, f wire.Format, size int) []byte {
	t.Helper()
	words := strings.Fields("the of and a to in is you that it he was for on are as with his they at be this from I have or by one had not but what all were when we there can an your which their said if do will each about how up out them then she many some so these would other into has more her two like him see time could no make than first been its who now people my made over did down only way find use may water long little very after words called just where most know get through back much before go good new write our me man too any day same right look think also around another came come work three word must because does part even place well such here take why help put different away again off went old number great tell men say small every found still between name should home big give air line set own under read last never us left end along while might next sound below saw something thought both few those always looked show large often together asked house don't world going want school important until form food keep children feet land side without boy once animals life enough took sometimes four head above kind began almost live page got earth need far hand high year mother light country father let night picture being study second soon story since white ever paper hard near sentence better best across during today however sure knew try told young sun thing whole hear example heard several change answer room sea against top turned learn point city play toward five using himself usually")
	next := func() func() string {
		var s uint64 = 0x9E3779B97F4A7C15
		return func() string {
			s = s*6364136223846793005 + 1442695040888963407
			return words[int(s>>33)%len(words)]
		}
	}()
	paras := 0
	para := func() string {
		paras++
		n := 40 + paras*7%50
		w := make([]string, n)
		for i := range w {
			w[i] = next()
		}
		w[0] = strings.ToUpper(w[0][:1]) + w[0][1:]
		return strings.Join(w, " ") + "."
	}
	var b strings.Builder
	switch f {
	case wire.FormatTXT:
		for b.Len() < size {
			b.WriteString(para() + "\n\n")
		}
	case wire.FormatMD:
		for i := 0; b.Len() < size; i++ {
			if i%12 == 0 {
				fmt.Fprintf(&b, "## Section %d\n\n", i/12)
			}
			if i%5 == 3 {
				for j := 0; j < 4; j++ {
					b.WriteString("- " + next() + " " + next() + " " + next() + " " + next() + "\n")
				}
				b.WriteString("\n")
				continue
			}
			b.WriteString(para() + "\n\n")
		}
	case wire.FormatHTML:
		b.WriteString("<!doctype html><html><head><title>Footprint</title><style>p{margin:0}</style></head><body>")
		for i := 0; b.Len() < size; i++ {
			if i%12 == 0 {
				fmt.Fprintf(&b, "<h2>Section %d</h2>", i/12)
			}
			fmt.Fprintf(&b, "<p>%s <b>%s</b> <a href=\"/x/%d\">%s</a></p>\n", para(), next(), i, next())
		}
		b.WriteString("</body></html>")
	case wire.FormatCSV:
		b.WriteString("id,name,region,quantity,note\n")
		for i := 0; b.Len() < size; i++ {
			fmt.Fprintf(&b, "%d,%s %s,%s,%d,%s %s %s\n", i, next(), next(), next(), i*7%1000, next(), next(), next())
		}
	case wire.FormatDOCX:
		var body strings.Builder
		for body.Len() < size {
			body.WriteString(parsetest.Para(para()))
		}
		return parsetest.DOCX(t, body.String(), nil)
	case wire.FormatPDF:
		var pages []string
		words := strings.Fields(strings.Repeat(para()+" ", 4))
		for total := 0; total < size; {
			var pg strings.Builder
			for line := 0; line < 45; line++ {
				l := make([]string, 11)
				for i := range l {
					l[i] = words[(line*11+i+len(pages))%len(words)]
				}
				text := strings.Join(l, " ")
				fmt.Fprintf(&pg, "BT /F1 11 Tf 72 %d Td (%s) Tj ET\n", 740-line*15, text)
				total += len(text) + 1
			}
			pages = append(pages, pg.String())
		}
		return parsetest.SimplePDF(pages...)
	}
	return []byte(b.String())
}
