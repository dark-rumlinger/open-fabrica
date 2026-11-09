// Package csvsafe neutralises spreadsheet formula injection in exported CSV.
//
// A cell that starts with = + - @ (or a tab or carriage return) is executed as a
// formula when the file is opened in Excel, LibreOffice or Sheets, so a value a
// visitor submitted through a public form can run on the owner's machine when
// they open an export. Prefixing such a cell with a single quote makes the
// spreadsheet treat it as text.
package csvsafe

import "strconv"

// Cell returns s ready to be written as a CSV field. A value that begins with a
// formula trigger gets a leading single quote, except a plain number such as
// "-5" or "+3.2", which is data rather than a formula and is left alone.
func Cell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '@', '\t', '\r':
		return "'" + s
	case '+', '-':
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return s
		}
		return "'" + s
	}
	return s
}
