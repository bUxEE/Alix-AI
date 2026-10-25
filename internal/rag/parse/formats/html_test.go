package formats

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

func TestHTMLStructure(t *testing.T) {
	in := `<!doctype html><html><head><title>Ignored title</title><style>p{color:red}</style><script>var x = "SCRIPT";</script></head>
<body><h1>Main  title</h1>
<p>First <b>bold</b> and <i>italic</i>
text.<p>Second paragraph, never closed
<ul><li>one<li>two</ul>
<pre>  code line 1
  code line 2</pre>
<table><thead><tr><th>Name</th><th>Qty</th></tr></thead><tr><td>apple</td><td>3</td></tr><tr><td>pear</td><td>5</td></tr></table>
<!-- a comment is not text --><noscript>noscript content</noscript>
<h2>Sub</h2><div>Tail &amp; entity &lt;ok&gt;</div></body></html>`
	o := mustParse(t, wire.FormatHTML, []byte(in))
	got := dump(o)
	want := []string{
		`H1 "Main title"`,
		`P "First bold and italic text."`,
		`P "Second paragraph, never closed"`,
		`P "one"`, `P "two"`,
		`C "  code line 1\n  code line 2"`,
		`TABLE header=true rows=3`, `"Name | Qty"`, `"apple | 3"`, `"pear | 5"`,
		`H2 "Sub"`, `P "Tail & entity <ok>"`,
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in\n%s", w, got)
		}
	}
	for _, bad := range []string{"SCRIPT", "Ignored title", "comment is not text", "noscript content", "color:red"} {
		if strings.Contains(string(o.Text), bad) {
			t.Errorf("%q must not be extracted:\n%s", bad, got)
		}
	}
}

func TestHTMLHiddenText(t *testing.T) {
	// Each case hides a marker some way; the marker must end up in a Hidden
	// block and nowhere else.
	cases := []struct{ name, html, reason string }{
		{"display none", `<div style="display:none">MARK</div>`, wire.HideDisplayNone},
		{"visibility hidden", `<p style="visibility: hidden">MARK</p>`, wire.HideVisibility},
		{"hidden attribute", `<p hidden>MARK</p>`, wire.HideHiddenAttr},
		{"aria hidden", `<p aria-hidden="true">MARK</p>`, wire.HideAria},
		{"opacity zero", `<p style="opacity:0">MARK</p>`, wire.HideZeroOpacity},
		{"font size zero", `<p style="font-size:0">MARK</p>`, wire.HideTinyFont},
		{"font size 1px", `<p style="font-size:1px;">MARK</p>`, wire.HideTinyFont},
		{"white on white", `<p style="color:#fff">MARK</p>`, wire.HideSameColour},
		{"white named", `<p style="color: white;">MARK</p>`, wire.HideSameColour},
		{"rgb white", `<p style="color:rgb(255,255,255)">MARK</p>`, wire.HideSameColour},
		{"transparent", `<p style="color:rgba(0,0,0,0)">MARK</p>`, wire.HideTransparent},
		{"same as background", `<div style="background:#336699"><p style="color:#336699">MARK</p></div>`, wire.HideSameColour},
		{"off screen", `<p style="position:absolute;left:-9999px">MARK</p>`, wire.HideOffscreen},
		{"text indent", `<p style="text-indent:-5000px">MARK</p>`, wire.HideOffscreen},
		{"clip", `<p style="clip:rect(0,0,0,0)">MARK</p>`, wire.HideClipped},
		{"zero box", `<p style="height:0;overflow:hidden">MARK</p>`, wire.HideClipped},
		{"class rule", `<style>.sr{display:none}</style><p class="x sr">MARK</p>`, wire.HideDisplayNone},
		{"id rule", `<style>#h{color:#ffffff}</style><p id="h">MARK</p>`, wire.HideSameColour},
		{"inherited", `<div style="display:none"><section><p><span>MARK</span></p></section></div>`, wire.HideDisplayNone},
		{"inside a table cell", `<table><tr><td>ok</td><td style="display:none">MARK</td></tr></table>`, wire.HideDisplayNone},
		{"inside a heading", `<h2>Shown <span hidden>MARK</span></h2>`, wire.HideHiddenAttr},
		{"rule after the element", `<p class="late">MARK</p><style>.late{display:none}</style>`, wire.HideDisplayNone},
		{"transparent over an image", `<div style="background:url(bg.png)"><p style="color:transparent">MARK</p></div>`, wire.HideTransparent},
		{"font colour", `<font color="#ffffff">MARK</font>`, wire.HideSameColour},
		{"font colour on a bgcolor table", `<table bgcolor="#336699"><tr><td><font color="#336699">MARK</font></td></tr></table>`, wire.HideSameColour},
		{"font colour without #", `<font color="FFFFFF">MARK</font>`, wire.HideSameColour},
		{"bgcolor without #", `<table bgcolor=336699><tr><td><font color="#336699">MARK</font></td></tr></table>`, wire.HideSameColour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := mustParse(t, wire.FormatHTML, []byte("<body><p>Visible text.</p>"+c.html+"<p>More visible.</p></body>"))
			if !strings.Contains(hiddenText(o), "MARK") {
				t.Fatalf("MARK not flagged hidden:\n%s", dump(o))
			}
			var reason string
			for _, b := range o.Blocks {
				if b.Hidden {
					reason = b.HiddenReason
				}
			}
			if reason != c.reason {
				t.Errorf("reason = %q, want %q\n%s", reason, c.reason, dump(o))
			}
			// A paragraph, never a heading, is where hidden text stands alone;
			// a mixed heading or table is hidden as a whole, so its visible
			// half is flagged too.
			for _, b := range o.Blocks {
				if b.Kind == wire.KindParagraph && !b.Hidden && strings.Contains(string(o.Text[b.Start:b.End]), "MARK") {
					t.Errorf("MARK in an unflagged paragraph:\n%s", dump(o))
				}
			}
			if !strings.Contains(string(o.Text), "Visible text.") || !strings.Contains(string(o.Text), "More visible.") {
				t.Errorf("visible text lost:\n%s", dump(o))
			}
		})
	}
}

func TestHTMLVisibleStaysVisible(t *testing.T) {
	// Look-alikes that a reader can see must not be flagged.
	in := `<body>
<p style="color:#fff;background:#000">white on black is readable</p>
<p style="color:#ccc">light grey is readable</p>
<div style="background:#000"><p style="color:white">nested white on black</p></div>
<p style="font-size:14px;color:#222">ordinary</p>
<p style="opacity:0.8">mostly opaque</p>
<span style="display:inline">inline</span>
<table bgcolor="black"><tr><td><font color="white">white on a black table</font></td></tr></table>
<table background="photo.jpg"><tr><td><font color="white">white over a picture</font></td></tr></table>
<div bgcolor="black"><font color="black">black on white: a div has no bgcolor</font></div>
<table bgcolor="000000"><tr><td><font color="ffffff">white on a black table, without #</font></td></tr></table>
<font color="fff">a legacy fff is nearly black, not white</font>
<p class="late">a later rule makes this readable</p><style>.late{color:#fff;background:#000}</style></body>`
	o := mustParse(t, wire.FormatHTML, []byte(in))
	if h := hiddenText(o); h != "" {
		t.Errorf("readable text flagged hidden: %q\n%s", h, dump(o))
	}
}

func TestLegacyColour(t *testing.T) {
	// HTML's rules for legacy colour values, as browsers apply them to
	// <font color> and bgcolor.
	for _, c := range []struct {
		in   string
		want *rgba
	}{
		{"ffffff", &rgba{255, 255, 255, 1}},
		{" #336699 ", &rgba{0x33, 0x66, 0x99, 1}},
		{"fff", &rgba{15, 15, 15, 1}},
		{"c00c00000000", &rgba{0xc0, 0, 0, 1}},
		{"000fff000fff000fff", &rgba{255, 255, 255, 1}},
		{"1234567890abcdef1234567890", &rgba{0x23, 0xab, 0x45, 1}},
		{"white", &rgba{255, 255, 255, 1}},
		{"teal", nil},
		{"", nil},
	} {
		got := legacyColour(c.in)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("legacyColour(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Many small pieces of text cost memory linear in their total: merging
// neighbouring segments and stripping CSS comments must not copy what was
// already built once per piece.
func TestLinearMerging(t *testing.T) {
	segs := make([]Seg, 100000)
	for i := range segs {
		segs[i] = Seg{Text: "a"}
	}
	var out []Seg
	if n := allocated(func() { out = prep(segs) }); n > 32<<20 {
		t.Errorf("prep of %d one-byte segments allocated %d bytes", len(segs), n)
	}
	if len(out) != 1 || out[0].Text != strings.Repeat("a", len(segs)) {
		t.Errorf("prep merged into %d segments", len(out))
	}

	css := strings.Repeat("a/**/", 20000)
	var stripped string
	if n := allocated(func() { stripped = stripCSSComments(css) }); n > 8<<20 {
		t.Errorf("stripping %d comments allocated %d bytes", 20000, n)
	}
	if want := strings.Repeat("a ", 20000); stripped != want {
		t.Errorf("stripped CSS = %.40q..., want %.40q...", stripped, want)
	}
	for in, want := range map[string]string{"a/* x */b": "a b", "/*/ x */c": " c", "a/* open": "a", "/**//**/": "  ", "a/b*c": "a/b*c", "a/*/*/b": "a b"} {
		if got := stripCSSComments(in); got != want {
			t.Errorf("stripCSSComments(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTMLDepthLimit(t *testing.T) {
	deep := strings.Repeat("<div>", 5000) + "x" + strings.Repeat("</div>", 5000)
	_, err := Parse(wire.FormatHTML, []byte(deep), testLimits())
	if fe, ok := err.(*Error); !ok || fe.Reason != wire.ReasonNestingTooDeep {
		t.Fatalf("err = %v, want nesting_too_deep", err)
	}
	// Unclosed paragraphs and list items are not nesting.
	flat := strings.Repeat("<p>x", 20000) + strings.Repeat("<li>y", 20000)
	if _, err := Parse(wire.FormatHTML, []byte(flat), testLimits()); err != nil {
		t.Fatalf("implied end tags: %v", err)
	}
}

func TestHTMLTokenSizeBounded(t *testing.T) {
	huge := "<p>" + strings.Repeat("a", maxHTMLToken+10) + "</p>"
	lim := testLimits()
	lim.MaxInputBytes = 64 << 20
	_, err := Parse(wire.FormatHTML, []byte(huge), lim)
	if err == nil {
		t.Fatal("a single token over the bound must be refused")
	}
}
