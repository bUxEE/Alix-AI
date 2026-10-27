package blocks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Mark is what one data record draws: the value of each encoded channel, taken
// from the record and from nothing else. A channel the chart does not encode
// is nil. There is no aggregation, no scale and no expression between the
// data and the mark, which is what makes a chart reproducible from the data
// and the encoding it declares. Y2 (combo) and Size (scatter) are additive in 1.5.
type Mark struct {
	X     any `json:"x,omitempty"`
	Y     any `json:"y,omitempty"`
	Y2    any `json:"y2,omitempty"`
	Color any `json:"color,omitempty"`
	Size  any `json:"size,omitempty"`
	Theta any `json:"theta,omitempty"`
}

// channelOrder is the fixed order channels are checked and written in.
var channelOrder = []string{"x", "y", "y2", "color", "size", "theta"}

// channels returns the encoded channels by name.
func chartChannels(e conversation.ChartEncoding) map[string]*conversation.ChartChannel {
	out := map[string]*conversation.ChartChannel{}
	for name, ch := range map[string]*conversation.ChartChannel{
		"x": e.X, "y": e.Y, "y2": e.Y2, "color": e.Color, "size": e.Size, "theta": e.Theta,
	} {
		if ch != nil {
			out[name] = ch
		}
	}
	return out
}

// ChartMarks derives the marks of a chart from its declared data and encoding.
// It fails when an encoded field is missing from a record, so a chart that
// cannot be reproduced from what it declares yields no marks at all. The
// result is a pure function of the chart: calling it twice, or on a chart
// decoded from the JSON the first one was encoded to, gives the same marks.
func ChartMarks(c conversation.ChartBlock) ([]Mark, error) {
	ch := chartChannels(c.Encoding)
	if len(ch) == 0 {
		return nil, reject("chart", Violation{Rule: RuleChartShape, Path: "/encoding"})
	}
	marks := make([]Mark, 0, len(c.Data))
	for i, rec := range c.Data {
		var m Mark
		for _, name := range channelOrder {
			channel, ok := ch[name]
			if !ok {
				continue
			}
			val, present := rec[string(channel.Field)]
			if !present {
				return nil, reject("chart", Violation{Rule: RuleChartField, Path: fmt.Sprintf("/data/%d/%s", i, safeToken(string(channel.Field)))})
			}
			switch name {
			case "x":
				m.X = val
			case "y":
				m.Y = val
			case "y2":
				m.Y2 = val
			case "color":
				m.Color = val
			case "size":
				m.Size = val
			case "theta":
				m.Theta = val
			}
		}
		marks = append(marks, m)
	}
	return marks, nil
}

// ChartDigest is a SHA-256 over the canonical form of everything a renderer
// needs to draw the chart: the mark type, the channel to field and type
// mapping and the marks. Two charts with the same digest draw the same picture;
// a host's renderer can be conformance-tested against it. Numbers are written
// as exact decimals, so the digest does not depend on how a number was spelled.
func ChartDigest(c conversation.ChartBlock) (string, error) {
	marks, err := ChartMarks(c)
	if err != nil {
		return "", err
	}
	type chan_ struct {
		Channel string `json:"channel"`
		Field   string `json:"field"`
		Type    string `json:"type"`
	}
	var chs []chan_
	for _, name := range channelOrder {
		if ch, ok := chartChannels(c.Encoding)[name]; ok {
			chs = append(chs, chan_{Channel: name, Field: string(ch.Field), Type: string(ch.Type)})
		}
	}
	norm := make([]Mark, len(marks))
	for i, m := range marks {
		norm[i] = Mark{
			X: canonicalScalar(m.X), Y: canonicalScalar(m.Y), Y2: canonicalScalar(m.Y2),
			Color: canonicalScalar(m.Color), Size: canonicalScalar(m.Size), Theta: canonicalScalar(m.Theta),
		}
	}
	doc, err := json.Marshal(struct {
		Mark     string  `json:"mark"`
		Channels []chan_ `json:"channels"`
		Marks    []Mark  `json:"marks"`
	}{string(c.Mark), chs, norm})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalScalar writes a number as its exact decimal string and leaves other
// values as they are.
func canonicalScalar(v any) any {
	if r, isNum, finite := asNumber(v); isNum && finite {
		return "n:" + decimalString(r)
	}
	return v
}

// decimalString writes a rational that is a finite decimal exactly.
func decimalString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	// FloatString with enough digits for any decimal a float64 or a Decimal
	// fact holds, trailing zeros removed.
	s := r.FloatString(18)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// checkChart is the part of chart validation JSON Schema cannot express: every
// encoded field exists, nothing is hidden in a field no channel reads, values
// have the type their channel says, and every number shown is an authoritative
// fact.
func (v *Validator) checkChart(c conversation.ChartBlock, ctx Context, col *collector) {
	lim := v.limits
	if len(c.Data) > lim.MaxChartPoints {
		col.add(RuleChartBounds, "/data")
		return
	}
	if rule := textRule(string(c.Description), kindLine); rule != "" {
		col.add(rule, "/description")
	}
	if c.Title != nil {
		if rule := textRule(string(*c.Title), kindLine); rule != "" {
			col.add(rule, "/title")
		}
	}
	ch := chartChannels(c.Encoding)
	for _, name := range channelOrder {
		channel, ok := ch[name]
		if !ok {
			continue
		}
		if channel.Title != nil {
			if rule := textRule(string(*channel.Title), kindLine); rule != "" {
				col.add(rule, "/encoding/"+name+"/title")
			}
		}
	}

	// The mark and its channels (the schema states these too; a typed value that
	// skipped it is still held to them).
	switch c.Mark {
	case conversation.ChartBlockMarkPie:
		if ch["theta"] == nil || ch["color"] == nil || ch["x"] != nil || ch["y"] != nil {
			col.add(RuleChartShape, "/encoding")
			return
		}
		if ch["theta"].Type != conversation.ChartChannelTypeQuantitative {
			col.add(RuleChartType, "/encoding/theta/type")
		}
		if t := ch["color"].Type; t != conversation.ChartChannelTypeNominal && t != conversation.ChartChannelTypeOrdinal {
			col.add(RuleChartType, "/encoding/color/type")
		}
	default:
		if ch["x"] == nil || ch["y"] == nil || ch["theta"] != nil {
			col.add(RuleChartShape, "/encoding")
			return
		}
		if ch["x"].Type != conversation.ChartChannelTypeQuantitative && ch["y"].Type != conversation.ChartChannelTypeQuantitative {
			col.add(RuleChartType, "/encoding")
		}
	}
	if col.failed() {
		return
	}

	// field -> the channels reading it (a field may feed more than one).
	fieldChannels := map[string][]string{}
	for _, name := range channelOrder {
		if channel, ok := ch[name]; ok {
			f := string(channel.Field)
			fieldChannels[f] = append(fieldChannels[f], name)
		}
	}

	reported := map[string]bool{}
	report := func(rule Rule, path, key string) {
		k := string(rule) + "|" + key
		if !reported[k] {
			reported[k] = true
			col.add(rule, path)
		}
	}
	for i, rec := range c.Data {
		for key := range rec {
			if _, ok := fieldChannels[key]; !ok {
				report(RuleChartHidden, fmt.Sprintf("/data/%d/%s", i, safeToken(key)), key)
			}
		}
		fields := make([]string, 0, len(fieldChannels))
		for f := range fieldChannels {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		for _, f := range fields {
			val, present := rec[f]
			path := fmt.Sprintf("/data/%d/%s", i, f)
			if !present {
				report(RuleChartField, path, f)
				continue
			}
			for _, name := range fieldChannels[f] {
				ctype := ch[name].Type
				v.checkChartValue(val, ctype, name == "theta", path, ctx, col, report, f)
			}
		}
	}
	if c.Mark == conversation.ChartBlockMarkPie && !col.failed() {
		// Slices are shares of a whole: none is negative and they are not all zero.
		total := new(big.Rat)
		thetaField := string(ch["theta"].Field)
		for i, rec := range c.Data {
			if r, isNum, finite := asNumber(rec[thetaField]); isNum && finite {
				if r.Sign() < 0 {
					report(RuleChartType, fmt.Sprintf("/data/%d/%s", i, thetaField), "negative")
				}
				total.Add(total, r)
			}
		}
		if total.Sign() <= 0 {
			col.add(RuleChartType, "/encoding/theta")
		}
	}
}

// checkChartValue checks one value of a channel.
func (v *Validator) checkChartValue(val any, ctype conversation.ChartChannelType, theta bool, path string,
	ctx Context, col *collector, report func(Rule, string, string), field string) {
	if val == nil {
		if ctype != conversation.ChartChannelTypeQuantitative || theta {
			report(RuleChartType, path, field+"|null")
		}
		return
	}
	switch x := val.(type) {
	case string:
		if rule := textRule(x, kindLine); rule != "" {
			report(rule, path, field+"|"+string(rule))
			return
		}
		switch ctype {
		case conversation.ChartChannelTypeQuantitative:
			report(RuleChartNumeric, path, field+"|quant")
		case conversation.ChartChannelTypeTemporal:
			if !isTemporal(x) {
				report(RuleChartType, path, field+"|temporal")
			}
		}
		// A string that is only a number presents a number.
		if r, ok := numericLiteral(x); ok && !ctx.Facts.has(r) {
			report(RuleChartNumber, path, field+"|num")
		}
	case bool:
		if ctype == conversation.ChartChannelTypeQuantitative || ctype == conversation.ChartChannelTypeTemporal {
			report(RuleChartType, path, field+"|bool")
		}
	default:
		r, isNum, finite := asNumber(val)
		switch {
		case !isNum:
			report(RuleChartShape, path, field+"|shape")
		case !finite:
			report(RuleChartNumeric, path, field+"|finite")
		case ctype == conversation.ChartChannelTypeTemporal:
			report(RuleChartType, path, field+"|temporal")
		case !ctx.Facts.has(r):
			// The number is not one the application stated: the model made it up,
			// computed it or copied it wrongly. It is never repaired.
			report(RuleChartNumber, path, field+"|num")
		}
	}
}

// isTemporal accepts a date (YYYY-MM-DD) or an RFC 3339 instant.
func isTemporal(s string) bool {
	if len(s) == 10 {
		_, err := time.Parse("2006-01-02", s)
		return err == nil
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}
