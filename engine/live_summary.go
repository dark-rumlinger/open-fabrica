package engine

// Owner-facing go-live extras: a "what's inside" overview and a sign-in block
// (with an auto-provisioned demo account when the project has auth), derived
// from the project snapshot. Deterministic - no LLM.

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/engine/snapshotbuild"
	"openfabrica/security"
)

// maxOverviewItems caps names per category before "+N more".
const maxOverviewItems = 8

// goLiveMarkerOpen/Close bracket a base64-encoded goLivePayload appended to the
// go-live chat message; the chat UI renders it as a structured card and falls
// back to the plain markdown when the payload is absent or unparseable. Kept in
// an HTML comment so non-card renderers treat it as inert. This string is a
// byte-for-byte contract with the frontend - see message.js parseGoLiveMarker.
const (
	goLiveMarkerOpen  = "<!--of-golive:"
	goLiveMarkerClose = "-->"
)

// goLivePayload is the structured go-live summary the chat card renders from.
// It mirrors the human markdown of the same message: the overview counts and
// names, the preview/publish links, and the sign-in block.
type goLivePayload struct {
	Kind        string   `json:"kind"` // always "golive"
	KnownIssues bool     `json:"known_issues"`
	PreviewURL  string   `json:"preview_url"`
	SettingsURL string   `json:"settings_url"`
	Pages       []string `json:"pages,omitempty"`
	// PageLinks names every page with its route and a preview link, so the owner
	// learns that /board or /staff exist without guessing from a title.
	PageLinks  []goLivePage `json:"page_links,omitempty"`
	Tables     []string     `json:"tables,omitempty"`
	APIs       []string     `json:"apis,omitempty"`
	HasAuthAPI bool         `json:"has_auth_api"`
	Login      *goLiveLogin `json:"login,omitempty"`
	// FlowUnverified names the user flows that did not pass at ship time. FlowCheck
	// carries the same result with the flow count and the skipped state; older cards
	// read only the names.
	FlowUnverified []string   `json:"flow_unverified,omitempty"`
	FlowCheck      *flowCheck `json:"flow_check,omitempty"`
	// IssueReasons say why the build shipped with known issues, one short phrase each.
	IssueReasons []string `json:"issue_reasons,omitempty"`
	// Notes are things the owner must know to use the project that only the build
	// knows, such as a secret it stored with a value it chose.
	Notes []string `json:"notes,omitempty"`
}

// goLivePage is one page of the go-live card: its title, route, and preview link.
// URL is empty for a parameterised route (/posts/:slug), which has no single page.
type goLivePage struct {
	Title string `json:"title"`
	Path  string `json:"path"`
	URL   string `json:"url,omitempty"`
}

// pluralS returns "s" unless n is 1.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// goLiveLogin is the sign-in block of the card. When Minted is false the project
// has auth but no demo account could be provisioned, so only LoginURL is set and
// the card points the owner at the seeded users instead of showing credentials.
type goLiveLogin struct {
	LoginURL   string `json:"login_url"`
	Label      string `json:"label,omitempty"` // "Email" or "Username"
	Identifier string `json:"identifier,omitempty"`
	Password   string `json:"password,omitempty"`
	Role       string `json:"role,omitempty"`
	Adopted    bool   `json:"adopted,omitempty"`
	Minted     bool   `json:"minted"`
}

// demoLogin holds the credentials surfaced for a sign-in-ready account. The
// plaintext password only ever appears in the owner-facing chat message; the
// row stores the bcrypt hash. Adopted is true when we reset the password on an
// existing seeded account (vs. creating a fresh demo@example.com row), which
// changes the wording so the owner knows which account it is.
type demoLogin struct {
	Email    string
	Password string
	Role     string
	Adopted  bool
	// Label titles the identifier in the sign-in block: "Email" when the auth
	// table logs in by an email column, "Username" otherwise. Without this the
	// block always said "Email" even for username-based auth.
	Label string
}

// goLiveSections builds the markdown overview and sign-in sections appended to
// the completion message, from an already-built snapshot and (optionally) an
// already-provisioned demo login. Provisioning happens once in completeBuild so
// this - and the structured card payload - share the same account rather than
// minting two.
func goLiveSections(snap *snapshotbuild.ProjectSnapshot, demo *demoLogin, previewURL string) string {
	var b strings.Builder
	if ov := overviewSection(snap, previewURL); ov != "" {
		b.WriteString("\n" + ov)
	}
	if ls := loginSection(demo, snapshotHasAuth(snap), loginPageURL(snap, previewURL)); ls != "" {
		b.WriteString("\n" + ls)
	}
	return b.String()
}

// goLiveMarker encodes the structured card payload as the trailing HTML-comment
// marker appended to the go-live message. Returns "" on marshal failure, which
// simply leaves the message as plain markdown - the card is an enhancement, not
// a requirement.
func goLiveMarker(p *goLivePayload) string {
	if p == nil {
		return ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return goLiveMarkerOpen + base64.StdEncoding.EncodeToString(raw) + goLiveMarkerClose
}

// buildGoLivePayload assembles the structured go-live card data from the same
// snapshot and demo login that produce the markdown, so the card and the prose
// never disagree.
func buildGoLivePayload(snap *snapshotbuild.ProjectSnapshot, demo *demoLogin, previewURL, settingsURL string, knownIssues bool, flowUnverified []string, flows *flowCheck) *goLivePayload {
	pages, tables, apis, hasAuthAPI := overviewData(snap)
	p := &goLivePayload{
		Kind:        "golive",
		KnownIssues: knownIssues,
		PreviewURL:  previewURL,
		SettingsURL: settingsURL,
		Pages:       pages,
		PageLinks:   pageLinks(snap, previewURL),
		Tables:      tables,
		APIs:        apis,
		HasAuthAPI:  hasAuthAPI,

		FlowUnverified: flowUnverified,
		FlowCheck:      flows,
	}
	switch {
	case demo != nil:
		label := demo.Label
		if label == "" {
			label = "Email"
		}
		p.Login = &goLiveLogin{
			LoginURL:   loginPageURL(snap, previewURL),
			Label:      label,
			Identifier: demo.Email,
			Password:   demo.Password,
			Role:       demo.Role,
			Adopted:    demo.Adopted,
			Minted:     true,
		}
	case snapshotHasAuth(snap):
		p.Login = &goLiveLogin{LoginURL: loginPageURL(snap, previewURL), Minted: false}
	}
	return p
}

// overviewData extracts the page names, table names, API paths, and whether an
// auth endpoint exists from a snapshot. Shared by the markdown overview and the
// structured card payload so both count and name the same things. A page reads
// "Title (/route)" so the owner sees where every page lives.
func overviewData(snap *snapshotbuild.ProjectSnapshot) (pageNames, tableNames, apiPaths []string, hasAuthAPI bool) {
	if snap == nil {
		return nil, nil, nil, false
	}
	pageNames = make([]string, 0, len(snap.Pages))
	for _, p := range snap.Pages {
		name := p.Path
		if strings.TrimSpace(p.Title) != "" {
			name = fmt.Sprintf("%s (%s)", p.Title, p.Path)
		}
		pageNames = append(pageNames, name)
	}
	tableNames = make([]string, 0, len(snap.Tables))
	for _, t := range snap.Tables {
		tableNames = append(tableNames, t.Name)
	}
	apiPaths = make([]string, 0, len(snap.Endpoints))
	seen := make(map[string]bool, len(snap.Endpoints))
	for _, e := range snap.Endpoints {
		if e.AuthKind == "auth" {
			hasAuthAPI = true
			continue
		}
		path := endpointURLPath(e)
		if seen[path] {
			continue
		}
		seen[path] = true
		apiPaths = append(apiPaths, path)
	}
	return pageNames, tableNames, apiPaths, hasAuthAPI
}

// endpointURLPath is the URL an endpoint is reached at. Stream, upload, LLM and
// websocket endpoints share their base path with the CRUD endpoint over the same
// table, so without the suffix the overview listed /api/orders twice.
func endpointURLPath(e snapshotbuild.EndpointRef) string {
	base := "/api/" + strings.Trim(e.Path, "/")
	suffix := map[string]string{"stream": "/stream", "upload": "/upload", "llm": "/chat", "ws": "/ws"}[e.AuthKind]
	if suffix != "" && !strings.HasSuffix(base, suffix) {
		base += suffix
	}
	return base
}

// pageLinks lists every page with its route and preview link, in snapshot order.
func pageLinks(snap *snapshotbuild.ProjectSnapshot, previewURL string) []goLivePage {
	if snap == nil {
		return nil
	}
	out := make([]goLivePage, 0, len(snap.Pages))
	for _, p := range snap.Pages {
		title := strings.TrimSpace(p.Title)
		if title == "" {
			title = p.Path
		}
		page := goLivePage{Title: title, Path: p.Path}
		if !strings.Contains(p.Path, ":") && previewURL != "" {
			page.URL = strings.TrimRight(previewURL, "/") + "/" + strings.TrimPrefix(p.Path, "/")
		}
		out = append(out, page)
	}
	return out
}

// overviewSection renders a short "what's inside" summary: pages (linked to the
// preview when a URL is known), data tables, APIs. Returns "" when there is
// nothing worth listing.
func overviewSection(snap *snapshotbuild.ProjectSnapshot, previewURL string) string {
	if snap == nil {
		return ""
	}

	_, tableNames, apiPaths, hasAuthAPI := overviewData(snap)

	var b strings.Builder
	if links := pageLinks(snap, previewURL); len(links) > 0 {
		names := make([]string, 0, len(links))
		for _, l := range links {
			if l.URL != "" {
				names = append(names, fmt.Sprintf("[%s](%s) `%s`", l.Title, l.URL, l.Path))
			} else {
				names = append(names, fmt.Sprintf("%s `%s`", l.Title, l.Path))
			}
		}
		fmt.Fprintf(&b, "- **Pages (%d):** %s\n", len(names), summarizeNames(names))
	}
	if len(tableNames) > 0 {
		fmt.Fprintf(&b, "- **Data tables (%d):** %s\n", len(tableNames), summarizeNames(tableNames))
	}
	if len(apiPaths) > 0 {
		line := fmt.Sprintf("- **APIs (%d):** %s", len(apiPaths), summarizeNames(apiPaths))
		if hasAuthAPI {
			line += " (plus a sign-in API)"
		}
		b.WriteString(line + "\n")
	} else if hasAuthAPI {
		b.WriteString("- **APIs:** a sign-in API\n")
	}

	if b.Len() == 0 {
		return ""
	}
	return "**What's inside**\n\n" + b.String()
}

// notesSection renders the "Good to know" list: what the owner needs to use the
// project that only the build knows. Renders only when there is something to say.
func notesSection(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return "\n**Good to know**\n\n- " + strings.Join(notes, "\n- ") + "\n"
}

// unfinishedSection names the checklist items that never got built. It renders
// only when something is missing, so a complete build is unchanged. The titles
// are the planner's own words for the work, which is what the owner recognises -
// and naming them is the difference between an owner who knows what is missing
// and one who has to discover it by using the project.
func unfinishedSection(unfinished []Task) string {
	if len(unfinished) == 0 {
		return ""
	}
	titles := make([]string, 0, len(unfinished))
	for _, t := range unfinished {
		titles = append(titles, t.Title)
	}
	return fmt.Sprintf("\n**Not built (%d)**\n\n- %s\n", len(titles), summarizeNames(titles))
}

// summarizeNames joins names with commas, capped with a "+N more" tail.
func summarizeNames(names []string) string {
	if len(names) <= maxOverviewItems {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(names[:maxOverviewItems], ", "), len(names)-maxOverviewItems)
}

// snapshotHasAuth reports whether the project exposes an auth endpoint.
func snapshotHasAuth(snap *snapshotbuild.ProjectSnapshot) bool {
	if snap == nil {
		return false
	}
	for _, e := range snap.Endpoints {
		if e.AuthKind == "auth" {
			return true
		}
	}
	return false
}

// loginPageURL joins the project's login page onto the preview base, falling
// back to the base when none is declared.
func loginPageURL(snap *snapshotbuild.ProjectSnapshot, previewURL string) string {
	if snap != nil {
		for _, p := range snap.Pages {
			if strings.Contains(strings.ToLower(p.Path), "login") {
				return strings.TrimRight(previewURL, "/") + "/" + strings.TrimPrefix(p.Path, "/")
			}
		}
	}
	return previewURL
}

// privilegedRoleNames are the role values an admin-style app most commonly gates
// its main screens on. provisionDemoLogin prefers an account holding one so the
// surfaced credential can actually USE the app, not just authenticate and then
// get bounced by a page-level requireRole('admin').
var privilegedRoleNames = []string{"admin", "administrator", "owner", "superadmin", "super_admin", "staff", "root", "manager"}

// provisionDemoLogin hands the owner a credential that signs in to their
// just-built project. Password columns are write-only bcrypt, so resetting a
// seeded password loses nothing: adopt an existing account (prefer a privileged
// role) and reset its password, or when the table is empty synthesize a
// demo@example.com row, filling every no-default NOT NULL column. Returns
// ok=false when there is no auth endpoint, tenancy is enabled, or nothing could
// be made to work.
func (w *Worker) provisionDemoLogin() (*demoLogin, bool) {
	reader := w.pdb.Reader()

	var tableName, userCol, passCol, roleCol, defaultRole, tenantMode string
	err := reader.QueryRow(
		`SELECT table_name, username_column, password_column,
		        COALESCE(NULLIF(role_column,''),'role'),
		        COALESCE(NULLIF(default_role,''),'user'),
		        COALESCE(tenant_signup_mode,'none')
		 FROM of_auth_endpoints ORDER BY id LIMIT 1`,
	).Scan(&tableName, &userCol, &passCol, &roleCol, &defaultRole, &tenantMode)
	if err != nil {
		return nil, false // no auth endpoint
	}
	if tenantMode != "none" && tenantMode != "" {
		return nil, false
	}

	// Defensive validation before interpolating identifiers into SQL.
	if security.ValidateTableName(tableName) != nil ||
		security.ValidateColumnName(userCol) != nil ||
		security.ValidateColumnName(passCol) != nil ||
		security.ValidateColumnName(roleCol) != nil {
		return nil, false
	}

	password, err := generateDemoPassword()
	if err != nil {
		return nil, false
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, false
	}

	qTable := security.QuoteIdent(tableName)
	writer := w.pdb.Writer()

	// How to title the login identifier: an email column reads "Email", anything
	// else (username, handle, phone) reads "Username". Same signal drives the
	// synthesized-account value below.
	identLabel := "Email"
	if !strings.Contains(strings.ToLower(userCol), "email") {
		identLabel = "Username"
	}

	// 1) Adopt an existing account when the table already has users. Resetting a
	//    privileged account's password gives the owner a credential that signs in
	//    AND clears role-gated pages.
	if id, email, role, ok := pickExistingAccount(reader, tableName, userCol, roleCol); ok {
		if _, err := writer.Exec(
			fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", qTable, security.QuoteIdent(passCol)),
			hash, id,
		); err != nil {
			w.logger.Warn("demo login password reset on existing account failed", "error", err)
			return nil, false
		}
		if strings.TrimSpace(role) == "" {
			role = defaultRole
		}
		return &demoLogin{Email: email, Password: password, Role: role, Adopted: true, Label: identLabel}, true
	}

	// 2) Empty table: synthesize a demo account, satisfying NOT NULL columns.
	demoEmail := "demo@example.com"
	if identLabel != "Email" {
		demoEmail = "demo"
	}
	cols := []string{security.QuoteIdent(userCol), security.QuoteIdent(passCol), security.QuoteIdent(roleCol)}
	vals := []any{demoEmail, hash, defaultRole}
	skip := map[string]bool{
		strings.ToLower(userCol): true,
		strings.ToLower(passCol): true,
		strings.ToLower(roleCol): true,
	}
	for _, f := range requiredFillColumns(reader, tableName, skip) {
		cols = append(cols, security.QuoteIdent(f.name))
		vals = append(vals, f.val)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(vals)), ", ")
	if _, err := writer.Exec(
		fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", qTable, strings.Join(cols, ", "), placeholders),
		vals...,
	); err != nil {
		w.logger.Warn("demo login provisioning failed", "error", err)
		return nil, false
	}
	return &demoLogin{Email: demoEmail, Password: password, Role: defaultRole, Label: identLabel}, true
}

// pickExistingAccount returns the id, username, and role of the account best
// suited to be the demo login: a privileged-role account if one exists, else the
// first account by id. ok=false when the table has no rows (or cannot be read),
// in which case the caller synthesizes one. Role-aware queries fall back
// gracefully when the table has no role column.
func pickExistingAccount(reader *sql.DB, table, userCol, roleCol string) (id int, email, role string, ok bool) {
	q := security.QuoteIdent
	if security.ValidateColumnName(roleCol) == nil {
		ph := make([]string, len(privilegedRoleNames))
		args := make([]any, len(privilegedRoleNames))
		for i, r := range privilegedRoleNames {
			ph[i] = "?"
			args[i] = r
		}
		privQ := fmt.Sprintf("SELECT id, %s, COALESCE(%s,'') FROM %s WHERE LOWER(%s) IN (%s) ORDER BY id LIMIT 1",
			q(userCol), q(roleCol), q(table), q(roleCol), strings.Join(ph, ","))
		if reader.QueryRow(privQ, args...).Scan(&id, &email, &role) == nil {
			return id, email, role, true
		}
		anyQ := fmt.Sprintf("SELECT id, %s, COALESCE(%s,'') FROM %s ORDER BY id LIMIT 1", q(userCol), q(roleCol), q(table))
		if reader.QueryRow(anyQ).Scan(&id, &email, &role) == nil {
			return id, email, role, true
		}
	}
	noRoleQ := fmt.Sprintf("SELECT id, %s FROM %s ORDER BY id LIMIT 1", q(userCol), q(table))
	if reader.QueryRow(noRoleQ).Scan(&id, &email) == nil {
		return id, email, "", true
	}
	return 0, "", "", false
}

// fillColumn is a NOT NULL column (no default) that an INSERT must supply, with
// a type-appropriate placeholder value.
type fillColumn struct {
	name string
	val  any
}

// requiredFillColumns introspects table and returns every NOT NULL column with
// no default (and not in skip, and not the primary key) that an INSERT must
// supply, each paired with a placeholder value. Returns nil on any read or
// validation trouble so the caller's insert falls back to its base columns.
func requiredFillColumns(reader *sql.DB, table string, skip map[string]bool) []fillColumn {
	rows, err := reader.Query(fmt.Sprintf("PRAGMA table_info(%s)", security.QuoteIdent(table)))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []fillColumn
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if pk == 1 || notnull == 0 || dflt.Valid || skip[strings.ToLower(name)] {
			continue
		}
		if security.ValidateColumnName(name) != nil {
			return nil // cannot safely interpolate the identifier; abandon the fill
		}
		out = append(out, fillColumn{name: name, val: placeholderValue(name, ctype)})
	}
	if rows.Err() != nil {
		return nil // read trouble: fall back to the base columns, as documented
	}
	return out
}

// placeholderValue picks a benign value for a required column from its declared
// type and name: zero for numerics, a current timestamp for date/time-ish names,
// a readable string otherwise.
func placeholderValue(name, ctype string) any {
	ct := strings.ToUpper(ctype)
	switch {
	case strings.Contains(ct, "INT"),
		strings.Contains(ct, "REAL"), strings.Contains(ct, "FLOA"),
		strings.Contains(ct, "DOUB"), strings.Contains(ct, "NUM"),
		strings.Contains(ct, "BOOL"):
		return 0
	}
	n := strings.ToLower(name)
	for _, k := range []string{"date", "time", "_at", "created", "updated", "active", "seen", "login", "signup", "joined"} {
		if strings.Contains(n, k) {
			return time.Now().UTC().Format(time.RFC3339)
		}
	}
	if strings.Contains(n, "name") {
		return "Demo User"
	}
	return "demo"
}

// generateDemoPassword returns a 14-char password from an alphabet without
// visually ambiguous characters (0/O, 1/l/I).
func generateDemoPassword() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const n = 14
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

// loginSection renders the sign-in block: working credentials when a demo login
// was provisioned, an honest pointer when auth exists but none could be minted,
// "" otherwise. It never tells the owner to "create an account" - the generated
// login page is sign-in-only unless the brief asked for registration, so that
// instruction sent owners to a dead end.
func loginSection(demo *demoLogin, authExists bool, loginURL string) string {
	if demo != nil {
		label := demo.Label
		if label == "" {
			label = "Email" // defensive: pre-label demoLogin values still read sensibly
		}
		note := fmt.Sprintf("This is a demo account (role: %s) I created so you can sign in right away. Change or remove it anytime from Data Tables.", demo.Role)
		if demo.Adopted {
			note = fmt.Sprintf("I set a fresh password on the seeded **%s** account (role: %s) so you can sign in right away. Change it anytime from Data Tables.", demo.Email, demo.Role)
		}
		return fmt.Sprintf(
			"**Sign in to try it**\n\n"+
				"- **Login page:** [%s](%s)\n"+
				"- **%s:** `%s`\n"+
				"- **Password:** `%s`\n"+
				"- %s\n",
			loginURL, loginURL, label, demo.Email, demo.Password, note,
		)
	}
	if authExists {
		return fmt.Sprintf(
			"**Sign in**\n\n"+
				"- This project has a login at [%s](%s). I couldn't auto-create a demo account, so sign in with one of the seeded users in **Data Tables** (or add one there).\n",
			loginURL, loginURL,
		)
	}
	return ""
}
