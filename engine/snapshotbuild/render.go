package snapshotbuild

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/config"
	"openfabrica/internal/stringutil"
)

// Render returns the snapshot as a compact markdown block intended to be
// appended as a per-turn user message. Empty sections are omitted.
func (s *ProjectSnapshot) Render() string {
	return s.renderWithHeader("## Current Project State (project_state)", withExplainers)
}

// RenderData is the full snapshot without the section explainers: every row, none of
// the standing prose. For a re-baseline inside a segment, whose first baseline already
// put the explainers in the (append-only) transcript.
func (s *ProjectSnapshot) RenderData() string {
	return s.renderWithHeader("## Current Project State (project_state)", dataOnly)
}

// explainMode selects whether a render carries each section's standing explainer - the
// paragraph that teaches what the section IS and how to use it, as opposed to the rows
// that say what the project currently HAS.
type explainMode bool

const (
	// withExplainers: the full baseline. The rules ride along with the data.
	withExplainers explainMode = true
	// dataOnly: a delta. The rows change every write turn; the explainer
	// paragraphs do not, and a delta is appended, so shipping them again
	// re-bills byte-identical prose every turn (the module-contracts paragraph
	// alone is ~1.1KB). The segment's first baseline already taught them, and the
	// transcript is append-only, so they stay in context for the whole segment.
	dataOnly explainMode = false
)

// explain writes a section's standing explainer, or nothing in dataOnly mode. Every
// explainer routes through here so "is this per-turn or per-baseline" is one decision at
// one call site rather than a judgement repeated at each section.
func explain(b *strings.Builder, mode explainMode, text string) {
	if mode == withExplainers {
		b.WriteString(text)
	}
}

// writeStyleFields renders one stylesheet's tokens/classes/keyframes and
// truncation note under indent. Shared by the shared-CSS and page-scope blocks,
// which differ only in how deep they sit.
func writeStyleFields(b *strings.Builder, sc StyleContract, indent string) {
	if len(sc.Vars) > 0 {
		fmt.Fprintf(b, "%stokens: %s\n", indent, joinTokens(sc))
	}
	if len(sc.Classes) > 0 {
		fmt.Fprintf(b, "%sclasses: %s\n", indent, strings.Join(sc.Classes, ", "))
	}
	if len(sc.Keyframes) > 0 {
		fmt.Fprintf(b, "%s@keyframes: %s\n", indent, strings.Join(sc.Keyframes, ", "))
	}
	if sc.Truncated {
		fmt.Fprintf(b, "%s(list truncated; read the file for the rest)\n", indent)
	}
}

// joinTokens renders a sheet's custom properties, each with its declared value
// when the sheet carries one (the shared design system does; a page one-off does
// not). Values are what let a later page reuse the look instead of guessing it.
func joinTokens(sc StyleContract) string {
	parts := make([]string, 0, len(sc.Vars))
	for _, v := range sc.Vars {
		if val := sc.VarValues[v]; val != "" {
			parts = append(parts, v+": "+val)
		} else {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "; ")
}

// renderWithHeader renders the snapshot under the given top-level header. Render
// uses the standard "Current Project State" header; the delta renderer passes
// its own so a changed-entity subset reads as a change list rather than the
// whole state, while reusing every per-section format below verbatim.
func (s *ProjectSnapshot) renderWithHeader(header string, mode explainMode) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(header + "\n")

	// Lead with any genuinely-orphaned page-scope artifacts so the issue is
	// impossible to miss. UnlinkedPageFiles is import-aware: a module reached
	// transitively from a linked entry is NOT listed - the page boots correctly
	// with only the entry linked. What remains is dead weight or a missing-link
	// bug (a JS file no linked module imports, or a CSS file, which an ES module
	// cannot pull in), surfaced so the agent links or prunes it.
	if len(s.UnlinkedPageFiles) > 0 {
		fmt.Fprintf(&b, "\n### Page-scope artifacts not yet reached by a linked entry (%d)\n", len(s.UnlinkedPageFiles))
		b.WriteString("Saved but not yet imported by a linked entry. Mid-split this is expected - keep saving modules, then `attach` ONLY the entry (e.g. main.js); its relative imports pull the rest of the graph in. Don't hand-link intermediate modules; the list clears itself when the entry is linked. Only a file no entry will import (a stray JS module, or any CSS, which an ES module cannot pull in) needs its own link:\n")
		for _, p := range s.UnlinkedPageFiles {
			fmt.Fprintf(&b, "- `%s`, link directly only if no entry imports it: `manage_files action=attach page_path=/<route> file_path=%s`\n", p, p)
		}
	}

	if len(s.Pages) > 0 {
		fmt.Fprintf(&b, "\n### Pages (%d)\n", len(s.Pages))
		for _, p := range s.Pages {
			line := "- `" + p.Path + "`"
			if p.Title != "" {
				line += ", " + p.Title
			}
			if len(p.Assets) > 0 {
				line += " [assets: " + strings.Join(p.Assets, ", ") + "]"
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	if len(s.Layouts) > 0 {
		fmt.Fprintf(&b, "\n### Layouts (%d)\n", len(s.Layouts))
		for _, l := range s.Layouts {
			fmt.Fprintf(&b, "- `%s`\n", l.Name)
		}
	}

	if len(s.Components) > 0 {
		fmt.Fprintf(&b, "\n### Components (%d)\n", len(s.Components))
		for _, c := range s.Components {
			fmt.Fprintf(&b, "- `%s`\n", c.Name)
		}
	}

	if len(s.Tables) > 0 {
		fmt.Fprintf(&b, "\n### Tables (%d)\n", len(s.Tables))
		for _, t := range s.Tables {
			cols := stringutil.Truncate(strings.Join(t.Columns, ", "), 800)
			// Surface the row count so the LLM can tell a seeded table from an
			// empty one and skip re-seeding. Omitted when the count is unknown.
			rc := ""
			switch {
			case t.RowCount == 0:
				rc = " (0 rows, empty)"
			case t.RowCount == 1:
				rc = " (1 row, seeded)"
			case t.RowCount > 1:
				rc = fmt.Sprintf(" (%d rows, seeded)", t.RowCount)
			}
			fmt.Fprintf(&b, "- `%s`%s: %s\n", t.Name, rc, cols)
		}
	}

	if len(s.Views) > 0 {
		fmt.Fprintf(&b, "\n### Views (%d)\n", len(s.Views))
		explain(&b, mode, "Read-only SQL views (create_view): query them like a table and expose them with a read API. They derive their rows from other tables and hold none of their own - never seed a view.\n")
		for _, v := range s.Views {
			cols := stringutil.Truncate(strings.Join(v.Columns, ", "), 200)
			fmt.Fprintf(&b, "- `%s`: %s\n", v.Name, cols)
		}
	}

	if len(s.Endpoints) > 0 {
		fmt.Fprintf(&b, "\n### Endpoints (%d)\n", len(s.Endpoints))
		for _, e := range s.Endpoints {
			if e.AuthKind == authKindAuth {
				fmt.Fprintf(&b, "- `POST /api/%s/register` - create user; body uses `password` plaintext\n", e.Path)
				fmt.Fprintf(&b, "- `POST /api/%s/login` - returns `{success, user_id, token}`; store the token only when `success` is true (a 2FA or tenant step answers 200 without one)\n", e.Path)
				fmt.Fprintf(&b, "- `GET  /api/%s/me` (Bearer) - current user only; NOT a user list\n", e.Path)
				fmt.Fprintf(&b, "- `POST /api/%s/refresh` - fresh token (lifetime: the endpoint's jwt_expiry_hours)\n", e.Path)
				continue
			}
			tag := ""
			switch {
			case e.AuthKind == authKindWS:
				// The mode replaces the "[ws]" tag rather than joining it: the row
				// already opens with "WS", so the kind was saying it twice.
				tag = " [" + wsTag(e) + "]"
			case e.AuthKind != "" && e.AuthKind != authKindAPI:
				tag = " [" + e.AuthKind + "]"
			}
			if e.Auth {
				if e.PublicWrite {
					// A public-submission endpoint: anyone may write, reads stay gated.
					tag += " (auth reads, writes public)"
				} else {
					tag += " (auth)"
				}
			}
			fmt.Fprintf(&b, "- `%s %s`%s\n", e.Methods, e.Path, tag)
		}
	}

	if len(s.Libraries) > 0 {
		fmt.Fprintf(&b, "\n### Libraries (%d)\n", len(s.Libraries))
		explain(&b, mode, "Declared CDN libraries, auto-injected by the renderer - use them directly; do NOT hand-write their <script>/<link> tags. Re-adding one as-is is a no-op, but re-adding with a changed url/module_name updates it in place (use this to fix a version mismatch, e.g. bumping core to match addons):\n")
		for _, l := range s.Libraries {
			if l.URL != "" {
				fmt.Fprintf(&b, "- `%s` (%s)\n", l.Name, l.URL)
			} else {
				fmt.Fprintf(&b, "- `%s`\n", l.Name)
			}
		}
	}

	if len(s.Files) > 0 {
		// Flag only the genuinely-orphaned page-scope files (same import-aware set as
		// the lead section), so a module reached through a linked entry's import
		// graph isn't tagged UNLINKED just because it has no of_page_assets row.
		orphan := make(map[string]bool, len(s.UnlinkedPageFiles))
		for _, p := range s.UnlinkedPageFiles {
			orphan[p] = true
		}
		fmt.Fprintf(&b, "\n### Artifacts (%d)\n", len(s.Files))
		explain(&b, mode, "Every file listed here is SAVED and CURRENT. A file flagged unlinked below is the sole exception worth acting on.\n")
		for _, f := range s.Files {
			line := "- `" + f.Path + "` (" + f.Kind
			if f.Scope != "" {
				line += ", " + f.Scope
			}
			line += ")"
			if orphan[f.Path] {
				line += ", not yet reached by a linked entry (expected mid-split; see the unlinked list above)."
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	if len(s.Modules) > 0 {
		fmt.Fprintf(&b, "\n### Module contracts (%d)\n", len(s.Modules))
		explain(&b, mode, "Each JS module's export/import surface. Import a sibling by the bare name shown in backticks (`import { x } from 'world.state'`) - never a './' path. Before referencing a shared binding (`net`, `engine`, `state`), find it in an export list here and import EXACTLY that name; a name not listed is not exported. An exported function shows its parameter list - call it with that many arguments, in that order (passing a value where a callback is expected, or one arg too many, throws at runtime). Importing a name a module doesn't export (or exporting one it never defines) throws when the page's module graph loads - keep consistent as you edit. A `shapes:` line lists the field keys of an exported object (a shared store module, a factory's returned frame), nested one level (`store{ input{ jump, forward } }`) - read and write EXACTLY those keys across modules; a producer that writes one key and a consumer that reads a different one (`input.jumpPressed` vs `input.jump`) passes every symbol check and only misbehaves at runtime, so no gate catches it. Define each shared payload's shape ONCE here and reuse it on both sides rather than hand-writing the object at each site.\n")
		for _, m := range s.Modules {
			header, details := moduleRecord(m)
			b.WriteString(header)
			b.WriteByte('\n')
			writeModuleDetails(&b, details, s.prevModuleDetails[m.Path])
		}
	}

	if len(s.PageDOM) > 0 {
		fmt.Fprintf(&b, "\n### Page DOM anchors (%d)\n", len(s.PageDOM))
		explain(&b, mode, "The element ids each page's static markup already provides (its body + the shared layout + any included components) - the hooks a page-scope script binds to with getElementById / querySelector. Bind to THESE; do not invent a container the page doesn't declare - a missing getElementById returns null and the first .appendChild / .add throws the instant that code runs, and because such wiring usually runs behind an interaction (entering a game, opening a panel) the page-LOAD probe never triggers it and it ships dead. Need an id that isn't here? Add the element to the page markup first; don't rebuild elements the page already declares.\n")
		for _, dc := range s.PageDOM {
			fmt.Fprintf(&b, "- `%s`: %s\n", dc.Path, strings.Join(dc.IDs, ", "))
			if dc.Truncated {
				b.WriteString("    (list truncated; read the page for the rest)\n")
			}
		}
	}

	if len(s.Styles) > 0 {
		fmt.Fprintf(&b, "\n### Design system (shared CSS) (%d)\n", len(s.Styles))
		explain(&b, mode, "Tokens and classes the global stylesheet already defines. REUSE these on every page instead of inventing ad-hoc classes or inline styles; when you need a new shared style, add it to the global CSS so it joins this list and later pages reuse it. This list is shown every turn (including after a context rollover, when the stylesheet body is no longer in front of you) - it is the canonical design vocabulary.\n")
		for _, sc := range s.Styles {
			fmt.Fprintf(&b, "- `%s`\n", sc.Path)
			writeStyleFields(&b, sc, "    ")
		}
	}

	if len(s.PageStyles) > 0 {
		fmt.Fprintf(&b, "\n### Page-scope styles (%d)\n", len(s.PageStyles))
		explain(&b, mode, "What each page's OWN stylesheet already defines. This is NOT shared vocabulary - it belongs to that page, and another page must not reach for it (the shared design system is the global CSS above). Use it so you don't re-declare a class this page already has, or invent a second name for it. Names only: read the file when you need the rules themselves.\n")
		for _, ps := range s.PageStyles {
			fmt.Fprintf(&b, "- page `%s`\n", ps.Page)
			for _, sc := range ps.Sheets {
				fmt.Fprintf(&b, "  - `%s`\n", sc.Path)
				writeStyleFields(&b, sc, "      ")
			}
		}
	}

	writeCSP(&b, mode, s.EffectiveCSP)
	writeUserFlowsOff(&b, s.UserFlowsOff)

	if len(s.Pages) == 0 && len(s.Layouts) == 0 && len(s.Components) == 0 && len(s.Tables) == 0 && len(s.Views) == 0 && len(s.Endpoints) == 0 && len(s.Files) == 0 {
		b.WriteString("\n*(empty project, nothing built yet)*\n")
	}

	return b.String()
}

// moduleRecord renders one module's contract record: the header line naming the
// module and its export surface, plus one detail line per import and a shapes
// line. Single home for the record format, shared by baseline and delta.
func moduleRecord(m ModuleContract) (header string, details []string) {
	exp := "(no named exports)"
	if len(m.Exports) > 0 {
		parts := make([]string, len(m.Exports))
		for i, name := range m.Exports {
			if params, ok := m.Sigs[name]; ok {
				parts[i] = name + "(" + strings.Join(params, ", ") + ")"
			} else {
				parts[i] = name
			}
		}
		exp = strings.Join(parts, ", ")
	}
	switch {
	case m.Bare != "":
		header = fmt.Sprintf("- `%s` (%s) exports: %s", m.Bare, m.Path, exp)
	case m.Collides:
		// Naming the cause matters: without it a colliding module reads exactly like
		// one that simply has no bare form, and the snapshot goes on listing an
		// unreachable module as though a sibling could import it.
		header = fmt.Sprintf("- `%s` exports: %s  **NO bare specifier: its basename `%s` is shared with another module, so both are out of the importmap and any bare import of that name throws at load. Rename one of them (manage_files action=rename).**",
			m.Path, exp, analysis.ProjectModuleName(m.Path))
	default:
		header = fmt.Sprintf("- `%s` exports: %s", m.Path, exp)
	}
	for _, im := range m.Imports {
		details = append(details, fmt.Sprintf("    imports { %s } from %s", strings.Join(im.Names, ", "), im.Spec))
	}
	// Render data shapes in export (source) order for stable output; every shape
	// name is an exported binding, so Exports is the canonical ordering.
	if len(m.Shapes) > 0 {
		shapeParts := make([]string, 0, len(m.Shapes))
		for _, name := range m.Exports {
			if keys, ok := m.Shapes[name]; ok {
				shapeParts = append(shapeParts, name+"{ "+strings.Join(keys, ", ")+" }")
			}
		}
		if len(shapeParts) > 0 {
			details = append(details, "    shapes: "+strings.Join(shapeParts, ", "))
		}
	}
	return header, details
}

// writeModuleDetails writes a record's detail lines. On a delta whose record
// strictly grew, lines the model already has verbatim collapse into a count;
// any removed or reworded line falls back to the full record, so the baseline
// plus deltas always replay to current state. prev nil renders everything.
func writeModuleDetails(b *strings.Builder, details, prev []string) {
	if len(prev) > 0 && len(details) >= len(prev) {
		seen := make(map[string]bool, len(prev))
		for _, p := range prev {
			seen[p] = true
		}
		fresh := details[:0:0]
		unchanged := 0
		for _, d := range details {
			if seen[d] {
				unchanged++
			} else {
				fresh = append(fresh, d)
			}
		}
		if unchanged == len(prev) {
			for _, d := range fresh {
				b.WriteString(d)
				b.WriteByte('\n')
			}
			if unchanged > 0 {
				fmt.Fprintf(b, "    (%d unchanged line(s) as last listed for this module)\n", unchanged)
			}
			return
		}
	}
	for _, d := range details {
		b.WriteString(d)
		b.WriteByte('\n')
	}
}

// prevModuleDetailLines indexes the previous snapshot's module detail lines by
// path, so a delta can suppress lines the model already has verbatim.
func prevModuleDetailLines(prev *ProjectSnapshot) map[string][]string {
	if prev == nil || len(prev.Modules) == 0 {
		return nil
	}
	out := make(map[string][]string, len(prev.Modules))
	for _, m := range prev.Modules {
		_, details := moduleRecord(m)
		out[m.Path] = details
	}
	return out
}

// subsetByKeys returns a snapshot holding only the entities whose stable key is in
// keep - keyed exactly as entityList computes them - with the live orphan list
// carried over whole (it is current-state action guidance, not per-entity detail).
// Rendering this subset reuses Render's per-section logic, so the change list and
// the full state can never drift in format.
func (s *ProjectSnapshot) subsetByKeys(keep map[string]bool) *ProjectSnapshot {
	if s == nil {
		return nil
	}
	// EffectiveCSP is deliberately NOT carried: it is a standing environment fact that
	// arrives with the baseline (and again on a re-baseline), not something a write turn
	// changes. Copying it here would re-ship the whole block on every delta.
	sub := &ProjectSnapshot{UnlinkedPageFiles: s.UnlinkedPageFiles}
	for _, p := range s.Pages {
		if keep["page:"+p.Path] {
			sub.Pages = append(sub.Pages, p)
		}
	}
	for _, l := range s.Layouts {
		if keep["layout:"+l.Name] {
			sub.Layouts = append(sub.Layouts, l)
		}
	}
	for _, c := range s.Components {
		if keep["component:"+c.Name] {
			sub.Components = append(sub.Components, c)
		}
	}
	for _, t := range s.Tables {
		if keep["table:"+t.Name] {
			sub.Tables = append(sub.Tables, t)
		}
	}
	for _, v := range s.Views {
		if keep["view:"+v.Name] {
			sub.Views = append(sub.Views, v)
		}
	}
	for _, e := range s.Endpoints {
		if keep["endpoint:"+e.AuthKind+":"+e.Methods+":"+e.Path] {
			sub.Endpoints = append(sub.Endpoints, e)
		}
	}
	for _, l := range s.Libraries {
		if keep["library:"+l.Name] {
			sub.Libraries = append(sub.Libraries, l)
		}
	}
	for _, f := range s.Files {
		if keep["file:"+f.Path] {
			sub.Files = append(sub.Files, f)
		}
	}
	for _, m := range s.Modules {
		if keep["module:"+m.Path] {
			sub.Modules = append(sub.Modules, m)
		}
	}
	for _, sc := range s.Styles {
		if keep["style:"+sc.Path] {
			sub.Styles = append(sub.Styles, sc)
		}
	}
	for _, dc := range s.PageDOM {
		if keep["dom:"+dc.Path] {
			sub.PageDOM = append(sub.PageDOM, dc)
		}
	}
	for _, ps := range s.PageStyles {
		if keep["pagestyle:"+ps.Page] {
			sub.PageStyles = append(sub.PageStyles, ps)
		}
	}
	return sub
}

// RenderDelta renders the entities that changed since the reference fingerprint
// map: each added or modified entity with its CURRENT full detail (reusing the
// normal per-section rendering), the names of any removed entities, and the live
// orphan guidance. Returns "" when nothing changed. The BUILD loop passes the
// fingerprints from the PREVIOUS update, so each delta is incremental and the
// chain of appended deltas plus the stable baseline reads as current state
// without any earlier message ever needing a rewrite.
func (s *ProjectSnapshot) RenderDelta(baseline map[string]string, prev *ProjectSnapshot) string {
	if s == nil {
		return ""
	}
	keep := map[string]bool{}
	for k, v := range s.Entities {
		if bv, ok := baseline[k]; !ok || bv != v {
			keep[k] = true
		}
	}
	var removed []string
	for k := range baseline {
		if _, ok := s.Entities[k]; !ok {
			removed = append(removed, k)
		}
	}
	if len(keep) == 0 && len(removed) == 0 {
		return ""
	}
	var b strings.Builder
	if len(keep) > 0 {
		// dataOnly: the section explainers already arrived with the baseline this delta
		// hangs off, and re-shipping them every write turn is the single largest block of
		// repeated prose in the loop (see explainMode).
		sub := s.subsetByKeys(keep)
		sub.prevModuleDetails = prevModuleDetailLines(prev)
		b.WriteString(sub.renderWithHeader(
			"## Changed since the previous update (current detail; a module's collapsed lines stay as last listed; the session-start state is in the project_state message above)",
			dataOnly))
	}
	if len(removed) > 0 {
		sort.Strings(removed)
		if b.Len() == 0 {
			b.WriteString("## Changed since the previous update\n")
		}
		fmt.Fprintf(&b, "\n### Removed\n%s\n", strings.Join(removed, ", "))
	}
	return b.String()
}

// writeUserFlowsOff tells the build the owner turned user-flow tests off. Silent when on.
func writeUserFlowsOff(b *strings.Builder, off bool) {
	if !off {
		return
	}
	b.WriteString("\n### User-flow tests\n")
	b.WriteString("- The owner turned user-flow tests OFF for this project: do not write or replay `of_test.js`, and ignore any guidance that asks for it. Page probes (`test_page`) still apply.\n")
}

// writeCSP reports the Content-Security-Policy the project's public pages are served
// under, and ONLY when it is not the permissive shipped default.
//
// It lives here, in the snapshot that rides every build turn, rather than behind a
// tool call: manage_project is not preloaded, so routing the model to read it would
// cost a find_tool hop on every build to learn a fact that is almost always "nothing
// is restricted". Silent on the default, it costs nothing there and arrives without
// being asked for on the installs where it changes what the model may write.
//
// Only the restrictions that actually differ from the default are named. Inline
// <script> is deliberately NOT listed as blocked: the renderer stamps a per-request
// nonce onto every inline script, so those keep running under a nonce-based policy -
// what stops is inline on*= handlers and cross-origin script sources.
func writeCSP(b *strings.Builder, mode explainMode, policy string) {
	if policy == "" || policy == config.DefaultPublicCSP {
		return
	}
	sources := config.CSPScriptSources(policy)
	if sources == "" {
		return // a policy that constrains scripts not at all changes nothing here
	}

	var limits []string
	if !strings.Contains(sources, "'unsafe-inline'") {
		limits = append(limits, "inline `on*=` event handlers never fire - bind with `addEventListener` from a module")
	}
	if !strings.Contains(sources, "*") && !strings.Contains(sources, "https:") {
		limits = append(limits, "cross-origin scripts are blocked, so a CDN library will not load however it is declared - build against what the project already serves")
	}
	if len(limits) == 0 {
		return
	}

	b.WriteString("\n### Content-Security-Policy\n")
	explain(b, mode, "This project serves under a non-default policy. The browser enforces it on every page, and the build cannot relax it - the owner sets it. Build within it.\n")
	fmt.Fprintf(b, "- script sources: `%s`\n", sources)
	for _, l := range limits {
		fmt.Fprintf(b, "- %s\n", l)
	}
}
