// Package xlsx writes a minimal, valid .xlsx (Office Open XML
// SpreadsheetML) file from a plain grid of string cells - stdlib only, no
// third-party dependency. It supports exactly what this module's reports
// need: a single unstyled sheet, one row per []string, every cell written
// as plain text (no number/date formatting, no cell styling). The result
// opens correctly in Excel, Google Sheets, and LibreOffice, but
// numeric-looking values show up as text (left-aligned, not directly
// summable) rather than real numbers - fine for a report meant to be read
// or re-imported, not calculated on directly.
package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"
)

// WriteFile creates path as a new .xlsx file containing a single sheet
// named "Sheet1" with one row per entry in rows (include your own header
// as rows[0] if you want one). Rows don't need to be the same length -
// each is written out to however many columns it actually has.
func WriteFile(path string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := Write(f, rows); err != nil {
		return err
	}
	return f.Close()
}

// Write is WriteFile without the file-creation step, for a caller that
// already has an io.Writer to target (e.g. an in-memory buffer).
func Write(w io.Writer, rows [][]string) error {
	zw := zip.NewWriter(w)

	parts := []struct {
		name string
		body string
	}{
		{"[Content_Types].xml", contentTypesXML},
		{"_rels/.rels", rootRelsXML},
		{"xl/workbook.xml", workbookXML},
		{"xl/_rels/workbook.xml.rels", workbookRelsXML},
		{"xl/worksheets/sheet1.xml", sheetXML(rows)},
	}
	for _, part := range parts {
		pw, err := zw.Create(part.name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(pw, part.body); err != nil {
			return err
		}
	}
	return zw.Close()
}

// ConvertFile reads the CSV file at csvPath and writes an equivalent
// single-sheet .xlsx file to xlsxPath - one row per CSV row (header
// included, since encoding/csv doesn't distinguish it), every field as
// plain text.
func ConvertFile(csvPath, xlsxPath string) error {
	f, err := os.Open(csvPath)
	if err != nil {
		return err
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return err
	}
	return WriteFile(xlsxPath, rows)
}

// ---------------------------------------------------------------------------
// Fixed (non-data) OOXML parts.
//
// A .xlsx is a zip of these small XML parts; the ones below never vary
// between reports since we only ever produce one plain, unstyled sheet.
// Only xl/worksheets/sheet1.xml (built by sheetXML) actually depends on the
// data being written.
// ---------------------------------------------------------------------------

const xmlDecl = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

const contentTypesXML = xmlDecl + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
	`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
	`<Default Extension="xml" ContentType="application/xml"/>` +
	`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
	`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
	`</Types>`

const rootRelsXML = xmlDecl + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`</Relationships>`

const workbookXML = xmlDecl + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
	`<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets>` +
	`</workbook>`

const workbookRelsXML = xmlDecl + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
	`</Relationships>`

// sheetXML builds xl/worksheets/sheet1.xml: one <row> per entry in rows,
// one <c> (cell) per field, all written as inline strings (t="inlineStr")
// so no shared-strings table is needed.
func sheetXML(rows [][]string) string {
	var b strings.Builder
	b.WriteString(xmlDecl)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for i, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, i+1)
		for col, cell := range row {
			fmt.Fprintf(&b, `<c r="%s%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`,
				columnName(col), i+1, escapeCellText(cell))
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// columnName converts a 0-based column index to its spreadsheet column
// letters (0 -> "A", 25 -> "Z", 26 -> "AA", ...).
func columnName(col int) string {
	col++ // switch to 1-based for the standard bijective base-26 algorithm
	var name string
	for col > 0 {
		col--
		name = string(rune('A'+col%26)) + name
		col /= 26
	}
	return name
}

// escapeCellText makes s safe to embed as XML character content: strips
// bytes that are outright illegal in XML 1.0 (most control characters -
// encoding/xml.EscapeText doesn't filter these, it just passes them
// through, which would produce a file some parsers reject), then escapes
// the rest the standard way.
func escapeCellText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r < 0x20:
			return -1
		case r >= 0xD800 && r <= 0xDFFF: // lone UTF-16 surrogate halves
			return -1
		default:
			return r
		}
	}, s)

	var buf bytes.Buffer
	// xml.EscapeText's own error is only ever from the underlying Writer;
	// a bytes.Buffer never fails to write.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
