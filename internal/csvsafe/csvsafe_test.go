package csvsafe

import "testing"

func TestCell(t *testing.T) {
	for in, want := range map[string]string{
		"":                         "",
		"hello":                    "hello",
		"a=b":                      "a=b",
		"=SUM(A1:A9)":              "'=SUM(A1:A9)",
		"@SUM(1)":                  "'@SUM(1)",
		"+cmd|' /C calc'!A0":       "'+cmd|' /C calc'!A0",
		"-2+3":                     "'-2+3",
		"\t=1":                     "'\t=1",
		"\r=1":                     "'\r=1",
		"-5":                       "-5",
		"+3.2":                     "+3.2",
		"-1e3":                     "-1e3",
		"-":                        "'-",
		"- dash list item":         "'- dash list item",
		"=HYPERLINK(\"http://x\")": "'=HYPERLINK(\"http://x\")",
	} {
		if got := Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}
