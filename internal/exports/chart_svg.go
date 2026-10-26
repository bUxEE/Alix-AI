package exports

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// chartSVG draws a static, self-contained SVG for a verified chart block. No
// script, no external resources. Deterministic for the same input.
func chartSVG(c conversation.ChartBlock) (string, error) {
	marks, err := blocks.ChartMarks(c)
	if err != nil {
		return "", err
	}
	if len(marks) == 0 {
		return emptyChartSVG(c), nil
	}
	switch c.Mark {
	case conversation.ChartBlockMarkPie:
		return pieSVG(c, marks), nil
	case conversation.ChartBlockMarkBar:
		return barSVG(c, marks), nil
	case conversation.ChartBlockMarkLine, conversation.ChartBlockMarkArea, conversation.ChartBlockMarkPoint:
		return cartesianSVG(c, marks), nil
	default:
		return cartesianSVG(c, marks), nil
	}
}

func emptyChartSVG(c conversation.ChartBlock) string {
	title := chartTitle(c)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="480" height="120" viewBox="0 0 480 120" role="img" aria-label="%s"><rect width="480" height="120" fill="#f8fafc"/><text x="24" y="64" font-family="system-ui,sans-serif" font-size="14" fill="#64748b">%s</text></svg>`,
		xmlEscape(title), xmlEscape(title))
}

func chartTitle(c conversation.ChartBlock) string {
	if c.Title != nil && strings.TrimSpace(string(*c.Title)) != "" {
		return strings.TrimSpace(string(*c.Title))
	}
	return strings.TrimSpace(string(c.Description))
}

func barSVG(c conversation.ChartBlock, marks []blocks.Mark) string {
	const W, H, padL, padR, padT, padB = 560.0, 320.0, 48.0, 24.0, 36.0, 48.0
	plotW := W - padL - padR
	plotH := H - padT - padB
	n := float64(len(marks))
	maxY := 0.0
	for _, m := range marks {
		if y, ok := asFloat(m.Y); ok && y > maxY {
			maxY = y
		}
	}
	if maxY <= 0 {
		maxY = 1
	}
	bw := plotW / n * 0.7
	gap := plotW / n
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, W, H, W, H, xmlEscape(chartTitle(c)))
	b.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/>`)
	fmt.Fprintf(&b, `<text x="%.0f" y="22" font-family="system-ui,sans-serif" font-size="14" font-weight="600" fill="#0f172a">%s</text>`, padL, xmlEscape(chartTitle(c)))
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#cbd5e1" stroke-width="1"/>`, padL, padT+plotH, padL+plotW, padT+plotH)
	for i, m := range marks {
		y, _ := asFloat(m.Y)
		h := (y / maxY) * plotH
		x := padL + gap*float64(i) + (gap-bw)/2
		yy := padT + plotH - h
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="#2563eb"/>`, x, yy, bw, h)
		label := fmt.Sprint(m.X)
		fmt.Fprintf(&b, `<text x="%.2f" y="%.1f" text-anchor="middle" font-family="system-ui,sans-serif" font-size="11" fill="#475569">%s</text>`,
			x+bw/2, padT+plotH+16, xmlEscape(truncate(label, 12)))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func cartesianSVG(c conversation.ChartBlock, marks []blocks.Mark) string {
	const W, H, padL, padR, padT, padB = 560.0, 320.0, 48.0, 24.0, 36.0, 48.0
	plotW := W - padL - padR
	plotH := H - padT - padB
	xs, ys := make([]float64, len(marks)), make([]float64, len(marks))
	useCatX := false
	for i, m := range marks {
		if x, ok := asFloat(m.X); ok {
			xs[i] = x
		} else {
			useCatX = true
			xs[i] = float64(i)
		}
		ys[i], _ = asFloat(m.Y)
	}
	minX, maxX := minMax(xs)
	minY, maxY := minMax(ys)
	if maxX == minX {
		maxX = minX + 1
	}
	if maxY == minY {
		maxY = minY + 1
	}
	if minY > 0 && c.Mark == conversation.ChartBlockMarkArea {
		minY = 0
	}
	sx := func(x float64) float64 { return padL + (x-minX)/(maxX-minX)*plotW }
	sy := func(y float64) float64 { return padT + plotH - (y-minY)/(maxY-minY)*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, W, H, W, H, xmlEscape(chartTitle(c)))
	b.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/>`)
	fmt.Fprintf(&b, `<text x="%.0f" y="22" font-family="system-ui,sans-serif" font-size="14" font-weight="600" fill="#0f172a">%s</text>`, padL, xmlEscape(chartTitle(c)))
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#cbd5e1"/>`, padL, padT+plotH, padL+plotW, padT+plotH)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#cbd5e1"/>`, padL, padT, padL, padT+plotH)

	if c.Mark == conversation.ChartBlockMarkArea && len(marks) > 0 {
		var path strings.Builder
		fmt.Fprintf(&path, "M %.2f %.2f", sx(xs[0]), sy(minY))
		for i := range marks {
			fmt.Fprintf(&path, " L %.2f %.2f", sx(xs[i]), sy(ys[i]))
		}
		fmt.Fprintf(&path, " L %.2f %.2f Z", sx(xs[len(xs)-1]), sy(minY))
		fmt.Fprintf(&b, `<path d="%s" fill="#93c5fd" fill-opacity="0.5" stroke="none"/>`, path.String())
	}
	if c.Mark == conversation.ChartBlockMarkLine || c.Mark == conversation.ChartBlockMarkArea {
		var path strings.Builder
		for i := range marks {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&path, "%s %.2f %.2f ", cmd, sx(xs[i]), sy(ys[i]))
		}
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="#2563eb" stroke-width="2"/>`, strings.TrimSpace(path.String()))
	}
	for i := range marks {
		fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="3.5" fill="#1d4ed8"/>`, sx(xs[i]), sy(ys[i]))
		if useCatX {
			fmt.Fprintf(&b, `<text x="%.2f" y="%.1f" text-anchor="middle" font-family="system-ui,sans-serif" font-size="11" fill="#475569">%s</text>`,
				sx(xs[i]), padT+plotH+16, xmlEscape(truncate(fmt.Sprint(marks[i].X), 12)))
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func pieSVG(c conversation.ChartBlock, marks []blocks.Mark) string {
	const W, H, cx, cy, r = 420.0, 320.0, 160.0, 170.0, 110.0
	total := 0.0
	vals := make([]float64, len(marks))
	for i, m := range marks {
		v, _ := asFloat(m.Theta)
		if v < 0 {
			v = 0
		}
		vals[i] = v
		total += v
	}
	if total <= 0 {
		return emptyChartSVG(c)
	}
	palette := []string{"#2563eb", "#0891b2", "#059669", "#ca8a04", "#dc2626", "#7c3aed", "#db2777", "#4b5563"}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, W, H, W, H, xmlEscape(chartTitle(c)))
	b.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/>`)
	fmt.Fprintf(&b, `<text x="24" y="28" font-family="system-ui,sans-serif" font-size="14" font-weight="600" fill="#0f172a">%s</text>`, xmlEscape(chartTitle(c)))
	angle := -math.Pi / 2
	for i, m := range marks {
		sweep := vals[i] / total * 2 * math.Pi
		x1, y1 := cx+r*math.Cos(angle), cy+r*math.Sin(angle)
		angle2 := angle + sweep
		x2, y2 := cx+r*math.Cos(angle2), cy+r*math.Sin(angle2)
		large := 0
		if sweep > math.Pi {
			large = 1
		}
		color := palette[i%len(palette)]
		fmt.Fprintf(&b, `<path d="M %.2f %.2f L %.2f %.2f A %.2f %.2f 0 %d 1 %.2f %.2f Z" fill="%s" stroke="#ffffff" stroke-width="1"/>`,
			cx, cy, x1, y1, r, r, large, x2, y2, color)
		fmt.Fprintf(&b, `<rect x="300" y="%.0f" width="12" height="12" fill="%s"/>`, 60+float64(i)*22, color)
		fmt.Fprintf(&b, `<text x="318" y="%.0f" font-family="system-ui,sans-serif" font-size="12" fill="#334155">%s</text>`,
			70+float64(i)*22, xmlEscape(truncate(fmt.Sprint(m.Color), 24)))
		angle = angle2
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	default:
		s := fmt.Sprint(v)
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
}

func minMax(vs []float64) (float64, float64) {
	if len(vs) == 0 {
		return 0, 1
	}
	lo, hi := vs[0], vs[0]
	for _, v := range vs[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return lo, hi
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
