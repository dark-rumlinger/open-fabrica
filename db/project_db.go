package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"openfabrica/internal/diskpath"
	"openfabrica/internal/sqliteretry"
)

//go:embed project_migrations/*.sql
var projectMigrationFS embed.FS

// ProjectDB wraps a per-project SQLite database with a dedicated writer pool.
// The read pool allows concurrent reads via WAL mode; the writer pool (single
// connection) serializes all writes at the connection level, eliminating
// SQLITE_BUSY without a Go-level mutex. The reader is unexported to prevent
// accidental writes through the read pool: all writes MUST go through Writer(),
// ExecWrite(), or BeginWriteTx().
type ProjectDB struct {
	reader    *sql.DB // read pool (concurrent reads), use Reader() to access
	writer    *sql.DB // write pool (single connection, serializes writes)
	ProjectID int
}

// Reader returns the multi-connection read pool.
func (s *ProjectDB) Reader() *sql.DB {
	return s.reader
}

// Writer returns the single-connection write pool for direct use.
func (s *ProjectDB) Writer() *sql.DB {
	return s.writer
}

// Query delegates to the read pool.
func (s *ProjectDB) Query(query string, args ...any) (*sql.Rows, error) {
	return s.reader.Query(query, args...)
}

// QueryRow delegates to the read pool.
func (s *ProjectDB) QueryRow(query string, args ...any) *sql.Row {
	return s.reader.QueryRow(query, args...)
}

// ExecWrite executes a write query through the serialized writer pool with retry.
func (s *ProjectDB) ExecWrite(query string, args ...any) (sql.Result, error) {
	return sqliteretry.Exec(s.writer, query, args...)
}

// QueryRowWriter executes a query on the writer pool that returns a single row.
func (s *ProjectDB) QueryRowWriter(query string, args ...any) *sql.Row {
	return s.writer.QueryRow(query, args...)
}

// ProjectDBManager handles the lifecycle of per-project databases.
type ProjectDBManager struct {
	mu      sync.RWMutex
	dataDir string
	dbs     map[int]*ProjectDB
}

// NewProjectDBManager creates a manager for per-project databases.
func NewProjectDBManager(dataDir string) *ProjectDBManager {
	return &ProjectDBManager{
		dataDir: dataDir,
		dbs:     make(map[int]*ProjectDB),
	}
}

// DataDir returns the configured instance data directory. Exposed so the tool
// loops can hand it to ToolContext.DataDir: the tools write project files, this
// manager decides where a project lives, and both must agree.
func (m *ProjectDBManager) DataDir() string {
	return m.dataDir
}

// ProjectDir returns the on-disk directory holding everything for a project: its
// database, uploaded and generated files, and export snapshots. Exported as the
// single source of that layout so HTTP handlers and disk-writing tools agree;
// rebuilding the path elsewhere silently disagrees when DataDir is not the default.
func (m *ProjectDBManager) ProjectDir(projectID int) string {
	return filepath.Join(m.dataDir, "projects", fmt.Sprintf("%d", projectID))
}

// dbPath returns the filesystem path for a project's database.
func (m *ProjectDBManager) dbPath(projectID int) string {
	return filepath.Join(m.ProjectDir(projectID), "project.db")
}

// Exists reports whether a project database is open or present on disk. Open
// creates a missing database, so a caller handling an arbitrary id from a request
// checks this first rather than conjuring an empty project for it.
func (m *ProjectDBManager) Exists(projectID int) bool {
	m.mu.RLock()
	_, open := m.dbs[projectID]
	m.mu.RUnlock()
	if open {
		return true
	}
	_, err := os.Stat(m.dbPath(projectID))
	return err == nil
}

// Open opens (or returns a cached) project database. Runs migrations on first open.
func (m *ProjectDBManager) Open(projectID int) (*ProjectDB, error) {
	m.mu.RLock()
	if sdb, ok := m.dbs[projectID]; ok {
		m.mu.RUnlock()
		return sdb, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock.
	if sdb, ok := m.dbs[projectID]; ok {
		return sdb, nil
	}

	sdb, err := m.openProjectDB(projectID)
	if err != nil {
		return nil, err
	}

	m.dbs[projectID] = sdb
	return sdb, nil
}

// Create creates a new project database and runs migrations.
func (m *ProjectDBManager) Create(projectID int) (*ProjectDB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sdb, ok := m.dbs[projectID]; ok {
		return sdb, nil
	}

	// Ensure the project directory exists.
	dbPath := m.dbPath(projectID)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("creating project db directory: %w", err)
	}

	sdb, err := m.openProjectDB(projectID)
	if err != nil {
		return nil, err
	}

	m.dbs[projectID] = sdb
	slog.Info("Project database created", "project_id", projectID, "path", dbPath)
	return sdb, nil
}

// EphemeralResetStmts deletes per-instance state that should not survive a project
// copy or export (logs, chat, jobs, analytics), while authored content (artifacts,
// endpoints, dynamic tables, secrets, plan) is preserved. Shared by CopyFrom and
// the export path so all duplication routes clear the same slate.
var EphemeralResetStmts = []string{
	"DELETE FROM of_chat_messages",
	"DELETE FROM of_jobs",
	"DELETE FROM of_task_runs",
	// of_runs itself is preserved: its latest row carries plan_json and the
	// completed/live status, which a copied project must inherit. Only the bulky
	// per-call/per-event history is reset.
	"DELETE FROM of_llm_calls",
	"DELETE FROM of_llm_cache",
	"DELETE FROM of_run_log",
	"DELETE FROM of_sessions",
	"DELETE FROM of_flow_budget",
	"DELETE FROM of_edit_failures",
	"DELETE FROM of_activity_log",
	"DELETE FROM of_runtime_errors",
	"DELETE FROM of_notifications",
	"DELETE FROM of_analytics",
	"DELETE FROM of_triggers",
	"DELETE FROM of_questions",
	"DELETE FROM of_answers",
	// Revocations name users of the source project; a copy or export carries none of
	// that history.
	"DELETE FROM of_token_revocations",
}

// SnapshotTo writes a consistent standalone copy of a project's database to
// destPath via SQLite's VACUUM INTO: safe with concurrent readers, fully
// checkpointed (no -wal/-shm sidecars). destPath must not already exist. Used by
// the export path to capture the per-project DB before sanitising it.
func (m *ProjectDBManager) SnapshotTo(projectID int, destPath string) error {
	srcDB, err := m.Open(projectID)
	if err != nil {
		return fmt.Errorf("opening project %d: %w", projectID, err)
	}
	if _, err := srcDB.ExecWrite("VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("vacuum into %s: %w", destPath, err)
	}
	return nil
}

// CopyFrom duplicates an existing project's database and on-disk assets into a
// new project slot. The source DB is snapshotted via VACUUM INTO (consistent even
// with active writers); auxiliary subdirectories (assets, files, blobs) are copied
// recursively. Ephemeral per-instance state is cleared in the new DB while authored
// content is preserved (see EphemeralResetStmts).
func (m *ProjectDBManager) CopyFrom(sourceID, newID int) (*ProjectDB, error) {
	srcDB, err := m.Open(sourceID)
	if err != nil {
		return nil, fmt.Errorf("opening source project %d: %w", sourceID, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.dbs[newID]; ok {
		return nil, fmt.Errorf("target project %d is already open", newID)
	}

	srcDir := m.ProjectDir(sourceID)
	dstDir := m.ProjectDir(newID)
	dstDBPath := filepath.Join(dstDir, "project.db")

	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return nil, fmt.Errorf("creating target project dir: %w", err)
	}

	cleanupOnError := func(cause error) error {
		_ = os.RemoveAll(dstDir)
		return cause
	}

	// Snapshot the source DB into the target path. VACUUM INTO is safe with
	// concurrent readers and produces a fully checkpointed standalone file.
	if _, err := srcDB.ExecWrite("VACUUM INTO ?", dstDBPath); err != nil {
		return nil, cleanupOnError(fmt.Errorf("vacuum into target db: %w", err))
	}

	// Copy auxiliary subdirectories (assets/, files/, blobs/, ...).
	// Skip the project.db file we just wrote and any SQLite sidecars.
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, cleanupOnError(fmt.Errorf("reading source dir: %w", err))
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "project.db") {
			continue
		}
		srcPath := filepath.Join(srcDir, name)
		dstPath := filepath.Join(dstDir, name)
		if err := copyPath(srcPath, dstPath); err != nil {
			return nil, cleanupOnError(fmt.Errorf("copying %s: %w", name, err))
		}
	}

	// Open the new DB. runProjectMigrations is idempotent (CREATE TABLE IF
	// NOT EXISTS) so it's a no-op for the copied schema.
	sdb, err := m.openProjectDB(newID)
	if err != nil {
		return nil, cleanupOnError(err)
	}

	// Reset ephemeral / per-instance state in the new DB. These rows would be
	// confusing to inherit (logs, chat, jobs, analytics tied to the source).
	for _, stmt := range EphemeralResetStmts {
		if _, err := sdb.ExecWrite(stmt); err != nil {
			// Tolerate missing tables; not all schema versions populate every table.
			if !strings.Contains(err.Error(), "no such table") {
				sdb.writer.Close()
				sdb.reader.Close()
				delete(m.dbs, newID)
				return nil, cleanupOnError(fmt.Errorf("resetting state (%s): %w", stmt, err))
			}
		}
	}

	// Stored file paths still name the source project; point them at the copy.
	if err := diskpath.RemapProject(sdb, sourceID, dstDir); err != nil {
		sdb.writer.Close()
		sdb.reader.Close()
		delete(m.dbs, newID)
		return nil, cleanupOnError(fmt.Errorf("relinking copied files: %w", err))
	}

	m.dbs[newID] = sdb
	slog.Info("project database copied", "source_id", sourceID, "new_id", newID, "path", dstDBPath)
	return sdb, nil
}

// copyPath recursively copies a file or directory from src to dst.
func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	// Regular file.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Delete closes a project's database and removes the DB file from disk.
func (m *ProjectDBManager) Delete(projectID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sdb, ok := m.dbs[projectID]; ok {
		sdb.writer.Close()
		sdb.reader.Close()
		delete(m.dbs, projectID)
	}

	// Remove the entire project directory (includes project.db, assets, files).
	projectDir := m.ProjectDir(projectID)
	if err := os.RemoveAll(projectDir); err != nil {
		return fmt.Errorf("removing project directory: %w", err)
	}

	slog.Info("project database deleted", "project_id", projectID)
	return nil
}

// Get returns a cached project database or nil if not open.
func (m *ProjectDBManager) Get(projectID int) *ProjectDB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dbs[projectID]
}

// CloseAll closes all open project databases. Called on shutdown.
func (m *ProjectDBManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, sdb := range m.dbs {
		sdb.writer.Close()
		sdb.reader.Close()
		slog.Info("project database closed", "project_id", id)
	}
	m.dbs = make(map[int]*ProjectDB)
}

// OpenProjectIDs returns a list of all currently open project IDs.
func (m *ProjectDBManager) OpenProjectIDs() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]int, 0, len(m.dbs))
	for id := range m.dbs {
		ids = append(ids, id)
	}
	return ids
}

// openProjectDB opens the SQLite file and runs project migrations. Must be called with m.mu held.
func (m *ProjectDBManager) openProjectDB(projectID int) (*ProjectDB, error) {
	dbPath := m.dbPath(projectID)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("creating project db directory: %w", err)
	}

	readDB, writeDB, err := openReadWritePools(dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening project database %d: %w", projectID, err)
	}

	sdb := &ProjectDB{reader: readDB, writer: writeDB, ProjectID: projectID}

	if err := runProjectMigrations(sdb); err != nil {
		writeDB.Close()
		readDB.Close()
		return nil, fmt.Errorf("running project migrations for project %d: %w", projectID, err)
	}

	slog.Info("project database initialized", "project_id", projectID, "path", dbPath)
	return sdb, nil
}

// runProjectMigrations applies pending project-level migrations.
func runProjectMigrations(sdb *ProjectDB) error {
	return applyMigrations(sdb.reader, sdb.writer, projectMigrationFS, "project_migrations", "project_id", sdb.ProjectID)
}
