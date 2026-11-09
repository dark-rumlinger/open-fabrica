package content

import (
	"strings"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"
)

// The get page budget must equal the shared single-read budget, and manage_files'
// MaxResult must sit above it - so a full get body is never truncated mid-line,
// and a file too big to pre-inline is still readable in one get rather than
// several paginated ones (see store.MaxSingleReadBody).
func TestGetBudgetCoupling(t *testing.T) {
	if getBodyBudget != store.MaxSingleReadBody {
		t.Fatalf("getBodyBudget (%d) must equal store.MaxSingleReadBody (%d) so get pages match the inline cap", getBodyBudget, store.MaxSingleReadBody)
	}
	def, err := tools.DefaultRegistry().Get("manage_files")
	if err != nil {
		t.Fatalf("manage_files not registered: %v", err)
	}
	if def.MaxResult <= getBodyBudget {
		t.Fatalf("manage_files MaxResult (%d) must exceed getBodyBudget (%d) so a full get body is not truncated", def.MaxResult, getBodyBudget)
	}
	if outlineTrigger >= getBodyBudget {
		t.Fatalf("outlineTrigger (%d) must sit below getBodyBudget (%d) so the no-outline-but-truncated zone cannot reappear", outlineTrigger, getBodyBudget)
	}
}

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
