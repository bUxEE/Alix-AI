package formats

import (
	"bytes"
	"compress/lzw"
	"compress/zlib"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

const (
	// maxPDFObjects bounds the objects a file may declare or contain. One
	// object takes tens of bytes of cross-reference table, so a 64 MiB file can
	// hold millions; past this the file is refused.
	maxPDFObjects = 2_000_000
	// maxResolve bounds a chain of indirect references.
	maxResolve = 32
)

type xrefEntry struct {
	typ  byte // 1 = at an offset, 2 = inside an object stream
	a, b int  // offset and generation, or object stream number and index
}

type objStream struct {
	nums []int
	offs []int
	data []byte
	n    int
	// first is the offset of the first object inside data.
	first int
}

type pdfFile struct {
	data    []byte
	lim     wire.Limits
	bud     *budget
	w       *warnings
	xref    map[int]xrefEntry
	trailer pdfDict
	cache   map[int]any
	busy    map[int]bool
	objstm  map[int]*objStream
	objects int
	// decoded holds the streams decoded so far: decoding one again counts
	// against the budget whatever its filters.
	decoded map[*pdfStream]bool
	// free is what first decodes produced without counting it. Distinct
	// streams do not overlap in a well-formed file, so past the file's size
	// they do (no /Length, one endstream) and count too.
	free int
	// endstreams holds where each "endstream" keyword starts, found once for a
	// stream with no usable /Length (see nextEndstream).
	endstreams []int
	endIndexed bool
}

func openPDF(data []byte, lim wire.Limits, bud *budget, w *warnings) (*pdfFile, error) {
	f := &pdfFile{
		data: data, lim: lim, bud: bud, w: w,
		xref: map[int]xrefEntry{}, cache: map[int]any{}, busy: map[int]bool{}, objstm: map[int]*objStream{},
		decoded: map[*pdfStream]bool{},
	}
	if err := f.readXrefChain(); err != nil {
		var fe *Error
		if errors.As(err, &fe) && fe.Reason != wire.ReasonMalformed {
			return nil, err
		}
		if err := f.recover(); err != nil {
			return nil, err
		}
		w.add("pdf_xref_recovered")
	} else if f.root() == nil {
		// The table was readable but does not lead to a catalog.
		f.xref, f.cache, f.objstm, f.trailer = map[int]xrefEntry{}, map[int]any{}, map[int]*objStream{}, nil
		if err := f.recover(); err != nil {
			return nil, err
		}
		w.add("pdf_xref_recovered")
	}
	if _, enc := f.trailer["Encrypt"]; enc {
		return nil, fail(wire.ReasonEncrypted, "the PDF is encrypted")
	}
	return f, nil
}

func (f *pdfFile) root() pdfDict {
	d, _ := f.resolve(f.trailer["Root"]).(pdfDict)
	return d
}

// ---- cross-reference ----

func (f *pdfFile) readXrefChain() error {
	tail := f.data
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	i := bytes.LastIndex(tail, []byte("startxref"))
	if i < 0 {
		return malformed("pdf: no startxref")
	}
	l := &lexer{b: tail, pos: i + len("startxref"), maxDepth: f.lim.MaxDepth}
	o, err := l.readObject(0)
	off, ok := o.(int64)
	if err != nil || !ok || off < 0 || off >= int64(len(f.data)) {
		return malformed("pdf: bad startxref")
	}
	seen := map[int64]bool{}
	for step := 0; ; step++ {
		if seen[off] || step > 1000 {
			return malformed("pdf: cross-reference chain loops")
		}
		seen[off] = true
		tr, err := f.readXrefAt(int(off))
		if err != nil {
			return err
		}
		if f.trailer == nil {
			f.trailer = tr
		} else {
			for k, v := range tr { // the newest trailer wins; older ones fill gaps
				if _, ok := f.trailer[k]; !ok {
					f.trailer[k] = v
				}
			}
		}
		if xs, ok := tr["XRefStm"].(int64); ok && xs > 0 && xs < int64(len(f.data)) && !seen[xs] {
			seen[xs] = true
			if _, err := f.readXrefAt(int(xs)); err != nil {
				return err
			}
		}
		prev, ok := tr["Prev"].(int64)
		if !ok || prev <= 0 || prev >= int64(len(f.data)) {
			return nil
		}
		off = prev
	}
}

// readXrefAt reads one cross-reference section (a classic table or a stream)
// at off, adds its entries where none exist yet, and returns its trailer.
func (f *pdfFile) readXrefAt(off int) (pdfDict, error) {
	l := &lexer{b: f.data, pos: off, maxDepth: f.lim.MaxDepth}
	l.skipWS()
	if bytes.HasPrefix(f.data[l.pos:], []byte("xref")) {
		l.pos += 4
		return f.readXrefTable(l)
	}
	return f.readXrefStream(off)
}

func (f *pdfFile) readXrefTable(l *lexer) (pdfDict, error) {
	for {
		l.skipWS()
		if l.pos >= len(l.b) {
			return nil, malformed("pdf: truncated cross-reference table")
		}
		if bytes.HasPrefix(l.b[l.pos:], []byte("trailer")) {
			l.pos += len("trailer")
			o, err := l.readObject(0)
			d, ok := o.(pdfDict)
			if err != nil || !ok {
				return nil, malformed("pdf: bad trailer")
			}
			return d, nil
		}
		so, err1 := l.readObject(0)
		co, err2 := l.readObject(0)
		start, ok1 := so.(int64)
		count, ok2 := co.(int64)
		if err1 != nil || err2 != nil || !ok1 || !ok2 || start < 0 || count < 0 {
			return nil, malformed("pdf: bad cross-reference subsection")
		}
		if count > int64(len(l.b)-l.pos)/10+1 || count > maxPDFObjects {
			return nil, malformed("pdf: cross-reference subsection is larger than the file")
		}
		for i := int64(0); i < count; i++ {
			oo, e1 := l.readObject(0)
			go_, e2 := l.readObject(0)
			ko, e3 := l.readObject(0)
			offset, ok1 := oo.(int64)
			gen, ok2 := go_.(int64)
			kind, ok3 := ko.(pdfKeyword)
			if e1 != nil || e2 != nil || e3 != nil || !ok1 || !ok2 || !ok3 {
				return nil, malformed("pdf: bad cross-reference entry")
			}
			num := int(start + i)
			if _, have := f.xref[num]; have || kind != "n" || offset <= 0 {
				if !have && kind == "f" {
					f.xref[num] = xrefEntry{typ: 0}
				}
				continue
			}
			if len(f.xref) >= maxPDFObjects {
				return nil, malformed("pdf: too many objects")
			}
			f.xref[num] = xrefEntry{typ: 1, a: int(offset), b: int(gen)}
		}
	}
}

func (f *pdfFile) readXrefStream(off int) (pdfDict, error) {
	obj, err := f.parseAt(off, 0, true)
	if err != nil {
		return nil, err
	}
	st, ok := obj.(*pdfStream)
	if !ok || nameOf(st.dict["Type"]) != "XRef" {
		return nil, malformed("pdf: not a cross-reference stream")
	}
	body, err := f.decode(st)
	if err != nil {
		return nil, err
	}
	w := f.ints(st.dict["W"])
	if len(w) != 3 || w[0] < 0 || w[1] < 0 || w[2] < 0 || w[0] > 8 || w[1] > 8 || w[2] > 8 || w[0]+w[1]+w[2] == 0 {
		return nil, malformed("pdf: bad cross-reference stream widths")
	}
	size, _ := st.dict["Size"].(int64)
	index := f.ints(st.dict["Index"])
	if len(index) == 0 {
		index = []int{0, int(size)}
	}
	rec := w[0] + w[1] + w[2]
	pos := 0
	for i := 0; i+1 < len(index); i += 2 {
		for n := 0; n < index[i+1]; n++ {
			if pos+rec > len(body) {
				break
			}
			get := func(width int) int {
				v := 0
				for k := 0; k < width; k++ {
					v = v<<8 | int(body[pos])
					pos++
				}
				return v
			}
			typ := 1
			if w[0] > 0 {
				typ = get(w[0])
			}
			a, b := get(w[1]), get(w[2])
			num := index[i] + n
			if _, have := f.xref[num]; have {
				continue
			}
			if len(f.xref) >= maxPDFObjects {
				return nil, malformed("pdf: too many objects")
			}
			switch typ {
			case 0:
				f.xref[num] = xrefEntry{typ: 0}
			case 1:
				f.xref[num] = xrefEntry{typ: 1, a: a, b: b}
			case 2:
				f.xref[num] = xrefEntry{typ: 2, a: a, b: b}
			}
		}
	}
	return st.dict, nil
}

var objHeaderRE = regexp.MustCompile(`(?:^|[\r\n\s])(\d{1,10})[ \t\r\n]+(\d{1,5})[ \t\r\n]+obj\b`)

// recover rebuilds the cross-reference information by scanning the whole file
// for "N G obj" headers, the way viewers repair a damaged PDF.
func (f *pdfFile) recover() error {
	f.xref = map[int]xrefEntry{}
	f.cache = map[int]any{}
	f.objstm = map[int]*objStream{}
	f.trailer = nil
	pos := 0
	for pos < len(f.data) {
		m := objHeaderRE.FindSubmatchIndex(f.data[pos:])
		if m == nil {
			break
		}
		num, _ := strconv.Atoi(string(f.data[pos+m[2] : pos+m[3]]))
		gen, _ := strconv.Atoi(string(f.data[pos+m[4] : pos+m[5]]))
		f.xref[num] = xrefEntry{typ: 1, a: pos + m[2], b: gen} // later definitions win
		if len(f.xref) > maxPDFObjects {
			return malformed("pdf: too many objects")
		}
		pos += m[1]
	}
	if len(f.xref) == 0 {
		return malformed("pdf: no objects found")
	}
	// Trailers, newest first: a key keeps the value of the newest trailer that
	// has it. Each read may run to the end of the file (an unterminated
	// string), so reading stops once it has read a few times the file: a file
	// of many "trailer" tokens costs linear time, not quadratic.
	read := 0
	for end := len(f.data); read <= 4*len(f.data); {
		i := bytes.LastIndex(f.data[:end], []byte("trailer"))
		if i < 0 {
			break
		}
		end = i
		l := &lexer{b: f.data, pos: i + len("trailer"), maxDepth: f.lim.MaxDepth}
		o, err := l.readObject(0)
		read += l.pos - (i + len("trailer"))
		if d, ok := o.(pdfDict); ok && err == nil {
			if f.trailer == nil {
				f.trailer = pdfDict{}
			}
			for k, v := range d {
				if _, have := f.trailer[k]; !have {
					f.trailer[k] = v
				}
			}
		}
	}
	// Object streams hold objects of their own, and a cross-reference stream
	// or a catalog may be the only record of the root.
	nums := make([]int, 0, len(f.xref))
	for n := range f.xref {
		nums = append(nums, n)
	}
	for _, n := range nums {
		e := f.xref[n]
		if e.typ != 1 {
			continue
		}
		o, err := f.parseAt(e.a, n, true)
		if err != nil {
			continue
		}
		var d pdfDict
		switch t := o.(type) {
		case *pdfStream:
			d = t.dict
		case pdfDict:
			d = t
		}
		switch nameOf(d["Type"]) {
		case "ObjStm":
			if st, ok := o.(*pdfStream); ok {
				if os, err := f.parseObjStm(st); err == nil {
					for i, on := range os.nums {
						if _, have := f.xref[on]; !have {
							f.xref[on] = xrefEntry{typ: 2, a: n, b: i}
						}
					}
					f.objstm[n] = os
				}
			}
		case "XRef":
			if f.trailer == nil {
				f.trailer = pdfDict{}
			}
			for _, k := range []string{"Root", "Encrypt", "Info"} {
				if v, ok := d[k]; ok {
					f.trailer[k] = v
				}
			}
		case "Catalog":
			if f.trailer == nil {
				f.trailer = pdfDict{}
			}
			if _, ok := f.trailer["Root"]; !ok {
				f.trailer["Root"] = pdfRef{n, e.b}
			}
		}
	}
	f.cache = map[int]any{}
	if f.root() == nil {
		return malformed("pdf: no document catalog")
	}
	return nil
}

// ---- objects ----

// parseAt reads the indirect object at off. When want is non-zero the object
// number must match. A stream's data is attached when streams is true.
func (f *pdfFile) parseAt(off, want int, streams bool) (any, error) {
	if off < 0 || off >= len(f.data) {
		return nil, malformed("pdf: object offset is outside the file")
	}
	l := &lexer{b: f.data, pos: off, maxDepth: f.lim.MaxDepth}
	no, e1 := l.readObject(0)
	_, e2 := l.readObject(0)
	ko, e3 := l.readObject(0)
	num, ok := no.(int64)
	if e1 != nil || e2 != nil || e3 != nil || !ok || ko != pdfKeyword("obj") {
		return nil, malformed("pdf: bad object header")
	}
	if want != 0 && int(num) != want {
		return nil, malformed("pdf: object number mismatch")
	}
	o, err := l.readObject(0)
	if err != nil {
		if errors.Is(err, errPDFSyntax) {
			return nil, malformed("pdf: bad object")
		}
		return nil, err
	}
	if k, ok := o.(pdfKeyword); ok {
		_ = k
		return nil, nil
	}
	if n, ok := o.(int64); ok {
		if r, ok := l.tryRef(n); ok {
			return r, nil
		}
	}
	d, isDict := o.(pdfDict)
	if !isDict || !streams {
		return o, nil
	}
	save := l.pos
	l.skipWS()
	if !bytes.HasPrefix(f.data[l.pos:], []byte("stream")) {
		l.pos = save
		return o, nil
	}
	p := l.pos + len("stream")
	if p < len(f.data) && f.data[p] == '\r' {
		p++
	}
	if p < len(f.data) && f.data[p] == '\n' {
		p++
	}
	return &pdfStream{dict: d, raw: f.streamBytes(d, p)}, nil
}

// streamBytes finds the encoded bytes of a stream that starts at p: Length
// bytes when that is trustworthy (followed by endstream), else up to the next
// endstream.
func (f *pdfFile) streamBytes(d pdfDict, p int) []byte {
	if n, ok := f.resolve(d["Length"]).(int64); ok && n >= 0 && n <= int64(len(f.data)-p) {
		end := p + int(n)
		l := &lexer{b: f.data, pos: end, maxDepth: f.lim.MaxDepth}
		l.skipWS()
		if bytes.HasPrefix(f.data[l.pos:], []byte("endstream")) {
			return f.data[p:end]
		}
	}
	end := f.nextEndstream(p)
	if end < 0 {
		return f.data[p:]
	}
	if end > p && f.data[end-1] == '\n' {
		end--
	}
	if end > p && f.data[end-1] == '\r' {
		end--
	}
	return f.data[p:end]
}

// nextEndstream returns where the first "endstream" at or after p starts, or -1.
// The keywords are found in one pass over the file, so many streams with no
// usable /Length do not each scan the rest of the file.
func (f *pdfFile) nextEndstream(p int) int {
	if !f.endIndexed {
		f.endIndexed = true
		kw := []byte("endstream")
		for at := 0; ; {
			i := bytes.Index(f.data[at:], kw)
			if i < 0 {
				break
			}
			f.endstreams = append(f.endstreams, at+i)
			at += i + len(kw)
		}
	}
	k := sort.SearchInts(f.endstreams, p)
	if k == len(f.endstreams) {
		return -1
	}
	return f.endstreams[k]
}

// load reads object num, from the file or from an object stream.
func (f *pdfFile) load(num int) any {
	if o, ok := f.cache[num]; ok {
		return o
	}
	if f.busy[num] {
		return nil // a reference cycle
	}
	e, ok := f.xref[num]
	if !ok || e.typ == 0 {
		return nil
	}
	f.busy[num] = true
	defer delete(f.busy, num)
	if f.objects++; f.objects > maxPDFObjects*4 {
		return nil
	}
	var o any
	switch e.typ {
	case 1:
		var err error
		o, err = f.parseAt(e.a, num, true)
		if err != nil {
			o = nil
			f.w.add("pdf_object_unreadable")
		}
	case 2:
		os := f.objstm[e.a]
		if os == nil {
			st, _ := f.load(e.a).(*pdfStream)
			if st != nil {
				var err error
				if os, err = f.parseObjStm(st); err == nil {
					f.objstm[e.a] = os
				}
			}
		}
		if os != nil {
			o = f.fromObjStm(os, num, e.b)
		}
	}
	f.cache[num] = o
	return o
}

func (f *pdfFile) parseObjStm(st *pdfStream) (*objStream, error) {
	n, _ := f.resolve(st.dict["N"]).(int64)
	first, _ := f.resolve(st.dict["First"]).(int64)
	if n < 0 || n > maxPDFObjects || first < 0 {
		return nil, malformed("pdf: bad object stream")
	}
	body, err := f.decode(st)
	if err != nil {
		return nil, err
	}
	if first > int64(len(body)) {
		return nil, malformed("pdf: bad object stream")
	}
	os := &objStream{data: body, first: int(first), n: int(n)}
	l := &lexer{b: body[:first], maxDepth: f.lim.MaxDepth}
	for i := 0; i < int(n); i++ {
		a, e1 := l.readObject(0)
		b, e2 := l.readObject(0)
		num, ok1 := a.(int64)
		off, ok2 := b.(int64)
		if e1 != nil || e2 != nil || !ok1 || !ok2 {
			break
		}
		os.nums = append(os.nums, int(num))
		os.offs = append(os.offs, int(off))
	}
	return os, nil
}

func (f *pdfFile) fromObjStm(os *objStream, num, idx int) any {
	if idx < 0 || idx >= len(os.nums) || os.nums[idx] != num {
		idx = -1
		for i, n := range os.nums {
			if n == num {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil
		}
	}
	start := os.first + os.offs[idx]
	if start < 0 || start >= len(os.data) {
		return nil
	}
	l := &lexer{b: os.data, pos: start, maxDepth: f.lim.MaxDepth}
	o, err := l.readObject(0)
	if err != nil {
		return nil
	}
	if k, ok := o.(pdfKeyword); ok {
		_ = k
		return nil
	}
	if n, ok := o.(int64); ok {
		if r, ok := l.tryRef(n); ok {
			return r
		}
	}
	return o
}

// resolve follows indirect references to a direct object.
func (f *pdfFile) resolve(o any) any {
	for i := 0; i < maxResolve; i++ {
		r, ok := o.(pdfRef)
		if !ok {
			return o
		}
		o = f.load(r.num)
	}
	return nil
}

// ---- typed access ----

func nameOf(o any) string {
	n, _ := o.(pdfName)
	return string(n)
}

func (f *pdfFile) dict(o any) pdfDict {
	switch t := f.resolve(o).(type) {
	case pdfDict:
		return t
	case *pdfStream:
		return t.dict
	}
	return nil
}

func (f *pdfFile) array(o any) pdfArray {
	a, _ := f.resolve(o).(pdfArray)
	return a
}

func (f *pdfFile) stream(o any) *pdfStream {
	s, _ := f.resolve(o).(*pdfStream)
	return s
}

func (f *pdfFile) number(o any) (float64, bool) {
	switch t := f.resolve(o).(type) {
	case int64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

func (f *pdfFile) ints(o any) []int {
	var out []int
	for _, v := range f.array(o) {
		if n, ok := f.number(v); ok {
			out = append(out, int(n))
		}
	}
	return out
}

func (f *pdfFile) floats(o any, n int) []float64 {
	a := f.array(o)
	if len(a) < n {
		return nil
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		v, ok := f.number(a[i])
		if !ok {
			return nil
		}
		out[i] = v
	}
	return out
}

// ---- stream decoding ----

var errUnsupportedFilter = errors.New("unsupported PDF filter")

// decode applies a stream's filters. Every inflating filter counts what it
// produces against the document's budget. A stream decoded again (one content
// stream listed many times, a form drawn on every page) counts again whatever
// its filters, so repetition cannot multiply it past the budget. A stream
// whose filter is not supported (an image codec) returns errUnsupportedFilter.
func (f *pdfFile) decode(s *pdfStream) ([]byte, error) {
	filters := f.filterNames(s.dict["Filter"])
	if len(filters) == 0 {
		filters = f.filterNames(s.dict["F"])
	}
	parms := f.array(s.dict["DecodeParms"])
	if len(parms) == 0 {
		if d := f.dict(s.dict["DecodeParms"]); d != nil {
			parms = pdfArray{d}
		} else if d := f.dict(s.dict["DP"]); d != nil {
			parms = pdfArray{d}
		}
	}
	data := s.raw
	charged := false // a filter counted all it produced
	for i, name := range filters {
		var dp pdfDict
		if i < len(parms) {
			dp = f.dict(parms[i])
		}
		var err error
		switch name {
		case "FlateDecode", "Fl":
			data, err = f.inflate(data, dp, false)
			charged = true
		case "LZWDecode", "LZW":
			data, err = f.inflate(data, dp, true)
			charged = true
		case "ASCIIHexDecode", "AHx":
			data = asciiHexDecode(data)
		case "ASCII85Decode", "A85":
			n := len(data)
			data = ascii85Decode(data)
			err = f.bud.take(int64(max(len(data)-n, 0))) // "z" makes four bytes of one
		case "RunLengthDecode", "RL":
			data, err = f.runLengthDecode(data)
			charged = true
		case "Crypt":
			// identity only (an encrypted file was refused earlier)
		default:
			return nil, errUnsupportedFilter
		}
		if err != nil {
			return nil, err
		}
	}
	if !charged {
		if f.decoded[s] || f.free+len(data) > len(f.data) {
			if err := f.bud.take(int64(len(data))); err != nil {
				return nil, err
			}
		} else {
			f.free += len(data)
		}
	}
	f.decoded[s] = true
	return data, nil
}

func (f *pdfFile) filterNames(o any) []string {
	switch t := f.resolve(o).(type) {
	case pdfName:
		return []string{string(t)}
	case pdfArray:
		var out []string
		for _, v := range t {
			if n, ok := f.resolve(v).(pdfName); ok {
				out = append(out, string(n))
			}
		}
		return out
	}
	return nil
}

// budgetReader counts the bytes read through it against the budget and stops
// at the limit.
type budgetReader struct {
	r   io.Reader
	f   *pdfFile
	n   int64
	raw int64
	err error
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if room := b.f.bud.room(); int64(len(p)) > room+1 {
		if room < 0 {
			room = 0
		}
		p = p[:room+1]
	}
	n, err := b.r.Read(p)
	if n > 0 {
		b.n += int64(n)
		if e := b.f.bud.take(int64(n)); e != nil {
			b.err = e
			return 0, e
		}
		if e := b.f.bud.checkRatio(b.n, b.raw); e != nil {
			b.err = e
			return 0, e
		}
	}
	return n, err
}

// inflate decodes Flate or LZW data. Data that is damaged part-way yields what
// was decoded before the damage, as viewers do; a budget failure is final.
func (f *pdfFile) inflate(data []byte, dp pdfDict, lzwMode bool) ([]byte, error) {
	var src io.Reader
	if lzwMode {
		src = lzw.NewReader(bytes.NewReader(data), lzw.MSB, 8)
	} else {
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			// A missing zlib header: some writers emit raw deflate.
			f.w.add("pdf_stream_unreadable")
			return nil, nil
		}
		src = zr
	}
	br := &budgetReader{r: src, f: f, raw: int64(len(data))}
	out, err := io.ReadAll(br)
	if err != nil {
		var fe *Error
		if errors.As(err, &fe) {
			return nil, fe
		}
		f.w.add("pdf_stream_damaged")
	}
	return f.predict(out, dp), nil
}

// predict undoes the PNG and TIFF predictors of a Flate or LZW stream.
func (f *pdfFile) predict(data []byte, dp pdfDict) []byte {
	if dp == nil {
		return data
	}
	pred := 1
	if n, ok := f.number(dp["Predictor"]); ok {
		pred = int(n)
	}
	if pred <= 1 {
		return data
	}
	colors, bpc, cols := 1, 8, 1
	if n, ok := f.number(dp["Colors"]); ok && n >= 1 && n <= 32 {
		colors = int(n)
	}
	if n, ok := f.number(dp["BitsPerComponent"]); ok && n >= 1 && n <= 16 {
		bpc = int(n)
	}
	if n, ok := f.number(dp["Columns"]); ok && n >= 1 && n <= 1<<20 {
		cols = int(n)
	}
	bpp := (colors*bpc + 7) / 8
	rowLen := (colors*bpc*cols + 7) / 8
	if rowLen <= 0 {
		return data
	}
	if pred == 2 { // TIFF, 8-bit only
		if bpc != 8 {
			return data
		}
		for r := 0; r+rowLen <= len(data); r += rowLen {
			for i := bpp; i < rowLen; i++ {
				data[r+i] += data[r+i-bpp]
			}
		}
		return data
	}
	// PNG: each row is a filter byte then rowLen bytes.
	stride := rowLen + 1
	rows := len(data) / stride
	if rows == 0 {
		return []byte{} // without allocating rows the data cannot fill
	}
	out := make([]byte, 0, rows*rowLen)
	prev := make([]byte, rowLen)
	cur := make([]byte, rowLen)
	for r := 0; r < rows; r++ {
		ft := data[r*stride]
		copy(cur, data[r*stride+1:(r+1)*stride])
		switch ft {
		case 1:
			for i := bpp; i < rowLen; i++ {
				cur[i] += cur[i-bpp]
			}
		case 2:
			for i := 0; i < rowLen; i++ {
				cur[i] += prev[i]
			}
		case 3:
			for i := 0; i < rowLen; i++ {
				left := 0
				if i >= bpp {
					left = int(cur[i-bpp])
				}
				cur[i] += byte((left + int(prev[i])) / 2)
			}
		case 4:
			for i := 0; i < rowLen; i++ {
				a, c := 0, 0
				if i >= bpp {
					a, c = int(cur[i-bpp]), int(prev[i-bpp])
				}
				cur[i] += paeth(a, int(prev[i]), c)
			}
		}
		out = append(out, cur...)
		prev, cur = cur, prev
	}
	return out
}

func paeth(a, b, c int) byte {
	p := a + b - c
	pa, pb, pc := abs(p-a), abs(p-b), abs(p-c)
	switch {
	case pa <= pb && pa <= pc:
		return byte(a)
	case pb <= pc:
		return byte(b)
	}
	return byte(c)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func asciiHexDecode(data []byte) []byte {
	var out []byte
	var hi byte
	have := false
	for _, c := range data {
		if c == '>' {
			break
		}
		v, ok := hexVal(c)
		if !ok {
			continue
		}
		if have {
			out = append(out, hi<<4|v)
			have = false
		} else {
			hi, have = v, true
		}
	}
	if have {
		out = append(out, hi<<4)
	}
	return out
}

func ascii85Decode(data []byte) []byte {
	var out []byte
	var group [5]byte
	n := 0
	data = bytes.TrimSpace(data)
	data = bytes.TrimPrefix(data, []byte("<~"))
	for _, c := range data {
		switch {
		case c == '~':
			goto flush
		case isPDFSpace(c):
		case c == 'z' && n == 0:
			out = append(out, 0, 0, 0, 0)
		case c >= '!' && c <= 'u':
			group[n] = c - '!'
			n++
			if n == 5 {
				v := uint32(0)
				for _, g := range group {
					v = v*85 + uint32(g)
				}
				out = append(out, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
				n = 0
			}
		}
	}
flush:
	if n > 1 {
		for i := n; i < 5; i++ {
			group[i] = 84
		}
		v := uint32(0)
		for _, g := range group {
			v = v*85 + uint32(g)
		}
		b := []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
		out = append(out, b[:n-1]...)
	}
	return out
}

func (f *pdfFile) runLengthDecode(data []byte) ([]byte, error) {
	var out []byte
	for i := 0; i < len(data); {
		n := int(data[i])
		i++
		switch {
		case n == 128:
			return out, nil
		case n < 128:
			if i+n+1 > len(data) {
				n = len(data) - i - 1
			}
			if n < 0 {
				return out, nil
			}
			out = append(out, data[i:i+n+1]...)
			i += n + 1
			if err := f.bud.take(int64(n + 1)); err != nil {
				return nil, err
			}
		default:
			if i >= len(data) {
				return out, nil
			}
			cnt := 257 - n
			for k := 0; k < cnt; k++ {
				out = append(out, data[i])
			}
			i++
			if err := f.bud.take(int64(cnt)); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
