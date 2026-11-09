package content

// Lifecycle handlers (delete / rename) for manage_files, which registers them
// in manage_files.go's init.

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/store"
	"openfabrica/tools"
)

// rejectPlatformManaged refuses a write (verb) to an artifact the platform emits and
// owns, such as the auth/ofnet/e2ee helper modules. Every writer shares this check so
// no action (patch, rename, restore, save, bulk_replace) edits one that delete protects.
func rejectPlatformManaged(ctx *tools.ToolContext, kind, path, verb string) *tools.Result {
	if !store.IsPlatformManagedArtifact(ctx.DB, kind, path) {
		return nil
	}
	return tools.FailCode("platform_managed",
		"cannot %s %q: it is a platform-managed file the platform emits and owns, and your pages load it at runtime. Leave it in place and use the globals/exports it provides; to change its behavior, change how you call it.",
		verb, path)
}

func artifactActionDelete(ctx *tools.ToolContext, kind, path string) (*tools.Result, error) {
	if rej := flowScriptWriteGuard(path); rej != nil {
		return rej, nil
	}
	// delete targets an artifact that already exists, so kind resolves from the
	// path exactly like patch/get; a genuinely ambiguous path is refused with the
	// candidate kinds (ambiguous_kind) rather than guessed.
	kind, path, errR := resolveKindPath(ctx, kind, path)
	if errR != nil {
		return errR, nil
	}
	// Platform-managed files are auto-emitted by the platform when a feature is
	// enabled and loaded by pages at runtime. The model sometimes "cleans up" one
	// as a stray/unlinked file, which ships a broken app (the file 404s and the
	// globals it exposes go undefined). Saves to these paths are already rejected;
	// reject deletes the same way.
	if store.IsPlatformManagedArtifact(ctx.DB, kind, path) {
		return tools.Fail("%q is a platform-managed file and cannot be deleted - the platform emits and owns it and your pages load it at runtime. It is NOT a stray or unlinked file; leave it in place and use the globals it exposes.", path), nil
	}
	// The pages a file was attached to, read before the delete drops those links, so
	// the echo says where it was taken off (a restore does not re-attach it).
	var detached []string
	if kind == "file" {
		detached, _ = store.PagesAttaching(ctx.DB, path)
	}
	ok, err := store.ArtifactDelete(ctx.DB, kind, path, ctx.ChangedBy())
	if err != nil {
		return tools.FailErr(err), nil
	}
	if !ok {
		return tools.NotFound(kind, path), nil
	}
	data := map[string]any{"deleted": path, "kind": kind}
	if len(detached) > 0 {
		data["detached_from"] = detached
	}
	return tools.Ok(data), nil
}

func artifactActionRestore(ctx *tools.ToolContext, kind, path string, version int) (*tools.Result, error) {
	// restore keeps requireKindPath: kind inference only sees live rows
	// (ArtifactKindsForPath filters is_deleted = 0), and restore's headline job
	// includes recovering a just-deleted file - inference would report exactly
	// that target as not-found.
	kind, path, errR := requireKindPath(kind, path)
	if errR != nil {
		return errR, nil
	}
	// No explicit version: undo the last change by restoring the most recent
	// snapshot that predates the current body (also recovers a just-deleted file).
	if version <= 0 {
		version = store.ArtifactRestorableVersion(ctx.DB, kind, path)
		if version == 0 {
			return tools.Fail("no prior snapshot to restore for %s %q", kind, path), nil
		}
	}
	if rej := rejectPlatformManaged(ctx, kind, path, "restore"); rej != nil {
		return rej, nil
	}
	if rej := flowScriptWriteGuard(path); rej != nil {
		return rej, nil
	}
	newVersion, err := store.ArtifactRestore(ctx.DB, kind, path, version, ctx.ChangedBy())
	if err != nil {
		return tools.FailErr(err), nil
	}
	return tools.Ok(map[string]any{
		"kind": kind, "file_path": path, "restored_from": version, "version": newVersion,
	}), nil
}

func artifactActionRename(ctx *tools.ToolContext, kind, path, newPath string) (*tools.Result, error) {
	if rej := flowScriptWriteGuard(path); rej != nil {
		return rej, nil
	}
	// The SOURCE must already exist, so its kind resolves from the path like
	// patch/get; only an ambiguous path demands an explicit kind. The target
	// inherits the resolved kind.
	kind, path, errR := resolveKindPath(ctx, kind, path)
	if errR != nil {
		return errR, nil
	}
	if rej := rejectPlatformManaged(ctx, kind, path, "rename"); rej != nil {
		return rej, nil
	}
	// The rename target shares the source's storage convention: a file moves to a
	// bare path, never a /files/ URL form, so it stays addressable by every other action.
	if kind == "file" {
		newPath = store.CanonicalFilePath(newPath)
	}
	if reservedUploadPath(kind, newPath) {
		return reservedUploadPathError(newPath), nil
	}
	if rej := validateRenameTarget(ctx, kind, path, newPath); rej != nil {
		return rej, nil
	}
	if err := store.ArtifactRename(ctx.DB, kind, path, newPath); err != nil {
		return tools.FailErr(err), nil
	}
	out := map[string]any{"kind": kind, "from": path, "to": newPath}
	if note := renameImporterNote(ctx, kind, path, newPath); note != "" {
		out["importer_note"] = note
	}
	return tools.Ok(out), nil
}

// validateRenameTarget applies the save-time path and module-name gates to a rename
// destination, so a rename cannot produce a path a save would have refused.
func validateRenameTarget(ctx *tools.ToolContext, kind, oldPath, newPath string) *tools.Result {
	if newPath == "" {
		return tools.FailCode(tools.CodeBadArg, "new_path is required")
	}
	if rej := rejectPlatformManaged(ctx, kind, newPath, "rename onto"); rej != nil {
		return rej
	}
	if err := store.ValidateArtifactPath(kind, newPath); err != nil {
		return tools.FailCode("invalid_path", "refusing to rename to %q: %s", newPath, err.Error())
	}
	if kind == "file" && isJSModulePath(newPath) {
		if name := analysis.ProjectModuleName(newPath); analysis.IsReservedModuleSpecifier(name) {
			return tools.FailCode("reserved_module_name",
				"refusing to rename to %q: the module specifier %q is reserved for a platform helper. Pick a basename that is not %q.",
				newPath, name, name)
		}
		if rej := rejectIfCollidingModuleName(ctx, newPath, false); rej != nil {
			return rej
		}
	}
	return nil
}

// renameImporterNote warns when other modules still import a renamed JS module by its
// old logical name, which the rename does not rewrite.
func renameImporterNote(ctx *tools.ToolContext, kind, oldPath, newPath string) string {
	if kind != "file" || !isJSModulePath(oldPath) {
		return ""
	}
	oldName, newName := analysis.ProjectModuleName(oldPath), analysis.ProjectModuleName(newPath)
	if oldName == "" || oldName == newName {
		return ""
	}
	var importers []string
	for p, body := range projectJSBodies(ctx, newPath, "") {
		if p == newPath {
			continue
		}
		for _, spec := range analysis.ExtractImportSpecifiers(analysis.StripJSComments(body)) {
			if spec == oldName {
				importers = append(importers, p)
				break
			}
		}
	}
	if len(importers) == 0 {
		return ""
	}
	sort.Strings(importers)
	return fmt.Sprintf("%d module(s) still import the old specifier %q and will fail to resolve: %s. Patch each import to %q.",
		len(importers), oldName, strings.Join(importers, ", "), newName)
}
