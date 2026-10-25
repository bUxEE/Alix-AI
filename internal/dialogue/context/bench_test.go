package dctx

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/sessions"
)

// The cost of the context path of one turn at turn 5 and turn 500 of a conversation
// (docs/efficiency/M3-03.md). BuildOnly is the builder on a state and a window that were
// already read; ContextPath adds the read of the session and of the window from SQLite,
// which is what the engine does each turn.
func BenchmarkBuild(b *testing.B) {
	for _, turns := range []int{5, 500} {
		c := newConvo(b)
		var sess sessions.Session
		for i := 1; i <= turns; i++ {
			sess = c.step(i)
		}
		bd := newBuilder(b, ProfileLite)
		ctx := context.Background()
		window, err := bd.Window(ctx, c.store, sess)
		if err != nil {
			b.Fatal(err)
		}
		in := baseInput("reserve the second one for me, I will collect it next Friday")
		in.State, in.Turns = sess.State, window

		name := map[int]string{5: "turn5", 500: "turn500"}[turns]
		b.Run(name+"/BuildOnly", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := bd.Build(ctx, c.scope, in); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/ContextPath", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				s, err := c.store.Open(ctx, c.scope)
				if err != nil {
					b.Fatal(err)
				}
				w, err := bd.Window(ctx, c.store, s)
				if err != nil {
					b.Fatal(err)
				}
				in := in
				in.State, in.Turns = s.State, w
				if _, err := bd.Build(ctx, c.scope, in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
