package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/dbh"
)

// cssImportURLRe matches @import declarations in CSS bodies. Captures the
// URL inside `url(...)` or quoted-string forms. Permissive on whitespace and
// quote style so the same regex catches `@import url('x')`, `@import "x"`,
// `@import url(x)` etc.
var cssImportURLRe = regexp.MustCompile(`(?i)@import\s+(?:url\(\s*)?["']?([^"')\s;]+)["']?\s*\)?`)

// cssImportURLs extracts every @import URL from a CSS (or HTML-with-<style>)
// body. Returns the URLs in source order, deduped.
func cssImportURLs(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range cssImportURLRe.FindAllStringSubmatch(body, -1) {
		u := strings.TrimSpace(m[1])
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// loadPlanExternalLibraryURLs reads the current plan's plan_json and returns a
// URL -> name map of plan.external_libraries entries. The current plan is the
// most recent of_runs row carrying one. Empty map (not nil) on any failure, validator
// falls open rather than blocking saves when the plan is missing or malformed.
func loadPlanExternalLibraryURLs(db dbh.Handle) map[string]string {
	out := map[string]string{}
	if db == nil {
		return out
	}
	var planJSON sql.NullString
	if err := db.QueryRow(`SELECT plan_json FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON); err != nil {
		return out
	}
	if !planJSON.Valid || planJSON.String == "" {
		return out
	}
	var shell struct {
		ExternalLibraries []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON.String), &shell); err != nil {
		return out
	}
	for _, lib := range shell.ExternalLibraries {
		u := strings.TrimSpace(lib.URL)
		if u != "" {
			out[u] = lib.Name
		}
	}
	return out
}

// ModuleImportmapSpecifiers reads the current plan's external_libraries and
// returns the bare specifiers the importmap will expose; a specifier ending in
// "/" is a subpath PREFIX. ok is false only when the plan cannot be read -
// distinct from a plan with zero module libraries (ok=true, empty slice) - so a
// caller can skip its check rather than over-report.
func ModuleImportmapSpecifiers(db *sql.DB) (specs []string, ok bool) {
	if db == nil {
		return nil, false
	}
	var planJSON sql.NullString
	if err := db.QueryRow(`SELECT plan_json FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON); err != nil {
		return nil, false
	}
	if !planJSON.Valid || planJSON.String == "" {
		return nil, false
	}
	var shell struct {
		ExternalLibraries []struct {
			Kind       string `json:"kind"`
			ModuleName string `json:"module_name"`
		} `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON.String), &shell); err != nil {
		return nil, false
	}
	for _, lib := range shell.ExternalLibraries {
		if !strings.EqualFold(strings.TrimSpace(lib.Kind), "module") {
			continue
		}
		if s := strings.TrimSpace(lib.ModuleName); s != "" {
			specs = append(specs, s)
		}
	}
	return specs, true
}

// validateNoDuplicatePlanImports rejects CSS @import URLs that already appear
// in plan.external_libraries. The platform auto-injects every plan library
// into <head> via {{assets}}; a duplicate @import re-fetches the same asset.
func validateNoDuplicatePlanImports(db dbh.Handle, body, kind string) error {
	if body == "" || db == nil {
		return nil
	}
	imports := cssImportURLs(body)
	if len(imports) == 0 {
		return nil
	}
	libs := loadPlanExternalLibraryURLs(db)
	if len(libs) == 0 {
		return nil
	}
	type dupe struct{ URL, Name string }
	var dupes []dupe
	for _, u := range imports {
		if name, ok := libs[u]; ok {
			dupes = append(dupes, dupe{u, name})
		}
	}
	if len(dupes) == 0 {
		return nil
	}
	var b strings.Builder
	if len(dupes) == 1 {
		fmt.Fprintf(&b, "%s contains @import url(%q) which duplicates plan.external_libraries[name=%q]. ",
			kind, dupes[0].URL, dupes[0].Name)
	} else {
		fmt.Fprintf(&b, "%s contains %d @import URLs that duplicate plan.external_libraries entries:\n", kind, len(dupes))
		for _, d := range dupes {
			fmt.Fprintf(&b, "  - %s (plan entry: %s)\n", d.URL, d.Name)
		}
	}
	b.WriteString("The platform auto-injects every plan.external_libraries entry into <head> via {{assets}}; a duplicate @import re-fetches the same stylesheet (or font) and was a manual fix point in past builds. Remove the @import line(s); the asset still loads from the plan entry.")
	return fmt.Errorf("%s", b.String())
}

// jsWindowAssignRe captures identifiers assigned to window.X (producers).
var jsWindowAssignRe = regexp.MustCompile(`window\.([A-Za-z_$][\w$]*)\s*=`)

// jsWindowAssignBracketRe captures the bracket form, window['X'] = / window["X"] =.
// Producers are scanned wider than reads on purpose: a missed producer reads as
// "assigned nowhere", which false-rejects a live wire.
var jsWindowAssignBracketRe = regexp.MustCompile(`window\[\s*["']([A-Za-z_$][\w$]*)["']\s*\]\s*=`)

// jsWindowReadRe captures every identifier referenced as window.X. Producers are a
// subset of these references; a caller subtracts them to get pure consumers.
var jsWindowReadRe = regexp.MustCompile(`window\.([A-Za-z_$][\w$]*)`)

// WindowGlobalRefs is the ONE home for the `window.<name>` property scan the
// window-global checks share. It returns every name referenced (`all`) and the
// subset assigned to (`producers`); consumers are `all` minus `producers`. Reads
// are dot form only; bracket and aliased reads are deliberately not matched (the
// miss-over-false-positive bias every window check holds). Producers also take the
// bracket form: a missed read is a missed catch, a missed producer is a false reject.
func WindowGlobalRefs(body string) (all, producers map[string]bool) {
	all = map[string]bool{}
	producers = map[string]bool{}
	for _, m := range jsWindowReadRe.FindAllStringSubmatch(body, -1) {
		all[m[1]] = true
	}
	for _, m := range jsWindowAssignRe.FindAllStringSubmatch(body, -1) {
		producers[m[1]] = true
	}
	for _, m := range jsWindowAssignBracketRe.FindAllStringSubmatch(body, -1) {
		producers[m[1]] = true
	}
	return all, producers
}

// The window-global allowlist: the single home shared by the save-time bridge
// gate and the post-build dead-global validator, so "a project-authored bridge"
// means the same thing on the write path and the build path.

// reservedWindowGlobals are the `window.__*` names the PLATFORM injects at
// runtime; page JS is MEANT to read these, so they are never bridges or dead
// reads. Keyed WITHOUT the `__` prefix; kept in sync with the injection sites by
// tools/content's TestReservedWindowGlobals_StillInjected.
var reservedWindowGlobals = map[string]bool{
	"routeParams":       true,
	"ofProbeViolations": true,
}

// platformWindowGlobals is the set of bare `window.*` API surfaces the platform
// injects as classic global scripts. It is EMPTY: every platform helper
// (auth -> "auth", realtime -> "ofnet", crypto -> "e2ee") is an ES module imported
// by specifier. Retained as the extension point for any future bare platform global.
var platformWindowGlobals = map[string]bool{}

// browserWindowGlobals are built-in window/BOM properties legitimately read as
// `window.X`; the language/DOM set is deferred to analysis.IsJSGlobal. Err broad:
// over-allowing only weakens the catch, while under-allowing false-positives a
// correct build - the failure mode these gates must never hit.
var browserWindowGlobals = map[string]bool{
	"innerWidth": true, "innerHeight": true, "outerWidth": true, "outerHeight": true,
	"scrollX": true, "scrollY": true, "pageXOffset": true, "pageYOffset": true,
	"devicePixelRatio": true, "visualViewport": true, "screenX": true, "screenY": true,
	"top": true, "parent": true, "frames": true, "opener": true, "length": true,
	"origin": true, "name": true, "closed": true, "frameElement": true,
	"open": true, "close": true, "focus": true, "blur": true, "print": true, "stop": true,
	"moveTo": true, "moveBy": true, "resizeTo": true, "resizeBy": true,
	"onload": true, "onresize": true, "onscroll": true, "onmessage": true,
}

// IsAllowedWindowGlobal reports whether a captured `window.<name>` property is a
// platform-injected or browser/BOM global that pages are meant to read across files -
// i.e. NOT a project-authored bridge. The reserved names inject as `window.__NAME`
// but are keyed bare, so strip a leading `__` before that check.
func IsAllowedWindowGlobal(name string) bool {
	bare := strings.TrimPrefix(name, "__")
	return reservedWindowGlobals[bare] ||
		platformWindowGlobals[name] ||
		browserWindowGlobals[name] ||
		analysis.IsJSGlobal(name)
}

// ReservedWindowGlobalNames returns the platform-injected reserved names (bare, no
// `__`), for the drift test that pins each to its injection site.
func ReservedWindowGlobalNames() []string {
	out := make([]string, 0, len(reservedWindowGlobals))
	for n := range reservedWindowGlobals {
		out = append(out, n)
	}
	return out
}

// PlatformWindowGlobalCount reports how many bare platform window globals are
// declared (0 by design; the drift test asserts it stays empty).
func PlatformWindowGlobalCount() int { return len(platformWindowGlobals) }

// jsWindowProducersConsumers returns the identifiers this body exports on window
// (LHS of `window.X =`) and the identifiers it reads from window but does NOT itself
// export. Pure consumers are the cross-file dependency set the order validator checks
// against. Thin split over the shared WindowGlobalRefs scan.
func jsWindowProducersConsumers(body string) (producers map[string]bool, consumers map[string]bool) {
	all, producers := WindowGlobalRefs(body)
	consumers = map[string]bool{}
	for id := range all {
		if !producers[id] {
			consumers[id] = true
		}
	}
	return producers, consumers
}
