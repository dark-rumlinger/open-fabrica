package content

// manage_files, unified LLM-facing tool over the of_files table.
// Single CRUD surface for every file-like artifact in a project (pages, layouts,
// components, assets, files). The LLM picks `kind` and the tool dispatches
// through the ArtifactStore helper. Storage specifics (text vs binary,
// content-type, metadata) are picked up from the kind + supplied args.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// --- shared helpers --------------------------------------------------------

// scriptWrapperRe / styleWrapperRe match an entire artifact body that has been
// mistakenly wrapped in <script>…</script> or <style>…</style> tags. The LLM
// occasionally treats JS/CSS artifacts like inline page snippets and adds these
// wrappers, which cause SyntaxError in the browser because the file is served
// with application/javascript / text/css content-type, not as HTML.
var scriptWrapperRe = regexp.MustCompile(`(?is)^\s*<script[^>]*>(.*)</script>\s*$`)
var styleWrapperRe = regexp.MustCompile(`(?is)^\s*<style[^>]*>(.*)</style>\s*$`)

// stripScriptStyleWrapper removes a <script>…</script> or <style>…</style>
// wrapper from a JS or CSS artifact body. Returns the inner content and true if
// stripping happened, otherwise returns the original body unchanged.
func stripScriptStyleWrapper(path, body string) (string, bool) {
	ext := strings.ToLower(path)
	var re *regexp.Regexp
	switch {
	case strings.HasSuffix(ext, ".js"):
		re = scriptWrapperRe
	case strings.HasSuffix(ext, ".css"):
		re = styleWrapperRe
	default:
		return body, false
	}
	if m := re.FindStringSubmatch(body); m != nil {
		return strings.TrimSpace(m[1]), true
	}
	return body, false
}

// notFoundForModify is used by patch. It keeps the same wording shape as
// tools.NotFound but adds the recovery hint - patch only modifies an existing
// artifact, so the correct next step for the caller is almost always `save`
// with a full body. Returning that guidance in the error saves the LLM an
// iteration of "what do I do now".
func notFoundForModify(ctx *tools.ToolContext, kind, path string) *tools.Result {
	hint := ""
	if ctx != nil {
		hint = store.SuggestArtifactPathsHint(ctx.DB, kind, path)
	}
	label := kind
	if label == "" {
		label = "artifact"
	}
	return tools.FailCode(tools.CodeNotFound,
		"%s %q does not exist. Call manage_files action=\"save\" with a full body to create it; patch only modifies an already-saved artifact.%s",
		label, path, hint,
	)
}

// decodeJSONArrayArg reads `args[key]` as either a native array (preferred,
// avoids a layer of escaping in the model's tool call) or a JSON-stringified
// array. The decoded value is unmarshaled into `out`, which must be a
// pointer to a slice. Returns a user-facing error on missing/invalid input.
func decodeJSONArrayArg(raw any, key string, out any) error {
	if raw == nil {
		return fmt.Errorf("%s is required", key)
	}
	var bytes []byte
	switch v := raw.(type) {
	case string:
		if v == "" {
			return fmt.Errorf("%s is required", key)
		}
		bytes = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("invalid %s: %v", key, err)
		}
		bytes = b
	}
	if err := json.Unmarshal(bytes, out); err != nil {
		return fmt.Errorf("invalid %s JSON: %v", key, err)
	}
	return nil
}

// resolveKindPath is the existing-artifact counterpart to requireKindPath: kind
// is optional and inferred from file_path when it uniquely identifies one artifact,
// killing the loop where a weaker model omits kind on a get/outline/history/diff and
// retries the same rejected shape before recovering. Actions that target an existing
// live artifact (reads, patch, delete, rename source) all infer this way; save keeps
// requireKindPath (a create has nothing to infer from), as does restore (inference
// only sees live rows, but restore must reach a just-deleted tombstone). When kind
// is empty: zero matches ->
// not-found; exactly one -> use it; more than one -> ask to disambiguate, naming the
// candidate kinds so the next call is correct.
// canonicalizeFilePath collapses the URL-style file references the model often emits
// ("/files/x", "files/x", "/x") to the bare path of_files keys on ("x"). A kind=file
// artifact is stored bare and served at /files/<path>, so without this a get/save on
// "/files/app.js" and one on "app.js" hit different rows and test_module bridges
// double the prefix. A leading "/files/" or "files/" is an unambiguous file signal,
// so kind is inferred "file" when omitted; an explicit kind=="file" also has a stray
// leading slash trimmed. Page/layout/component paths (legitimately "/"-leading) pass
// through untouched.
func canonicalizeFilePath(kind, path string) (string, string) {
	if kind == "file" {
		return "file", store.CanonicalFilePath(path)
	}
	// Infer the kind only when the caller left it blank: a "/files/" prefix is the
	// file-serving route, so a bare-kind reference to it is a file. An explicit
	// non-file kind is respected as given, never overridden by the path shape.
	if kind == "" && (strings.HasPrefix(path, "/files/") || strings.HasPrefix(path, "files/")) {
		return "file", store.CanonicalFilePath(path)
	}
	return kind, path
}

func resolveKindPath(ctx *tools.ToolContext, kind, path string) (string, string, *tools.Result) {
	if path == "" {
		return "", "", tools.FailCode(tools.CodeMissingArg, "file_path is required")
	}
	kind, path = canonicalizeFilePath(kind, path)
	if kind != "" {
		return kind, path, nil
	}
	kinds, err := store.ArtifactKindsForPath(ctx.DB, path)
	if err != nil {
		return "", "", tools.FailErr(err)
	}
	// URL-form retry: save accepts "/js/app.js" (inferSaveKind + CanonicalFilePath
	// store it as "js/app.js"), so a kind-less edit of the same string must find it
	// too. Retry the lookup with the canonical path rather than forcing kind=file,
	// so an extensioned artifact of another kind still resolves to its own kind.
	// Gated on an asset extension, mirroring save-side inference: extension-less
	// page/component names never retry.
	if len(kinds) == 0 && pathLooksLikeArtifact(path) {
		if canon := store.CanonicalFilePath(path); canon != path {
			kinds, err = store.ArtifactKindsForPath(ctx.DB, canon)
			if err != nil {
				return "", "", tools.FailErr(err)
			}
			if len(kinds) > 0 {
				path = canon
			}
		}
	}
	switch len(kinds) {
	case 1:
		return kinds[0], path, nil
	case 0:
		return "", "", tools.FailCode(tools.CodeNotFound,
			"no artifact exists at %q (any kind).%s",
			path, store.SuggestArtifactPathsHint(ctx.DB, "", path))
	default:
		return "", "", tools.FailCode("ambiguous_kind",
			"path %q exists as multiple kinds (%s); pass kind= to pick one.",
			path, strings.Join(kinds, ", "),
		)
	}
}

// inferSaveKind returns the kind a create can safely assume from the path alone:
//
//   - "file" for the /files/ serving route or any static-asset extension. A page,
//     layout, or component is always extension-less, so an extension is an
//     unambiguous file signal.
//   - "page" for an extension-less path with a leading "/". ValidateFilePath
//     rejects a "/" anywhere in a layout or component path (both are referenced by
//     bare name, never by URL), and a kind=file path is stored bare, so a leading
//     slash leaves "page" as the only kind the path can legally be saved under.
//     validatePagePath requires that same leading slash from the other direction.
//
// Returns "" only for a bare extension-less name ("default", "header"), where
// layout-vs-component can't be guessed; the caller then falls through to
// requireKindPath and asks for an explicit kind. An explicit kind always wins: this
// runs only when the caller left kind empty. It mirrors the kind-inference the
// read/patch actions already do.
func inferSaveKind(path string) string {
	if strings.HasPrefix(path, "/files/") || strings.HasPrefix(path, "files/") {
		return "file"
	}
	if pathLooksLikeArtifact(path) {
		return "file"
	}
	if strings.HasPrefix(path, "/") {
		return "page"
	}
	return ""
}

// reservedUploadPath reports whether a kind=file path falls under the
// "uploads/" prefix. That namespace is owned by the runtime file-upload handler
// (server/public/upload.go writes visitor uploads there as kind=file rows in
// of_files and meters the per-project upload quota on "uploads/%"). A builder-
// authored artifact saved or renamed into it could collide with - or be
// silently overwritten by - a visitor upload via the of_files (kind, path) ON
// CONFLICT, and would skew the quota accounting. Code and assets belong under
// js/, styles/, assets/, etc. The check runs on the canonicalized path, so the
// "/files/uploads/x", "/uploads/x", and "uploads/x" URL forms all match.
func reservedUploadPath(kind, path string) bool {
	return kind == "file" && (path == "uploads" || strings.HasPrefix(path, "uploads/"))
}

// reservedUploadPathError is the rejection returned when a save/rename targets
// the reserved uploads/ namespace.
func reservedUploadPathError(path string) *tools.Result {
	return tools.FailCode("reserved_path", "path %q is under the reserved \"uploads/\" prefix, which is owned by the runtime file-upload handler - visitor uploads land there and the project upload quota is metered on it. Save code or assets under a different path (e.g. js/, styles/, assets/).", path)
}

// likeEscape escapes the LIKE wildcards (and the escape character) in s for a
// pattern written with ESCAPE '\'.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func requireKindPath(kind, path string) (string, string, *tools.Result) {
	kind, path = canonicalizeFilePath(kind, path)
	if kind == "" {
		if path == "" {
			return "", "", tools.FailCode(tools.CodeMissingArg, "kind is required")
		}
		// Only `save` can reach this with an empty kind: every other caller declares
		// kind as a required arg, enforced pre-dispatch by tools.ValidateArgs. And
		// save has already run inferSaveKind, which resolves a leading-slash route to
		// "page" and an extension / "/files/" prefix to "file" - so what survives is
		// a BARE extension-less name, ambiguous only between layout and component.
		// Name that actual choice: the previous message restated the arg as missing
		// and guessed "did you mean kind=file?", which was unreachable here (an
		// extensioned path never gets this far) and pointed at the one kind the path
		// cannot be.
		return "", "", tools.FailCode(tools.CodeMissingArg,
			"kind is required - %q is a bare name, which is ambiguous between kind=\"layout\" (the shared shell, "+
				"saved at path \"default\") and kind=\"component\" (a reusable fragment included with {{include:%s}}). "+
				"Pass whichever you meant. Only bare names need this: a page is saved at a leading-slash route "+
				"(\"/\", \"/about\") and a static asset carries an extension (\"js/app.js\") - both are inferred from "+
				"file_path and never need an explicit kind.",
			path, path,
		)
	}
	if path == "" {
		return "", "", tools.FailCode(tools.CodeMissingArg, "file_path is required")
	}
	return kind, path, nil
}

// pathLooksLikeArtifact returns true when the path carries a recognised
// static-file extension. Pages, layouts and components are typically extension-less
// names (e.g. "default", "header", "/about"), so an extension is a strong
// hint that the LLM meant kind="file" instead of one of the document kinds.
func pathLooksLikeArtifact(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{
		".js", ".mjs", ".css", ".html", ".htm",
		".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp",
		".json", ".pdf", ".txt", ".ico", ".woff", ".woff2", ".ttf",
	} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// DefaultContentTypeForKind returns a reasonable MIME for a given kind+path.
// The LLM can override via the content_type arg; this is just the fallback.
func DefaultContentTypeForKind(kind, path string) string {
	switch kind {
	case "page", "layout", "component":
		return "text/html; charset=utf-8"
	}
	// For kind=file, pick from the path extension.
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".mjs"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(lower, ".html"), strings.HasSuffix(lower, ".htm"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".txt"), strings.HasSuffix(lower, ".ts"), strings.HasSuffix(lower, ".tsx"), strings.HasSuffix(lower, ".jsx"):
		return "text/plain; charset=utf-8"
	case strings.HasSuffix(lower, ".cjs"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(lower, ".md"):
		return "text/markdown; charset=utf-8"
	case strings.HasSuffix(lower, ".csv"):
		return "text/csv; charset=utf-8"
	case strings.HasSuffix(lower, ".xml"):
		return "text/xml; charset=utf-8"
	case strings.HasSuffix(lower, ".yml"), strings.HasSuffix(lower, ".yaml"):
		return "text/yaml; charset=utf-8"
	}
	return "application/octet-stream"
}

// lineCount returns the number of newline-separated lines in body, matching
// the 1-indexed semantics of sliceLines so save / get responses can advertise
// total_lines without the caller having to count.
func lineCount(body string) int {
	if body == "" {
		return 0
	}
	return strings.Count(body, "\n") + 1
}

// sliceLines returns the slice of body between from and to line numbers
// (1-indexed, inclusive). If to is 0, runs to end.
func sliceLines(body string, from, to int) string {
	if from <= 0 {
		from = 1
	}
	lines := strings.Split(body, "\n")
	if from > len(lines) {
		return ""
	}
	if to <= 0 || to > len(lines) {
		to = len(lines)
	}
	return strings.Join(lines[from-1:to], "\n")
}

// numberLines prefixes every line of body with an unpadded line-number gutter
// ("N| ") starting at startLine (1-indexed, matching sliceLines). Unpadded because
// padding is pure context cost: a 300-line read paid ~1KB of spaces for it.
// The gutter is display-only - callers must not feed it back into patch
// search text. An empty body returns "".
func numberLines(body string, startLine int) string {
	if body == "" {
		return ""
	}
	if startLine <= 0 {
		startLine = 1
	}
	lines := strings.Split(body, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d| %s", startLine+i, line)
	}
	return b.String()
}

// trimToByteBudget trims a numbered block to the last whole line that fits in
// budget bytes, returning the trimmed text and the number of lines kept. If the
// whole block already fits (or budget is non-positive), it is returned intact.
// A single line longer than budget is still returned whole (count 1) so a get
// never yields an empty page.
func trimToByteBudget(numbered string, budget int) (string, int) {
	if numbered == "" {
		return "", 0
	}
	lines := strings.Split(numbered, "\n")
	if budget <= 0 || len(numbered) <= budget {
		return numbered, len(lines)
	}
	used := 0
	kept := 0
	for i, line := range lines {
		// +1 for the newline that joins this line to the previous one.
		add := len(line)
		if i > 0 {
			add++
		}
		if kept > 0 && used+add > budget {
			break
		}
		used += add
		kept++
	}
	if kept == 0 {
		kept = 1
	}
	return strings.Join(lines[:kept], "\n"), kept
}
