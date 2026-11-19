package admin

import (
	"net/http"
	"runtime"
	"strconv"

	"openfabrica/store"
)

// ProjectDiagnosticsHandler handles health/integrity/errors for admin.
type ProjectDiagnosticsHandler struct {
	deps *Deps
}

// Health returns runtime and project-level stats.
func (h *ProjectDiagnosticsHandler) Health(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	writeJSON(w, http.StatusOK, map[string]any{
		"runtime": map[string]any{
			"go_version": runtime.Version(),
			"goroutines": runtime.NumGoroutine(),
			"cpus":       runtime.NumCPU(),
			"os":         runtime.GOOS,
			"arch":       runtime.GOARCH,
		},
		"memory": map[string]any{
			"alloc_mb":    mem.Alloc / 1024 / 1024,
			"total_alloc": mem.TotalAlloc / 1024 / 1024,
			"sys_mb":      mem.Sys / 1024 / 1024,
			"num_gc":      mem.NumGC,
		},
		"project": store.LoadProjectHealthCounts(projectDB),
	})
}

// Integrity checks layout, pages, and asset consistency.
func (h *ProjectDiagnosticsHandler) Integrity(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	issues := store.CheckProjectIntegrity(projectDB)
	writeJSON(w, http.StatusOK, map[string]any{
		"issues": issues,
		"ok":     len(issues) == 0,
	})
}

// Errors returns recent failure entries from of_run_log.
func (h *ProjectDiagnosticsHandler) Errors(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	writeJSON(w, http.StatusOK, store.ListStageErrorLogs(projectDB, limit))
}
