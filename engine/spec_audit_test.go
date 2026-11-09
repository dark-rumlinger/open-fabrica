package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"

	_ "modernc.org/sqlite"
)

// The spec-audit must see a hosted LLM/agent endpoint's system prompt, which lives in
// config, not a file: otherwise it calls a correctly-built endpoint missing (the false
// spec_gap that shipped Naughty Nick broken). A truly empty prompt still reads as unset,
// so a real gap is not hidden.
func TestEndpointPromptsSurfacesSystemPrompt(t *testing.T) {
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`CREATE TABLE of_llm_endpoints (path TEXT, system_prompt TEXT);
		CREATE TABLE of_agent_endpoints (path TEXT, system_prompt TEXT);
		INSERT INTO of_llm_endpoints(path, system_prompt) VALUES ('santa', 'You are Bad Santa, grumpy and sarcastic.'), ('mute', '');
		INSERT INTO of_agent_endpoints(path, system_prompt) VALUES ('triage', 'Route the ticket to a queue.');`); err != nil {
		t.Fatal(err)
	}
	got := endpointPrompts(d)
	if !strings.Contains(got, "Bad Santa") {
		t.Errorf("a set LLM system prompt must be shown to the auditor, got:\n%s", got)
	}
	if !strings.Contains(got, "/api/santa/chat and /complete") {
		t.Errorf("the LLM endpoint route label is missing, got:\n%s", got)
	}
	if !strings.Contains(got, "Route the ticket") || !strings.Contains(got, "/api/triage/run") {
		t.Errorf("an agent endpoint prompt must be shown too, got:\n%s", got)
	}
	if !strings.Contains(got, "/api/mute/chat and /complete: (no system prompt set)") {
		t.Errorf("a genuinely empty prompt must read as unset so a real gap is not hidden, got:\n%s", got)
	}
}

// crudAccess must surface a public write so the auditor does not read a
// public-submission endpoint (anonymous POST, auth-gated GET: a contact form) as
// blocking the POST - the false spec_gap that shipped a correct build as broken.
func TestCrudAccessNamesPublicWrite(t *testing.T) {
	// The contact-form shape: requires_auth with public_write, reads gated.
	got := crudAccess(true, false, true, "", "", "")
	if !strings.Contains(got, "writes are public") {
		t.Fatalf("public-submission endpoint must name its public write, got %q", got)
	}
	if !strings.Contains(got, "authenticated") {
		t.Fatalf("reads should still read as authenticated, got %q", got)
	}
	// Plain authenticated endpoint (no public write): unchanged single word.
	if got := crudAccess(true, false, false, "", "", ""); got != "authenticated" {
		t.Fatalf("a gated endpoint should stay %q, got %q", "authenticated", got)
	}
	// Already-public endpoint: no redundant write note.
	if got := crudAccess(false, true, true, "", "", ""); got != "public" {
		t.Fatalf("an already-public endpoint should stay %q, got %q", "public", got)
	}
}

func TestParseSpecAudit(t *testing.T) {
	for name, reply := range map[string]string{
		"bare":   `{"items":[{"id":"a","verdict":"partial","gaps":["x"]}]}`,
		"fenced": "```json\n{\"items\":[{\"id\":\"a\",\"verdict\":\"partial\",\"gaps\":[\"x\"]}]}\n```",
		"prose":  "Here is the review:\n{\"items\":[{\"id\":\"a\",\"verdict\":\"partial\",\"gaps\":[\"x\"]}]}\nDone.",
	} {
		v, err := parseSpecAudit(reply)
		if err != nil || len(v) != 1 || v[0].ID != "a" || v[0].Gaps[0] != "x" {
			t.Errorf("%s: got %+v, %v", name, v, err)
		}
	}
	if _, err := parseSpecAudit("no json here"); err == nil {
		t.Error("an unreadable reply is an error")
	}
}

// A verdict becomes a finding when the item exists and is not met; gaps are capped,
// and the finding points at the item's page. A non-met verdict that names no gap still
// drives a repair, through a gap synthesized from the item's done-when.
func TestSpecGapIssues(t *testing.T) {
	tasks := map[string]Task{
		"staff-page": {ID: "staff-page", Title: "Staff page", Paths: []string{"/staff", "/files/staff.js"}},
		"orders":     {ID: "orders", Title: "Orders table", Paths: []string{"orders"}, DoneWhen: "old orders are pruned nightly"},
	}
	got := specGapIssues([]specVerdict{
		{ID: "staff-page", Verdict: "partial", Gaps: []string{"undo is not wired", "no recent-orders view", "no Brewing filter", "a fourth"}},
		{ID: "orders", Verdict: "missing", Gaps: []string{"pruning never runs"}},
		{ID: "orders", Verdict: "partial", Evidence: "no purge code"},
		{ID: "ghost", Verdict: "missing", Gaps: []string{"x"}},    // not on the checklist
		{ID: "staff-page", Verdict: "met", Gaps: []string{"nit"}}, // met is met
	}, tasks, 1)
	if len(got) != 3 {
		t.Fatalf("want 3 findings, got %+v", got)
	}
	if !strings.Contains(got[2].Message, "old orders are pruned nightly") || !strings.Contains(got[2].Message, "no purge code") {
		t.Errorf("a gap-less verdict must carry its done-when and evidence: %+v", got[2])
	}
	staff := got[0]
	if staff.Page != "/staff" || staff.Category != specGapCategory || !staff.Deterministic ||
		!strings.Contains(staff.Message, "no Brewing filter") || strings.Contains(staff.Message, "a fourth") {
		t.Errorf("staff finding = %+v", staff)
	}
	if got[1].Page != "" || got[1].File != "orders" {
		t.Errorf("a table item points at its resource: %+v", got[1])
	}
	if ids := specGapTaskIDs(got); strings.Join(ids, ",") != "staff-page,orders,orders" {
		t.Errorf("re-audit ids = %v", ids)
	}
}

// The source shown to the auditor leaves out platform helpers and binary assets,
// puts markup before scripts before styles, and fits the budget by cutting the
// largest file first.
func TestSpecAuditSource(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.CloseAll)
	big := strings.Repeat("// a line of real code\n", 3000) // ~69 KB
	for _, f := range []struct{ kind, path, body, meta string }{
		{"file", "app.css", ".a{color:red}", "{}"},
		{"file", "app.js", big, "{}"},
		{"page", "/", "<main id=app></main>", "{}"},
		{"file", "js/ofnet.js", "export function connect(){}", `{"platform_managed":true}`},
		{"file", "logo.png", "PNGDATA", "{}"},
	} {
		if _, err := pdb.ExecWrite(`INSERT INTO of_files (kind, path, body, metadata, scope) VALUES (?, ?, ?, ?, 'page')`,
			f.kind, f.path, f.body, f.meta); err != nil {
			t.Fatal(err)
		}
	}
	src := specAuditSource(pdb.Reader(), "", 10_000)
	if strings.Contains(src, "ofnet") || strings.Contains(src, "PNGDATA") {
		t.Errorf("platform helpers and assets stay out:\n%.300s", src)
	}
	page, js, css := strings.Index(src, "=== / (page)"), strings.Index(src, "=== app.js"), strings.Index(src, "=== app.css")
	if page < 0 || js < page || css < js {
		t.Errorf("order must be page, script, style: %d %d %d", page, js, css)
	}
	if len(src) > 11_000 || !strings.Contains(src, "more bytes of this file not shown") {
		t.Errorf("source not clipped to budget: %d bytes", len(src))
	}
}

// What the platform serves from configuration reaches the auditor: without it an SEO
// item set through manage_seo, a UNIQUE slug index and a table-linked upload all read
// as missing, and the repair rounds chase them until the build ships flagged.
func TestSpecAuditSource_PlatformConfiguration(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.CloseAll)
	for _, s := range []string{
		`INSERT INTO of_files (kind, path, body, metadata) VALUES ('page', '/about', '<main></main>', '{"title":"About"}')`,
		`INSERT INTO of_seo_meta (page_path, description, og_image) VALUES ('/about', 'Who we are', '/files/og.png')`,
		`CREATE TABLE links (id INTEGER PRIMARY KEY, slug TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX uidx_links_slug ON links(slug)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('links', '{}')`,
		`INSERT INTO of_upload_endpoints (path, table_name, max_size_mb) VALUES ('links', 'links', 20)`,
		`INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES ('links', 'upload', 'POST', 'upload')`,
		`INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES ('santa', 'chat', 'POST', 'llm'), ('santa', 'complete', 'POST', 'llm')`,
		`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth) VALUES ('notes', 'links', '["GET","POST"]', 1)`,
		`INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES ('notes', '', 'GET', 'crud'), ('notes', '', 'POST', 'crud')`,
		`INSERT INTO of_auth_endpoints (path, table_name) VALUES ('auth', 'users')`,
	} {
		if _, err := pdb.ExecWrite(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	src := specAuditSource(pdb.Reader(), "User-agent: *\nDisallow: /admin", 50_000)
	for _, want := range []string{
		`/about: <title>="About" description="Who we are" og:title="About" og:image="/files/og.png"`,
		"Disallow: /admin",
		"/sitemap.xml is generated live",
		"CREATE UNIQUE INDEX uidx_links_slug ON links(slug)",
		"POST /api/links/upload (upload): multipart field `file`, max 20 MB",
		"inserts a row into links",
		// The LLM proxy mounts under /chat and /complete: a page fetching
		// /api/santa/chat is right, and the auditor must see that route.
		"POST /api/santa/chat (llm): server-sent token stream",
		"POST /api/santa/complete (llm): one JSON reply",
		"GET|POST /api/notes (crud): table links, access authenticated; one row at /api/notes/<id>",
		"POST /api/auth/login",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("auditor source lacks %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "of_files") {
		t.Error("platform tables must stay out of the schema block")
	}
}

// The builder's answer to a spec gap reaches the re-audit beside the item. A builder
// that showed the gap was already closed (a stale view, a platform setting) was
// otherwise flagged again every round until the build shipped broken.
func TestSpecGapResponseReachesReaudit(t *testing.T) {
	w, _ := buildE2EWorker(t, &enginetest.Provider{})
	gap := Issue{Category: specGapCategory, Signature: specGapCategory + "|files-table"}
	w.repairSignatures([]Issue{gap}, 1)
	id := repairTaskID(gap, 1)
	if _, err := w.pdb.ExecWrite(`INSERT INTO of_tasks (id, title, status, origin, result) VALUES (?, 'fix', ?, ?, ?)`,
		id, TaskDone, TaskOriginRepair, "disputed: the UNIQUE index uidx_files_slug already exists"); err != nil {
		t.Fatal(err)
	}
	responses := w.specGapResponses()
	if got := responses["files-table"]; !strings.Contains(got, "uidx_files_slug") {
		t.Fatalf("response for files-table = %q", got)
	}
	req := specAuditRequest("", "", "", []Task{{ID: "files-table", Title: "Files table"}}, nil, responses, "", "")
	if !strings.Contains(req, "Builder's response to the last audit: disputed: the UNIQUE index") {
		t.Errorf("re-audit request does not carry the builder's response:\n%s", req)
	}
}

// A re-check sees the gaps it flagged before, and a gap it calls new is a different
// finding to the repair loop. In two builds a repair fixed the named gap, the re-check
// found another one in the same item (once a bug the repair itself introduced), and the
// loop read the unchanged item key as a stall and shipped it.
func TestSpecGapNewGapEarnsAnotherRound(t *testing.T) {
	tasks := map[string]Task{"avatar-builder": {ID: "avatar-builder", Title: "Avatar builder"}}
	first := specGapIssues([]specVerdict{{ID: "avatar-builder", Verdict: "partial", Gaps: []string{"no setColour control"}}}, tasks, 1)

	req := specAuditRequest("", "", "", []Task{tasks["avatar-builder"]}, specGapsNamed(first), nil, "", "")
	if !strings.Contains(req, "Gaps the last audit named:\n    - no setColour control") {
		t.Errorf("the re-check must see the gaps it flagged before:\n%s", req)
	}

	reworded := specGapIssues([]specVerdict{{ID: "avatar-builder", Verdict: "partial", Gaps: []string{"setColour is still missing"}}}, tasks, 2)
	if repairProgress(first, reworded) {
		t.Error("the same gap reworded is not progress")
	}
	fresh := specGapIssues([]specVerdict{{ID: "avatar-builder", Verdict: "partial", NewGaps: true,
		Gaps: []string{"setColour's nametag redraw never runs"}}}, tasks, 2)
	if !repairProgress(first, fresh) {
		t.Error("a gap the re-check calls new earns another round")
	}
	if ids := specGapTaskIDs(fresh); len(ids) != 1 || ids[0] != "avatar-builder" {
		t.Errorf("a new gap still re-audits its item, got %v", ids)
	}
	// Called new again on the next re-check, it is again a different finding.
	again := specGapIssues([]specVerdict{{ID: "avatar-builder", Verdict: "partial", NewGaps: true, Gaps: []string{"x"}}}, tasks, 3)
	if !repairProgress(fresh, again) {
		t.Error("each new gap is its own finding")
	}
}

// auditProvider answers the spec audit's calls from a script of its own and every
// other call from the inner provider, so a test can steer the audit independently
// of how many turns the build and repair sessions take.
type auditProvider struct {
	*enginetest.Provider
	audits   []string
	auditLog []llm.CompletionRequest
}

func (p *auditProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if req.System == specAuditSystem {
		p.auditLog = append(p.auditLog, req)
		reply := `{"items":[]}`
		if i := len(p.auditLog) - 1; i < len(p.audits) {
			reply = p.audits[i]
		}
		return &llm.CompletionResponse{Content: reply, StopReason: "end_turn"}, nil
	}
	return p.Provider.Complete(ctx, req)
}

func (p *auditProvider) Stream(ctx context.Context, req llm.CompletionRequest, cb llm.StreamCallback) error {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return err
	}
	cb(llm.StreamChunk{Delta: resp.Content, Usage: &resp.Usage})
	return nil
}

// A gap the audit names reopens the build as a repair item carrying the gap; the
// re-audit after the round covers only that item, and once it reads met the project
// ships clean.
func TestSpecAuditGapDrivesRepairRound(t *testing.T) {
	inner := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
		saveStep,
		completeStep("c2"),
	}}
	w, mgr := buildE2EWorker(t, inner)
	p := &auditProvider{Provider: inner, audits: []string{
		`{"items":[{"id":"home-page","verdict":"partial","gaps":["the split-by-people input is missing from /"]}]}`,
		`{"items":[{"id":"home-page","verdict":"met"}]}`,
	}}
	w.providerOverride = staticProviderSource{rp: resolvedProvider{Provider: p, Model: "fake-model", ProviderID: 1}}
	validatorMu.Lock()
	validatorSet = nil
	validatorMu.Unlock()
	driveToCompletion(t, w)

	if len(p.auditLog) != 2 {
		t.Fatalf("audits = %d, want 2 (after the build, then the flagged item after the repair)", len(p.auditLog))
	}
	var sawRepair bool
	for _, req := range inner.Requests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "[mode: repair]") && strings.Contains(m.Content, "split-by-people input is missing") {
				sawRepair = true
			}
		}
	}
	if !sawRepair {
		t.Fatal("the repair round must carry the audit's gap")
	}
	pdb, _ := mgr.Open(1)
	var n int
	_ = pdb.Reader().QueryRow(`SELECT COUNT(*) FROM of_chat_messages WHERE content LIKE '%known issues%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("a gap the repair closed must not ship as a known issue, found %d", n)
	}
	_ = pdb.Reader().QueryRow(`SELECT COUNT(*) FROM of_chat_messages WHERE content LIKE '%{"items"%'`).Scan(&n)
	if n != 0 {
		t.Fatal("the audit's JSON is working context, never a chat message")
	}
}
