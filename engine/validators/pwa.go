package validators

// The static PWA-readiness validator: catches a build PLANNED as an installable
// PWA that never ran enable_pwa - the build agent can mark the task done with
// prose while never calling it, so nothing fires beforeinstallprompt. The
// backstop on the FINAL state behind manage_tasks' completion guard. PWA is
// "required" ONLY when a planned task explicitly declares the manage_project
// tool - never guessed from goal/design prose. When required, it verifies
// manifest.json and service-worker.js, plus the manifest <link> but only when a
// `default` layout exists to patch.

import (
	"context"
	"database/sql"
	"strings"

	"openfabrica/store"
)

type pwaReadinessValidator struct{}

func (pwaReadinessValidator) Name() string { return "pwa_readiness" }

func (pwaReadinessValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if !planRequiresPWA(pdb) {
		return nil
	}

	var missing []string
	if !store.ArtifactExists(pdb, "file", "manifest.json") {
		missing = append(missing, "manifest.json")
	}
	if !store.ArtifactExists(pdb, "file", "service-worker.js") {
		missing = append(missing, "service-worker.js")
	}
	// Require the manifest <link> only when a `default` layout exists to patch: a
	// layout-less single-page project has nothing to wire it into, so flagging it
	// there raises an issue no fix-pass can resolve. manifest.json + service-worker.js
	// still gate "did enable_pwa run".
	if hasDefaultLayout(pdb) && !layoutLinksManifest(pdb) {
		missing = append(missing, "the manifest <link> in the layout")
	}
	if len(missing) == 0 {
		return nil
	}

	return []Issue{{
		Severity:      "error",
		Category:      "pwa_not_enabled",
		Deterministic: true,
		Message: "This project is planned as an installable PWA but is missing " + strings.Join(missing, ", ") +
			". A browser will not offer to install the app without a linked manifest and a registered service worker, so the install prompt never appears. " +
			"Run `manage_project action=enable_pwa` as the FINAL build step (after every page is saved): it generates the manifest, service worker, and /offline page and patches the layout. Do not hand-write these files.",
	}}
}

// planRequiresPWA reports whether the persisted plan calls for an installable PWA:
// any planned task declaring the manage_project tool (the enable_pwa carrier). Fails
// closed to "not required" on any read/parse error, so a transient DB issue never
// fabricates a PWA requirement and loops the fix-pass.
func planRequiresPWA(pdb *sql.DB) bool {
	var planJSON sql.NullString
	if err := pdb.QueryRow(`SELECT plan_json FROM of_runs
		WHERE plan_json IS NOT NULL AND plan_json != ''
		ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON); err != nil {
		return false
	}
	if !planJSON.Valid || planJSON.String == "" {
		return false
	}
	plan, err := ParsePlan(planJSON.String)
	if err != nil {
		return false
	}
	for _, t := range plan.Tasks {
		for _, tool := range t.Tools {
			if strings.EqualFold(strings.TrimSpace(tool), "manage_project") {
				return true
			}
		}
	}
	// No prose fallback by design: a PWA is required only when the plan put an
	// explicit enable_pwa task in the checklist. Guessing from goal/design copy
	// over-reaches onto projects that merely mention "offline" or "home screen".
	return false
}

// hasDefaultLayout reports whether the project has a shared `default` layout -
// the artifact enable_pwa patches the manifest <link> into. Without it there is
// nothing to patch, so the <link> requirement is not enforced (see Validate).
func hasDefaultLayout(pdb *sql.DB) bool {
	var n int
	if err := pdb.QueryRow(
		"SELECT COUNT(*) FROM of_files WHERE kind = 'layout' AND path = 'default' AND is_deleted = 0",
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// layoutLinksManifest reports whether any layout body wires up the web app
// manifest. It checks for the "/manifest.json" reference enable_pwa patches into
// the default layout - the same marker enable_pwa uses to decide a layout is
// already patched - so the check and the action agree on what "wired" means.
func layoutLinksManifest(pdb *sql.DB) bool {
	// A nil map (read error) reports "not wired", the conservative answer.
	for _, body := range layoutBodies(pdb) {
		if strings.Contains(body, "/manifest.json") {
			return true
		}
	}
	return false
}

func init() {
	RegisterValidator(pwaReadinessValidator{})
}
