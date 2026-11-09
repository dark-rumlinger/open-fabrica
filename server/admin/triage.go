package admin

// Triage HTTP surface: a loopback-only, read-only sweep over every project's
// build history that classifies build anomalies and ranks the noisiest builds
// first. The analysis engine lives in internal/triage; this file is only the
// thin handler that parses the request, drives the scan, and writes the result.
//
// Deliberately NOT behind the admin JWT: mounted only for loopback peers
// (requireLoopback), matching the admin panel's own 127.0.0.1 trust boundary.
// It reads; it never writes.

import (
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"openfabrica/db/models"
	"openfabrica/internal/triage"
)

// TriageHandler serves the cross-project build-anomaly scan.
type TriageHandler struct {
	deps *Deps
}

// Scan sweeps every project's builds and returns a ranked anomaly report.
// GET /admin/api/triage
//
// Query params (all optional):
//
//	project=<id>   restrict to one project
//	build=<run id> restrict to one build (implies its project via project=)
//	limit=<n>      builds scanned per project, newest first (default 40)
//	all=1          include clean builds (default: only builds with findings)
//	raw=1&build=X  return the FULL build export for run X instead of the scan
//	format=text    render the scan as a report instead of JSON; requires
//	               project= or build=, since a text dump of every run reads as
//	               noise rather than an answer
func (h *TriageHandler) Scan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 40
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	includeClean := q.Get("all") == "1"
	buildFilter := strings.TrimSpace(q.Get("build"))
	asText := q.Get("format") == "text"
	_, hasProject := projectFilter(q)
	if asText && !hasProject && buildFilter == "" {
		writeError(w, http.StatusBadRequest, "format=text requires project=<id> or build=<run id>")
		return
	}

	central := h.deps.DB.DB
	projects, err := models.ListProjects(central)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list projects")
		return
	}
	if pid, ok := projectFilter(q); ok {
		filtered := projects[:0:0]
		for _, p := range projects {
			if p.ID == pid {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}

	// raw=1&build=<run id>: hand back the FULL build export (same payload the
	// JWT-gated Export endpoint serves) for one flagged build, so a drill-down
	// needs no login and no manual export. The owning project is located by id.
	if q.Get("raw") == "1" {
		h.serveRawExport(w, projects, buildFilter)
		return
	}

	open := func(projectID int) (*sql.DB, error) {
		pdb, err := h.deps.ProjectDBManager.Open(projectID)
		if err != nil {
			return nil, err
		}
		return pdb.Reader(), nil
	}
	report := triage.BuildReport(projects, open, h.deps.Logger, triage.Options{
		BuildFilter:  buildFilter,
		Limit:        limit,
		IncludeClean: includeClean,
	})

	if asText {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(triage.RenderText(report)))
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// projectFilter reads the project= param, reporting whether it was present and
// numeric.
func projectFilter(q url.Values) (int, bool) {
	pid, err := strconv.Atoi(q.Get("project"))
	return pid, err == nil
}

// serveRawExport finds the project owning runID and writes its full build export
// payload, reusing the Export handler's builder. projects is already narrowed by
// any ?project= filter.
func (h *TriageHandler) serveRawExport(w http.ResponseWriter, projects []models.Project, runID string) {
	if runID == "" {
		writeError(w, http.StatusBadRequest, "raw mode requires build=<run id>")
		return
	}
	for _, p := range projects {
		pdb, err := h.deps.ProjectDBManager.Open(p.ID)
		if err != nil {
			continue
		}
		var found string
		if pdb.Reader().QueryRow(`SELECT id FROM of_runs WHERE id = ?`, runID).Scan(&found) != nil {
			continue // not this project's run
		}
		payload, err := (&BuildsHandler{deps: h.deps}).exportPayload(pdb, runID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to build export")
			return
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}
	writeError(w, http.StatusNotFound, "build not found in any project")
}
