package dctx

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
)

// markerIn returns the turn numbers of the unique markers (MARK-U-0007, MARK-A-0007) the
// text contains.
var markerRE = regexp.MustCompile(`MARK-[UA]-(\d{4})`)

func markersIn(s string) []int {
	var out []int
	for _, m := range markerRE.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// spySource passes RecentTurns through to the real store and records how it was called.
// It implements TurnSource and nothing more: the builder has no other way to read history.
type spySource struct {
	src    TurnSource
	calls  int
	limits []int
	rows   int
}

func (s *spySource) RecentTurns(ctx context.Context, sess sessions.Session, limit int) ([]sessions.Turn, error) {
	s.calls++
	s.limits = append(s.limits, limit)
	out, err := s.src.RecentTurns(ctx, sess, limit)
	s.rows += len(out)
	return out, err
}

func TestOldTurnsNotReplayed(t *testing.T) {
	ctx := context.Background()
	b := newBuilder(t, ProfileLite)
	p := b.Profile()
	c := newConvo(t)
	var sess sessions.Session
	for i := 1; i <= 500; i++ {
		sess = c.step(i)
	}

	// The transcript holds the whole conversation; the model's context does not read it.
	rows, err := c.store.TranscriptLen(ctx, c.scope)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1000 {
		t.Fatalf("the transcript holds %d rows, want 1000 (500 turns of two rows)", rows)
	}

	spy := &spySource{src: c.store}
	window, err := b.Window(ctx, spy, sess)
	if err != nil {
		t.Fatal(err)
	}
	if spy.calls != 1 || spy.limits[0] != p.CandidateRows {
		t.Fatalf("Window read the source %d times with limits %v, want one read of %d rows", spy.calls, spy.limits, p.CandidateRows)
	}
	if len(window) != p.CandidateRows {
		t.Fatalf("the window has %d rows, want the %d candidate rows", len(window), p.CandidateRows)
	}
	if first := markersIn(window[0].Body); len(first) == 0 || first[0] < 500-p.CandidateRows/2-1 {
		t.Fatalf("the window starts at %v: it should hold only the newest turns", first)
	}

	// A message that shares words with most rows of the window, the worst case for replay.
	in := baseInput("do you have anything about gardening or bread baking")
	in.State, in.Turns = sess.State, window
	built := mustBuild(t, b, in)
	independentCheck(t, b, built)

	var replayed []int
	turnItems := 0
	for _, it := range built.Items {
		if it.Kind != ItemTurn {
			continue
		}
		turnItems++
		replayed = append(replayed, markersIn(it.Content)...)
	}
	if turnItems == 0 || turnItems > p.MaxTurnRows {
		t.Fatalf("%d turn rows replayed, want between 1 and %d", turnItems, p.MaxTurnRows)
	}
	all := markersIn(content(built))
	for _, n := range all {
		if n <= 500-p.CandidateRows/2 {
			t.Errorf("turn %d of 500 is in the prompt; only the last %d turns are candidates", n, p.CandidateRows/2)
		}
	}
	for _, old := range []string{mark(1, "U"), mark(1, "A"), mark(10, "U"), mark(100, "A"), mark(250, "U"), mark(479, "A")} {
		if strings.Contains(content(built), old) {
			t.Errorf("%s is in the prompt: an old turn was replayed", old)
		}
	}
	if !strings.Contains(content(built), mark(500, "A")) {
		t.Errorf("the newest reply %s is missing: the question being answered must be replayed", mark(500, "A"))
	}
	t.Logf("500 turns, %d transcript rows: the prompt replays %d rows (turns %v)", rows, turnItems, replayed)

	// A session that never had a turn reads nothing at all.
	empty := &spySource{src: c.store}
	if w, err := b.Window(ctx, empty, sessions.Session{Scope: c.scope}); err != nil || len(w) != 0 || empty.calls != 0 {
		t.Fatalf("an empty session read %d times (%v, %v)", empty.calls, w, err)
	}
	// A window longer than the candidate rows is cut to the newest ones, so the work is
	// bounded by the profile and not by what a source returns.
	long := heavyTurns(200)
	in = baseInput("hello")
	in.Turns = long
	if c := mustBuild(t, b, in); c.Report.RowsConsidered != p.CandidateRows {
		t.Fatalf("%d rows considered of 200, want %d", c.Report.RowsConsidered, p.CandidateRows)
	}

	t.Run("selection is by relevance and not by recency alone", func(t *testing.T) {
		var window []sessions.Turn
		for i := 1; i <= 12; i++ {
			body := fmt.Sprintf("smalltalk number %d about the weather", i)
			switch i {
			case 3:
				body = "I am looking for a book about sourdough bread baking"
			case 5:
				body = "the title I picked is on shelf twelve"
			}
			window = append(window,
				sessions.Turn{TurnID: fmt.Sprintf("trn-%d", i), N: int64(i), Role: "user", Body: body},
				sessions.Turn{TurnID: fmt.Sprintf("trn-%d", i), N: int64(i), Role: "assistant", Body: fmt.Sprintf("reply %d unrelated", i)})
		}
		in := baseInput("is that sourdough book still available")
		in.Turns = window
		got := content(mustBuild(t, b, in))
		if !strings.Contains(got, "sourdough bread baking") {
			t.Error("an old turn that shares words with the message was not replayed")
		}
		if strings.Contains(got, "smalltalk number 7") || strings.Contains(got, "reply 8 unrelated") {
			t.Error("unrelated rows in the middle of the window were replayed")
		}
		if !strings.Contains(got, "reply 12 unrelated") {
			t.Error("the newest row was not replayed")
		}

		// A turn the state points at is replayed even with no shared word.
		in.State.Goal = &sessions.Goal{CapabilityID: "reservations.create", Phase: "open", Slots: map[string]sessions.Slot{
			"title": {Value: "t_9", Origin: sessions.OriginUserTurn, TurnID: "trn-5"},
		}}
		got = content(mustBuild(t, b, in))
		if !strings.Contains(got, "shelf twelve") {
			t.Error("the turn that set a slot of the goal was not replayed")
		}
	})

	t.Run("no reference to the transcript in the package source", func(t *testing.T) {
		banned := map[string]bool{"Transcript": true, "TranscriptTurn": true, "TranscriptLen": true, "ClearTranscript": true, "PruneTranscript": true, "MaxTranscriptRunes": true}
		var files int
		err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			files++
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if banned[x.Sel.Name] {
						t.Errorf("%s: references %s: the context path must not read the transcript", path, x.Sel.Name)
					}
				case *ast.Ident:
					if banned[x.Name] {
						t.Errorf("%s: references %s: the context path must not read the transcript", path, x.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if files < 4 {
			t.Fatalf("scanned %d source files; the scan is not looking at the package", files)
		}
	})
}

// heapInUse returns the live heap after a collection.
func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// SEC-076: a 500-turn history stays within budget, in prompt size and in memory.
func TestLongHistory500TurnsBounded(t *testing.T) {
	b := newBuilder(t, ProfileLite)
	p := b.Profile()
	c := newConvo(t)
	utterance := "reserve the second one for me, I will collect it next Friday"

	type point struct {
		turn         int
		rep          Report
		heap         uint64
		allocPerTurn uint64
		naive        int
		last6        int // the rows of the M1-14 engine window: the last 6 rows, clipped, no selection
	}
	var pts []point
	checkpoints := map[int]bool{5: true, 50: true, 500: true}
	var maxEst, maxHist int
	for i := 1; i <= 500; i++ {
		c.step(i)
		built := c.build(b, utterance)
		independentCheck(t, b, built)
		maxEst, maxHist = max(maxEst, built.Report.EstTokens), max(maxHist, built.Report.HistoryToks)
		if !checkpoints[i] {
			continue
		}

		// Allocation of the context path of one turn at this point (read the session, read the
		// window, build), averaged over 50 turns.
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		for range 50 {
			c.build(b, utterance)
		}
		runtime.ReadMemStats(&m1)

		// What replaying the whole transcript would cost, for the efficiency review: every
		// transcript row, fenced, with no selection and no cap.
		naive := built.Report.FrameToks + built.Report.ConstraintToks + built.Report.UserToks
		for after := int64(0); ; {
			page, err := c.store.Transcript(context.Background(), c.scope, after, sessions.MaxTranscriptPage)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			for _, r := range page {
				naive += b.msgToks(fence(tagEarlier, r.Body))
			}
			after = page[len(page)-1].N
		}
		// The replay window of the M1-14 engine: the last six rows, clipped, whatever they say.
		sess, err := c.store.Open(context.Background(), c.scope)
		if err != nil {
			t.Fatal(err)
		}
		recent, err := c.store.RecentTurns(context.Background(), sess, 6)
		if err != nil {
			t.Fatal(err)
		}
		var last6 int
		for _, r := range recent {
			tag := tagEarlier
			if r.Role == "assistant" {
				tag = tagReply
			}
			last6 += b.msgToks(fence(tag, clipRunes(sanitize(r.Body), 240)))
		}
		pts = append(pts, point{turn: i, rep: built.Report, heap: heapInUse(), allocPerTurn: (m1.TotalAlloc - m0.TotalAlloc) / 50, naive: naive, last6: last6})
	}
	if len(pts) != 3 {
		t.Fatalf("%d checkpoints measured", len(pts))
	}

	for _, pt := range pts {
		r := pt.rep
		t.Logf("turn %3d: est_toks=%4d bytes=%5d (frame %d, constraint %d, state %d, turns %d, user %d) history=%3d rows=%d capsules=%d+%d suspended=%d trimmed=%v | heap=%.2f MiB, %.1f KiB allocated per turn (session read + window + build) | replaying the whole transcript would be %d est_toks, the last 6 rows alone %d",
			pt.turn, r.EstTokens, r.Bytes, r.FrameToks, r.ConstraintToks, r.StateToks, r.TurnToks, r.UserToks, r.HistoryToks, r.TurnRows,
			r.CapsulesFull, r.CapsulesDigested, r.Suspended, r.Trimmed, float64(pt.heap)/(1<<20), float64(pt.allocPerTurn)/1024, pt.naive, pt.last6)
	}
	t5, t50, t500 := pts[0], pts[1], pts[2]

	// Every turn of the 500 was within budget (checked in the loop); the maxima are the claim.
	if maxEst > p.MaxPromptTokens || maxHist > p.MaxHistoryTokens {
		t.Fatalf("max est tokens %d (cap %d), max history %d (cap %d)", maxEst, p.MaxPromptTokens, maxHist, p.MaxHistoryTokens)
	}
	t.Logf("over all 500 turns: max est_toks=%d of %d, max history=%d of %d", maxEst, p.MaxPromptTokens, maxHist, p.MaxHistoryTokens)

	// The plateau is the claim: turn 500 costs no more than turn 50, within one turn row.
	slack := p.MaxRowRunes/2 + 64
	if t500.rep.EstTokens > t50.rep.EstTokens+slack {
		t.Errorf("the prompt grows: %d est tokens at turn 500 against %d at turn 50", t500.rep.EstTokens, t50.rep.EstTokens)
	}
	if t500.rep.EstTokens > p.MaxPromptTokens {
		t.Errorf("turn 500 prompt is %d est tokens, over the %d budget", t500.rep.EstTokens, p.MaxPromptTokens)
	}
	// Replaying the transcript would have blown the budget: the cap is what held it.
	if t500.naive <= p.MaxPromptTokens {
		t.Errorf("the whole-transcript baseline (%d) fits the budget (%d): the test does not show the cap working", t500.naive, p.MaxPromptTokens)
	}
	if t500.naive < 10*t500.rep.EstTokens {
		t.Errorf("replaying the transcript costs %d against %d: the comparison is not meaningful", t500.naive, t500.rep.EstTokens)
	}

	// Bounded does not mean empty: the newest turn, a capsule and the goal are still there.
	last := c.build(b, utterance)
	txt := content(last)
	if !strings.Contains(txt, mark(500, "A")) {
		t.Error("the newest reply is missing at turn 500")
	}
	if last.Report.CapsulesFull+last.Report.CapsulesDigested == 0 || !strings.Contains(txt, "Completed earlier") {
		t.Errorf("no completed-goal capsule at turn 500: %+v", last.Report)
	}
	if last.Report.CapsulesFull > p.MaxCapsulesFull || last.Report.Suspended > p.MaxSuspended || last.Report.TurnRows > p.MaxTurnRows {
		t.Errorf("section counts exceed the profile at turn 500: %+v", last.Report)
	}
	// With turn 5 there is little history; the prompt is smaller, never larger than at 500.
	if t5.rep.EstTokens > t500.rep.EstTokens+slack {
		t.Errorf("turn 5 (%d) is larger than turn 500 (%d)", t5.rep.EstTokens, t500.rep.EstTokens)
	}

	// Memory: from turn 50 to turn 500 the live heap does not grow with the history. The
	// comparison starts at 50 so that the one-off warm-up (SQLite page cache, regexp and
	// table initialisation) is not counted. The bound is generous on purpose: the heap of a
	// test binary moves by a few hundred KiB between two collections, and what it must not
	// do is grow by the size of the history (450 turns of about 150 bytes plus the
	// transcript rows, a few hundred KiB if they were all held, megabytes if replayed).
	growth := int64(t500.heap) - int64(t50.heap)
	t.Logf("live heap: turn 5 %.2f MiB, turn 50 %.2f MiB, turn 500 %.2f MiB; growth 50 to 500 = %.0f KiB", float64(t5.heap)/(1<<20), float64(t50.heap)/(1<<20), float64(t500.heap)/(1<<20), float64(growth)/1024)
	if growth > 4<<20 {
		t.Errorf("the live heap grew by %d KiB between turn 50 and turn 500", growth/1024)
	}
	if t500.allocPerTurn > 2*t50.allocPerTurn+64<<10 {
		t.Errorf("the allocation of one build grew from %d to %d bytes between turn 50 and 500", t50.allocPerTurn, t500.allocPerTurn)
	}
}

// the builder works with the runtime budget wrapper: what it builds passes
// providers.WithBudget unchanged.
func TestBuiltContextPassesTheRuntimeBoundary(t *testing.T) {
	b := newBuilder(t, ProfileLite)
	c := newConvo(t)
	for i := 1; i <= 30; i++ {
		c.step(i)
	}
	built := c.build(b, "extend my loan by two weeks")
	req, err := b.Budget().CheckChat(built.ChatRequest())
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != b.Profile().MaxOutputTokens {
		t.Fatalf("MaxTokens = %d, want the profile cap %d", req.MaxTokens, b.Profile().MaxOutputTokens)
	}
	if req.Role != providers.RoleController || req.Temperature != 0 {
		t.Fatalf("request role %q temperature %v", req.Role, req.Temperature)
	}
}

// Every profile holds its own budget over 500 turns and its prompt stops growing: the
// figures go into docs/efficiency/M3-03.md.
func TestLongHistoryEveryProfile(t *testing.T) {
	utterance := "reserve the second one for me, I will collect it next Friday"
	for _, name := range ProfileNames() {
		t.Run(name, func(t *testing.T) {
			b := newBuilder(t, name)
			p := b.Profile()
			c := newConvo(t)
			est := map[int]Report{}
			for i := 1; i <= 500; i++ {
				c.step(i)
				built := c.build(b, utterance)
				independentCheck(t, b, built)
				if i == 5 || i == 50 || i == 500 {
					est[i] = built.Report
				}
			}
			slack := p.MaxRowRunes/2 + 64
			if est[500].EstTokens > est[50].EstTokens+slack {
				t.Errorf("the prompt grows: %d est tokens at turn 500 against %d at turn 50", est[500].EstTokens, est[50].EstTokens)
			}
			t.Logf("%-16s cap %4d: turn 5 = %4d, turn 50 = %4d, turn 500 = %4d est_toks (history %d/%d/%d of %d, shortlist %d, rows %d)", name, p.MaxPromptTokens,
				est[5].EstTokens, est[50].EstTokens, est[500].EstTokens, est[5].HistoryToks, est[50].HistoryToks, est[500].HistoryToks, p.MaxHistoryTokens, est[500].Shortlist, est[500].TurnRows)
		})
	}
}
