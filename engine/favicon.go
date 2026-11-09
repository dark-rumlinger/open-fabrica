package engine

// ensureFavicon guarantees every UI build ships a project-matched favicon
// WITHOUT an LLM token: an SVG synthesised from the project title, saved as the
// global favicon.svg. A floor, not a ceiling: an existing favicon.svg is left
// untouched, page-less projects are skipped, and it never blocks the build. The
// SVG comes from the shared internal/favicon package so the PWA icon and this
// favicon are the same mark.

import (
	"encoding/json"
	"strings"

	"openfabrica/db/models"
	"openfabrica/engine/snapshotbuild"
	"openfabrica/internal/favicon"
	"openfabrica/store"
)

func (w *Worker) ensureFavicon(snap *snapshotbuild.ProjectSnapshot) {
	if snap == nil || len(snap.Pages) == 0 {
		return // no UI surface to decorate
	}
	db := w.pdb.Writer()
	if store.ArtifactExists(db, "file", "favicon.svg") {
		return // a model-, prior-build-, or enable_pwa-authored favicon wins
	}

	title := ""
	if project, err := models.GetProjectByID(w.deps.DB.DB, w.projectID); err == nil && project != nil {
		title = strings.TrimSpace(project.Name)
	}
	// The project's own primary when the plan committed one. A mark in a colour from a
	// fixed pool contradicts the art direction everything else was built to.
	primary := ""
	if planJSON, err := latestCompletedPlan(w.pdb); err == nil && planJSON != "" {
		var plan Plan
		if json.Unmarshal([]byte(planJSON), &plan) == nil && plan.DesignSystem != nil {
			primary = plan.DesignSystem.Colors.Primary
		}
	}
	a := &store.Artifact{
		Kind:        "file",
		Path:        "favicon.svg",
		Scope:       "global",
		ContentType: "image/svg+xml",
		Body:        favicon.SVGWithPrimary(title, primary),
	}
	if _, _, err := store.ArtifactSave(db, a, "platform:favicon"); err != nil {
		w.logger.Warn("favicon generation: save failed, build ships without a favicon", "error", err)
		return
	}
	w.logger.Info("generated platform favicon", "title", title)
}
