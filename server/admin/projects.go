package admin

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/projectident"
	"openfabrica/tlsproxy"
)

// ProjectsHandler handles project CRUD endpoints.
type ProjectsHandler struct {
	deps *Deps
}

type createProjectRequest struct {
	Name        string  `json:"name"`
	Domain      *string `json:"domain"`
	Description *string `json:"description"`
	LLMModelID  int     `json:"llm_model_id"`
	// AmbitionLevel tunes how expansive EXPAND and PLAN are: 'off' (skip
	// EXPAND), 'low', 'medium', or 'high'. Pointer so an absent field is
	// distinguishable from an explicit value; unset (or unrecognised) falls
	// back to the default ('medium') via engine.NormalizeAmbition.
	AmbitionLevel *string `json:"ambition_level,omitempty"`
	// UserFlows turns the project's user-flow tests (of_test.js) on or off. Pointer so
	// an absent field keeps the default, which is off (the feature is experimental).
	UserFlows *bool `json:"user_flows,omitempty"`
}

type updateProjectRequest struct {
	Name        string  `json:"name"`
	Domain      *string `json:"domain"`
	Description *string `json:"description"`
	LLMModelID  int     `json:"llm_model_id"`
	// BannedIPs is the owner-managed block list of IP addresses and CIDR
	// ranges. Pointer so an absent field leaves the existing list untouched;
	// an explicit empty array clears it.
	BannedIPs *[]string `json:"banned_ips,omitempty"`
	// CSP is an optional per-project Content-Security-Policy override for the
	// public site. Pointer so an absent field leaves it untouched; an explicit
	// empty string clears it (back to the instance default).
	CSP *string `json:"csp,omitempty"`
	// UserFlows turns the project's user-flow tests on or off. Pointer so an absent
	// field leaves the setting untouched.
	UserFlows *bool `json:"user_flows,omitempty"`
}

// List returns all projects, with optional pagination via ?limit=&offset= query params.
func (h *ProjectsHandler) List(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	// If limit is provided, use paginated query.
	if limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			limit = 50
		}
		offset := 0
		if offsetStr != "" {
			offset, _ = strconv.Atoi(offsetStr)
			if offset < 0 {
				offset = 0
			}
		}

		projects, total, err := models.ListProjectsPaginated(h.deps.DB.DB, limit, offset)
		if err != nil {
			h.deps.serverError(w, "failed to list projects", err)
			return
		}
		if projects == nil {
			projects = []models.Project{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": h.deps.viewProjects(projects),
			"total": total,
		})
		return
	}

	// Default: return all projects (used by dashboard, sidebar, etc.)
	projects, err := models.ListProjects(h.deps.DB.DB)
	if err != nil {
		h.deps.serverError(w, "failed to list projects", err)
		return
	}

	if projects == nil {
		projects = []models.Project{}
	}

	writeJSON(w, http.StatusOK, h.deps.viewProjects(projects))
}

// Create creates a new project.
func (h *ProjectsHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Blank name (and optionally domain) means the user wants us to mint one.
	// Generate a friendly, collision-checked adjective-noun identity. The
	// domain is taken from the same pair unless the user supplied their own.
	nameWasGenerated := req.Name == ""
	if req.Name == "" {
		genName, genDomain, genErr := projectident.Generate(h.deps.DB.DB)
		if genErr != nil {
			h.deps.Logger.Error("failed to generate project identity", "error", genErr)
			writeError(w, http.StatusInternalServerError, "failed to generate a project name")
			return
		}
		req.Name = genName
		if req.Domain == nil || *req.Domain == "" {
			req.Domain = &genDomain
		}
	}
	if req.LLMModelID <= 0 {
		// No model specified, try to use the system default.
		defModel, _, defErr := models.GetDefaultModel(h.deps.DB.DB)
		if defErr != nil {
			writeError(w, http.StatusBadRequest, "no model selected and no system default configured, please select a provider and model")
			return
		}
		req.LLMModelID = defModel.ID
	} else {
		// Verify the specified model actually exists.
		if _, err := models.GetModelByID(h.deps.DB.DB, req.LLMModelID); err != nil {
			writeError(w, http.StatusBadRequest, "selected model does not exist")
			return
		}
	}

	// Normalize empty domain to NULL so the UNIQUE constraint doesn't conflict.
	if req.Domain != nil && *req.Domain == "" {
		req.Domain = nil
	}

	ambitionLevel := engine.DefaultAmbition
	if req.AmbitionLevel != nil {
		ambitionLevel = engine.NormalizeAmbition(*req.AmbitionLevel)
	}
	project, err := models.CreateProject(h.deps.DB.DB, req.Name, req.Domain, req.Description, req.LLMModelID, nil, ambitionLevel)
	if err != nil {
		h.deps.Logger.Error("failed to create project", "error", err)
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "a project with this domain already exists")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to create project")
		}
		return
	}
	// The project will build with this model: settle what its listing left unknown
	// (tool use, reasoning) while the owner is still writing the brief.
	probeModelInBackground(h.deps, req.LLMModelID)

	if req.UserFlows != nil {
		if err := models.SetProjectUserFlows(h.deps.DB.DB, project.ID, *req.UserFlows); err != nil {
			h.deps.Logger.Warn("failed to store the user-flows setting", "project_id", project.ID, "error", err)
		}
	}

	// Remember that this name was auto-minted so the engine can replace it with
	// an LLM-generated, description-derived title before the build's first stage.
	if nameWasGenerated {
		if err := models.SetProjectNameAutoGenerated(h.deps.DB.DB, project.ID, true); err != nil {
			h.deps.Logger.Warn("failed to flag project name as auto-generated", "project_id", project.ID, "error", err)
		}
	}

	// Create the per-project database.
	if _, err := h.deps.ProjectDBManager.Create(project.ID); err != nil {
		h.deps.Logger.Error("failed to create project db", "project_id", project.ID, "error", err)
		// Clean up the global row since the project DB failed.
		_ = models.DeleteProject(h.deps.DB.DB, project.ID)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create project database: %v", err))
		return
	}

	// The project-created event starts the first build, so record the language first.
	h.deps.recordReplyLanguage(r, project.ID)

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProjectCreated, project.ID, map[string]any{
			"name": project.Name,
		}))
	}

	writeJSON(w, http.StatusCreated, h.deps.viewProject(project))
}

type copyProjectRequest struct {
	Name   string  `json:"name"`
	Domain *string `json:"domain"`
}

// Copy duplicates an existing project (DB + on-disk assets) into a new project.
// The new project keeps the source's description and llm_model_id but gets a
// fresh activity slate (chat/jobs/logs cleared) and an independent DB.
func (h *ProjectsHandler) Copy(w http.ResponseWriter, r *http.Request) {
	sourceID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	var req copyProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Domain != nil && *req.Domain == "" {
		req.Domain = nil
	}

	source, err := models.GetProjectByID(h.deps.DB.DB, sourceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "source project not found")
		return
	}

	project, err := models.CreateProject(h.deps.DB.DB, req.Name, req.Domain, source.Description, source.LLMModelID, &sourceID, source.AmbitionLevel)
	if err != nil {
		h.deps.Logger.Error("failed to create project", "error", err)
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "a project with this domain already exists")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to create project")
		}
		return
	}

	// A copy keeps the source's choice about user-flow tests.
	if err := models.SetProjectUserFlows(h.deps.DB.DB, project.ID, models.ProjectUserFlowsEnabled(source)); err != nil {
		h.deps.Logger.Warn("failed to copy the user-flows setting", "project_id", project.ID, "error", err)
	}

	if _, err := h.deps.ProjectDBManager.CopyFrom(sourceID, project.ID); err != nil {
		h.deps.Logger.Error("failed to copy project db", "source_id", sourceID, "new_id", project.ID, "error", err)
		_ = models.DeleteProject(h.deps.DB.DB, project.ID)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to copy project: %v", err))
		return
	}

	// A copy of a finished project is itself finished: the build artifacts and
	// pipeline state come over verbatim. Inherit the live mode so the copy does
	// not re-enter the build pipeline against an already-complete task queue.
	if source.Mode == "live" {
		if err := models.UpdateProjectMode(h.deps.DB.DB, project.ID, "live"); err != nil {
			h.deps.Logger.Warn("failed to set copied project mode", "id", project.ID, "error", err)
		} else {
			project.Mode = "live"
		}
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProjectCreated, project.ID, map[string]any{
			"name":              project.Name,
			"source_project_id": sourceID,
		}))
	}

	writeJSON(w, http.StatusCreated, h.deps.viewProject(project))
}

// Get returns a single project by ID.
func (h *ProjectsHandler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	writeJSON(w, http.StatusOK, h.deps.viewProject(project))
}

// Update updates an existing project.
func (h *ProjectsHandler) Update(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	var req updateProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	if req.LLMModelID <= 0 {
		writeError(w, http.StatusBadRequest, "llm_model_id is required")
		return
	}

	if req.Domain != nil && *req.Domain == "" {
		req.Domain = nil
	}

	// Validate the ban list up front so a malformed entry rejects the whole
	// save instead of half-applying it.
	var normalizedBans []string
	if req.BannedIPs != nil {
		n, err := normalizeBannedIPs(*req.BannedIPs)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		normalizedBans = n
	}

	if err := models.UpdateProject(h.deps.DB.DB, projectID, req.Name, req.Domain, req.Description, req.LLMModelID); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "that domain is already in use by another project")
			return
		}
		h.deps.serverError(w, "failed to update project", err)
		return
	}

	if req.BannedIPs != nil {
		if err := models.SetProjectBannedIPs(h.deps.DB.DB, projectID, normalizedBans); err != nil {
			h.deps.serverError(w, "failed to update banned IPs", err)
			return
		}
	}

	if req.CSP != nil {
		if err := models.SetProjectCSP(h.deps.DB.DB, projectID, *req.CSP); err != nil {
			h.deps.serverError(w, "failed to update CSP", err)
			return
		}
	}

	if req.UserFlows != nil {
		if err := models.SetProjectUserFlows(h.deps.DB.DB, projectID, *req.UserFlows); err != nil {
			h.deps.serverError(w, "failed to update the user-flows setting", err)
			return
		}
	}

	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project updated but failed to reload")
		return
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProjectUpdated, projectID, map[string]any{
			"name": project.Name,
		}))
	}

	// When a custom domain is set, bring HTTPS up synchronously so a bind
	// failure (ports 80/443 owned by another web server) is reported here at
	// assignment time instead of being swallowed in a background log line. The
	// domain is persisted regardless; only the TLS activation can warn.
	if project.Domain != nil && *project.Domain != "" &&
		h.deps.TLSManager != nil && h.deps.Config != nil && !h.deps.Config.TLSDisabled {
		if err := h.deps.TLSManager.Reload(); err != nil {
			writeJSON(w, http.StatusOK, struct {
				projectView
				TLSWarning string `json:"tls_warning"`
			}{h.deps.viewProject(project), fmt.Sprintf("Domain saved, but HTTPS could not start: %v. Set OPENFABRICA_TLS_DISABLED=1 if another web server owns ports 80/443.", err)})
			return
		}
	}

	writeJSON(w, http.StatusOK, h.deps.viewProject(project))
}

// normalizeBannedIPs trims and validates a list of IP addresses and CIDR
// ranges, dropping blanks. It returns an error naming the first invalid entry
// so the owner sees exactly what to fix.
func normalizeBannedIPs(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			if _, _, err := net.ParseCIDR(e); err != nil {
				return nil, fmt.Errorf(msgcat.Mark("invalid IP range %q"), e)
			}
		} else if net.ParseIP(e) == nil {
			return nil, fmt.Errorf(msgcat.Mark("invalid IP address %q"), e)
		}
		out = append(out, e)
	}
	return out, nil
}

// Summary returns aggregated stats for the project home page.
func (h *ProjectsHandler) Summary(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	type summary struct {
		TotalTokens       int    `json:"total_tokens"`
		BrainActions      int    `json:"brain_actions"`
		Model             string `json:"model"`
		PageViews         int    `json:"page_views"`
		UniqueVisitors    int    `json:"unique_visitors"`
		PagesCount        int    `json:"pages_count"`
		EngineTokensToday int    `json:"engine_tokens_today"`
		DailyTokenBudget  int    `json:"daily_token_budget"`
	}

	var s summary

	// Token usage + engine call count (per-project DB). of_llm_calls logs both
	// engine and chat calls, tagged by `source`: total tokens count all LLM spend,
	// while brain_actions stays scoped to engine calls so live-chat turns don't
	// inflate the build action count.
	_ = projectDB.QueryRow(
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0),
		        COALESCE(SUM(CASE WHEN source = 'engine' THEN 1 ELSE 0 END), 0)
		 FROM of_llm_calls`,
	).Scan(&s.TotalTokens, &s.BrainActions)

	// Today's engine spend vs the configured daily ceiling, so the owner can see
	// how close the autonomous build/self-repair loop is to pausing. Budget 0 =
	// unlimited; the field is still returned so the UI can show "X used today".
	_ = projectDB.QueryRow(
		`SELECT COALESCE(SUM(input_tokens + output_tokens + cache_read_input_tokens + cache_creation_input_tokens), 0)
		 FROM of_llm_calls WHERE source = 'engine' AND created_at >= date('now')`,
	).Scan(&s.EngineTokensToday)
	if h.deps.Config != nil {
		s.DailyTokenBudget = h.deps.Config.EngineDailyTokenBudget
	}

	// Page views + unique visitors (last 24h) (per-project DB)
	_ = projectDB.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT visitor_hash)
		 FROM of_analytics WHERE created_at > datetime('now', '-1 day')`,
	).Scan(&s.PageViews, &s.UniqueVisitors)

	// Published pages count (per-project DB)
	_ = projectDB.QueryRow(
		`SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND is_deleted = 0`,
	).Scan(&s.PagesCount)

	// Configured model (global DB): the model the engine runs for this project,
	// shown next to the token spend so the owner can see what's using them.
	_ = h.deps.DB.DB.QueryRow(
		`SELECT m.display_name FROM projects p
		   JOIN llm_models m ON m.id = p.llm_model_id
		  WHERE p.id = ?`,
		projectID,
	).Scan(&s.Model)

	writeJSON(w, http.StatusOK, s)
}

// Delete removes a project and all its data.
func (h *ProjectsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	// Stop the engine and job workers before deleting data so neither keeps
	// issuing queries against the project database once its pools are closed.
	// The delete proceeds either way - the DB/file teardown below is the
	// authoritative cleanup - but the two failure modes are not the same event
	// and must not share a log level: no worker is the ordinary idle-project
	// case, while a timeout means a LIVE worker is about to have its project row
	// and database deleted underneath it, and every error it then logs traces
	// back to here.
	if h.deps.Engine != nil {
		switch err := h.deps.Engine.StopProject(projectID); {
		case err == nil:
		case errors.Is(err, engine.ErrNoWorker):
			h.deps.Logger.Debug("no engine worker to stop before delete", "project_id", projectID)
		default:
			h.deps.Logger.Error("engine worker did not stop before delete; it will keep running against a deleted project until it returns",
				"project_id", projectID, "error", err)
		}
	}
	if h.deps.Jobs != nil {
		h.deps.Jobs.StopProject(projectID)
	}

	// Delete via serialized writer to ensure the row is actually removed
	// before we clean up files. Using raw Exec can race with other writes.
	res, err := h.deps.DB.ExecWrite("DELETE FROM projects WHERE id = ?", projectID)
	if err != nil {
		h.deps.serverError(w, "failed to delete project", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	// Remove the per-project database.
	if err := h.deps.ProjectDBManager.Delete(projectID); err != nil {
		h.deps.Logger.Error("failed to delete project db", "project_id", projectID, "error", err)
		// Project row already deleted; log but don't fail the request.
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProjectDeleted, projectID, nil))
	}

	writeStatus(w, "deleted")
}

type publishRequest struct {
	Published bool `json:"published"`
}

// SetPublished publishes or unpublishes a project. Unpublished projects stop
// being served; the builder is unaffected.
func (h *ProjectsHandler) SetPublished(w http.ResponseWriter, r *http.Request) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return
	}

	var req publishRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if _, err := models.GetProjectByID(h.deps.DB.DB, projectID); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	if err := models.SetProjectPublished(h.deps.DB.DB, projectID, req.Published); err != nil {
		h.deps.Logger.Error("failed to set project published", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update project")
		return
	}

	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil || project == nil {
		h.deps.Logger.Error("failed to reload project after publish change", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load project")
		return
	}

	// Announce so the TLS proxy reloads and stops or starts serving the domain.
	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventProjectUpdated, projectID, map[string]any{
			"name":      project.Name,
			"published": project.Published,
		}))
	}

	writeJSON(w, http.StatusOK, h.deps.viewProject(project))
}

// ValidateDomain checks if a domain is properly configured for HTTPS.
// Returns the validation result with any warnings or errors.
func (h *ProjectsHandler) ValidateDomain(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		writeError(w, http.StatusBadRequest, "domain query parameter is required")
		return
	}

	check := tlsproxy.ValidateDomain(domain)
	warnings := make([]string, len(check.Warnings))
	for i, w := range check.Warnings {
		warnings[i] = msgcat.Localize(r, w)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"domain":          check.Domain,
		"valid":           check.Valid,
		"https_available": check.HTTPSAvailable,
		"warnings":        warnings,
		"error":           msgcat.Localize(r, check.Error),
		"message":         check.FormatCheckResult(),
	})
}
