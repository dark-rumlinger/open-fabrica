package content

// Per-page <title> backstop. The PLAN stage authors a display-ready title for
// every page (PageSpec.Title) and persists the plan on the run (of_runs). BUILD
// saves pages via manage_files, but ArtifactSave overwrites metadata, so a
// page saved without metadata.title would render with the project name. We seed
// the planned title here when the model omits one.
//
// The plan is read with a self-contained SQL query and a minimal struct rather
// than importing the engine: the engine registers these content tools, so
// importing it back would create a cycle. We only need {pages:[{path,title}]}.

import (
	"encoding/json"

	"openfabrica/internal/dbh"
)

// planTitleForPath returns the PLAN stage's title for the page at path, or ""
// when there is no plan, no matching page, or any read/parse error. The match
// is exact on the route template (both PageSpec.Path and the saved page path
// use forms like "/blog/:slug"). All failure modes return "" so the caller
// simply falls through to the existing project-name fallback.
func planTitleForPath(db dbh.Handle, path string) string {
	if db == nil || path == "" {
		return ""
	}
	var planJSON string
	err := db.QueryRow(`SELECT COALESCE(plan_json, '') FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON)
	if err != nil || planJSON == "" {
		return ""
	}
	var plan struct {
		Pages []struct {
			Path  string `json:"path"`
			Title string `json:"title"`
		} `json:"pages"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return ""
	}
	for _, p := range plan.Pages {
		if p.Path == path {
			return p.Title
		}
	}
	return ""
}
