package content

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// A full-budget HTML body must survive the executor marshal within manage_files'
// MaxResult, with envelope headroom to spare. HTML-escaped marshaling used to
// inflate every angle bracket 5x and push a full get past the cap AFTER the read
// tracker had recorded the window as delivered, so the model patched a tail it
// never saw (the stale-copy spiral).
func TestGetWorstCaseBodyFitsMaxResult(t *testing.T) {
	line := `<div class="card" data-i="7">&nbsp;Hello &amp; welcome, "friend" <b>now</b></div>`
	var b strings.Builder
	for b.Len() < getBodyBudget {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	body := b.String()[:getBodyBudget]
	res := tools.Ok(map[string]any{
		"kind":         "page",
		"file_path":    "/",
		"content_type": "text/html",
		"version":      3,
		"metadata":     map[string]any{"size": len(body)},
		"note":         "lines 1-400 of 400",
		"content":      body,
	})
	data, err := tools.MarshalToolResult(res)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	def, err := tools.DefaultRegistry().Get("manage_files")
	if err != nil {
		t.Fatalf("manage_files not registered: %v", err)
	}
	if len(data) > def.MaxResult-1500 {
		t.Fatalf("worst-case get payload (%d bytes) must fit MaxResult (%d) with envelope headroom", len(data), def.MaxResult)
	}
}
