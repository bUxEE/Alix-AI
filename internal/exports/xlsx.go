package exports

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Minimal OOXML spreadsheet (ECMA-376): one workbook, shared strings, one sheet
// per dataset. No external dependency. Licence: original AlixAI code.

func renderXLSX(sets []Dataset) ([]byte, error) {
	if len(sets) == 0 {
		return nil, fmt.Errorf("no tabular data")
	}
	var shared []string
	index := map[string]int{}
	share := func(s string) int {
		s = guardFormula(s)
		if i, ok := index[s]; ok {
			return i
		}
		i := len(shared)
		shared = append(shared, s)
		index[s] = i
		return i
	}

	sheets := make([]xlsxSheet, 0, len(sets))
	usedNames := map[string]int{}
	for i, set := range sets {
		name := uniqueSheetName(sanitizeSheetName(set.Name), usedNames, i+1)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
		b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
		// header row
		b.WriteString(`<row r="1">`)
		for c, h := range set.Headers {
			writeInlineCell(&b, 1, c+1, "s", strconv.Itoa(share(h)))
		}
		b.WriteString(`</row>`)
		for r, row := range set.Rows {
			b.WriteString(`<row r="` + strconv.Itoa(r+2) + `">`)
			for c, cell := range row {
				kind := kindText
				if c < len(set.Kinds) {
					kind = set.Kinds[c]
				}
				writeTypedCell(&b, r+2, c+1, cell, kind, share)
			}
			b.WriteString(`</row>`)
		}
		b.WriteString(`</sheetData></worksheet>`)
		sheets = append(sheets, xlsxSheet{name: name, xml: b.String()})
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	must := func(name, body string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(body))
		return err
	}
	if err := must("[Content_Types].xml", contentTypesXML(len(sheets))); err != nil {
		return nil, err
	}
	if err := must("_rels/.rels", rootRelsXML); err != nil {
		return nil, err
	}
	if err := must("xl/workbook.xml", workbookXML(sheets)); err != nil {
		return nil, err
	}
	if err := must("xl/_rels/workbook.xml.rels", workbookRelsXML(len(sheets))); err != nil {
		return nil, err
	}
	if err := must("xl/sharedStrings.xml", sharedStringsXML(shared)); err != nil {
		return nil, err
	}
	if err := must("xl/styles.xml", stylesXML); err != nil {
		return nil, err
	}
	for i, sh := range sheets {
		if err := must(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), sh.xml); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func uniqueSheetName(base string, used map[string]int, n int) string {
	if base == "" {
		base = "Sheet" + strconv.Itoa(n)
	}
	name := base
	if used[name] > 0 {
		suffix := strconv.Itoa(used[name] + 1)
		trim := 31 - len(suffix) - 1
		if trim < 1 {
			trim = 1
		}
		if len(base) > trim {
			base = base[:trim]
		}
		name = base + "_" + suffix
	}
	used[name]++
	if used[base] == 0 {
		used[base] = 1
	} else {
		used[base]++
	}
	return name
}

func writeTypedCell(b *strings.Builder, row, col int, raw string, kind cellKind, share func(string) int) {
	switch kind {
	case kindNumber:
		if raw == "" {
			writeInlineCell(b, row, col, "inlineStr", "")
			return
		}
		if _, err := strconv.ParseFloat(raw, 64); err == nil {
			writeInlineCell(b, row, col, "n", raw)
			return
		}
	case kindBool:
		switch strings.ToLower(raw) {
		case "true", "1":
			writeInlineCell(b, row, col, "b", "1")
			return
		case "false", "0":
			writeInlineCell(b, row, col, "b", "0")
			return
		}
	case kindDate:
		if t, ok := parseExportDate(raw); ok {
			// Excel serial date (days since 1899-12-30).
			serial := t.UTC().Sub(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)).Hours() / 24
			writeInlineCell(b, row, col, "n", strconv.FormatFloat(serial, 'f', -1, 64))
			return
		}
	}
	writeInlineCell(b, row, col, "s", strconv.Itoa(share(raw)))
}

func writeInlineCell(b *strings.Builder, row, col int, typ, v string) {
	ref := colName(col) + strconv.Itoa(row)
	switch typ {
	case "s":
		fmt.Fprintf(b, `<c r="%s" t="s"><v>%s</v></c>`, ref, xmlEscape(v))
	case "n":
		fmt.Fprintf(b, `<c r="%s"><v>%s</v></c>`, ref, xmlEscape(v))
	case "b":
		fmt.Fprintf(b, `<c r="%s" t="b"><v>%s</v></c>`, ref, xmlEscape(v))
	case "inlineStr":
		fmt.Fprintf(b, `<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, ref, xmlEscape(v))
	default:
		fmt.Fprintf(b, `<c r="%s" t="s"><v>%s</v></c>`, ref, xmlEscape(v))
	}
}

func colName(n int) string {
	var s []byte
	for n > 0 {
		n--
		s = append([]byte{byte('A' + n%26)}, s...)
		n /= 26
	}
	return string(s)
}

func parseExportDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func contentTypesXML(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	b.WriteString(`<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>`)
	b.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`</Relationships>`

const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
	`<fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>` +
	`<fills count="1"><fill><patternFill patternType="none"/></fill></fills>` +
	`<borders count="1"><border/></borders>` +
	`<cellStyleXfs count="1"><xf/></cellStyleXfs>` +
	`<cellXfs count="1"><xf xfId="0"/></cellXfs>` +
	`</styleSheet>`

type xlsxSheet struct {
	name string
	xml  string
}

func workbookXML(sheets []xlsxSheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	b.WriteString(`<sheets>`)
	for i, sh := range sheets {
		fmt.Fprintf(&b, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEscape(sh.name), i+1, i+1)
	}
	b.WriteString(`</sheets></workbook>`)
	return b.String()
}

func workbookRelsXML(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
	}
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>`, n+1)
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, n+2)
	b.WriteString(`</Relationships>`)
	return b.String()
}

func sharedStringsXML(shared []string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	fmt.Fprintf(&b, `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="%d" uniqueCount="%d">`, len(shared), len(shared))
	for _, s := range shared {
		fmt.Fprintf(&b, `<si><t>%s</t></si>`, xmlEscape(s))
	}
	b.WriteString(`</sst>`)
	return b.String()
}
