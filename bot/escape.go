package bot

import (
	"fmt"
	"strings"
)

// escapeHTML escapes the three characters that Telegram's HTML parse mode
// treats specially: &, <, >. Other punctuation is fine.
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// htmlf formats a template, escaping each argument's HTML-significant characters
// while leaving the template's own tags intact. Use this whenever interpolating
// user-controlled or DB-sourced strings into a message that uses HTML parse mode.
func htmlf(format string, args ...any) string {
	escaped := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case string:
			escaped[i] = escapeHTML(v)
		case error:
			escaped[i] = escapeHTML(v.Error())
		default:
			escaped[i] = escapeHTML(fmt.Sprint(v))
		}
	}
	return fmt.Sprintf(format, escaped...)
}
