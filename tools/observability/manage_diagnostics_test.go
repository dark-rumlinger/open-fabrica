package observability

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/store"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupFilesDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func setupFilesDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

// test_page's render verdict fails ONLY on deterministic, reproducible breakage:
// a thrown error or a failed same-origin asset. A page that paints nothing is NOT
// auto-failed (a timing-coupled blank verdict is a fix-loop trap); visible content
// is measured and surfaced as info, never a hard fail.
func TestPageRenderFailureReason(t *testing.T) {
	// No visible content and no media: still NOT a hard failure - blank is
	// informational now, not a verdict (it false-positives correct cold-load pages).
	if _, failed := (&pageRenderReport{visibleChars: 0, mediaCount: 0}).failureReason(); failed {
		t.Error("an empty paint must NOT auto-fail render verification - it is informational only")
	}
	// Real text content passes.
	if _, failed := (&pageRenderReport{visibleChars: 400}).failureReason(); failed {
		t.Error("a page with substantial visible text must pass")
	}
	// A purely visual page (image gallery / canvas game) passes too.
	if _, failed := (&pageRenderReport{visibleChars: 0, mediaCount: 3}).failureReason(); failed {
		t.Error("a page whose content is visible media must pass")
	}
	// A thrown error is reported first, ahead of the no-content catch-all.
	reason, failed := (&pageRenderReport{exceptions: []string{"ReferenceError: x"}}).failureReason()
	if !failed || !strings.Contains(reason, "uncaught error") {
		t.Errorf("a thrown error must fail with that reason, got (%q,%v)", reason, failed)
	}
	// A failed sub-resource is reported when there is no thrown error.
	reason, failed = (&pageRenderReport{visibleChars: 400, failedAssets: []string{"404 /files/app.js"}}).failureReason()
	if !failed || !strings.Contains(reason, "failed to load") {
		t.Errorf("a failed script/stylesheet must fail with that reason, got (%q,%v)", reason, failed)
	}
	// Console errors are SURFACED for the model to judge, never auto-failed: a page
	// that rendered real content but logged a console.error still passes the render
	// verdict (the build agent decides whether the error means broken behaviour).
	if _, failed := (&pageRenderReport{visibleChars: 400, consoleErrors: []string{"THREE.Object3D.add: object not an instance of THREE.Object3D."}}).failureReason(); failed {
		t.Error("console errors must not flip the render verdict - the model judges them")
	}
}

// blankRenderWarning is advisory (never a hard fail), but a 200-but-blank page must
// not pass silently. When the DOM holds text that never painted, the warning names
// the hidden cause (the fingerprint of a reveal animation that never ran); when the
// page is genuinely empty it gives the generic investigate-this message. A page that
// painted any visible text or media gets no warning.
func TestBlankRenderWarning(t *testing.T) {
	// Stuck reveal: text present in the DOM but hidden by opacity -> specific warning.
	w := (&pageRenderReport{visibleChars: 0, mediaCount: 0, hiddenChars: 120, hiddenReason: "opacity"}).blankRenderWarning()
	if w == "" {
		t.Fatal("a page with hidden-but-present text must produce a warning")
	}
	if !strings.Contains(w, "hidden") || !strings.Contains(w, "opacity") {
		t.Errorf("hidden-text warning must name the hidden cause, got %q", w)
	}
	if !strings.Contains(w, "120") {
		t.Errorf("hidden-text warning should report the hidden char count, got %q", w)
	}

	// Genuinely empty paint: generic, non-empty warning.
	if w := (&pageRenderReport{visibleChars: 0, mediaCount: 0}).blankRenderWarning(); w == "" {
		t.Error("a fully blank page must produce a (generic) warning")
	}

	// Real visible text: no warning.
	if w := (&pageRenderReport{visibleChars: 400}).blankRenderWarning(); w != "" {
		t.Errorf("a page with visible text must not warn, got %q", w)
	}

	// Purely visual page (image gallery / canvas): no warning.
	if w := (&pageRenderReport{visibleChars: 0, mediaCount: 3}).blankRenderWarning(); w != "" {
		t.Errorf("a page whose content is visible media must not warn, got %q", w)
	}
}

// expectedStatusOK is the status-comparison the test_endpoint/test_url/test_page
// probes share. An UNSET expectation (0) must accept any 2xx so a create that
// correctly returns 201 (or a 204) is not false-failed against an implicit 200 -
// the exact mismatch that read a healthy create endpoint as broken at the end of
// a build. A pinned expectation stays an exact match so a caller can still assert
// a specific code.
func TestExpectedStatusOK(t *testing.T) {
	cases := []struct {
		status, expected int
		want             bool
	}{
		{200, 0, true},  // unset: any 2xx passes
		{201, 0, true},  // create: 201 must pass when unset (the regression)
		{204, 0, true},  // no-content: 2xx passes
		{301, 0, false}, // 3xx is not a success
		{404, 0, false}, // client error fails
		{500, 0, false}, // server error fails
		{200, 200, true},
		{201, 200, false}, // pinned 200 still rejects a 201
		{201, 201, true},
		{200, 201, false}, // pinned 201 still rejects a 200
	}
	for _, c := range cases {
		if got := expectedStatusOK(c.status, c.expected); got != c.want {
			t.Errorf("expectedStatusOK(status=%d, expected=%d) = %v, want %v", c.status, c.expected, got, c.want)
		}
	}

	// The result payload and error phrasing reflect whether the code was pinned.
	if got := expectedStatusLabel(0); got != "2xx" {
		t.Errorf("unset expectation should label as 2xx, got %v", got)
	}
	if got := expectedStatusLabel(201); got != 201 {
		t.Errorf("pinned expectation should label as its code, got %v", got)
	}
	if msg := expectedStatusError(0, 500); !strings.Contains(msg, "2xx") {
		t.Errorf("unset-expectation error should mention 2xx, got %q", msg)
	}
	if msg := expectedStatusError(200, 201); !strings.Contains(msg, "200") {
		t.Errorf("pinned-expectation error should name the pinned code, got %q", msg)
	}
}

// test_url on a /files/ path is answered from the saved-file table, not an HTTP
// probe: an existing file reports served (a saved file always serves at its
// path), so the model never needs to probe static assets.
func TestTestURL_StaticFileServedFromDB(t *testing.T) {
	db := setupFilesDB(t)
	if _, err := db.Exec(`INSERT INTO of_files (kind, path) VALUES ('file','styles.css')`); err != nil {
		t.Fatal(err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, err := (&DiagTestURL{URLPath: "/files/styles.css"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Success {
		t.Fatalf("a saved file must report served, got: %+v", res)
	}
	if res.Data.(map[string]any)["served"] != "static-file" {
		t.Errorf("expected served=static-file, got %+v", res.Data)
	}
}

// A cross-origin absolute URL passed as url_path is the CDN-hotlink flail: the
// probe must not prepend "/" and 404 on the app's own server ("/https://cdn..."),
// but steer the model to declare the library via manage_libraries add_library.
func TestTestURL_ExternalURLSteersToAddLibrary(t *testing.T) {
	ctx := &tools.ToolContext{}
	res, err := (&DiagTestURL{URLPath: "https://cdn.jsdelivr.net/npm/qrcode@1.5.3/build/qrcode.min.js"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatal("probing an external CDN URL must not report success")
	}
	if !strings.Contains(res.Error, "add_library") {
		t.Errorf("error should steer to manage_libraries add_library: %q", res.Error)
	}
	if strings.Contains(res.Error, "/https://") {
		t.Errorf("must not have prepended a slash and probed the app server: %q", res.Error)
	}
}

// A /files/ miss names the real cause (no file at that path - a typo) and lists
// same-extension saved files, instead of a bare 404 from an HTTP probe.
func TestTestURL_StaticFileMissNamesTypo(t *testing.T) {
	db := setupFilesDB(t)
	if _, err := db.Exec(`INSERT INTO of_files (kind, path) VALUES ('file','styles.css')`); err != nil {
		t.Fatal(err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, _ := (&DiagTestURL{URLPath: "/files/style.css"}).Run(ctx) // note: missing the 's'
	if res.Success {
		t.Fatal("a path with no saved file must fail")
	}
	data := res.Data.(map[string]any)
	if data["status"] != 404 {
		t.Errorf("expected status 404, got %v", data["status"])
	}
	near, _ := data["saved_files_with_same_extension"].([]string)
	found := false
	for _, p := range near {
		if p == "styles.css" {
			found = true
		}
	}
	if !found {
		t.Errorf("miss should surface the likely-intended file styles.css, got %v", near)
	}
}

// TestIsEmailLikeColumn pins the probe's login-value shape decision: an email
// column gets an email-shaped synthetic value, a plain handle column does not
// (so a username column with a no-'@' rule is not falsely 400'd by the probe).
func TestIsEmailLikeColumn(t *testing.T) {
	email := []string{"email", "Email", "user_email", "email_address", "e_mail", "contact_mail"}
	handle := []string{"username", "user_name", "handle", "login", "nick", "phone", "id"}
	for _, c := range email {
		if !isEmailLikeColumn(c) {
			t.Errorf("expected %q to be treated as email-like", c)
		}
	}
	for _, c := range handle {
		if isEmailLikeColumn(c) {
			t.Errorf("expected %q to be treated as a plain handle, not email-like", c)
		}
	}
}

// TestBuildProbeUser pins that the auth-flow probe's synthetic body fills EVERY
// required column (not just the login key + password) so a users table with a
// second NOT NULL column - the common username+email shape - is not falsely 400'd
// on a NOT NULL constraint. It also pins that the password rides under the literal
// "password" API field (not the column name) and that the server-managed role/id
// columns are never sent.
func TestBuildProbeUser(t *testing.T) {
	const suffix = int64(4242)
	cols := []store.ColumnInfo{
		{Name: "id", Type: "INTEGER"},
		{Name: "username", Type: "TEXT", NotNull: true},
		{Name: "email", Type: "TEXT", NotNull: true},
		{Name: "org_id", Type: "INTEGER", NotNull: true},
		{Name: "password", Type: "TEXT"},
		{Name: "role", Type: "TEXT", NotNull: true},
	}
	// Auth logs in by email; username, email, org_id, role are all NOT NULL.
	required := map[string]bool{"username": true, "email": true, "org_id": true, "role": true}
	body := buildProbeUser("email", "password", "role", required, cols, suffix)

	// Login key is the email column, email-shaped.
	if v, _ := body["email"].(string); !strings.Contains(v, "@") {
		t.Errorf("email login value should be email-shaped, got %v", body["email"])
	}
	// The other required TEXT column is filled with a plain (non-email) handle.
	if v, _ := body["username"].(string); v == "" || strings.Contains(v, "@") {
		t.Errorf("username should be a filled non-email handle, got %v", body["username"])
	}
	// A NOT NULL INTEGER required column gets the numeric suffix, not a string.
	if v, ok := body["org_id"].(int64); !ok || v != suffix {
		t.Errorf("org_id should be the numeric suffix %d, got %v (%T)", suffix, body["org_id"], body["org_id"])
	}
	// Password is keyed under the literal API field, not the column name.
	if body["password"] != "TestPass123!" {
		t.Errorf("password should be under the literal \"password\" key, got %v", body["password"])
	}
	// The server force-sets role and auto-fills id; the probe must send neither,
	// even though role is required here.
	if _, ok := body["role"]; ok {
		t.Error("probe body must not carry the role column (server force-sets it)")
	}
	if _, ok := body["id"]; ok {
		t.Error("probe body must not carry the id column")
	}

	// A renamed password column is still keyed under "password" and never filled
	// directly (the server writes the hash into it).
	body2 := buildProbeUser("username", "pass_hash", "role",
		map[string]bool{"username": true, "pass_hash": true},
		[]store.ColumnInfo{
			{Name: "username", Type: "TEXT", NotNull: true},
			{Name: "pass_hash", Type: "TEXT", NotNull: true},
		}, suffix)
	if _, ok := body2["pass_hash"]; ok {
		t.Error("password column must not be filled by the probe (server sets the hash)")
	}
	if body2["password"] != "TestPass123!" {
		t.Errorf("password should be under literal \"password\" key, got %v", body2["password"])
	}
	if v, _ := body2["username"].(string); strings.Contains(v, "@") {
		t.Errorf("username login value should be a plain handle, got %v", body2["username"])
	}
}
