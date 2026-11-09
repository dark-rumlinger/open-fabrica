package admin

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/internal/httpx"
)

// collectRows is a package-local alias for db.CollectRows, kept terse for the
// many admin list handlers. The implementation (and the debug log on a skipped
// row) lives in one place now; see db.CollectRows.
func collectRows[T any](rows *sql.Rows, scan func(*sql.Rows) (T, error)) []T {
	return db.CollectRows(rows, scan)
}

// colVal pairs a column with the value to write to it in a partial UPDATE.
type colVal struct {
	col string
	val any
}

// buildUpdate assembles a partial UPDATE from the given column/value pairs:
//
//	UPDATE <table> SET c1 = ?, c2 = ? WHERE <whereCol> = ?
//
// with args in the same order plus whereVal appended. Callers decide which
// fields to include (pass a nil val to write SQL NULL). It is the one assembler
// for a simple partial update, so no handler carries its own setClauses/values
// slice pair. Returns ok=false when there are no fields to update. Handlers that
// need literal SET expressions or extra WHERE conditions keep their own assembly.
func buildUpdate(table string, fields []colVal, whereCol string, whereVal any) (query string, args []any, ok bool) {
	if len(fields) == 0 {
		return "", nil, false
	}
	sets := make([]string, len(fields))
	args = make([]any, 0, len(fields)+1)
	for i, f := range fields {
		sets[i] = f.col + " = ?"
		args = append(args, f.val)
	}
	args = append(args, whereVal)
	return "UPDATE " + table + " SET " + strings.Join(sets, ", ") + " WHERE " + whereCol + " = ?", args, true
}

// writeJSON, writeError and decodeJSON are package-local aliases for the
// canonical helpers in internal/httpx, kept so the many admin call sites read
// tersely. The implementation (and the {"error":...} wire shape) lives in one
// place now; see internal/httpx.
func writeJSON(w http.ResponseWriter, status int, v any)       { httpx.WriteJSON(w, status, v) }
func writeError(w http.ResponseWriter, status int, msg string) { httpx.WriteError(w, status, msg) }
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return httpx.DecodeJSON(w, r, v)
}

// maxAdminBodySize is the admin body cap, aliased to httpx.MaxBodySize for the
// handlers that set http.MaxBytesReader themselves (file upload, bot).
const maxAdminBodySize = httpx.MaxBodySize

// serverError logs err under msg and answers a plain 500 carrying the same
// message - the one-call form of the dominant handler failure tail. When the
// log line should carry more context than the response leaks, keep the
// explicit Logger.Error + writeError pair instead.
func (d *Deps) serverError(w http.ResponseWriter, msg string, err error) {
	d.Logger.Error(msg, "error", err)
	writeError(w, http.StatusInternalServerError, msg)
}

// writeStatus answers 200 with the {"status": s} envelope the admin mutation
// endpoints use for a result with nothing else to report.
func writeStatus(w http.ResponseWriter, s string) {
	writeJSON(w, http.StatusOK, map[string]string{"status": s})
}

// requireProjectID extracts and validates the {projectID} URL param, writing a
// 400 and returning ok=false on a malformed id. Use this for handlers that need
// only the id (engine control, global-DB operations); use requireProjectDB when
// the per-project database is also needed.
func requireProjectID(w http.ResponseWriter, r *http.Request) (int, bool) {
	return requireIntParam(w, r, "projectID", "invalid project ID")
}

// requireIntParam extracts and validates an integer {param} URL param, writing a
// 400 with msg and returning ok=false on a malformed value. The sibling of
// requireProjectID for the many handlers that also carry an entity id
// (endpoint, file, component, provider, task, …) in the route.
func requireIntParam(w http.ResponseWriter, r *http.Request, param, msg string) (int, bool) {
	id, err := strconv.Atoi(chi.URLParam(r, param))
	if err != nil {
		writeError(w, http.StatusBadRequest, msg)
		return 0, false
	}
	return id, true
}

// requireProjectDB extracts the projectID from the URL, opens the project database,
// and returns both. Writes an error response and returns (0, nil) on failure.
func requireProjectDB(w http.ResponseWriter, r *http.Request, mgr *db.ProjectDBManager) (int, *db.ProjectDB) {
	projectID, ok := requireProjectID(w, r)
	if !ok {
		return 0, nil
	}
	projectDB := openProjectDB(w, mgr, projectID)
	if projectDB == nil {
		return 0, nil
	}
	return projectID, projectDB
}

// openProjectDB opens a project's DB by id, writing the standard 500 and
// returning nil on failure - the mid-function counterpart to requireProjectDB
// for handlers that already hold a validated project id. Call sites with
// bespoke failure handling (skip-and-continue loops, import cleanup) open the
// manager directly.
func openProjectDB(w http.ResponseWriter, mgr *db.ProjectDBManager, projectID int) *db.ProjectDB {
	// Open creates a database for any id it is given; an id with no project must
	// answer 404 instead of leaving an empty project directory behind (a stale poll
	// after a delete would otherwise resurrect it).
	if !mgr.Exists(projectID) {
		writeError(w, http.StatusNotFound, "project not found")
		return nil
	}
	projectDB, err := mgr.Open(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open project database")
		return nil
	}
	return projectDB
}
