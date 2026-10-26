package exports

import "strings"

// formulaPrefixes are the characters that start a spreadsheet formula when a
// cell is opened in Excel, LibreOffice or Google Sheets. Prefixed cells are
// written as text so a model-supplied cell cannot become a formula.
var formulaPrefixes = [256]bool{
	'=': true, '+': true, '-': true, '@': true,
	'\t': true, '\r': true, // leading whitespace before a formula marker
}

// guardFormula returns s with a leading apostrophe when it would be read as a
// formula. The apostrophe is the spreadsheet convention for "force text".
func guardFormula(s string) string {
	if s == "" {
		return s
	}
	// Trim only the leading BOM / space that would hide a marker; keep the rest.
	t := strings.TrimLeft(s, " \t\r\n")
	if t == "" {
		return s
	}
	if formulaPrefixes[t[0]] {
		return "'" + s
	}
	return s
}
