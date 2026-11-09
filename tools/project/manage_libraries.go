package project

import (
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/analysis"
	"openfabrica/tools"
)

// manage_libraries is the narrow surface for amending the project's declared external
// libraries mid-build. Work is tracked in of_tasks (see manage_tasks); the one
// thing BUILD mutates on the plan itself is external_libraries, which the public
// renderer reads to auto-inject CDN <script>/<link> tags. The cross-origin asset
// validators (tools/store) point the agent here when a hand-written tag is rejected.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_libraries",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult: 8000,
		Category:  "project",
		OneLiner:  "Declare or remove an external CDN library the platform auto-injects as a <script>/<link> tag.",
		WhenToUse: `Call add_library when an artifact save is rejected with "cross-origin
<script src>/<link rel=\"stylesheet\"> not allowed": declare the URL here and the
renderer injects the tag (with crossorigin + integrity when supplied) into every
page, then retry the save without the hand-written tag. Pin the version; never @latest.
add_library is an UPSERT keyed on name: re-add a declared library with a changed
url/module_name to BUMP its version in place (the fix for a core/addons version
mismatch) - the result names exactly what changed. Re-adding identical values is a
no-op. Call remove_library to drop a declared library ENTIRELY - the fix for one
added by mistake, no longer used, or pointing at a nonexistent URL whose injected
tag 404s on every page (for a merely wrong url/version, prefer add_library, which
bumps in place rather than removing). Use get_plan to read the current plan JSON
back. This tool manages external libraries only; the build checklist/tasks live in
manage_tasks.`,
		Actions: []tools.Action{
			&LibraryAdd{},
			&LibraryRemove{},
			&LibraryGetPlan{},
		},
	})
}

// The current plan lives on the most recent of_runs row that carries one (the
// engine writes plan_json there at PLAN; updates target that same row).
const currentPlanQuery = `SELECT plan_json FROM of_runs
	WHERE plan_json IS NOT NULL AND plan_json != ''
	ORDER BY started_at DESC, id DESC LIMIT 1`

const updateCurrentPlanStmt = `UPDATE of_runs SET plan_json = ?, updated_at = CURRENT_TIMESTAMP
	WHERE id = (SELECT id FROM of_runs
	            WHERE plan_json IS NOT NULL AND plan_json != ''
	            ORDER BY started_at DESC, id DESC LIMIT 1)`

func loadPlanMap(ctx *tools.ToolContext) (map[string]any, error) {
	var planJSON string
	if err := ctx.DB.QueryRow(currentPlanQuery).Scan(&planJSON); err != nil || planJSON == "" {
		return nil, fmt.Errorf("no plan loaded")
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, fmt.Errorf("failed to parse plan")
	}
	return plan, nil
}

// LibraryAdd appends an external CDN library to plan.external_libraries,
// deduping by name. The renderer injects the tag from the URL at request time.
type LibraryAdd struct {
	Library map[string]any `arg:"library,required" desc:"Library spec: {\"name\":\"marked\", \"url\":\"https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js\"}. Optional: \"kind\":\"classic\"|\"module\" (default classic = UMD/IIFE <script src>; module = ES module via importmap, requires \"module_name\" = the bare specifier used in import statements), \"integrity\":\"sha384-...\", \"purpose\":\"brief note\". A classic library MUST also carry \"global_name\" = the exact window global it defines (\"marked\", \"Chart\", \"qrcode\") - without it the platform cannot tell that global from a typo and every module's undefined-reference check drops to non-blocking, so a forgotten import ships as a live ReferenceError. Pin the version; never @latest."`
}

func (a *LibraryAdd) ActionName() string { return "add_library" }
func (a *LibraryAdd) Doc() string {
	return "Add or update an external CDN library in plan.external_libraries. Use when a save fails with a cross-origin tag rejection: declare the URL here, then retry the save without the hand-written tag. UPSERT keyed on name: re-adding a declared library with a changed url/kind/module_name/integrity updates it in place (e.g. bumping three.js core to match the addons version) and the result reports exactly which fields changed; re-adding the same values is an idempotent no-op. Declared libraries are listed in the project_state snapshot."
}
func (a *LibraryAdd) Example() any {
	return &LibraryAdd{Library: map[string]any{
		"name":        "marked",
		"url":         "https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js",
		"global_name": "marked",
	}}
}
func (a *LibraryAdd) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name, _ := a.Library["name"].(string)
	if name == "" {
		return tools.Fail("library.name is required"), nil
	}
	url, _ := a.Library["url"].(string)
	if url == "" {
		return tools.Fail("library.url is required"), nil
	}
	if analysis.ExternalURLIsFontFile(url) {
		return tools.Fail("library.url %q is a font file, not a loadable stylesheet: a bare .woff2/.woff/.ttf/.otf (or fonts.gstatic.com) URL has no @font-face rule, so the platform can only preload it - the font never actually applies, and older builds injected it as a <script> the browser refused to execute (\"MIME type 'font/woff2' is not executable\"). Declare the Google Fonts stylesheet endpoint instead, e.g. url=\"https://fonts.googleapis.com/css2?family=<Family+Name>&display=swap\" - that injects a <link rel=\"stylesheet\"> whose @font-face rules pull in the woff2 files automatically.", url), nil
	}

	// Atomic read-modify-write: external_libraries is a single shared JSON
	// document. A transaction (the writer pool acquires the write lock immediately
	// via _txlock=immediate) serializes the load/append/save so the update is
	// atomic - correct even if a future caller mutates the plan alongside a build.
	tx, err := ctx.DB.Begin()
	if err != nil {
		return tools.FailErr(err), nil
	}
	defer tx.Rollback()

	var planJSON string
	if err := tx.QueryRow(currentPlanQuery).Scan(&planJSON); err != nil || planJSON == "" {
		return tools.Fail("no plan loaded"), nil
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return tools.Fail("failed to parse plan"), nil
	}
	libs, _ := plan["external_libraries"].([]any)
	for i, e := range libs {
		em, ok := e.(map[string]any)
		if !ok || em["name"] != name {
			continue
		}
		// A library with this name is already declared. Treat the re-add as an
		// UPSERT, not a no-op: fill any field the existing entry is missing AND
		// update any field whose value differs. The load-bearing cases are
		// module_name (a kind="module" lib declared without it is SILENTLY dropped
		// from the importmap) and url/version - reconciling a core/addons version
		// mismatch means changing the url of the already-declared core, which a
		// fill-empty-only path refuses, sending the model off to invent importmap
		// workarounds. Only `name` (the key) is immutable. Crucially the response
		// names exactly what changed, so a real update is never mistaken for the
		// silent no-op that caused the model to think a version bump had applied
		// when it had not.
		var filled, updated []string
		for k, v := range a.Library {
			if k == "name" || libFieldEmpty(v) {
				continue
			}
			switch {
			case libFieldEmpty(em[k]):
				em[k] = v
				filled = append(filled, k)
			case !libFieldsEqual(em[k], v):
				em[k] = v
				updated = append(updated, k)
			}
		}
		// A new url is a different file: the fields that describe the old file no
		// longer hold. A stale global_name makes the global checks accept the old
		// name and reject the one the new file really defines; a stale integrity
		// hash blocks the new file from loading. Clear each one the call does not
		// supply again, and say so.
		var cleared []string
		if containsString(updated, "url") {
			for _, k := range []string{"global_name", "integrity"} {
				if libFieldEmpty(a.Library[k]) && !libFieldEmpty(em[k]) {
					delete(em, k)
					cleared = append(cleared, k)
				}
			}
		}
		if len(filled) == 0 && len(updated) == 0 {
			return tools.Ok(map[string]any{
				"name":  name,
				"total": len(libs),
				"note":  fmt.Sprintf("library '%s' is already declared with these exact values; nothing changed - it's ready to use", name),
			}), nil
		}
		libs[i] = em
		plan["external_libraries"] = libs
		b, err := json.Marshal(plan)
		if err != nil {
			return tools.Fail("failed to marshal plan"), nil
		}
		if _, err := tx.Exec(updateCurrentPlanStmt, string(b)); err != nil {
			return tools.FailErr(err), nil
		}
		if err := tx.Commit(); err != nil {
			return tools.FailErr(err), nil
		}
		out := map[string]any{
			"updated":        name,
			"total":          len(libs),
			"fields_filled":  filled,
			"fields_updated": updated,
			"note":           upsertNote(name, filled, updated, cleared, em),
		}
		if len(cleared) > 0 {
			out["fields_cleared"] = cleared
		}
		return tools.Ok(out), nil
	}
	// A module library the importmap cannot key is dropped from it in silence, so
	// every `import ... from '<name>'` would fail at load: refuse it here, where the
	// fix is one field away.
	kind, _ := a.Library["kind"].(string)
	if strings.EqualFold(strings.TrimSpace(kind), "module") && libFieldEmpty(a.Library["module_name"]) {
		return tools.Fail("library %q is kind=\"module\" but has no module_name: the importmap is keyed on it, so without it the library is never importable. Add module_name = the bare specifier your imports use (e.g. \"three\", or \"three/addons/\" for a subpath prefix).", name), nil
	}
	libs = append(libs, a.Library)
	plan["external_libraries"] = libs
	b, err := json.Marshal(plan)
	if err != nil {
		return tools.Fail("failed to marshal plan"), nil
	}
	if _, err := tx.Exec(updateCurrentPlanStmt, string(b)); err != nil {
		return tools.FailErr(err), nil
	}
	if err := tx.Commit(); err != nil {
		return tools.FailErr(err), nil
	}
	out := map[string]any{"added": name, "total": len(libs)}
	// A classic script still loads without global_name, so this warns rather than
	// refuses - but without it every module's undefined-reference check turns
	// non-blocking, which is exactly how a forgotten import ships.
	if classicScriptURL(kind, url) && libFieldEmpty(a.Library["global_name"]) {
		out["warning"] = fmt.Sprintf("library %q is a classic script with no global_name: until you re-add it with global_name = the exact window global it defines, the undefined-reference check cannot tell that global from a typo and stops blocking.", name)
	}
	return tools.Ok(out), nil
}

// classicScriptURL reports whether a library entry is a classic (UMD/IIFE) JS script:
// kind unset or "classic", and a URL whose path ends in .js. Stylesheets and fonts
// define no global and are left alone.
func classicScriptURL(kind, url string) bool {
	if k := strings.ToLower(strings.TrimSpace(kind)); k != "" && k != "classic" {
		return false
	}
	path := strings.ToLower(url)
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	return strings.HasSuffix(path, ".js")
}

// libFieldEmpty reports whether an external-library field is absent or an empty/
// whitespace string, the test add_library's upgrade uses to decide which fields a
// re-declaration may fill. A present non-string value counts as set.
func libFieldEmpty(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// libFieldsEqual compares two external-library field values. The fields that
// matter (url, kind, module_name, integrity, purpose) are strings; a stringwise
// compare decides whether a re-add is changing a value or restating it.
func libFieldsEqual(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// upsertNote describes exactly what a re-add changed so the model can act on it
// (e.g. retest the page after a version bump) instead of guessing. When the url
// changed it is echoed in full - the version bump is the case where the model
// most needs to confirm the new bytes are live.
func upsertNote(name string, filled, updated, cleared []string, em map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "library '%s' updated in place", name)
	if len(updated) > 0 {
		fmt.Fprintf(&b, "; changed %s", strings.Join(updated, ", "))
	}
	if len(filled) > 0 {
		fmt.Fprintf(&b, "; filled %s", strings.Join(filled, ", "))
	}
	for _, k := range updated {
		if k == "url" {
			if u, _ := em["url"].(string); u != "" {
				fmt.Fprintf(&b, ". It now resolves to %s - retest the page to confirm the new version loads.", u)
			}
			break
		}
	}
	if containsString(cleared, "global_name") {
		b.WriteString(". The url changed, so the old global_name was cleared: if this is a classic script, re-add it with global_name set to the window global the NEW file defines (check the library's docs), or code reading that global is reported as undefined.")
	}
	if containsString(cleared, "integrity") {
		b.WriteString(" The old integrity hash was cleared too; it belonged to the previous file.")
	}
	return b.String()
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// LibraryRemove deletes a declared external CDN library from plan.external_libraries,
// keyed on name - the counterpart to LibraryAdd. add_library is an upsert, so it can
// bump a wrong url/version in place but can NEVER drop an entry (an empty field is
// skipped, not cleared); remove_library is the only way to take a library back out.
// The load-bearing case is a library added by mistake or with a nonexistent URL: the
// renderer injects its <script>/<link> unconditionally on every page, so a dead URL
// 404s everywhere until the declaration is removed.
type LibraryRemove struct {
	Name string `arg:"name,required" desc:"Exact name of the declared library to remove - the key add_library dedupes on (e.g. \"marked\", \"three.js core\"). Read get_plan or the project_state snapshot for the declared names. Removing it stops the renderer injecting that library's tag on every page; if page code still references its window global, remove that usage too or the undefined-reference check will flag it. For a merely wrong url/version, re-add with add_library (bumps in place) instead of removing."`
}

func (a *LibraryRemove) ActionName() string { return "remove_library" }
func (a *LibraryRemove) Doc() string {
	return "Remove a declared external CDN library from plan.external_libraries, keyed on name. The counterpart to add_library: use it to drop a library added by mistake, no longer needed, or pointing at a nonexistent/typo'd URL that 404s on every page. (For a merely wrong url/version, prefer re-adding the same name - add_library is an upsert that bumps in place; it can never remove.) Fails with the list of currently-declared names when the name is not found, so a typo self-corrects. Declared libraries are listed in the project_state snapshot and via get_plan."
}
func (a *LibraryRemove) Example() any {
	return &LibraryRemove{Name: "marked"}
}
func (a *LibraryRemove) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return tools.Fail("name is required"), nil
	}

	// Same atomic read-modify-write as add_library: external_libraries is a single
	// shared JSON document, so the transaction (writer pool takes the write lock via
	// _txlock=immediate) serializes the load/filter/save against a concurrent add.
	tx, err := ctx.DB.Begin()
	if err != nil {
		return tools.FailErr(err), nil
	}
	defer tx.Rollback()

	var planJSON string
	if err := tx.QueryRow(currentPlanQuery).Scan(&planJSON); err != nil || planJSON == "" {
		return tools.Fail("no plan loaded"), nil
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return tools.Fail("failed to parse plan"), nil
	}
	libs, _ := plan["external_libraries"].([]any)

	// Filter out every entry matching name (add dedupes, so normally one), keeping
	// the rest - including any malformed non-map entries, which are never the target.
	// Collect the declared names in the same pass to make a miss self-correcting.
	kept := make([]any, 0, len(libs))
	var declared []string
	removed := 0
	for _, e := range libs {
		if em, ok := e.(map[string]any); ok {
			if n, _ := em["name"].(string); n != "" {
				declared = append(declared, n)
			}
			if em["name"] == name {
				removed++
				continue
			}
		}
		kept = append(kept, e)
	}
	if removed == 0 {
		if len(declared) == 0 {
			return tools.Fail("no external library named %q is declared - the plan declares no external libraries, so there is nothing to remove", name), nil
		}
		return tools.Fail("no external library named %q is declared; declared libraries are: %s. Pass the exact name (see get_plan).", name, strings.Join(declared, ", ")), nil
	}

	plan["external_libraries"] = kept
	b, err := json.Marshal(plan)
	if err != nil {
		return tools.Fail("failed to marshal plan"), nil
	}
	if _, err := tx.Exec(updateCurrentPlanStmt, string(b)); err != nil {
		return tools.FailErr(err), nil
	}
	if err := tx.Commit(); err != nil {
		return tools.FailErr(err), nil
	}
	return tools.Ok(map[string]any{
		"removed": name,
		"total":   len(kept),
		"note":    fmt.Sprintf("library '%s' removed; the renderer will no longer inject its tag. If any page still references its global, remove that usage too.", name),
	}), nil
}

// LibraryGetPlan returns the current plan JSON (goal, voice, design, external_libraries).
type LibraryGetPlan struct{}

func (a *LibraryGetPlan) ActionName() string { return "get_plan" }
func (a *LibraryGetPlan) Doc() string        { return "Return the current plan JSON." }
func (a *LibraryGetPlan) ReadOnly() bool     { return true }
func (a *LibraryGetPlan) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	plan, err := loadPlanMap(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	return tools.Ok(plan), nil
}
