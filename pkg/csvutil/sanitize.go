// Package csvutil guards CSV exports against spreadsheet formula injection.
package csvutil

import (
	"encoding/csv"
	"io"
	"strings"
)

// SanitizeCell guards against CSV formula injection. Spreadsheet apps (Excel,
// LibreOffice, Google Sheets) evaluate a cell as a formula when — after any
// leading whitespace/control bytes — it begins with '=', '+', '-', or '@'.
// Attacker-controlled values that reach a CSV export (an API-key holder's
// model name, request id, fail_reason, owner label, ...) could otherwise
// execute arbitrary formulas when an admin opens the export.
//
// Leading control bytes (tab, CR, LF, NUL, VT, FF, space) are stripped first
// so an attacker can't evade the first-byte check by prefixing "=evil" with
// "\t" or "\n". Cells whose first non-control byte is a formula marker are
// then prefixed with a tab, which spreadsheet apps treat as a text marker
// (invisible in most viewers); this is the OWASP-recommended mitigation.
//
// Empty / all-control strings are returned unchanged.
func SanitizeCell(s string) string {
	t := strings.TrimLeft(s, "\t\r\n\x00\v\f ")
	if t == "" {
		return s
	}
	switch t[0] {
	case '=', '+', '-', '@':
		return "\t" + t
	}
	return s
}

// SanitizeRow applies SanitizeCell to every cell of a CSV record in place.
// Use it on every row built from user-controlled data before csv.Writer.Write.
func SanitizeRow(row []string) []string {
	for i, c := range row {
		row[i] = SanitizeCell(c)
	}
	return row
}

// WriteCSV writes the UTF-8 BOM + headers + sanitized records to w in one
// shot. It centralizes the CSV-write pattern (BOM, header, per-row sanitize,
// flush) that every exporter needs, so the BOM convention and the sanitize
// discipline live in one place instead of being copy-pasted per service.
//
// Callers MUST have already built records successfully before calling this —
// a failure here is a write-time error (client disconnect, broken pipe), not
// a build error, and by then the HTTP response is already committed.
func WriteCSV(w io.Writer, headers []string, records [][]string) error {
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(headers); err != nil {
		return err
	}
	for _, r := range records {
		if err := cw.Write(SanitizeRow(r)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
