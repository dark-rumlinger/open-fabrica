package content

// Write handlers (save / patch / bulk_replace)
// for manage_files, which registers them in manage_files.go's init.

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"openfabrica/analysis"
	"openfabrica/probe"
	"openfabrica/store"
	"openfabrica/tools"
)

func artifactActionSave(ctx *tools.ToolContext, act *ArtSave) (*tools.Result, error) {
	kind, path, body, contentType := act.Kind, act.FilePath, act.Body, act.ContentType
	scope, metadata, attachTo, overwrite := act.Scope, act.Metadata, act.AttachTo, act.Overwrite
	// kind is optional when the path is self-describing (a "/files/" route or a
	// static-asset extension): infer "file" so a JS/CSS save without a kind succeeds.
	// An extension-less page/layout/component create still needs an explicit kind.
	if kind == "" {
		kind = inferSaveKind(path)
	}
	kind, path, errR := requireKindPath(kind, path)
	if errR != nil {
		return errR, nil
	}
	if reservedUploadPath(kind, path) {
		return reservedUploadPathError(path), nil
	}
	if rej := rejectPlatformManaged(ctx, kind, path, "overwrite"); rej != nil {
		return rej, nil
	}
	// save exposes no status/storage args; keep the prior defaults.
	status := "published"
	storage := store.StorageMode("")

	// metadata arrives as a typed object straight from the action struct and is
	// used directly, so every field (page title, ...) is preserved.
	meta := metadata
	if meta == nil {
		meta = map[string]any{}
	}
	scope, scopeHoisted := resolveSaveScope(scope, meta)

	// A full re-save replaces the body, not the artifact's identity: metadata keys and
	// the content type the caller did not restate carry over from the stored row, so a
	// page saved with {title, layout} and re-saved without metadata keeps both.
	var prevMeta map[string]any
	if prev, _ := store.ArtifactGet(ctx.DB, kind, path); prev != nil {
		prevMeta = prev.Metadata
		if contentType == "" {
			contentType = prev.ContentType
		}
		merged := make(map[string]any, len(prevMeta)+len(meta))
		for k, v := range prevMeta {
			merged[k] = v
		}
		for k, v := range meta {
			merged[k] = v
		}
		meta = merged
	}
	if contentType == "" {
		contentType = DefaultContentTypeForKind(kind, path)
	}

	// Backstop: a page saved without a title would render with the project
	// name. Seed the PLAN stage's per-page title so every page gets a distinct
	// <title>. A model-authored title always wins; the plan is only the floor.
	if kind == "page" {
		if t, _ := meta["title"].(string); t == "" {
			if planTitle := planTitleForPath(ctx.DB, path); planTitle != "" {
				if meta == nil {
					meta = map[string]any{}
				}
				meta["title"] = planTitle
			}
		}
	}

	var wrapperStripped bool
	if kind == "file" {
		body, wrapperStripped = stripScriptStyleWrapper(path, body)
	}

	// Look up the currently-stored artifact once, up front: prevBody feeds the
	// oversized-body crossing guard just below, the redundant-resave short-circuit,
	// the clobber gate, and the syntax/import gates further down.
	var prevBody, prevScope string
	exists := false
	if prev, _ := store.ArtifactGet(ctx.DB, kind, path); prev != nil {
		prevBody = prev.Body
		prevScope = prev.Scope
		exists = true
	}

	// A full-body save past the save ceiling risks the model truncating it mid-write
	// (shipping a corrupt artifact), so it is refused with a guide to scaffold a smaller
	// body and extend it. Guarded on crossing (prevBody at/under the ceiling) so a
	// corrective re-save of a file already grown past it via warn-only patch growth is not
	// bounced into a patch loop; mirrors module_too_large_client's crossing condition.
	if len(body) > store.MaxSingleSaveBody && len(prevBody) <= store.MaxSingleSaveBody {
		return tools.FailCode("oversized_body",
			"refusing to save %q in one call - body is %d bytes, over the %s single-save limit. A body this large gets truncated mid-write and ships corrupt. Preferred fix: split this into several smaller files (entry + leaf modules), each written complete in one shot. If it is genuinely one cohesive file, save a smaller initial body now (real structure and first sections, not stubs) and add the rest in focused follow-up edits.",
			path, len(body), store.HumanSize(store.MaxSingleSaveBody),
		), nil
	}

	// The save is gated like an edit: a JS file the browser cannot parse ships a page that
	// throws on load, and a full save would persist it unconditionally. A mid-split module
	// importing a not-yet-created sibling still parses clean, so only genuine breakage is
	// refused.
	// Redundant-resave guard: a full save byte-identical to what is stored writes nothing
	// useful (burns output tokens, bumps the version for no change), so short-circuit to a
	// no-op success and steer future small edits to action=patch. Comparison is against the
	// normalized body (wrapper stripped above), matching what ArtifactSave stores.
	// The shortcut applies only when nothing else about the artifact would change: a
	// restated scope, content type or metadata key is a real edit even with the same body.
	metaSame := reflect.DeepEqual(prevMeta, meta) || (len(prevMeta) == 0 && len(meta) == 0)
	scopeSame := act.Scope == "" && !scopeHoisted || scope == prevScope
	if exists && body == prevBody && metaSame && scopeSame && act.ContentType == "" {
		return tools.Ok(map[string]any{
			"kind":        kind,
			"file_path":   path,
			"unchanged":   true,
			"total_lines": lineCount(body),
			"note":        "no write performed - the body is byte-identical to the current version. For a small edit use action=patch, not a full re-save.",
		}), nil
	}
	// Same-path clobber gate: a save aimed at an EXISTING scope=global file with a
	// different, unrelated body silently takes the global file off every page that depends
	// on it - a site-wide regression the model has no reason to look for. Two triggers,
	// both scoped to global files and off when overwrite=true: (A) the save carries page
	// intent (attach_to or scope=page) and the body diverges; (B) no intent signal, but the
	// body is a drastic shrink sharing almost none of the old file's lines. A legitimate
	// rewrite keeps similar size or shared vocabulary and passes; a wrong fire recovers with
	// overwrite=true.
	if rej := rejectIfSamePathClobber(kind, path, prevBody, prevScope, body, scope, attachTo, exists, overwrite); rej != nil {
		return rej, nil
	}
	// Reserved-specifier gate: js/ofnet.js, js/e2ee.js, and js/auth.js are platform
	// ES-module helpers imported by the bare specifiers "ofnet"/"e2ee"/"auth". A user
	// module whose basename derives one of those names would collide in ProjectModuleMap
	// and silently knock the helper out of the importmap, making both unresolvable. Refuse
	// it with a rename nudge. (The platform's own emit writes these through
	// store.ArtifactSave, not this model-facing path, so it is unaffected.)
	if kind == "file" && isJSModulePath(path) {
		if name := analysis.ProjectModuleName(path); analysis.IsReservedModuleSpecifier(name) {
			return tools.FailCode("reserved_module_name",
				"refusing to save %q: the module specifier %q is reserved for a platform helper. Import it instead - `import { ... } from '%s'` - and rename this file so its basename is not %q.",
				path, name, name, name,
			), nil
		}
		// Colliding-basename gate: the SAME failure shape as the reserved gate above,
		// one file over. Two project modules sharing a basename make that bare name
		// ambiguous, so ProjectModuleMap leaves it out of the importmap and BOTH become
		// unresolvable at load. The post-build validator reports it, but only after the
		// whole app is written - here the model is holding both filenames and a rename
		// costs one call.
		if rej := rejectIfCollidingModuleName(ctx, path, exists); rej != nil {
			return rej, nil
		}
	}
	// Client-JS split gate: a client module born as one oversized blob ships broken (a
	// forgotten import in the header goes unseen hundreds of lines below), so a single save
	// crossing MaxClientModuleBody is refused, forcing an entry+leaf split or a
	// smaller-body-then-patch flow. The cap sits above the MaxSingleReadBody growth warning
	// and below MaxSingleSaveBody. Guarded on crossing (prevBody at/under the line) so it
	// never traps a corrective re-save of a file already over it.
	if kind == "file" && isJSModulePath(path) && len(body) > store.MaxClientModuleBody && len(prevBody) <= store.MaxClientModuleBody {
		return tools.FailCode("module_too_large_client",
			"refusing to save client module %q in one call - body is %d bytes, over the %s client-module cap. One oversized module written in a single shot is where forgotten imports and contract drift hide (a missing import in the header goes unseen hundreds of lines below) and ships broken. Split it into focused sibling files (a zero-import store module, an entry, one leaf per subsystem - each complete in one shot, the entry importing the leaves), or save a smaller initial body now (real structure and first sections, not stubs) and add the rest in focused follow-up edits.",
			path, len(body), store.HumanSize(store.MaxClientModuleBody),
		), nil
	}
	// The body as the model submitted it, captured BEFORE any platform mutation below
	// (the duplicate-import dedupe or the auto-wire/rewire in the import gate). Compared
	// against the stored body after the save to decide whether the model still holds every
	// stored byte (see the residence branch after ArtifactSave).
	submittedBody := body
	// Heal imports and normalize specifiers through the shared resolver (save and patch
	// treat imports identically): dedupe a duplicate import, auto-wire a forgotten one,
	// rewire a wrong-source one, collapse relative specifiers. A standing reject on a
	// create stored nothing, so steer the model to a full-body re-save.
	healedBody, autoImports, normalizedSpecs, pendingImports, rejImports := resolveWrittenImports(ctx, kind, path, prevBody, body, attachTo, exists)
	if rejImports != nil {
		return steerCreateRejectToResave(rejImports, kind, path, exists), nil
	}
	body = healedBody

	a := &store.Artifact{
		Kind:        kind,
		Path:        path,
		Storage:     storage,
		Body:        body,
		ContentType: contentType,
		Metadata:    meta,
		Scope:       scope,
		Status:      status,
	}
	id, version, err := store.ArtifactSave(ctx.DB, a, ctx.ChangedBy())
	if err != nil {
		return tools.FailErr(err), nil
	}
	data := map[string]any{
		"id":          id,
		"kind":        kind,
		"file_path":   path,
		"version":     version,
		"total_lines": lineCount(body),
	}
	addParseStatus(data, kind, path, body)
	addImportStatus(data, pendingImports)
	if len(autoImports) > 0 {
		// Surface the platform-added imports so the model knows its stored file now carries
		// wiring it did not type. The `updated` echo below shows WHERE they landed.
		data["auto_imported"] = autoImports
	}
	if len(normalizedSpecs) > 0 {
		// Surface the specifier rewrites so the model learns the bare-name style
		// from the correction instead of re-typing relative paths next save.
		data["normalized_specifiers"] = normalizedSpecs
	}
	// Read-dedup residence + patch-anchor drift safety. Normally the model authored every
	// stored byte, so mark it delivered and a reflexive get after the save stubs instead of
	// re-sending. But when the platform MUTATED the body (the auto-wire prepend; CRLF->LF
	// normalization does not count, it shifts no line) the model holds its pre-mutation
	// submission, not a.Body: marking it delivered would stub a re-get so the model never
	// sees the injected lines and its next patch anchors against stale content
	// (patch_anchor_not_found). So echo the changed region back and record only those spans
	// as resident, so any un-echoed line re-serves in full on the next get.
	if normalizeNewlines(submittedBody) == a.Body {
		ctx.ReadTracker.RecordDelivered(tools.ReadCacheKey(kind, path), tools.ReadFingerprint(a.Body))
	} else {
		spans := addEditEcho(data, normalizeNewlines(submittedBody), a.Body)
		recordEchoResidence(ctx, kind, path, a.Body, spans)
	}
	// A full save re-sends the whole body (the escalation's advice out of a patch
	// loop), so clear both failure streaks on this file.
	clearEditStreaks(ctx, kind, path)
	addGrowthWarning(data, prevBody, body)
	// Collect every applicable notice instead of letting each assignment clobber the
	// previous one (a save can trip several at once). These are notices about a functional
	// correction the save just made (a stripped wrapper, a hoisted scope).
	var hints []string
	if wrapperStripped {
		hints = append(hints, "script/style wrapper tags were stripped, artifact bodies must be pure code with no <script> or <style> wrappers (files are served as application/javascript / text/css, not HTML)")
	}
	if scopeHoisted {
		hints = append(hints, fmt.Sprintf("read scope=%q from metadata, pass `scope` as a top-level argument (not inside `metadata`) on future saves", scope))
	}
	// Near-identical full re-save: the save re-sent the whole body to change only a few
	// bytes - the case action=patch is for. Nudge toward patch so the next small edit costs
	// a search/replace anchor instead of the whole file's output tokens.
	if exists {
		if changed := resaveChurn(prevBody, body); changed > 0 && len(prevBody) >= resaveChurnMinBytes && changed*resaveChurnRatio <= len(prevBody) {
			hints = append(hints, fmt.Sprintf("this full save rewrote a %d-byte file to change only ~%d byte(s); for a localized edit use action=patch with a search/replace anchor - it costs a fraction of the output tokens of re-sending the whole body", len(prevBody), changed))
		}
	}
	if len(hints) > 0 {
		data["hint"] = strings.Join(hints, " | ")
	}
	// CSS contract advisory: markup that uses a class no stylesheet defines ships an
	// unstyled element, and nothing downstream (no JS gate, no runtime probe) can see
	// it. Surface it as a non-blocking note so the model can add the rule or reuse an
	// existing class; never a rejection.
	if note := styleContractNote(undefinedClassesUsed(ctx.DB, kind, path, body)); note != "" {
		data["style_notes"] = note
	}
	if note := revealNote(path, prevBody, body); note != "" {
		data["reveal_note"] = note
	}
	addBlindEditNote(ctx, data)
	return tools.Ok(data), nil
}

// resaveChurn thresholds: below resaveChurnMinBytes a full save is cheap enough
// not to nag; resaveChurnRatio of 20 means the hint fires only when <=5% of the
// file actually changed (changed*20 <= len(prev)).
const (
	resaveChurnMinBytes = 1500
	resaveChurnRatio    = 20
)

// resaveChurn estimates how many bytes a full re-save changed by peeling the common
// prefix and suffix off the old and new bodies: a localized edit leaves a large common
// prefix+suffix and a tiny changed middle, a genuine rewrite little in common. Cheap and
// allocation-free, which is all the resave nudge needs.
func resaveChurn(prev, next string) int {
	i := 0
	for i < len(prev) && i < len(next) && prev[i] == next[i] {
		i++
	}
	j := 0
	for j < len(prev)-i && j < len(next)-i && prev[len(prev)-1-j] == next[len(next)-1-j] {
		j++
	}
	cp := len(prev) - i - j
	cn := len(next) - i - j
	if cp > cn {
		return cp
	}
	return cn
}

// Clobber-gate thresholds. clobberShrinkRatio of 4 means the body must shrink under a
// quarter of the existing file to count as destructive; the overlap ratios bound how much
// of the incoming body may already live in the old file before it reads as "the same file,
// edited". The blind trigger (no page-intent signal) is stricter than the intent trigger,
// and only files at/over clobberMinPrevBytes are guarded.
const (
	clobberShrinkRatio   = 4
	clobberOverlapIntent = 0.5
	clobberOverlapBlind  = 0.2
	clobberMinPrevBytes  = 2000
)

// sharedLineRatio reports the fraction of next's distinct non-blank trimmed lines that
// also occur in prev - a cheap, order-insensitive vocabulary-overlap measure. 1.0 when
// next has no non-blank lines: an empty body carries no evidence of being a different
// file, so the shrink clause decides alone.
func sharedLineRatio(prev, next string) float64 {
	prevSet := map[string]bool{}
	for _, ln := range strings.Split(prev, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			prevSet[t] = true
		}
	}
	distinct, shared := 0, 0
	seen := map[string]bool{}
	for _, ln := range strings.Split(next, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		distinct++
		if prevSet[t] {
			shared++
		}
	}
	if distinct == 0 {
		return 1.0
	}
	return float64(shared) / float64(distinct)
}

// rejectIfSamePathClobber refuses a save that looks like a DIFFERENT file aimed
// at an existing scope=global file's path. See the call site comment for the
// trigger design; the returned message is this rule's single home (the gate
// teaches; frames don't pre-explain).
func rejectIfSamePathClobber(kind, path, prevBody, prevScope, body, scope, attachTo string, exists, overwrite bool) *tools.Result {
	if !exists || overwrite || kind != "file" || prevScope != "global" || body == prevBody {
		return nil
	}
	shrunk := len(body)*clobberShrinkRatio < len(prevBody)
	pageIntent := attachTo != "" || scope == "page"
	fired := false
	if pageIntent && (shrunk || sharedLineRatio(prevBody, body) < clobberOverlapIntent) {
		fired = true
	} else if len(prevBody) >= clobberMinPrevBytes && shrunk && sharedLineRatio(prevBody, body) < clobberOverlapBlind {
		fired = true
	}
	if !fired {
		return nil
	}
	intent := ""
	if attachTo != "" {
		intent = fmt.Sprintf(" carrying page intent (attach_to=%q)", attachTo)
	} else if scope == "page" {
		intent = " carrying page intent (scope=page)"
	}
	return &tools.Result{
		Success: false,
		Code:    "same_path_clobber",
		Error: fmt.Sprintf(
			"refusing to save %q: this path already holds a %d-line scope=global file (site-wide - it loads on every page), and this save would replace it with an unrelated %d-line body%s. That is a DIFFERENT file aimed at an existing name - landing it would take the global file off every page that depends on it, and nothing in this turn would report the breakage. Save the new file under its own path and attach that; the existing file keeps this one. If you genuinely mean to replace this file's entire content from scratch, re-send the same save with overwrite=true.",
			path, lineCount(prevBody), lineCount(body), intent,
		),
		Data: map[string]any{
			"existing_scope": "global",
			"existing_lines": lineCount(prevBody),
			"incoming_lines": lineCount(body),
			"attach_to":      attachTo,
		},
	}
}

// resolveWrittenImports runs the platform's import auto-corrections on a body about to be
// written and normalizes its specifiers - the SHARED home for the import-what-you-call
// contract, run identically on save AND patch. It dedupes a duplicate import, auto-wires a
// forgotten one, rewires a wrong-source one, and collapses relative specifiers to bare
// form, so the model USES a symbol and the platform owns the import line.
//
// Every heal RE-VALIDATES and is accepted only when the re-check is clean, so a mis-heal
// can never ship. Returns the possibly-mutated body, the import labels and normalized
// specifiers for the echo, the pending cross-module imports, and the standing reject (nil
// when clean or fully healed); the caller wraps that reject with its own save/patch steer.
func resolveWrittenImports(ctx *tools.ToolContext, kind, path, prevBody, body, attachTo string, exists bool) (outBody string, autoImports, normalizedSpecs []string, pendingImports []map[string]any, rej *tools.Result) {
	outBody = body
	// A redundant duplicate import parses as an "already declared" syntax error, yet
	// removing the stray copy changes nothing - collapse it and re-check; only a clean
	// result lets the write land, else the original reject stands.
	if r := rejectIfBrokeSyntax(kind, path, prevBody, outBody, exists); r != nil {
		deduped, removed, ok := tryDedupeImports(outBody)
		if !ok || rejectIfBrokeSyntax(kind, path, prevBody, deduped, exists) != nil {
			return outBody, autoImports, normalizedSpecs, nil, r
		}
		outBody, autoImports = deduped, append(autoImports, removed...)
	}
	// Collapse a relative project-module import to its bare logical form BEFORE the
	// import gate, so the gate's messages reason over the normalized body.
	if normalized, rws, ok := tryNormalizeSpecifiers(ctx, kind, path, prevBody, outBody, exists); ok {
		outBody, normalizedSpecs = normalized, rws
	}
	// Cross-module gate: a forgotten import (a bare call to a sibling-exported name) is
	// auto-wired; a wrong-source import (the symbol lives in another sibling) is rewired.
	// Each fires only when unambiguous (exactly one exporter, no cycle) and re-validates.
	rejImports, pending := rejectIfUnresolvedImports(ctx, kind, path, prevBody, outBody, attachTo)
	// A forgotten capitalized sibling class (`new Widget()`) is demoted out of the reject
	// (it may be a browser constructor), so the gate returns clean for it; the wire pass
	// still adds the import when exactly one sibling exports that name.
	if rejImports == nil && kind == "file" && isJSModulePath(path) {
		if wired, added, ok := tryAutoWireImports(ctx, path, prevBody, outBody); ok {
			if rej2, pending2 := rejectIfUnresolvedImports(ctx, kind, path, prevBody, wired, attachTo); rej2 == nil && rejectIfBrokeSyntax(kind, path, prevBody, wired, exists) == nil {
				outBody, pending = wired, pending2
				autoImports = append(autoImports, added...)
			}
		}
	}
	if rejImports != nil && rejImports.Code == "undefined_reference" {
		if wired, added, ok := tryAutoWireImports(ctx, path, prevBody, outBody); ok {
			if rej2, pending2 := rejectIfUnresolvedImports(ctx, kind, path, prevBody, wired, attachTo); rej2 == nil {
				outBody, pending, rejImports = wired, pending2, nil
				autoImports = append(autoImports, added...)
			}
		}
	}
	if rejImports != nil && rejImports.Code == "unresolved_import" {
		if rewired, moved, ok := tryRewireWrongSourceImports(ctx, path, prevBody, outBody); ok {
			if rej2, pending2 := rejectIfUnresolvedImports(ctx, kind, path, prevBody, rewired, attachTo); rej2 == nil && rejectIfBrokeSyntax(kind, path, prevBody, rewired, exists) == nil {
				outBody, pending, rejImports = rewired, pending2, nil
				autoImports = append(autoImports, moved...)
			}
		}
	}
	return outBody, autoImports, normalizedSpecs, pending, rejImports
}

// insertShapeNote teaches the insert shape on the no-op reject. Patch has no
// locate-only step, so a hunk that repeats its anchor unchanged does nothing.
const insertShapeNote = " Patch has no locate-only step: a hunk never just marks a place. To ADD text, use the insert form, which names the anchor once and the new lines once: {\"insert_after\": \"load();\", \"text\": \"setInterval(load, 5000);\"} (or insert_before, or {\"append\": ...} / {\"prepend\": ...} for the end or start of the file). To CHANGE text, send search and replace with different text."

// quotedLabels renders each patch search string as a short, quoted label for a
// failure message (e.g. `"net.on('join'..."`). Shared by the not-found and no-op
// rejections so both read identically.
func quotedLabels(searches []string) []string {
	out := make([]string, 0, len(searches))
	for _, s := range searches {
		out = append(out, fmt.Sprintf("%q", shortLabel(s)))
	}
	return out
}

// shortLabels truncates each search string for result data fields.
func shortLabels(searches []string) []string {
	out := make([]string, 0, len(searches))
	for _, s := range searches {
		out = append(out, shortLabel(s))
	}
	return out
}

// fuzzyDeletionResult builds the reject for a batch blocked by refused hunks: each
// would delete a span located only by an approximate match. The text WAS located,
// so it must never read as anchor drift. Consumed or no-op hunks in the same batch
// are named too, so one resend fixes everything.
func fuzzyDeletionResult(kind, path string, pr PatchResult, total int) *tools.Result {
	data := map[string]any{
		"kind":                kind,
		"file_path":           path,
		"matched_rolled_back": pr.Applied,
		"refused":             shortLabels(pr.Refused),
	}
	if len(pr.Warnings) > 0 {
		data["warnings"] = pr.Warnings
	}
	consumedSuffix := ""
	if len(pr.NotFound) > 0 {
		data["consumed_by_earlier_hunk"] = shortLabels(pr.NotFound)
		consumedSuffix = fmt.Sprintf(" Separately, %d anchor(s) WERE in the file but an earlier hunk in this same call rewrote that region before they ran: %s - make those hunks independent, or fold both edits into one hunk spanning the region.",
			len(pr.NotFound), strings.Join(quotedLabels(pr.NotFound), ", "))
	}
	noopSuffix := ""
	if len(pr.NoOp) > 0 {
		data["no_op"] = pr.NoOp
		noopSuffix = fmt.Sprintf(" Also, %d edit(s) had `replace` identical to `search` (change nothing): %s - re-send those with the intended new `replace` text.",
			len(pr.NoOp), strings.Join(quotedLabels(pr.NoOp), ", "))
	}
	noopSuffix += chainedSuffix(data, pr.Chained)
	return &tools.Result{
		Success: false,
		Code:    "patch_fuzzy_deletion",
		Error: fmt.Sprintf(
			"patch refused, so NO change was applied and the file is UNCHANGED: %d of %d edit(s) would DELETE a span located only by an approximate (fuzzy) match - the pattern that silently truncates a file: %s. Re-send each with an exact `search` copied verbatim from the file, or use `save` to rewrite the file.%s%s",
			len(pr.Refused), total, strings.Join(quotedLabels(pr.Refused), ", "), consumedSuffix, noopSuffix),
		Data: data,
	}
}

func artifactActionPatch(ctx *tools.ToolContext, kind, path string, patchesArg []map[string]any) (*tools.Result, error) {
	// patch targets an existing artifact, so its kind is resolvable from the path
	// (resolveKindPath) with no need to re-supply it. requireKindPath stays on save/create
	// (nothing to infer from) and restore (a just-deleted file is invisible to inference).
	argPath := path
	kind, path, errR := resolveKindPath(ctx, kind, path)
	if errR != nil {
		if errR.Code == tools.CodeNotFound {
			// Nothing exists at the path under any kind: give the modify-specific
			// steer (save creates; patch only modifies) instead of a bare not-found.
			return notFoundForModify(ctx, "", argPath), nil
		}
		return errR, nil
	}
	a, err := store.ArtifactGet(ctx.DB, kind, path)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if a == nil {
		return notFoundForModify(ctx, kind, path), nil
	}
	if pm, _ := a.Metadata["platform_managed"].(bool); pm {
		return rejectPlatformManaged(ctx, kind, path, "patch"), nil
	}
	// Accept other editors' patch dialects (old_string/new_string, find, from/to, ...) by
	// rewriting alias keys to search/replace before both the decode and the pre-flight, so a
	// mis-keyed hunk applies instead of bouncing off patch_empty_search.
	normalizePatchAliases(patchesArg)
	if err := expandInsertHunks(a.Body, patchesArg); err != nil {
		return tools.FailCode(tools.CodeBadArg, "patch rejected before any edit was attempted, so the file is UNCHANGED: %s", err.Error()), nil
	}
	var patches []SearchReplacePatch
	if err := decodeJSONArrayArg(patchesArg, "patches", &patches); err != nil {
		return tools.FailCode(tools.CodeBadArg, "%s", err.Error()), nil
	}
	// Validate the hunk ARRAY before any text is mutated. A defect visible in the
	// array itself (an empty anchor, a hunk with no replace key) would otherwise
	// surface downstream as a silent drop or a silent deletion.
	if rej := patchSetRejection(patches, patchesArg); rej != nil {
		return rej, nil
	}
	oldBody := a.Body
	newBody, pr := ApplyPatches(a.Body, patches)
	// Anchor failures are atomic: if any anchor missed or a deletion was refused, commit
	// NOTHING rather than persist a half-applied edit under success:true (a partial apply
	// leaves the file in a state the model never intended and only notices round-trips
	// later). Matched hunks are reported under `matched_rolled_back`, NOT `applied`,
	// because an atomic failure persists nothing and an `applied` field would read as
	// "these landed". A refused hunk blocks the commit like a miss, but the text WAS
	// located: report it under its own code so it never reads as anchor drift. When a
	// genuine miss is also in the batch, the anchor-not-found report below owns the result.
	// A no-op hunk (`replace` identical to `search`, anchor present) is NOT an anchor
	// failure and does not block the batch: the hunks that change bytes land and the
	// no-op ones are surfaced on the success result. Only a batch that changes nothing
	// at all is rejected (below).
	if len(pr.Refused) > 0 {
		if _, absent := splitConsumedAnchors(a.Body, pr.NotFound); len(absent) == 0 {
			forgetIfNoChange(ctx, kind, path, a.Body, a.Body)
			return escalateStuckPatch(ctx, kind, path, fuzzyDeletionResult(kind, path, pr, len(patches))), nil
		}
	}
	// A hunk that matched only text an earlier hunk in this call wrote also blocks the
	// commit. Its anchor is not drifted, so it gets its own code and steer; a genuine
	// miss in the same batch hands ownership to the anchor-not-found report below.
	if len(pr.Chained) > 0 {
		if consumed, absent := splitConsumedAnchors(a.Body, pr.NotFound); len(absent) == 0 {
			forgetIfNoChange(ctx, kind, path, a.Body, a.Body)
			return escalateStuckPatch(ctx, kind, path, chainedHunkResult(kind, path, pr, consumed, len(patches))), nil
		}
	}
	// A hunk with replace identical to search whose text is not in the file changes
	// nothing wherever it lands, so it is never sent to the re-read steer below. It
	// blocks the commit (its intent is unknown) and counts on the no-change streak.
	if len(pr.Unanchored) > 0 {
		if _, absent := splitConsumedAnchors(a.Body, pr.NotFound); len(absent) == 0 {
			forgetIfNoChange(ctx, kind, path, a.Body, a.Body)
			dir, consec, cum := noopStopDirective(ctx, kind, path)
			res := unanchoredIdenticalResult(a.Body, pr, len(patches), dir, consec, cum)
			if data, ok := res.Data.(map[string]any); ok {
				data["kind"], data["file_path"] = kind, path
			}
			return res, nil
		}
	}
	if len(pr.NotFound) > 0 {
		// An unmatched anchor that IS still in the original body was correct when sent:
		// an earlier hunk in THIS call rewrote that region. That needs the opposite fix
		// from anchor drift, so it never gets the re-read steer below.
		consumed, absent := splitConsumedAnchors(a.Body, pr.NotFound)
		if len(absent) == 0 {
			forgetIfNoChange(ctx, kind, path, a.Body, a.Body)
			return escalateStuckPatch(ctx, kind, path, consumedAnchorResult(kind, path, consumed, pr.Applied, len(patches))), nil
		}
		data := map[string]any{
			"kind":                kind,
			"file_path":           path,
			"matched_rolled_back": pr.Applied,
			"not_found":           shortLabels(absent),
		}
		if len(pr.Warnings) > 0 {
			data["warnings"] = pr.Warnings
		}
		// A mixed batch: name the consumed bucket too, so both defects are fixed in one
		// resend instead of tripping the consumed path on the next turn.
		consumedSuffix := ""
		if len(consumed) > 0 {
			data["consumed_by_earlier_hunk"] = shortLabels(consumed)
			consumedSuffix = fmt.Sprintf(" Separately, %d anchor(s) WERE in the file but an earlier hunk in this same call rewrote that region before they ran: %s - do not re-anchor those, make the hunks independent (one hunk per region, or fold both edits into one hunk spanning it).",
				len(consumed), strings.Join(quotedLabels(consumed), ", "))
		}
		// Hand back the current lines nearest each unmatched anchor so the model
		// re-anchors from the real file in this same step instead of a separate get.
		// Only for genuinely absent anchors: a consumed one is still in the original, so
		// this would hand back the anchor the model just sent and loop the retry.
		hintSuffix := ""
		if regions := nearestRegions(a.Body, absent); len(regions) > 0 {
			data["nearest_current_regions"] = regions
			hintSuffix = " The current lines nearest each unmatched anchor are in `nearest_current_regions` - copy an anchor verbatim from there instead of re-reading the file."
		}
		// An absent anchor that matches the file's PREVIOUS stored version is the
		// most common real cause: the model is quoting bytes its own earlier edit
		// replaced. Naming that turns a puzzling miss into an instant correction.
		staleSuffix := ""
		if prev := store.ArtifactPreviousBody(ctx.DB, kind, path); prev != "" {
			stale := 0
			for _, s := range absent {
				if anchorLocatable(prev, s) {
					stale++
				}
			}
			if stale > 0 {
				data["anchors_match_previous_version"] = stale
				staleSuffix = fmt.Sprintf(" %d of the unmatched anchor(s) DO match the PREVIOUS version of this file - you are quoting bytes an earlier edit already changed; re-anchor on the current content, not on what you last wrote.", stale)
			}
		}
		// A no-op edit in the SAME batch also blocked the commit; name it here so the
		// model fixes both in one resend instead of tripping the no-op path next turn.
		noopSuffix := ""
		if len(pr.NoOp) > 0 {
			data["no_op"] = pr.NoOp
			noopSuffix = fmt.Sprintf(" Also, %d edit(s) had `replace` identical to `search` (change nothing): %s - re-send those with the intended new `replace` text.",
				len(pr.NoOp), strings.Join(quotedLabels(pr.NoOp), ", "))
		}
		// A refused deletion in the SAME batch also blocked the commit; the text for
		// those WAS located, so they are named apart from the misses.
		refusedSuffix := ""
		if len(pr.Refused) > 0 {
			data["refused"] = shortLabels(pr.Refused)
			refusedSuffix = fmt.Sprintf(" Also, %d edit(s) were refused: each would DELETE a span located only by an approximate (fuzzy) match - re-send those with an exact `search` copied verbatim from the file: %s.",
				len(pr.Refused), strings.Join(quotedLabels(pr.Refused), ", "))
		}
		refusedSuffix += chainedSuffix(data, pr.Chained) + unanchoredSuffix(data, pr.Unanchored) + alreadyAppliedSuffix(data, pr.AlreadyApplied)
		// Name the cause when it is one nearest-line matching cannot express: the edit
		// already landed, the text lives in another file, or the anchor skipped lines.
		diag, diagSuffix := missDiagnosis(ctx, kind, path, a.Body, absent, patches)
		for k, v := range diag {
			data[k] = v
		}
		hintSuffix = diagSuffix + hintSuffix
		labels := quotedLabels(absent)
		total := len(patches)
		// Nothing was persisted, so the body the model holds is still current - but
		// forget its read residence so the follow-up get re-serves in full and the
		// model can copy a correct anchor, instead of being stubbed as "unchanged".
		forgetIfNoChange(ctx, kind, path, a.Body, a.Body)
		// Circuit-breaker: a second consecutive miss on this file means the model is
		// patching a stale copy it can no longer see (a stuck edit/repair loop).
		// The base message already offers re-save as a fallback; after the second miss
		// make it the decisive directive so the loop stops instead of re-patching.
		loopSuffix := ""
		if fails := recordAnchorMiss(ctx, kind, path); fails >= 2 {
			data["edit_loop"] = fails
			loopSuffix = fmt.Sprintf(" STOP: you have now missed a patch anchor on %s %d times in a row - that is the signal you are patching a stale copy. Do NOT patch this file again; re-`save` the whole corrected body instead (a full save carries your own complete text and cannot drift), rebuilding it from `nearest_current_regions` above if you no longer hold it.", path, fails)
		}
		// A genuine miss has a kind-specific cause: only a JS module is rewritten by import
		// auto-wire; a page/layout/component is stored verbatim, so a miss is a stale draft.
		driftCause := "you are patching a draft or an out-of-date copy instead of the file's current bytes, or you hold it only in ranges"
		if kind == "file" && isJSModulePath(path) {
			driftCause = "the file was auto-wired, you hold it only in ranges, or you are patching an out-of-date copy"
		}
		return &tools.Result{
			Success: false,
			Code:    "patch_anchor_not_found",
			Error: fmt.Sprintf(
				"%d of %d patch anchors were not found, so NO change was applied and the file is UNCHANGED (patch is atomic - it never half-edits a file). Any anchors that DID match are in `matched_rolled_back` - they were undone, not saved, so do not assume any hunk landed or patch against a half-edited file. Unmatched search text: %s. That text is not in the current file (an anchor copied from your own draft can miss when %s). To fix: `get` the exact region and copy the anchor verbatim into `search`, then patch again; or, if you do not hold the file's current bytes, re-save the whole corrected body with `save` (it carries your own complete text and cannot drift).%s%s%s%s%s%s",
				len(absent), total, strings.Join(labels, ", "), driftCause, hintSuffix, staleSuffix, consumedSuffix, noopSuffix, refusedSuffix, loopSuffix),
			Data: data,
		}, nil
	}
	// Idempotent: no genuine miss, and the batch changed nothing, but at least one hunk
	// was ALREADY APPLIED (its `replace` end state is already in the file). The model is
	// re-sending an edit a previous call landed - a build-stability trap, because treating
	// it as a miss would roll the whole atomic batch back and bounce it into a retry loop.
	// The intended state exists, so this is a success: report it, clear the streaks, and
	// let the model move on. Placed before the no-op reject so a resent landed edit never
	// reads as "no-op loop".
	if newBody == oldBody && len(pr.AlreadyApplied) > 0 {
		forgetIfNoChange(ctx, kind, path, a.Body, a.Body)
		clearEditStreaks(ctx, kind, path)
		data := map[string]any{
			"kind":            kind,
			"file_path":       path,
			"already_applied": shortLabels(pr.AlreadyApplied),
			"note": fmt.Sprintf(
				"no change needed: %d edit(s) are ALREADY APPLIED - the file already holds their `replace` text, so a previous call landed them. Nothing was left to change. These edits are done; move on.",
				len(pr.AlreadyApplied)),
		}
		if len(pr.NoOp) > 0 {
			data["no_op"] = shortLabels(pr.NoOp)
		}
		if len(pr.Warnings) > 0 {
			data["warnings"] = pr.Warnings
		}
		return tools.Ok(data), nil
	}
	// No genuine miss, but the batch changed nothing: every edit either had `replace`
	// identical to `search` (NoOp) or applied without altering a byte (a hunk differing
	// only in whitespace the tolerant matcher normalized away). This is NOT an anchor
	// problem: the anchors are correct, so the model decides per edit whether the file
	// already holds the outcome (drop the hunk) or a new `replace` is needed - never
	// re-read or re-save. Reported under its own code so it never reads as "anchor not
	// found" or counts against the anchor-miss guardrail; it carries its own streak
	// instead (see noopStreakKey). This is also the only no-op shape that rejects: a
	// batch whose other hunks DO change bytes falls through to the save below.
	if newBody == oldBody {
		forgetIfNoChange(ctx, kind, path, a.Body, a.Body)
		noops := append(append([]string{}, pr.NoOp...), pr.Applied...)
		data := map[string]any{
			"kind":      kind,
			"file_path": path,
			"no_op":     noops,
		}
		if len(pr.Warnings) > 0 {
			data["warnings"] = pr.Warnings
		}
		msg := fmt.Sprintf(
			"patch made NO change and the file is UNCHANGED: %d edit(s) left the body byte-identical (%s) - each `replace` is the text the file already holds at its anchor. The anchors ARE in the file, so do NOT re-read the file and do NOT re-save the whole body. Decide per listed edit: if the file already holds the outcome you intended there, the edit is DONE - drop it and move on; otherwise re-send JUST that hunk with the intended NEW text in `replace` (it must differ from what the file holds).",
			len(noops), strings.Join(quotedLabels(noops), ", "))
		if len(pr.NoOp) > 0 {
			// A hunk sent with `replace` identical to `search` is usually an attempt to
			// mark an anchor before an insert. Name the insert shape directly.
			msg += insertShapeNote
		}
		// Circuit-breaker: the model is re-sending an edit whose outcome the file
		// already carries, either twice in a row or many times across the build.
		if dir, consec, cum := noopStopDirective(ctx, kind, path); dir != "" {
			data["noop_loop"] = consec
			if cum >= noopCumulativeStop {
				data["noop_total"] = cum
			}
			msg += dir
		}
		return &tools.Result{
			Success: false,
			Code:    "patch_noop_replace",
			Error:   msg,
			Data:    data,
		}, nil
	}
	// Heal imports exactly as save does (the same resolver): a patch introducing a call to
	// a sibling-exported symbol gets the import AUTO-WIRED, so the model never hand-patches
	// a fragile import line. Every heal re-validates, and a standing reject bounces to a
	// full re-save. attachTo is empty (patch never attaches); exists is always true.
	healedBody, autoImports, normalizedSpecs, pendingImports, rejImports := resolveWrittenImports(ctx, kind, path, a.Body, newBody, "", true)
	if rejImports != nil {
		return steerEditRejectToResave(rejImports), nil
	}
	newBody = healedBody
	a.Body = newBody
	a.Size = len(newBody)
	_, version, err := store.ArtifactSave(ctx.DB, a, ctx.ChangedBy())
	if err != nil {
		return tools.FailErr(err), nil
	}
	data := map[string]any{
		"kind":      kind,
		"file_path": path,
		"applied":   pr.Applied,
		"version":   version,
	}
	if len(pr.Warnings) > 0 {
		data["warnings"] = pr.Warnings
	}
	if len(pr.NoOp) > 0 {
		// No-op hunks in a batch whose other hunks landed: surfaced, never fatal.
		// Blocking the whole batch here is what caused cascades - the model retried
		// assuming the real hunks had landed.
		data["no_op"] = shortLabels(pr.NoOp)
		data["no_op_note"] = fmt.Sprintf(
			"%d edit(s) changed nothing (`replace` identical to `search`): %s. The OTHER edits landed (see `applied`). For each listed no-op: if the file already holds what you intended there, it is DONE; if you meant a change, send one follow-up patch for JUST that hunk with the NEW text in `replace`. A hunk never just marks a place, so leave identical hunks out of future patches.",
			len(pr.NoOp), strings.Join(quotedLabels(pr.NoOp), ", "))
	}
	if len(pr.AlreadyApplied) > 0 {
		// Some hunks were already applied by an earlier call; the OTHER hunks landed here.
		// Surface them so the model does not re-send the landed ones expecting a change.
		data["already_applied"] = shortLabels(pr.AlreadyApplied)
	}
	if len(normalizedSpecs) > 0 {
		data["normalized_specifiers"] = normalizedSpecs
	}
	if len(autoImports) > 0 {
		// The patch introduced a call whose import the platform wired in - surface it so
		// the model knows the stored file now carries an import it did not type; the edit
		// echo below shows WHERE for any next edit.
		data["auto_imported"] = autoImports
	}
	addParseStatus(data, kind, path, newBody)
	if note := revealNote(path, oldBody, newBody); note != "" {
		data["reveal_note"] = note
	}
	addImportStatus(data, pendingImports)
	spans := addEditEcho(data, oldBody, newBody)
	// Carry what the model held before the patch over to the edited body (before the
	// echo is recorded, which starts the new fingerprint's state), then the echo.
	carryResidence(ctx, kind, path, oldBody, newBody, spans)
	recordEchoResidence(ctx, kind, path, newBody, spans)
	addGrowthWarning(data, oldBody, newBody)
	forgetIfNoChange(ctx, kind, path, oldBody, newBody)
	// A clean patch means the anchors matched: the model holds a current copy, so
	// reset both failure streaks on this file.
	clearEditStreaks(ctx, kind, path)
	addBlindEditNote(ctx, data)
	return tools.Ok(data), nil
}

// recordEchoResidence marks the line spans shown in the post-edit echo as resident in the
// model's context, at the new body's fingerprint. A re-read of an echoed window is then
// stubbed, while a get of any OTHER range re-serves in full instead of being wrongly
// stubbed as "unchanged". Residence is claimed only over bytes the echo actually delivered
// (truncated spans are not passed in), so it never covers unseen bytes.
func recordEchoResidence(ctx *tools.ToolContext, kind, path, newBody string, echoedSpans [][2]int) {
	if len(echoedSpans) == 0 {
		return
	}
	key := tools.ReadCacheKey(kind, path)
	fingerprint := tools.ReadFingerprint(newBody)
	for _, sp := range echoedSpans {
		ctx.ReadTracker.RecordDeliveredRange(key, fingerprint, sp[0], sp[1])
	}
}

// forgetIfNoChange drops the read-dedup residence for an edit that changed nothing (a
// patch/regex whose anchor matched no text). The body is byte-identical, so a follow-up
// get would otherwise be stubbed as "already shown" - exactly when the model needs the
// bytes back to see why its anchor missed. Forgetting forces the next get to re-serve in
// full. A real edit shifts the fingerprint and invalidates the dedup on its own.
func forgetIfNoChange(ctx *tools.ToolContext, kind, path, oldBody, newBody string) {
	if newBody == oldBody {
		ctx.ReadTracker.Forget(tools.ReadCacheKey(kind, path))
	}
}

// addGrowthWarning flags an edit that pushes a file past the single-read-page budget
// (store.MaxSingleReadBody), past which every later read paginates and the file drifts
// toward a monolith the build cannot edit coherently. Advisory (the save ceiling at 2x
// still hard-refuses an oversized write); it nudges a split into sibling modules and fires
// only on the edit that crosses the line, so it is not repeated on every subsequent edit.
func addGrowthWarning(data map[string]any, oldBody, newBody string) {
	budget := store.MaxSingleReadBody
	if len(newBody) <= budget || len(oldBody) > budget {
		return
	}
	msg := fmt.Sprintf("this edit grew the file past %d bytes (now %d), so it no longer fits a single read page and must paginate on every future read. If it is a client module, this is the point to split it into focused sibling files (entry + leaves, each complete in one shot) rather than letting one file keep growing - a too-large module is what the build loop ends up re-reading and editing incoherently.", budget, len(newBody))
	if existing, ok := data["warnings"].([]string); ok {
		data["warnings"] = append(existing, msg)
	} else {
		data["warnings"] = []string{msg}
	}
}

// addEditEcho attaches the post-edit changed region (with current line numbers) to a
// mutating action's result so the model can keep editing without a follow-up `get`. No-op
// when the body is unchanged. The echo is deliberately small (2 context lines, 2000-byte
// cap): it is full-price new tokens on every edit and its job is anchor-copying, not a
// general file view. Returns the [from,to] line spans it rendered in full, so the caller
// records exactly those as resident.
func addEditEcho(data map[string]any, oldBody, newBody string) [][2]int {
	echo, truncated, spans := echoChangedRegion(newBody, changedLineRanges(oldBody, newBody), 2, 2000)
	if echo == "" {
		return nil
	}
	note := "Region after your edit, with current line numbers. Use these for any further edits; no need to re-read the file."
	if truncated {
		// Large edit: the echo shows only the first hunks, so the model is blind to the
		// rest of what changed. Patching an unshown region from its stale draft is the
		// main source of patch_anchor_not_found on big files - steer it to get first.
		note += " Large change: only the first hunks are shown, and other regions of this file changed too. Before you patch a part NOT shown here, `get` that region first so your anchor matches the current bytes, not a stale draft."
	}
	data["updated"] = echo
	data["updated_note"] = note
	return spans
}

// resolveSaveScope returns the effective artifact scope and whether it was recovered from
// metadata. An explicit top-level `scope` arg wins; otherwise a `scope` nested in
// `metadata` is hoisted (a common model mistake), and either way the stray key is deleted
// from meta so it never reaches stored metadata.
//
// When neither source supplies a scope the result is "" (unspecified), NOT "global":
// store.ArtifactSave owns defaulting, where it inherits a prior save's scope and infers
// page-vs-global from kind/path. Defaulting to "global" here would re-create the trap where
// page-scope JS/CSS silently saves as global.
func resolveSaveScope(argScope string, meta map[string]any) (scope string, hoisted bool) {
	explicit := argScope != ""
	scope = argScope
	if meta == nil {
		return scope, false
	}
	ms, ok := meta["scope"].(string)
	if !ok {
		return scope, false
	}
	delete(meta, "scope")
	// Only the two real scopes hoist: anything else would be stored verbatim and
	// match neither the global injector nor a page link.
	if ms = strings.TrimSpace(ms); !explicit && (ms == "page" || ms == "global") {
		return ms, true
	}
	return scope, false
}

// artifactActionBulkReplace runs one regex replacement across every INLINE-stored artifact
// matching the kind / path_prefix / path_glob filters (a disk-backed artifact is never
// scanned, so files_scanned is the inline set). At least one of the three filters is
// required, AND-combined when several are given: an unscoped bulk rewrite of every artifact
// in the project is too dangerous to expose. preview=true reports per-file replacement
// counts without writing.
func artifactActionBulkReplace(ctx *tools.ToolContext, act *ArtBulkReplace) (*tools.Result, error) {
	pattern, replace, kindFilter := act.Pattern, act.Replace, act.Kind
	pathPrefix, pathGlob, count, preview := act.PathPrefix, act.PathGlob, act.Count, act.Preview
	if pattern == "" {
		return tools.Fail("pattern is required (Go-syntax regex)"), nil
	}
	if pathPrefix == "" && pathGlob == "" && kindFilter == "" {
		return tools.Fail("scope filter required: set kind, path_prefix, or path_glob to bound the rewrite"), nil
	}
	if pathGlob != "" {
		if _, err := matchPathGlob(pathGlob, ""); err != nil {
			return tools.Fail("invalid path_glob: %v", err), nil
		}
	}

	// Compile up front so a bad pattern is reported even when no artifact matches.
	if _, err := regexp.Compile(pattern); err != nil {
		return tools.FailCode(tools.CodeBadArg, "invalid regex pattern %q: %v", pattern, err), nil
	}
	repl := []RegexReplacement{{Pattern: pattern, Replace: replace, Count: count}}

	// The flow script is owned by manage_testing (kind "test"): never rewritten here.
	q := `SELECT kind, path FROM of_files WHERE is_deleted = 0 AND storage = 'inline' AND kind <> ?`
	qArgs := []any{probe.FlowKind}
	if kindFilter != "" {
		q += ` AND kind = ?`
		qArgs = append(qArgs, kindFilter)
	}
	if pathPrefix != "" {
		// Stored file paths are bare ("js/app.js"), so the /files/ URL form must be
		// canonicalized before it can match.
		_, pathPrefix = canonicalizeFilePath(kindFilter, pathPrefix)
		// LIKE treats _ and % as wildcards (and ignores ASCII case), so escape the
		// prefix and re-check it exactly below.
		q += ` AND path LIKE ? ESCAPE '\'`
		qArgs = append(qArgs, likeEscape(pathPrefix)+"%")
	}
	q += ` ORDER BY kind, path`
	rows, err := ctx.DB.Query(q, qArgs...)
	if err != nil {
		return tools.Fail("scanning artifacts: %v", err), nil
	}
	type candidate struct{ kind, path string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.kind, &c.path); err != nil {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(c.path, pathPrefix) {
			continue
		}
		if pathGlob != "" {
			ok, _ := matchPathGlob(pathGlob, c.path)
			if !ok {
				continue
			}
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return tools.Fail("scanning artifacts: %v", err), nil
	}

	type fileResult struct {
		Kind    string `json:"kind"`
		Path    string `json:"file_path"`
		Applied int    `json:"applied"`
		Version int    `json:"version,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	var changed []fileResult
	// Files the rewrite would have broken: left untouched and reported so one bad
	// match never corrupts a whole batch. Populated in both preview and write
	// mode so the model sees the breakage before committing.
	var skipped []fileResult
	totalApplied := 0
	for _, c := range candidates {
		a, err := store.ArtifactGet(ctx.DB, c.kind, c.path)
		if err != nil || a == nil {
			continue
		}
		newBody, applied, err := ApplyRegexReplacements(a.Body, repl)
		if err != nil {
			return tools.Fail("regex error on %s %s: %v", c.kind, c.path, err), nil
		}
		if applied == 0 {
			continue
		}
		// Platform-owned helpers are never rewritten; report them like any other
		// file the batch could not touch.
		if pm, _ := a.Metadata["platform_managed"].(bool); pm {
			skipped = append(skipped, fileResult{Kind: c.kind, Path: c.path, Error: "platform-managed file, not editable"})
			continue
		}
		// Syntax-gated per file. The cross-module import gate (run by save/patch) is NOT
		// applied here: bulk_replace is coordinated multi-file editing, and the import gate
		// checks each file against siblings' currently-stored bodies - mid-batch those are
		// not yet updated, so a legitimate cross-file rename would false-reject. The
		// post-build module_imports validator still catches any net breakage.
		if rej := rejectIfBrokeSyntax(c.kind, c.path, a.Body, newBody, true); rej != nil {
			skipped = append(skipped, fileResult{Kind: c.kind, Path: c.path, Error: rej.Error})
			continue
		}
		fr := fileResult{Kind: c.kind, Path: c.path, Applied: applied}
		if !preview {
			a.Body = newBody
			a.Size = len(newBody)
			_, version, saveErr := store.ArtifactSave(ctx.DB, a, ctx.ChangedBy())
			if saveErr != nil {
				// The rewrite did NOT persist. Report it in the skipped bucket instead of
				// counting it among the changed files - otherwise files_changed/total_applied
				// claim a change that never hit the DB and the model won't know to retry.
				skipped = append(skipped, fileResult{Kind: c.kind, Path: c.path, Error: saveErr.Error()})
				continue
			}
			fr.Version = version
		}
		changed = append(changed, fr)
		totalApplied += applied
	}

	data := map[string]any{
		"preview":       preview,
		"files_changed": len(changed),
		"total_applied": totalApplied,
		"files_scanned": len(candidates),
		"changes":       changed,
	}
	if len(skipped) > 0 {
		data["files_skipped"] = len(skipped)
		data["skipped"] = skipped
	}
	return tools.Ok(data), nil
}
