package engine

// The spec audit: one LLM call after the build that reads the brief, the checklist
// and the code, and says for each checklist item whether the code delivers it. The
// validators and the explore probe test that what was built works; nothing else
// asks whether what was built is what was asked for. A task is completed when its
// files exist, so a page that renders but skipped half its "done when" passes every
// other check. Each gap the audit names becomes a repair item, and after a repair
// round only the items it flagged are audited again.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"openfabrica/engine/snapshotbuild"
	"openfabrica/internal/endpoints"
	"openfabrica/internal/jsonrepair"
	"openfabrica/internal/stringutil"
	"openfabrica/providers/llm"
	"openfabrica/store"
)

const (
	// StageReview is the stage the audit's call is recorded under. Its replies are
	// working JSON, never chat (see sessionEvents).
	StageReview = "review"
	// specAuditCodeBudget bounds the source shown to the auditor (~60K tokens, inside a
	// 128K window with the brief and checklist). A project fits whole; past it, the
	// largest files are cut first.
	specAuditCodeBudget = 240_000
	// specAuditFileCap and specAuditStyleCap bound one file, and one stylesheet,
	// which rarely decides whether a behaviour exists.
	specAuditFileCap  = 60_000
	specAuditStyleCap = 20_000
	// maxSpecGapsPerItem keeps one item from crowding the repair round.
	maxSpecGapsPerItem = 3
	// specGapCategory is the finding category the audit reports under.
	specGapCategory = "spec_gap"
)

const specAuditSystem = `You review a finished web project against what it was supposed to do. You get the brief, the build checklist (each item's intent and "done when"), the project's state (pages, tables, endpoints, modules) and its source code.

For each checklist item decide whether the code actually delivers it:
- met: every behaviour its intent and "done when" name is implemented and reachable by a user (from a page control, or from the API for a headless item).
- partial: some named behaviour is missing, stubbed, faked with canned data, or wired to nothing a user can reach.
- missing: the item's core behaviour is absent.

Behaviour the platform serves from configuration (the sources marked "platform configuration": page head tags, robots.txt, the live sitemap, the schema's constraints and indexes, endpoint routes) is delivered when it is listed there; never ask for it to be hand-written. A page calls an endpoint at the route listed under "endpoint routes", never at the bare endpoint path.

Judge only from the code you are shown, and look before you judge: a behaviour is implemented when you can point at the code that does it and the control or route that reaches it. Report a gap only when you can name the specific missing behaviour and the file or page it belongs in. Never report styling preferences, refactors, missing tests, or anything the brief and the item never asked for.

Some items carry the builder's own claim of where each clause is implemented. Check each claim in the code; it is a pointer, never proof.

On a re-check after a repair, an item lists the gaps the last audit named. Set new_gaps true only when a gap you name now is a different problem from every one of those (a defect the repair introduced, or a missing behaviour nobody named), never for the same gap reworded or a part of it still open; otherwise leave it false.

Also check the brief's features: when a "Brief features" list is given, each feature's behaviours must be delivered by the items that build it, and a gap there belongs to those items; otherwise, a feature the brief names that no item delivers is a gap on the item it is closest to. A feature marked OUT OF SCOPE is not a gap.

Each gap is one sentence a developer can act on: what is missing, and where. At most 3 gaps per item, the most important first. Every item that is partial or missing MUST carry at least one gap, and every item carries evidence (the file and function that implement it, or what is absent): a verdict without them cannot be acted on and is discarded. Also look across pages: the same header, navigation, headings, spacing and component classes should come from shared styles; report a page that restyles them with its own inline or local rules as a gap on that page's item. Reply with the JSON object only.`

// specAuditSchema is the structured reply: one verdict per reviewed item.
func specAuditSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Name:   "spec_audit",
		Strict: false,
		Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["items"],
  "properties": {
    "items": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "verdict", "evidence", "gaps"],
        "properties": {
          "id": {"type": "string", "description": "The checklist item id, exactly as given."},
          "verdict": {"type": "string", "enum": ["met", "partial", "missing"]},
          "evidence": {"type": "string", "description": "Where the item is implemented (file and function), or what is absent."},
          "gaps": {"type": "array", "items": {"type": "string"}, "description": "Empty when met."},
          "new_gaps": {"type": "boolean", "description": "Re-check only: a gap here is a different problem from every gap the last audit named."}
        }
      }
    }
  }
}`),
	}
}

// specVerdict is one item of the audit's reply.
type specVerdict struct {
	ID       string   `json:"id"`
	Verdict  string   `json:"verdict"`
	Evidence string   `json:"evidence"`
	Gaps     []string `json:"gaps"`
	// NewGaps is the re-check's own judgement that a gap is a different problem from
	// the ones the last audit named. Left out, the gaps count as the same.
	NewGaps bool `json:"new_gaps"`
}

// reviewThinkingDefault is the spec audit's reasoning default (the review stage).
var reviewThinkingDefault = thinkingOff

// parseSpecAudit reads the reply, tolerating a fenced or prose-wrapped object from a
// provider without structured output.
func parseSpecAudit(content string) ([]specVerdict, error) {
	s := jsonrepair.Object(content)
	var reply struct {
		Items []specVerdict `json:"items"`
	}
	if err := json.Unmarshal([]byte(s), &reply); err != nil {
		return nil, fmt.Errorf("spec audit reply is not JSON: %w", err)
	}
	return reply.Items, nil
}

// specGapIssues turns the verdicts into findings, one per item that is not met. An
// item whose verdict names no gap gets a synthesized one. Ids the checklist does not
// have are dropped: the auditor may only judge what it was asked about.
//
// A finding is keyed spec_gap|<item>. A gap the re-check calls new gets the audit's
// sequence number appended (spec_gap|<item>|<seq>), so the repair loop sees a
// different finding and gives it a round, while the same gap reworded keeps its key
// and a stalled item still ends the loop.
func specGapIssues(verdicts []specVerdict, tasks map[string]Task, seq int) []Issue {
	var out []Issue
	for _, v := range verdicts {
		t, ok := tasks[v.ID]
		if !ok || v.Verdict == "met" {
			continue
		}
		var gaps []string
		for _, g := range v.Gaps {
			if g = strings.TrimSpace(g); g != "" {
				gaps = append(gaps, g)
			}
		}
		if len(gaps) == 0 {
			gaps = []string{synthesizedGap(t, v)}
		}
		if len(gaps) > maxSpecGapsPerItem {
			gaps = gaps[:maxSpecGapsPerItem]
		}
		is := Issue{
			Severity:      store.SeverityError,
			Category:      specGapCategory,
			Deterministic: true,
			Signature:     specGapCategory + "|" + t.ID,
			Message: fmt.Sprintf("checklist item `%s` (%s) is %s against its intent and done-when:\n  - %s",
				t.ID, t.Title, v.Verdict, strings.Join(gaps, "\n  - ")),
		}
		if v.NewGaps {
			is.Signature += fmt.Sprintf("|%d", seq)
		}
		for _, p := range t.Paths {
			if isRootedNonAsset(p) {
				is.Page = p
				break
			}
		}
		if is.Page == "" && len(t.Paths) > 0 {
			is.File = t.Paths[0]
		}
		out = append(out, is)
	}
	return out
}

// synthesizedGap stands in for the gap a model left out of a non-met verdict. The
// verdict itself says the item falls short, so the repair is pointed at the item's own
// done-when (or intent) and the auditor's evidence, and the builder verifies it by reading
// the code instead of the finding being dropped.
func synthesizedGap(t Task, v specVerdict) string {
	want := strings.TrimSpace(t.DoneWhen)
	if want == "" {
		want = strings.TrimSpace(t.Intent)
	}
	if want == "" {
		want = t.Title
	}
	s := fmt.Sprintf("the auditor judged this item %s but named no specific gap: re-read the code against \"%s\" and implement whatever of it is absent or unreachable", v.Verdict, want)
	if ev := strings.TrimSpace(v.Evidence); ev != "" {
		s += " (auditor's evidence: " + ev + ")"
	}
	return s
}

// specGapTaskID returns the checklist id a spec-gap signature names.
func specGapTaskID(sig string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(sig, specGapCategory+"|"), "|")
	return id
}

// specGapTaskIDs returns the checklist ids behind spec-gap findings, for the
// narrowed re-audit after a repair round.
func specGapTaskIDs(issues []Issue) []string {
	var ids []string
	for _, is := range issues {
		if is.Category == specGapCategory {
			ids = append(ids, specGapTaskID(is.Signature))
		}
	}
	return ids
}

// specGapsNamed maps each checklist id to the gaps a spec-gap finding named, the
// lines after its heading, so the re-check can tell a new gap from an old one.
func specGapsNamed(issues []Issue) map[string]string {
	out := map[string]string{}
	for _, is := range issues {
		if is.Category != specGapCategory {
			continue
		}
		if _, gaps, ok := strings.Cut(is.Message, ":\n"); ok {
			out[specGapTaskID(is.Signature)] = strings.TrimSpace(gaps)
		}
	}
	return out
}

// runSpecAudit audits every plan item when prior is nil; after a repair round, prior
// holds the previous audit's spec-gap findings, and only their items are audited
// again, each shown with the gaps it was flagged for. The gaps come back as findings.
// Any failure (no provider, a failed call, an unreadable reply) yields no findings:
// the audit adds a check, it never blocks a build. ok is false for such a failure,
// so a caller re-auditing after a repair can tell "the gaps are fixed" from "the
// audit could not run".
func (w *Worker) runSpecAudit(ctx context.Context, prior []Issue) (issues []Issue, ok bool) {
	var only []string
	var named map[string]string
	if prior != nil {
		only, named = specGapTaskIDs(prior), specGapsNamed(prior)
		if len(only) == 0 {
			return nil, true
		}
	}
	w.specAudits++
	all, err := LoadTasks(w.pdb.Reader())
	if err != nil {
		return nil, false
	}
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	byID := map[string]Task{}
	var review []Task
	for _, t := range all {
		if t.Origin == TaskOriginRepair || t.Status == TaskBlocked {
			continue // repair items are the audit's own output; a blocked item is shipped without, knowingly
		}
		if stringutil.TaskSlug(t.ID) == userFlowsTaskID {
			continue // the flow replay in the validator sweep is its verdict
		}
		if only != nil && !want[t.ID] {
			continue
		}
		byID[t.ID] = t
		review = append(review, t)
	}
	if len(review) == 0 {
		return nil, true
	}

	snapText := ""
	if snap, serr := snapshotbuild.BuildProjectSnapshot(ctx, w.pdb.Reader(), nil); serr == nil && snap != nil {
		snapText = snap.Render()
	}
	var responses map[string]string
	if only != nil {
		responses = w.specGapResponses() // a re-audit after a repair round
	}
	user := specAuditRequest(w.buildBrief(), w.changeRequest(), renderFeaturesForBuild(w.loadPlanOrEmpty(w.run)), review, named, responses, snapText, specAuditSource(w.pdb.Reader(), w.customRobots(), specAuditCodeBudget))

	spec := SessionSpec{
		Stage:         StageReview,
		System:        specAuditSystem,
		Seed:          []llm.Message{{Role: llm.RoleUser, Content: user}},
		MaxIterations: 1,
		MaxTokens:     16384,
		Timeout:       5 * time.Minute,
		// The review stage's default (off); a model's config may turn it on.
		Thinking:      &reviewThinkingDefault,
		ThinkingStage: StageReview,
		Policy:        policyPlan,
		// A re-audit exists to judge the code again after a repair; a replayed
		// verdict would only repeat the one the repair was meant to answer.
		NoReplayCache: true,
	}
	sess := w.newStageSession(StageReview)
	if rp, rerr := sess.resolver.resolve(); rerr == nil && supportsStructuredOutput(rp) {
		spec.ResponseSchema = specAuditSchema()
	}
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		w.logger.Warn("spec audit call failed, skipping it", "error", err)
		return nil, false
	}
	verdicts, err := parseSpecAudit(res.Content)
	if err != nil {
		w.logger.Warn("spec audit reply unreadable, skipping it", "error", err)
		return nil, false
	}
	issues = specGapIssues(verdicts, byID, w.specAudits)
	w.logger.Info("spec audit", "reviewed", len(review), "gaps", len(issues))
	if w.run != nil {
		w.runlog.entry(w.run.ID, RunLogIssue, "spec_audit", map[string]any{"verdicts": verdicts}, res.InputTokens, res.OutputTokens, 0)
	}
	return issues, true
}

// customRobots is the robots.txt manage_seo set_robots stored, empty when none is
// set (the platform then serves its default).
func (w *Worker) customRobots() string {
	if w.deps == nil || w.deps.ProjectDBManager == nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(w.deps.ProjectDBManager.ProjectDir(w.projectID), "files", "robots.txt"))
	if err != nil {
		return ""
	}
	return string(b)
}

// specAuditRequest assembles the auditor's one message. On a re-audit, named holds per
// item the gaps the last audit flagged it for, and responses the builder's answer.
func specAuditRequest(brief, change, features string, tasks []Task, named, responses map[string]string, state, source string) string {
	var b strings.Builder
	if brief != "" {
		b.WriteString("## Brief\n\n" + brief + "\n\n")
	}
	if change != "" {
		b.WriteString("## Change the owner asked for in this run\n\n" + change + "\n\n")
	}
	if features != "" {
		b.WriteString(strings.TrimLeft(features, "\n") + "\n")
	}
	b.WriteString("## Checklist items to review\n\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "- `%s` %s\n", t.ID, t.Title)
		if s := strings.TrimSpace(t.Intent); s != "" {
			b.WriteString("  Intent: " + s + "\n")
		}
		if s := strings.TrimSpace(t.DoneWhen); s != "" {
			b.WriteString("  Done when: " + s + "\n")
		}
		if len(t.Paths) > 0 {
			b.WriteString("  Owns: " + strings.Join(t.Paths, ", ") + "\n")
		}
		if g := named[t.ID]; g != "" {
			b.WriteString("  Gaps the last audit named:\n    " + strings.ReplaceAll(g, "\n", "\n  ") + "\n")
		}
		if r := responses[t.ID]; r != "" {
			b.WriteString("  Builder's response to the last audit: " + r + "\n")
		}
	}
	if state != "" {
		b.WriteString("\n## Project state\n\n" + state + "\n")
	}
	b.WriteString("\n## Source\n\n" + source)
	b.WriteString("\nReview every checklist item above and reply with the JSON object.")
	return b.String()
}

// sourceFile is one artifact shown to the auditor.
type sourceFile struct {
	path, kind, body string
	style            bool
}

// specAuditSource renders the project's own code within budget: pages, layouts and
// the platform configuration first (they name the controls and what the platform
// serves), then scripts and server functions (they do the work), then stylesheets.
// Platform-managed helpers and binary assets are left out. robots is the custom
// robots.txt, empty when none is set. Over budget, the largest files are cut first,
// each keeping its head.
func specAuditSource(d *sql.DB, robots string, budget int) string {
	var files []sourceFile
	rows, err := d.Query(`SELECT kind, path, COALESCE(body,''), COALESCE(metadata,'') FROM of_files
		WHERE is_deleted = 0 AND kind IN ('page','layout','component','file','test')`)
	if err == nil {
		for rows.Next() {
			var f sourceFile
			var meta string
			if rows.Scan(&f.kind, &f.path, &f.body, &meta) != nil || strings.TrimSpace(f.body) == "" {
				continue
			}
			if isPlatformManagedMeta(meta) {
				continue
			}
			lower := strings.ToLower(f.path)
			if f.kind == "file" {
				switch {
				case strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs") || strings.HasSuffix(lower, ".html"):
				case strings.HasSuffix(lower, ".css"):
					f.style = true
				default:
					continue // images, fonts, data, manifests
				}
			}
			files = append(files, f)
		}
		_ = rows.Err()
		rows.Close()
	}
	if fr, ferr := d.Query(`SELECT path, code FROM of_function_endpoints`); ferr == nil {
		for fr.Next() {
			var f sourceFile
			if fr.Scan(&f.path, &f.body) == nil && strings.TrimSpace(f.body) != "" {
				f.path = "/api/" + strings.Trim(f.path, "/")
				f.kind = "server function"
				files = append(files, f)
			}
		}
		_ = fr.Err()
		fr.Close()
	}
	// Schedules and triggers are configuration, not files, yet a checklist item such as
	// "purge expired links nightly" is delivered by one; without them the auditor would
	// call it missing.
	if sr, serr := d.Query(`SELECT name, COALESCE(description,''), COALESCE(cron_expression,''), COALESCE(interval_seconds,0), COALESCE(native_action,''), COALESCE(prompt,'') FROM of_scheduled_tasks`); serr == nil {
		for sr.Next() {
			var name, desc, cron, native, prompt string
			var every int
			if sr.Scan(&name, &desc, &cron, &every, &native, &prompt) == nil {
				files = append(files, sourceFile{path: "schedule " + name, kind: "scheduled task",
					body: fmt.Sprintf("%s\ncron: %s\nevery_seconds: %d\nnative_action: %s\nprompt: %s", desc, cron, every, native, prompt)})
			}
		}
		_ = sr.Err()
		sr.Close()
	}
	if tr, terr := d.Query(`SELECT name, event_type, COALESCE(event_filter,''), trigger_type, trigger_config, timing FROM of_triggers`); terr == nil {
		for tr.Next() {
			var name, event, filter, typ, cfg, timing string
			if tr.Scan(&name, &event, &filter, &typ, &cfg, &timing) == nil {
				files = append(files, sourceFile{path: "trigger " + name, kind: "trigger",
					body: fmt.Sprintf("on %s %s filter %s -> %s %s", timing, event, filter, typ, cfg)})
			}
		}
		_ = tr.Err()
		tr.Close()
	}
	files = append(files, platformConfigSources(d, robots)...)
	rank := func(f sourceFile) int {
		switch {
		case f.kind == "page", f.kind == "layout", f.kind == "component", f.kind == platformConfigKind:
			return 0
		case f.style:
			return 2
		default:
			return 1
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		if rank(files[i]) != rank(files[j]) {
			return rank(files[i]) < rank(files[j])
		}
		return files[i].path < files[j].path
	})
	for i := range files {
		limit := specAuditFileCap
		if files[i].style {
			limit = specAuditStyleCap
		}
		files[i].body = clipSource(files[i].body, limit)
	}
	// Shrink the largest file until the whole set fits.
	for total := sourceTotal(files); total > budget; total = sourceTotal(files) {
		big := 0
		for i := range files {
			if len(files[i].body) > len(files[big].body) {
				big = i
			}
		}
		if len(files[big].body) <= 2_000 {
			break // everything is already a head; stop rather than cut to nothing
		}
		clipped := clipSource(files[big].body, len(files[big].body)-(total-budget)-200)
		if len(clipped) >= len(files[big].body) {
			break // the clip's own note keeps it as long as it was: no further progress
		}
		files[big].body = clipped
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "=== %s (%s) ===\n%s\n\n", f.path, f.kind, strings.TrimRight(f.body, "\n"))
	}
	return b.String()
}

// platformConfigKind labels what the platform delivers from configuration rather
// than from a file: the auditor would otherwise read it as missing code.
const platformConfigKind = "platform configuration"

// platformConfigSources renders the behaviour the platform serves from
// configuration: each page's head tags (the renderer writes <title>, description
// and Open Graph tags from page metadata overlaid by manage_seo), robots.txt and the
// live sitemap, the tables' DDL with their indexes and constraints, and the upload
// endpoints with the table each one records into. None of it is in a file, so
// without this block every SEO, uniqueness or upload item reads as undelivered.
func platformConfigSources(d *sql.DB, robots string) []sourceFile {
	var out []sourceFile
	var head strings.Builder
	if rows, err := d.Query(`SELECT f.path, COALESCE(json_extract(f.metadata,'$.title'),''), COALESCE(json_extract(f.metadata,'$.description'),''),
			COALESCE(s.title,''), COALESCE(s.description,''), COALESCE(s.og_title,''), COALESCE(s.og_image,'')
		FROM of_files f LEFT JOIN of_seo_meta s ON s.page_path = f.path
		WHERE f.kind = 'page' AND f.is_deleted = 0 ORDER BY f.path`); err == nil {
		for rows.Next() {
			var path, title, desc, seoTitle, seoDesc, ogTitle, ogImage string
			if rows.Scan(&path, &title, &desc, &seoTitle, &seoDesc, &ogTitle, &ogImage) != nil {
				continue
			}
			if seoTitle != "" {
				title = seoTitle
			}
			if seoDesc != "" {
				desc = seoDesc
			}
			if ogTitle == "" {
				ogTitle = title
			}
			fmt.Fprintf(&head, "%s: <title>=%q description=%q og:title=%q og:image=%q\n", path, title, desc, ogTitle, ogImage)
		}
		_ = rows.Err()
		rows.Close()
	}
	if head.Len() > 0 {
		out = append(out, sourceFile{path: "page head tags", kind: platformConfigKind,
			body: "The renderer writes these into every page's <head> (an empty title falls back to the project name; og:description falls back to the description):\n" + head.String()})
	}
	if strings.TrimSpace(robots) == "" {
		robots = "User-agent: *\nAllow: /\nSitemap: /sitemap.xml (platform default)"
	}
	out = append(out, sourceFile{path: "/robots.txt and /sitemap.xml", kind: platformConfigKind,
		body: "/robots.txt serves:\n" + strings.TrimSpace(robots) + "\n/sitemap.xml is generated live from the published pages on every request."})

	var ddl strings.Builder
	if rows, err := d.Query(`SELECT sql FROM sqlite_master
		WHERE type IN ('table','index') AND sql IS NOT NULL AND tbl_name IN (SELECT table_name FROM of_dynamic_tables)
		ORDER BY tbl_name, type DESC, name`); err == nil {
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				ddl.WriteString(strings.TrimSpace(s) + ";\n")
			}
		}
		_ = rows.Err()
		rows.Close()
	}
	if ddl.Len() > 0 {
		out = append(out, sourceFile{path: "schema", kind: platformConfigKind, body: ddl.String()})
	}
	if routes := endpointRoutes(d); routes != "" {
		out = append(out, sourceFile{path: "endpoint routes", kind: platformConfigKind,
			body: "Every route the server answers, as mounted (an endpoint's tool path gets a fixed suffix per kind, so the URL a page calls is the one listed here):\n" + routes})
	}
	// An LLM or agent endpoint carries its behaviour - the persona, rules, length and
	// tone a checklist item asks for - in a system_prompt stored in config, not in any
	// file. Without it the auditor cannot see that "the endpoint establishes <voice>" is
	// delivered and calls a correctly-built endpoint missing (a false spec_gap that ships
	// the build broken and re-fires every repair round). Surface each prompt the way
	// schedules and triggers are, clipped to keep a long prompt from crowding the budget.
	if prompts := endpointPrompts(d); prompts != "" {
		out = append(out, sourceFile{path: "LLM/agent endpoint prompts", kind: platformConfigKind,
			body: "The system prompt each hosted LLM/agent endpoint runs with (stored in config, not a file):\n" + prompts})
	}
	return out
}

// endpointPrompts lists the system prompt of every hosted LLM and agent endpoint, each
// clipped, so the auditor can judge an item that asks the endpoint to establish a voice,
// rules or behaviour. Empty when the project hosts none.
func endpointPrompts(d *sql.DB) string {
	var b strings.Builder
	emit := func(query, suffix string) {
		rows, err := d.Query(query)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var path, prompt string
			if rows.Scan(&path, &prompt) != nil {
				continue
			}
			if strings.TrimSpace(prompt) == "" {
				fmt.Fprintf(&b, "/api/%s%s: (no system prompt set)\n\n", strings.Trim(path, "/"), suffix)
				continue
			}
			fmt.Fprintf(&b, "/api/%s%s system prompt:\n%s\n\n", strings.Trim(path, "/"), suffix, clipSource(prompt, 2_000))
		}
	}
	emit(`SELECT path, COALESCE(system_prompt,'') FROM of_llm_endpoints ORDER BY path`, "/chat and /complete")
	emit(`SELECT path, COALESCE(system_prompt,'') FROM of_agent_endpoints ORDER BY path`, "/run")
	return b.String()
}

// endpointRoutes lists the routes the public server answers, read from the of_endpoints
// spine runtime dispatch resolves against, plus the auth and feed namespaces that live
// outside it. Each kind mounts under its own suffix (an LLM proxy at /chat and
// /complete, an upload at /upload, a socket at /ws), so the bare tool path is never
// the URL a page calls; an auditor shown only that path calls a correct fetch broken.
// Rows are read before the per-kind detail lookups so no query runs inside a cursor.
func endpointRoutes(d *sql.DB) string {
	type route struct{ path, sub, kind string }
	var order []route
	methods := map[route][]string{}
	if rows, err := d.Query(`SELECT path, sub_segment, method, kind FROM of_endpoints ORDER BY path, sub_segment, method`); err == nil {
		for rows.Next() {
			var r route
			var method string
			if rows.Scan(&r.path, &r.sub, &method, &r.kind) != nil {
				continue
			}
			if _, seen := methods[r]; !seen {
				order = append(order, r)
			}
			if method != "*" {
				methods[r] = append(methods[r], method)
			} else if methods[r] == nil {
				methods[r] = []string{}
			}
		}
		_ = rows.Err()
		rows.Close()
	}
	var b strings.Builder
	for _, r := range order {
		url := "/api/" + strings.Trim(r.path, "/")
		if r.sub != "" {
			url += "/" + r.sub
		}
		verbs := strings.Join(methods[r], "|")
		if verbs == "" {
			verbs = "ANY"
		}
		fmt.Fprintf(&b, "%s %s (%s)%s\n", verbs, url, r.kind, routeDetail(d, r.kind, r.path, r.sub))
	}
	var auths, feeds []string
	if rows, err := d.Query(`SELECT path, table_name FROM of_auth_endpoints ORDER BY path`); err == nil {
		for rows.Next() {
			var p, t string
			if rows.Scan(&p, &t) == nil {
				auths = append(auths, fmt.Sprintf("POST /api/%[1]s/register, POST /api/%[1]s/login, GET /api/%[1]s/me (auth, users in %[2]s)", strings.Trim(p, "/"), t))
			}
		}
		_ = rows.Err()
		rows.Close()
	}
	if rows, err := d.Query(`SELECT path, format FROM of_feed_endpoints ORDER BY path`); err == nil {
		for rows.Next() {
			var p, f string
			if rows.Scan(&p, &f) == nil {
				feeds = append(feeds, fmt.Sprintf("GET /feed/%s (%s feed)", strings.Trim(p, "/"), f))
			}
		}
		_ = rows.Err()
		rows.Close()
	}
	for _, l := range append(auths, feeds...) {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// routeDetail is the kind-specific fact a route line needs to be judged: who may call a
// CRUD route, what an upload records, what an LLM route returns.
func routeDetail(d *sql.DB, kind, path, sub string) string {
	switch {
	case kind == "crud":
		var table, role, owner, scope string
		var auth, publicRead, publicWrite bool
		if d.QueryRow(`SELECT table_name, COALESCE(requires_auth,0), COALESCE(public_read,0), COALESCE(public_write,0), COALESCE(required_role,''), COALESCE(owner_column,''), COALESCE(owner_scope,'')
			FROM of_api_endpoints WHERE path = ?`, path).Scan(&table, &auth, &publicRead, &publicWrite, &role, &owner, &scope) != nil {
			return ""
		}
		return fmt.Sprintf(": table %s, access %s; one row at /api/%s/<id>", table, crudAccess(auth, publicRead, publicWrite, role, owner, scope), strings.Trim(path, "/"))
	case kind == "upload":
		var table, types string
		var maxMB int
		if d.QueryRow(`SELECT COALESCE(table_name,''), COALESCE(allowed_types,''), COALESCE(max_size_mb,0) FROM of_upload_endpoints WHERE path = ?`, path).Scan(&table, &types, &maxMB) != nil {
			return ""
		}
		s := fmt.Sprintf(": multipart field `file`, max %d MB, types %s", maxMB, types)
		if table != "" {
			s += fmt.Sprintf(", inserts a row into %s (filename, url, content_type, size; other columns take their defaults) and returns it as `row`", table)
		}
		return s
	case kind == "llm" && sub == "chat":
		return ": server-sent token stream"
	case kind == "llm" && sub == "complete":
		return ": one JSON reply"
	}
	return ""
}

// crudAccess describes a CRUD endpoint's access for the auditor. The single AuthModel
// word is read-oriented and ignores public_write, so a public-submission endpoint
// (anonymous POST, auth-gated GET: a contact form) reads to the auditor as blocking the
// POST and gets wrongly flagged as a gap - the build then ships "broken" and the repair
// loop churns (it opens public_write round one, the re-audit still sees "authenticated").
// When public_write opens writes the model would otherwise gate, name it, so the auditor
// sees the anonymous submit is delivered.
func crudAccess(requiresAuth, publicRead, publicWrite bool, role, owner, scope string) string {
	model := endpoints.AuthModel(requiresAuth, publicRead, role, owner, scope)
	if publicWrite && model != "public" {
		return model + " for reads; writes are public (anyone may POST/PUT/DELETE without signing in)"
	}
	return model
}

// isPlatformManagedMeta reports whether an artifact's metadata JSON carries the
// platform_managed flag, whatever spacing the JSON was written with.
func isPlatformManagedMeta(meta string) bool {
	if strings.TrimSpace(meta) == "" {
		return false
	}
	var m map[string]any
	if json.Unmarshal([]byte(meta), &m) != nil {
		return false
	}
	pm, _ := m["platform_managed"].(bool)
	return pm
}

// clipSource keeps the head of a body up to limit bytes, cut at a line break, and
// says how much was left out.
func clipSource(body string, limit int) string {
	if limit < 2_000 {
		limit = 2_000
	}
	if len(body) <= limit {
		return body
	}
	cut := strings.LastIndex(body[:limit], "\n")
	if cut < limit/2 {
		// No line break near the limit (minified code): cut on a rune boundary.
		cut = limit
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
	}
	return body[:cut] + fmt.Sprintf("\n[%d more bytes of this file not shown]", len(body)-cut)
}

func sourceTotal(files []sourceFile) int {
	n := 0
	for _, f := range files {
		n += len(f.body) + len(f.path) + 20
	}
	return n
}
