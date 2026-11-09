package content

// of_test.js is the user-flow script that manage_testing owns, stored under its own kind
// so no page can load it. manage_files hands a save or patch of it to that tool, serves a
// read, and refuses every other write so a second copy is never created.

import (
	"strings"

	"openfabrica/probe"
	"openfabrica/tools"
)

// isFlowScriptPath reports whether a manage_files path names the user-flow script.
func isFlowScriptPath(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimPrefix(p, "files/")
	p = strings.TrimPrefix(p, "js/")
	return p == probe.FlowPath
}

// flowScriptWriteGuard refuses a manage_files write to of_test.js, or returns nil.
func flowScriptWriteGuard(path string) *tools.Result {
	if !isFlowScriptPath(path) {
		return nil
	}
	return tools.FailCode("flow_script",
		"%s is the user-flow script, which manage_testing owns. This manage_files action cannot touch it: it would create a second copy test_flows never reads, or remove the one it does. "+
			"Change it with manage_testing action=write_flows (the whole script, validated, saved and replayed); a patch or save of the script through manage_files is routed there for you.", probe.FlowPath)
}

// flowScriptGet serves the stored user-flow script to a manage_files get, or returns
// nil when the path is not of_test.js.
func flowScriptGet(ctx *tools.ToolContext, path string) *tools.Result {
	if !isFlowScriptPath(path) {
		return nil
	}
	src := probe.LoadFlowSource(ctx.DB)
	if src == "" {
		return tools.Fail("no %s saved yet; write one with manage_testing action=write_flows", probe.FlowPath)
	}
	return tools.Ok(map[string]any{
		"file_path": probe.FlowPath,
		"kind":      probe.FlowKind,
		"body":      src,
		"note":      "The user-flow script is owned by manage_testing. Edit it with manage_files action=patch (a small change) or action=save, or manage_testing action=write_flows (full source): each is validated, saved and replayed through write_flows.",
	})
}

// flowScriptSave hands a manage_files save of the script to its owner. It returns nil when
// the path is not the script or no owner is loaded.
func flowScriptSave(ctx *tools.ToolContext, path, body string) *tools.Result {
	if !isFlowScriptPath(path) || tools.FlowScriptSaver == nil || body == "" {
		return nil
	}
	res, err := tools.FlowScriptSaver(ctx, body)
	if err != nil {
		return tools.FailErr(err)
	}
	return withFlowRoutedNote(res)
}

// flowScriptPatch applies a patch to the stored script and hands it to its owner. A patch
// that does not apply cleanly changes nothing.
func flowScriptPatch(ctx *tools.ToolContext, path string, patchesArg []map[string]any) *tools.Result {
	if !isFlowScriptPath(path) || tools.FlowScriptSaver == nil {
		return nil
	}
	src := probe.LoadFlowSource(ctx.DB)
	if src == "" {
		return tools.Fail("no %s saved yet; write one with manage_testing action=write_flows", probe.FlowPath)
	}
	normalizePatchAliases(patchesArg)
	if err := expandInsertHunks(src, patchesArg); err != nil {
		return tools.FailCode(tools.CodeBadArg, "patch rejected before any edit was attempted, so the script is UNCHANGED: %s", err.Error())
	}
	var patches []SearchReplacePatch
	if err := decodeJSONArrayArg(patchesArg, "patches", &patches); err != nil {
		return tools.FailCode(tools.CodeBadArg, "%s", err.Error())
	}
	if rej := patchSetRejection(patches, patchesArg); rej != nil {
		return rej
	}
	next, pr := ApplyPatches(src, patches)
	if len(pr.NotFound) > 0 || len(pr.Refused) > 0 || len(pr.Chained) > 0 || len(pr.Unanchored) > 0 || len(pr.Applied) == 0 || next == src {
		return tools.FailCode("flow_script_patch",
			"the patch did not apply cleanly to %s, so nothing changed (%d hunks applied, %d not found, %d no-op or unanchored). "+
				"Read the script with manage_files action=get, or re-save the whole corrected script with manage_testing action=write_flows.",
			probe.FlowPath, len(pr.Applied), len(pr.NotFound), len(pr.NoOp)+len(pr.Unanchored))
	}
	res, err := tools.FlowScriptSaver(ctx, next)
	if err != nil {
		return tools.FailErr(err)
	}
	return withFlowRoutedNote(res)
}

// withFlowRoutedNote says the edit went to the script's owner and the result is its replay.
func withFlowRoutedNote(res *tools.Result) *tools.Result {
	if res == nil {
		return nil
	}
	if data, ok := res.Data.(map[string]any); ok {
		data["routed"] = "of_test.js is owned by manage_testing: your edit was saved through write_flows and replayed, so this result is that replay"
	}
	return res
}
