package chunk

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var snippets = []string{
	"Short.",
	"A plain English sentence about invoices and refunds.",
	"Les clients peuvent demander un remboursement dans un délai de trente jours.",
	"Die Rückerstattung erfolgt innerhalb von vierzehn Werktagen nach Eingang der Ware.",
	"Клиент может запросить возврат средств в течение тридцати дней.",
	"顧客は三十日以内に返金を請求することができます。詳細は規約をご覧ください。",
	"客户可以在三十天内申请退款，具体条款见服务协议。",
	"고객은 30일 이내에 환불을 요청할 수 있습니다.",
	"يمكن للعميل طلب استرداد المبلغ خلال ثلاثين يوما.",
	"ग्राहक तीस दिनों के भीतर रिफंड का अनुरोध कर सकते हैं।",
	"Ref ABC-12345 / PO#99-1001 amount 1,234.50 EUR 🙂",
	strings.Repeat("A long run of words that keeps going without any break. ", 20),
	strings.Repeat("長い文章が続きます。", 60),
	strings.Repeat("x", 700),
}

// randomDoc builds a document with a body under every heading, so the
// coverage invariant of checkInvariants applies, and a random mix of blocks.
func randomDoc(r *rand.Rand, id string) Document {
	b := newBuilder(id)
	pageOpen := false
	maybePage := func() {
		if !pageOpen || r.Intn(6) == 0 {
			b.page()
			pageOpen = true
		}
	}
	snippet := func() string {
		s := snippets[r.Intn(len(snippets))]
		if r.Intn(3) == 0 {
			s += "\n" + snippets[r.Intn(len(snippets))]
		}
		return s
	}
	for i, n := 0, 1+r.Intn(7); i < n; i++ {
		if r.Intn(4) != 0 {
			b.heading(1+r.Intn(4), fmt.Sprintf("Section %d %s", i, snippets[r.Intn(6)]))
		}
		for j, m := 0, 1+r.Intn(5); j < m; j++ {
			if r.Intn(3) == 0 {
				maybePage()
			}
			switch r.Intn(10) {
			case 0:
				b.hidden(snippet())
			case 1:
				b.code(snippet())
			case 2:
				rows := []string{"| k | v |"}
				for k, rn := 0, 1+r.Intn(8); k < rn; k++ {
					rows = append(rows, fmt.Sprintf("| %d | %s |", k, snippets[r.Intn(len(snippets))]))
				}
				b.table(r.Intn(3) != 0, rows...)
			default:
				b.para(snippet())
			}
		}
	}
	return b.build()
}

func TestRandomDocumentsKeepInvariants(t *testing.T) {
	r := rand.New(rand.NewSource(20260802))
	for i := 0; i < 150; i++ {
		cfg := Config{
			ChildMinToks:  r.Intn(60),
			ChildMaxToks:  16 + r.Intn(300),
			ParentMaxToks: 0,
		}
		if cfg.ChildMinToks > cfg.ChildMaxToks {
			cfg.ChildMinToks = cfg.ChildMaxToks
		}
		cfg.ParentMaxToks = cfg.ChildMaxToks + r.Intn(1500)
		doc := randomDoc(r, fmt.Sprintf("src_rand%d", i))

		c := mustChunker(t, cfg)
		res := mustChunk(t, c, doc) // checks every invariant, including exact coverage
		again := mustChunk(t, c, doc)
		if diff := cmp.Diff(res, again); diff != "" {
			t.Fatalf("doc %d, cfg %+v: not deterministic:\n%s", i, cfg, diff)
		}
		if t.Failed() {
			t.Fatalf("doc %d failed with cfg %+v", i, cfg)
		}
	}
}

func TestLargeDocuments(t *testing.T) {
	c := mustChunker(t, Config{ChildMinToks: 0, ChildMaxToks: 64, ParentMaxToks: 2000})

	t.Run("many paragraphs", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString("# Huge\n\n")
		for i := 0; i < 20000; i++ {
			fmt.Fprintf(&sb, "Paragraph %d has a dozen or so words in it, enough to be a child.\n\n", i)
		}
		res, err := c.Chunk(context.Background(), FromMarkdown("src_huge", sb.String()))
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, ch := range res.Children {
			ids[ch.ID] = true
		}
		if len(res.Children) != 20000 || len(ids) != 20000 {
			t.Errorf("got %d children with %d distinct IDs, want 20000", len(res.Children), len(ids))
		}
	})

	t.Run("one two-megabyte paragraph with no break", func(t *testing.T) {
		text := "# Blob\n\n" + strings.Repeat("0123456789abcdef", 1<<17)
		res, err := c.Chunk(context.Background(), FromMarkdown("src_blob", text))
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, ch := range res.Children {
			total += len(ch.Text)
			if ch.EstToks > 64 {
				t.Fatalf("piece over the bound: %d", ch.EstToks)
			}
		}
		if total != 1<<21 {
			t.Errorf("pieces hold %d bytes, want %d", total, 1<<21)
		}
	})
}

func BenchmarkChunk1MB(b *testing.B) {
	r := rand.New(rand.NewSource(7))
	var sb strings.Builder
	for sb.Len() < 1<<20 {
		sb.WriteString(fmt.Sprintf("# Section %d\n\n", sb.Len()))
		for i := 0; i < 8; i++ {
			sb.WriteString(snippets[r.Intn(len(snippets))])
			sb.WriteString("\n\n")
		}
	}
	doc := FromMarkdown("src_bench", sb.String())
	c, _ := New(DefaultConfig())
	b.SetBytes(int64(len(doc.Text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Chunk(context.Background(), doc); err != nil {
			b.Fatal(err)
		}
	}
}
