package content

import (
	"openfabrica/tools"
)

// manage_files is the central project-file surface: pages, layouts, components, CSS/JS
// files, and page-asset linking. Handlers live across manage_files_*.go and
// manage_files_handlers.go; this file declares the LLM-facing schema.
//
// One arg struct per action, each exposing only the fields its handler reads. The
// wire schema still merges every action's args into one union, so bulk_replace's
// top-level replace/count sit beside patch's [{search, replace}] list there; their
// descriptions say "bulk_replace only" to keep the two apart.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_files",
		Category:           "content",
		FullResultInEvents: true,
		MaxResult:          33000, // headroom over store.MaxSingleReadBody (a full get body) + JSON escaping + envelope
		Summarize:          tools.SummarizeStripBodies,
		OneLiner:           "Manage pages, layouts, components, and files - save, patch, get, list, attach to pages, rename.",
		// Disambiguation only. What each action DOES is that action's own Doc(), and the
		// patch-vs-resave decision rule is the build ground rules' - both already ride this
		// same prompt, so restating them here bills the model for the words twice.
		// The "work from the updated field" rule is the ground rules' and rides each
		// patch result's updated_note; it is not restated here.
		WhenToUse: `Same change across many artifacts -> bulk_replace, never repeated patches; a single artifact -> patch or save per the ground rules' one decision rule.`,
		Actions: []tools.Action{
			&ArtSave{}, &ArtPatch{}, &ArtGet{}, &ArtList{}, &ArtOutline{},
			&ArtBulkReplace{},
			&ArtRename{}, &ArtDelete{}, &ArtRestore{},
			&FileAttach{}, &FileDetach{}, &FileListPageAssets{},
		},
	})
}

// --- save -------------------------------------------------------------------

type ArtSave struct {
	Kind        string         `arg:"kind"                   desc:"File kind. Usually omit it: a route (\"/about\", \"/projects/:slug\") infers page, a \"/files/\" path or asset extension (.js/.css/.png) infers file. Required ONLY for a bare name (\"default\", \"sidebar\"): layout vs component. An explicit kind wins." enum:"page,layout,component,file"`
	FilePath    string         `arg:"file_path,required" desc:"Path identifier: a page route as a clean URL with no file extension (\"/\" for home, \"/about\" not \"/about.html\"), an asset path, or a file name. For kind=layout, save the primary layout at path \"default\" - pages with no explicit metadata.layout resolve there automatically, so any other name forces every page to set it. Do not also save the same layout under another name."`
	Body        string         `arg:"body,required"          desc:"Full artifact body, up to 48 KB (client-side JS modules up to 36 KB); build a larger file by splitting it into smaller files, or by saving a smaller complete first version and growing it in focused follow-up edits. See the action docstring for the page/layout body-shape contract."`
	ContentType string         `arg:"content_type"           desc:"MIME type override."`
	Scope       string         `arg:"scope"                  desc:"Asset visibility, a TOP-LEVEL arg - never nest it inside metadata. Usually omit it: JS/CSS files default to \"page\" (a single-page split) and a re-save keeps the existing scope. Pass \"global\" only for assets genuinely shared across every page (sitewide analytics/helpers)." enum:"page,global"`
	Metadata    map[string]any `arg:"metadata"               desc:"Free-form metadata stored with the artifact. On a kind=page, {title: \"Contact\"} sets the page <title>; omit it and the plan's per-page title is used automatically (never the project name)."`
	AttachTo    string         `arg:"attach_to"              desc:"kind=file only: page route this file attaches to, linked in this same call so no follow-up action=attach is needed. Pass it on every page entry JS and every page CSS. Leave it OFF a leaf module an entry imports - the import graph reaches those, and attaching them is wrong."`
	Position    int            `arg:"position"               desc:"Page-scope attach order, used with attach_to only (default 0, lower loads first; CSS cascade order - JS modules resolve through imports, not tag order)."`
	Overwrite   bool           `arg:"overwrite"              desc:"Set true ONLY to deliberately overwrite this path with entirely different content (a from-scratch rewrite). Not needed for normal edits or re-saves; a save that looks like it is accidentally clobbering an unrelated existing file is refused unless this is set."`
}

func (a *ArtSave) ActionName() string { return "save" }
func (a *ArtSave) Doc() string {
	return "Create or overwrite an artifact (page = HTML fragment, layout = the shell; see the page/layout law). Attach page-scope JS/CSS with attach_to, never a hand-written <script>/<link>. A malformed save is rejected with the exact fix."
}
func (a *ArtSave) Example() any {
	return &ArtSave{Kind: "page", FilePath: "/", Body: "<h1>Home</h1>"}
}
func (a *ArtSave) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if res := flowScriptSave(ctx, a.FilePath, a.Body); res != nil {
		return res, nil
	}
	if rej := flowScriptWriteGuard(a.FilePath); rej != nil {
		return rej, nil
	}
	res, err := artifactActionSave(ctx, a)
	if err != nil || res == nil || !res.Success {
		return res, err
	}
	data, _ := res.Data.(map[string]any)
	if a.AttachTo == "" {
		// No attach_to: say now if the file landed where no page can load it. The next
		// turn's project_state reports the same gap, but only after this turn has
		// ended, so the fix can no longer ride the save that caused it.
		if data != nil {
			// The RESOLVED kind, for the same reason the attach_to branch below uses it.
			if savedKind, _ := data["kind"].(string); savedKind == "file" {
				if note := unlinkedSaveNote(ctx, a.FilePath); note != "" {
					data["link_note"] = note
				}
			}
		}
		return res, nil
	}
	// attach_to shortcut: the save landed, so run the page link inline instead of a
	// follow-up attach call. An attach failure never fails the save (the file is stored);
	// it is reported so the model can fix just the attachment.
	// Gate on the RESOLVED kind (what was actually saved), not the raw arg:
	// artifactActionSave infers kind=file for self-describing paths (e.g. "css/home.css")
	// when the model omits kind. Checking a.Kind instead would skip the attach and stamp a
	// false "not a file" attach_error on those saves, shipping a page without its CSS/JS.
	savedKind, _ := data["kind"].(string)
	if savedKind != "file" {
		if data != nil {
			data["attach_error"] = "attach_to only applies to kind=file saves; pages and layouts are not attachable"
		}
		return res, nil
	}
	attached, fail := linkFileToPage(ctx, a.FilePath, a.AttachTo, a.Position)
	if data != nil {
		if fail != nil {
			data["attach_error"] = fail.Error
		} else {
			data["attached"] = attached
		}
	}
	return res, nil
}

// --- patch ------------------------------------------------------------------

type ArtPatch struct {
	Kind     string           `arg:"kind"                   desc:"File kind. Optional: omit to infer from file_path when it identifies one artifact." enum:"page,layout,component,file"`
	FilePath string           `arg:"file_path,required" desc:"Path identifier."`
	Patches  []map[string]any `arg:"patches,required"       desc:"Array of hunks, applied in order: {search, replace} to change text, or {insert_after|insert_before, text} / {append} / {prepend} to add text. Prefer a search that occurs once; if it matches more than once only the FIRST is replaced and a warning is returned. Give each region exactly ONE hunk: a hunk that only matches text an earlier hunk in the same call just wrote is refused. near targets a single occurrence; count is ignored when near is set."`
}

func (a *ArtPatch) ActionName() string { return "patch" }

// ItemsSchema spells out the hunk shape on the wire schema, so the model reads the
// per-field contract (and the insert shape) where it writes each hunk.
func (a *ArtPatch) ItemsSchema(arg string) map[string]any {
	if arg != "patches" {
		return nil
	}
	return map[string]any{
		"type": "object",
		"properties": tools.OrderedProps{
			// The anchor first: a model writes fields in the order it reads them.
			Keys: []string{"search", "replace", "insert_after", "insert_before", "text", "append", "prepend", "near", "count"},
			Props: map[string]any{
				"search": map[string]any{
					"type":        "string",
					"description": "Exact current text of the file to replace, copied verbatim as one contiguous run (never the 'N| ' line-number gutter).",
				},
				"replace": map[string]any{
					"type":        "string",
					"description": "The full new text for the matched span; empty deletes it. It must differ from search (an identical replace changes nothing). To ADD text without replacing any, use insert_after / insert_before / append / prepend instead.",
				},
				"insert_after": map[string]any{
					"type":        "string",
					"description": "Insert form: an existing line (or lines) of the file; `text` is added right after it. Use instead of search/replace when you are only adding.",
				},
				"insert_before": map[string]any{
					"type":        "string",
					"description": "Insert form: an existing line (or lines) of the file; `text` is added right before it.",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "The new lines for insert_after / insert_before.",
				},
				"append": map[string]any{
					"type":        "string",
					"description": "Insert form: new lines added at the very end of the file; no anchor needed.",
				},
				"prepend": map[string]any{
					"type":        "string",
					"description": "Insert form: new lines added at the very start of the file; no anchor needed.",
				},
				"count": map[string]any{
					"type":        "integer",
					"description": "Occurrences to replace: omit for the first only, -1 for all, N for the first N.",
				},
				"near": map[string]any{
					"type":        "string",
					"description": "Text close to the intended occurrence when search repeats; the nearest occurrence is replaced.",
				},
			},
		},
	}
}
func (a *ArtPatch) Doc() string {
	return "Apply [{search, replace}] pairs to one artifact body (whitespace/indentation-tolerant; replaces the first match by default - see the patches arg for count/near). The preferred targeted edit when a unique anchor exists."
}
func (a *ArtPatch) Example() any {
	return &ArtPatch{
		Kind:     "page",
		FilePath: "/",
		Patches:  []map[string]any{{"search": "Hello", "replace": "Welcome"}},
	}
}
func (a *ArtPatch) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if res := flowScriptPatch(ctx, a.FilePath, a.Patches); res != nil {
		return res, nil
	}
	if rej := flowScriptWriteGuard(a.FilePath); rej != nil {
		return rej, nil
	}
	return artifactActionPatch(ctx, a.Kind, a.FilePath, a.Patches)
}

// --- bulk_replace -----------------------------------------------------------

type ArtBulkReplace struct {
	Pattern    string `arg:"pattern,required" desc:"Single Go regex applied across every matched artifact."`
	Replace    string `arg:"replace"          desc:"Replacement (empty deletes matches); $1 and ${x} expand groups, so write $$ for a literal $. A patch carries its replacement in its hunk, never here."`
	Kind       string `arg:"kind"             desc:"Scope filter: limit to this artifact kind. At least one of kind / path_prefix / path_glob is required (call rejected otherwise)." enum:"page,layout,component,file"`
	PathPrefix string `arg:"path_prefix"      desc:"Scope filter, only artifacts whose path starts with this."`
	PathGlob   string `arg:"path_glob"        desc:"Scope filter, path.Match glob: * does not cross /, no ** (css/*.css). ANDs with path_prefix / kind."`
	Count      int    `arg:"max_per_file"     desc:"Max replacements per file (0 = unlimited)."`
	Preview    bool   `arg:"preview"          desc:"Report per-file replacement counts without writing."`
}

func (a *ArtBulkReplace) ActionName() string { return "bulk_replace" }
func (a *ArtBulkReplace) Doc() string {
	return "Apply ONE regex pattern+replace across every artifact matching kind / path_prefix / path_glob (at least one required). Use preview=true first. Inline-stored artifacts only."
}
func (a *ArtBulkReplace) Example() any {
	return &ArtBulkReplace{
		Kind:    "file",
		Pattern: "#fff",
		Replace: "#f5f5f5",
		Preview: true,
	}
}
func (a *ArtBulkReplace) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionBulkReplace(ctx, a)
}

// --- get --------------------------------------------------------------------

type ArtGet struct {
	Kind     string `arg:"kind"                   desc:"File kind. Optional: omit to infer from file_path when it identifies one artifact." enum:"page,layout,component,file"`
	FilePath string `arg:"file_path,required" desc:"Path identifier."`
	FromLine int    `arg:"from_line"              desc:"Start line for a ranged read (1-based)."`
	ToLine   int    `arg:"to_line"                desc:"End line for a ranged read (inclusive)."`
	Fresh    bool   `arg:"fresh"                  desc:"Force a full re-serve even if this range was already shown this session. Use when you no longer hold the body (or an edit anchor will not match) and need to re-read to re-anchor; normally leave false so unchanged repeat reads stay deduped."`
}

func (a *ArtGet) ActionName() string { return "get" }
func (a *ArtGet) Doc() string {
	return "Read an artifact body, or a from_line/to_line range (paginates via next_from_line). The 'N| ' gutter is display-only - never copy it into patch search text. A bare get of a large file returns its OUTLINE; pass a range for real source."
}
func (a *ArtGet) ReadOnly() bool { return true }
func (a *ArtGet) Example() any {
	return &ArtGet{Kind: "page", FilePath: "/"}
}
func (a *ArtGet) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if res := flowScriptGet(ctx, a.FilePath); res != nil {
		return res, nil
	}
	return artifactActionGetImpl(ctx, a)
}

// --- outline ----------------------------------------------------------------

type ArtOutline struct {
	Kind     string `arg:"kind"                   desc:"File kind. Optional: omit to infer from file_path when it identifies one artifact." enum:"page,layout,component,file"`
	FilePath string `arg:"file_path,required" desc:"Path identifier."`
}

func (a *ArtOutline) ActionName() string { return "outline" }
func (a *ArtOutline) Doc() string {
	return "Line-numbered structural map of one artifact (JS signatures, CSS selectors incl. @media, or HTML structure), bodies collapsed. The cheapest way to orient in a big file: outline, then get just the range you need."
}
func (a *ArtOutline) ReadOnly() bool { return true }
func (a *ArtOutline) Example() any {
	return &ArtOutline{Kind: "page", FilePath: "/"}
}
func (a *ArtOutline) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionOutline(ctx, a.Kind, a.FilePath)
}

// --- list -------------------------------------------------------------------

type ArtList struct {
	Kind string `arg:"kind" desc:"Optional kind filter. Omit to list every artifact." enum:"page,layout,component,file"`
}

func (a *ArtList) ActionName() string { return "list" }
func (a *ArtList) Doc() string        { return "List artifacts, optionally filtered by kind." }
func (a *ArtList) ReadOnly() bool     { return true }
func (a *ArtList) Example() any {
	return &ArtList{Kind: "page"}
}
func (a *ArtList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionList(ctx, a.Kind)
}

// --- delete -----------------------------------------------------------------

type ArtDelete struct {
	Kind     string `arg:"kind"                   desc:"File kind. Optional: omit to infer from file_path when it identifies one artifact." enum:"page,layout,component,file"`
	FilePath string `arg:"file_path,required" desc:"Path identifier."`
}

func (a *ArtDelete) ActionName() string { return "delete" }
func (a *ArtDelete) Doc() string {
	return "Soft-delete an artifact (tombstone)."
}
func (a *ArtDelete) Example() any {
	return &ArtDelete{Kind: "page", FilePath: "/scratch"}
}
func (a *ArtDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionDelete(ctx, a.Kind, a.FilePath)
}

// --- rename -----------------------------------------------------------------

type ArtRename struct {
	Kind     string `arg:"kind"                   desc:"File kind. Optional: omit to infer from file_path when it identifies one artifact." enum:"page,layout,component,file"`
	FilePath string `arg:"file_path,required" desc:"Current path."`
	NewPath  string `arg:"new_path,required"      desc:"Destination path."`
}

func (a *ArtRename) ActionName() string { return "rename" }
func (a *ArtRename) Doc() string        { return "Move an artifact to a new path." }
func (a *ArtRename) Example() any {
	return &ArtRename{Kind: "file", FilePath: "/files/app.js", NewPath: "/files/main.js"}
}
func (a *ArtRename) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if rej := flowScriptWriteGuard(a.NewPath); rej != nil {
		return rej, nil
	}
	return artifactActionRename(ctx, a.Kind, a.FilePath, a.NewPath)
}

// --- restore ----------------------------------------------------------------

type ArtRestore struct {
	Kind     string `arg:"kind,required"      desc:"File kind." enum:"page,layout,component,file"`
	FilePath string `arg:"file_path,required" desc:"Path identifier."`
	Version  int    `arg:"version"            desc:"Version number to restore the body from (see the version each save/edit returns). Omit to undo the last change - restores the most recent prior snapshot, which also recovers a just-deleted file."`
}

func (a *ArtRestore) ActionName() string { return "restore" }
func (a *ArtRestore) Doc() string {
	return "Undo: roll an artifact back to the given version, or (omitted) its last snapshot - also recovers a just-deleted file. Recorded as a new version. Inline text artifacts only, last 20 versions."
}
func (a *ArtRestore) Example() any {
	return &ArtRestore{Kind: "page", FilePath: "/"}
}
func (a *ArtRestore) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionRestore(ctx, a.Kind, a.FilePath, a.Version)
}
