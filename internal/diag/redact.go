package diag

import (
	"bytes"
	"encoding/json"

	alog "github.com/buxee/alix-ai/internal/log"
)

// walker redacts a generic JSON value (maps, slices, strings, numbers) with the
// log package's rules and counts what it replaced.
type walker struct {
	red   *alog.Redactor
	count int
}

// walk returns a redacted copy of x. A key that looks sensitive loses its whole
// value, whatever it holds; a string loses any secret embedded in it.
func (w *walker) walk(x any) any {
	switch t := x.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			if w.red.SensitiveKey(k) {
				out[k] = alog.Redacted
				w.count++
				continue
			}
			out[k] = w.walk(v)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = w.walk(v)
		}
		return out
	case string:
		if s := w.red.String(t); s != t {
			w.count++
			return s
		}
	}
	return x
}

// generic returns the JSON form of v as generic values, numbers kept exact.
func generic(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// redactValue redacts any value through its JSON form. A value that cannot be
// serialised is dropped, not passed through: it is replaced by a marker.
func redactValue(red *alog.Redactor, v any) (out any, count int) {
	g, err := generic(v)
	if err != nil {
		return alog.Redacted + " (unserialisable value)", 1
	}
	w := &walker{red: red}
	out = w.walk(g)
	return out, w.count
}
