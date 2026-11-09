package engine

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/engine/snapshotbuild"

	_ "modernc.org/sqlite"
)

func TestParseTitleCandidates(t *testing.T) {
	got := parseTitleCandidates("1. \"Tide Tracker\"\n- Harbor Watch \n\n*Reef Notes*\n")
	want := []string{"Tide Tracker", "Harbor Watch", "Reef Notes"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate %d = %q, want %q", i, got[i], want[i])
		}
	}

	long := strings.Repeat("Word ", 30)
	if c := sanitizeTitle(long); len(c) > maxGeneratedTitleLen {
		t.Fatalf("title not capped: %d chars", len(c))
	}
	if sanitizeReaction("  \"What a fun idea — truly!\"\nsecond line") != "What a fun idea truly!" {
		t.Fatalf("reaction sanitize: %q", sanitizeReaction("  \"What a fun idea — truly!\"\nsecond line"))
	}
}

// A build that skipped work must say so. Zero pages is a legal shape (a headless
// project), so the overview's absent Pages line cannot carry the news; the
// checklist is the only thing that knows the work was owed.
func TestUnfinishedSection(t *testing.T) {
	if unfinishedSection(nil) != "" {
		t.Error("a complete build must render no not-built section")
	}
	got := unfinishedSection([]Task{
		{ID: "app-shell", Title: "App shell + layout"},
		{ID: "club-spine", Title: "Club scene spine"},
	})
	for _, want := range []string{"**Not built (2)**", "App shell + layout", "Club scene spine"} {
		if !strings.Contains(got, want) {
			t.Errorf("not-built section missing %q:\n%s", want, got)
		}
	}
	// Long lists cap like the overview rather than flooding the message.
	var many []Task
	for i := 0; i < maxOverviewItems+3; i++ {
		many = append(many, Task{Title: "task " + string(rune('a'+i))})
	}
	if !strings.Contains(unfinishedSection(many), "+3 more") {
		t.Errorf("not-built section not capped:\n%s", unfinishedSection(many))
	}
}

// The go-live report keys off task status, so pin which statuses count as unbuilt.
// Blocked counts: the owner is owed an answer about it, not silence.
func TestUnfinishedTasks(t *testing.T) {
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec(`CREATE TABLE of_tasks (id TEXT PRIMARY KEY, run_id TEXT, title TEXT,
		intent TEXT, done_when TEXT, status TEXT, deps TEXT, paths TEXT, edits TEXT, tools TEXT,
		exports TEXT, origin TEXT, result TEXT, conventions TEXT, position INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i, r := range []struct{ id, status string }{
		{"done-one", TaskDone},
		{"still-pending", TaskPending},
		{"in-flight", TaskActive},
		{"blocked-one", TaskBlocked},
	} {
		if _, err := d.Exec(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES (?, ?, '', ?, ?)`,
			r.id, r.id, r.status, i); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	got, err := unfinishedTasks(d)
	if err != nil {
		t.Fatalf("unfinishedTasks: %v", err)
	}
	var ids []string
	for _, tk := range got {
		ids = append(ids, tk.ID)
	}
	want := []string{"still-pending", "in-flight", "blocked-one"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("unfinished = %v, want %v", ids, want)
	}

	// An unreadable checklist must surface as an error, never as "nothing missing".
	if _, err := d.Exec(`DROP TABLE of_tasks`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := unfinishedTasks(d); err == nil {
		t.Error("a failed checklist read must report an error, not an empty list")
	}
}

func TestOverviewAndLoginSections(t *testing.T) {
	snap := &snapshotbuild.ProjectSnapshot{
		Pages: []snapshotbuild.PageRef{
			{Path: "/", Title: "Home"},
			{Path: "/about"},
		},
		Tables: []snapshotbuild.TableRef{{Name: "posts"}},
		Endpoints: []snapshotbuild.EndpointRef{
			{Path: "posts", AuthKind: "api"},
			{Path: "users", AuthKind: "auth"},
		},
	}
	ov := overviewSection(snap, "http://x")
	for _, want := range []string{"**Pages (2):** [Home](http://x/) `/`, [/about](http://x/about) `/about`", "**Data tables (1):** posts", "**APIs (1):** /api/posts", "plus a sign-in API"} {
		if !strings.Contains(ov, want) {
			t.Errorf("overview missing %q:\n%s", want, ov)
		}
	}
	if overviewSection(&snapshotbuild.ProjectSnapshot{}, "http://x") != "" {
		t.Error("empty snapshot must render no overview")
	}

	// A stream over the same table as a CRUD endpoint is its own URL, not a second
	// /api/orders, and a parameterised page gets no link.
	shared := &snapshotbuild.ProjectSnapshot{
		Pages: []snapshotbuild.PageRef{{Path: "/staff", Title: "Staff"}, {Path: "/posts/:slug", Title: "Post"}},
		Endpoints: []snapshotbuild.EndpointRef{
			{Path: "orders", AuthKind: "api"},
			{Path: "orders", AuthKind: "stream"},
			{Path: "orders", AuthKind: "api"},
		},
	}
	_, _, apis, _ := overviewData(shared)
	if strings.Join(apis, ",") != "/api/orders,/api/orders/stream" {
		t.Errorf("apis = %v, want the CRUD path and the stream path once each", apis)
	}
	links := pageLinks(shared, "http://x/")
	if len(links) != 2 || links[0].URL != "http://x/staff" || links[1].URL != "" {
		t.Errorf("page links = %+v", links)
	}
	if s := notesSection([]string{"a", "b"}); !strings.Contains(s, "**Good to know**") || !strings.Contains(s, "- a\n- b") {
		t.Errorf("notes section = %q", s)
	}
	if notesSection(nil) != "" {
		t.Error("no notes must render nothing")
	}

	// +N more capping.
	many := &snapshotbuild.ProjectSnapshot{}
	for i := 0; i < maxOverviewItems+3; i++ {
		many.Tables = append(many.Tables, snapshotbuild.TableRef{Name: "t" + string(rune('a'+i))})
	}
	if !strings.Contains(overviewSection(many, ""), "+3 more") {
		t.Errorf("overview not capped:\n%s", overviewSection(many, ""))
	}

	if !snapshotHasAuth(snap) || snapshotHasAuth(&snapshotbuild.ProjectSnapshot{}) {
		t.Error("snapshotHasAuth misclassifies")
	}
	if got := loginPageURL(&snapshotbuild.ProjectSnapshot{Pages: []snapshotbuild.PageRef{{Path: "/login"}}}, "http://localhost:5733"); got != "http://localhost:5733/login" {
		t.Errorf("loginPageURL = %q", got)
	}

	// Login sections.
	demo := &demoLogin{Email: "demo@example.com", Password: "p", Role: "user", Label: "Email"}
	if s := loginSection(demo, true, "http://x/login"); !strings.Contains(s, "`demo@example.com`") || !strings.Contains(s, "demo account (role: user) I created") {
		t.Errorf("synthesized demo login section wrong:\n%s", s)
	}
	if s := loginSection(demo, true, "http://x/login"); !strings.Contains(s, "**Email:** `demo@example.com`") {
		t.Errorf("email-auth section should title the identifier 'Email':\n%s", s)
	}
	// Username-based auth must title the identifier "Username", not "Email".
	userDemo := &demoLogin{Email: "demo", Password: "p", Role: "user", Label: "Username"}
	if s := loginSection(userDemo, true, "http://x/login"); !strings.Contains(s, "**Username:** `demo`") || strings.Contains(s, "**Email:**") {
		t.Errorf("username-auth section should title the identifier 'Username':\n%s", s)
	}
	// An adopted (existing) account reads differently and names the account.
	adopted := &demoLogin{Email: "admin@acme.co", Password: "p", Role: "admin", Adopted: true}
	if s := loginSection(adopted, true, "http://x/login"); !strings.Contains(s, "seeded **admin@acme.co** account") {
		t.Errorf("adopted login section wrong:\n%s", s)
	}
	// Fallback when no demo could be minted: never promise a non-existent signup.
	s := loginSection(nil, true, "http://x/login")
	if strings.Contains(s, "create an account") {
		t.Errorf("fallback must not tell the owner to create an account:\n%s", s)
	}
	if !strings.Contains(s, "Data Tables") {
		t.Errorf("fallback should point at the seeded users in Data Tables:\n%s", s)
	}
	if loginSection(nil, false, "http://x") != "" {
		t.Error("no auth must render no login section")
	}
}

func TestGoLivePayloadAndMarker(t *testing.T) {
	snap := &snapshotbuild.ProjectSnapshot{
		Pages:  []snapshotbuild.PageRef{{Path: "/", Title: "Home"}, {Path: "/login"}},
		Tables: []snapshotbuild.TableRef{{Name: "posts"}},
		Endpoints: []snapshotbuild.EndpointRef{
			{Path: "posts", AuthKind: "api"},
			{Path: "users", AuthKind: "auth"},
		},
	}
	demo := &demoLogin{Email: "demo", Password: "pw", Role: "admin", Label: "Username"}

	p := buildGoLivePayload(snap, demo, "http://localhost:5733", "/projects/7/settings-sharing", false, nil, nil)
	if p.Kind != "golive" {
		t.Fatalf("kind = %q, want golive", p.Kind)
	}
	if len(p.Pages) != 2 || len(p.Tables) != 1 || len(p.APIs) != 1 || !p.HasAuthAPI {
		t.Fatalf("payload counts wrong: %+v", p)
	}
	if p.Login == nil || !p.Login.Minted || p.Login.Label != "Username" || p.Login.Identifier != "demo" {
		t.Fatalf("payload login wrong: %+v", p.Login)
	}
	// loginURL should resolve to the project's /login page, not the bare preview.
	if !strings.HasSuffix(p.Login.LoginURL, "/login") {
		t.Errorf("login URL = %q, want a /login page", p.Login.LoginURL)
	}

	// The marker must wrap base64-encoded JSON that round-trips back to the payload.
	marker := goLiveMarker(p)
	if !strings.HasPrefix(marker, goLiveMarkerOpen) || !strings.HasSuffix(marker, goLiveMarkerClose) {
		t.Fatalf("marker not bracketed: %q", marker)
	}
	b64 := strings.TrimSuffix(strings.TrimPrefix(marker, goLiveMarkerOpen), goLiveMarkerClose)
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("marker payload not base64: %v", err)
	}
	var got goLivePayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("marker payload not JSON: %v", err)
	}
	if got.Kind != "golive" || got.Login == nil || got.Login.Label != "Username" {
		t.Fatalf("decoded payload wrong: %+v", got)
	}

	// Auth exists but no demo could be minted: card points at seeded users only.
	noDemo := buildGoLivePayload(snap, nil, "http://localhost:5733", "/projects/7/settings-sharing", true, nil, nil)
	if noDemo.Login == nil || noDemo.Login.Minted || noDemo.Login.Password != "" {
		t.Fatalf("no-demo payload should carry an unminted login: %+v", noDemo.Login)
	}
	if !noDemo.KnownIssues {
		t.Error("knownIssues flag not carried into payload")
	}
}
