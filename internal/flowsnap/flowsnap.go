// Package flowsnap lets user-flow replays (of_test.js) run against a project's
// real data and then puts that data back. Flows place orders, cancel bookings,
// upload files and advance statuses; left in place, those writes pile up as junk
// and change the data the next replay starts from, so the same script passes one
// run and fails the next.
//
// Take records the app's state before a replay: every app-owned table (plus the
// platform tables app traffic writes to), their AUTOINCREMENT counters, and the
// project's files. Restore undoes whatever changed since: rows the replay added are
// deleted, rows it changed or deleted are put back, counters are reset, new files
// are removed and removed or replaced files are restored. Platform tables the engine
// writes during a replay (run log, LLM calls, the build's own files) are never
// touched.
//
// Restore also reverts writes real users make while a replay runs, so a replay on a
// project with a public domain first checks for recent real traffic (see Open).
package flowsnap

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"openfabrica/db"

	"openfabrica/security"
)

// runtimeTables are platform tables that app traffic (not the build) writes to, so
// a replay's writes land there too: page views, errors the app reported, LLM token
// counters, sessions and tokens, tenancy, subscriptions, notifications.
var runtimeTables = map[string]bool{
	"of_analytics":          true,
	"of_runtime_errors":     true,
	"of_llm_usage_daily":    true,
	"of_llm_endpoints":      true, // carries the daily token counter
	"of_agent_runs":         true,
	"of_ws_room_tokens":     true,
	"of_ws_room_state":      true,
	"of_user_totp":          true,
	"of_magic_tokens":       true,
	"of_push_subscriptions": true,
	"of_tenants":            true,
	"of_tenant_members":     true,
	"of_tenant_invites":     true,
	"of_subscriptions":      true,
	"of_notifications":      true,
	"of_webhook_logs":       true,
	"of_triggers":           true,
}

// rowidCol is the column the snapshot copies each row's rowid into.
const rowidCol = "__of_rowid"

type table struct {
	name       string
	cols       []string // quoted column names, in table order
	rowidAlias bool     // an INTEGER PRIMARY KEY column is the rowid
	withoutRow bool     // WITHOUT ROWID table: restored wholesale
}

type fileInfo struct {
	size    int64
	modTime time.Time
}

// Snapshot is the state Restore returns the project to.
type Snapshot struct {
	pdb        *db.ProjectDB
	projectDir string
	snapDB     string // side database holding the table copies
	fileDir    string // hard links (or copies) of the project's files
	tables     []table
	seq        map[string]int64 // AUTOINCREMENT counters of the snapshotted tables
	files      map[string]fileInfo
}

// Take snapshots the project. The returned snapshot must be Restored (which also
// removes its temporary files) or Discarded.
func Take(ctx context.Context, pdb *db.ProjectDB, projectDir string) (*Snapshot, error) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	tag := hex.EncodeToString(b)
	s := &Snapshot{
		pdb:        pdb,
		projectDir: projectDir,
		snapDB:     filepath.Join(os.TempDir(), "of-flowsnap-"+tag+".db"),
		seq:        map[string]int64{},
		files:      map[string]fileInfo{},
	}
	if err := s.takeTables(ctx); err != nil {
		s.Discard()
		return nil, err
	}
	if err := s.takeFiles(); err != nil {
		s.Discard()
		return nil, err
	}
	return s, nil
}

// Discard removes the snapshot's temporary files without restoring anything.
func (s *Snapshot) Discard() {
	_ = os.Remove(s.snapDB)
	_ = os.Remove(s.snapDB + "-journal")
	if s.fileDir != "" {
		_ = os.RemoveAll(s.fileDir)
	}
}

// takeTables copies every restorable table into the side database on one pinned
// writer connection (ATTACH is per connection).
func (s *Snapshot) takeTables(ctx context.Context) error {
	conn, err := s.pdb.Writer().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tables, err := restorableTables(ctx, conn)
	if err != nil {
		return err
	}
	s.tables = tables

	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS ofsnap`, s.snapDB); err != nil {
		return fmt.Errorf("flowsnap: attach: %w", err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE ofsnap`) //nolint:errcheck
	for _, t := range tables {
		sel := strings.Join(t.cols, ", ")
		if !t.withoutRow {
			sel = "rowid AS " + rowidCol + ", " + sel
		}
		q := fmt.Sprintf(`CREATE TABLE ofsnap.%s AS SELECT %s FROM main.%s`, security.QuoteIdent(t.name), sel, security.QuoteIdent(t.name))
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("flowsnap: copying %s: %w", t.name, err)
		}
	}
	// sqlite_sequence only exists once an AUTOINCREMENT table does, so a failed
	// query just means there are no counters to capture.
	rows, err := conn.QueryContext(ctx, `SELECT name, seq FROM main.sqlite_sequence`)
	if err == nil {
		for rows.Next() {
			var name string
			var seq int64
			if rows.Scan(&name, &seq) == nil {
				s.seq[name] = seq
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("flowsnap: reading sqlite_sequence: %w", err)
		}
	}
	return nil
}

// restorableTables lists the app-owned tables (anything not prefixed of_ or
// sqlite_, minus full-text indexes and their shadow tables, which their own
// triggers keep in sync) plus the runtime platform tables.
func restorableTables(ctx context.Context, conn *sql.Conn) ([]table, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name, COALESCE(sql, '') FROM main.sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	type entry struct{ name, sql string }
	var all []entry
	var virtual []string
	for rows.Next() {
		var e entry
		if rows.Scan(&e.name, &e.sql) == nil {
			all = append(all, e)
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(e.sql)), "CREATE VIRTUAL TABLE") {
				virtual = append(virtual, e.name)
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	var out []table
	for _, e := range all {
		if strings.HasPrefix(e.name, "sqlite_") || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(e.sql)), "CREATE VIRTUAL TABLE") {
			continue
		}
		shadow := false
		for _, v := range virtual {
			if strings.HasPrefix(e.name, v+"_") {
				shadow = true
				break
			}
		}
		if shadow {
			continue
		}
		if strings.HasPrefix(e.name, "of_") && !runtimeTables[e.name] {
			continue
		}
		t, err := describe(ctx, conn, e.name, e.sql)
		if err != nil {
			return nil, err
		}
		if len(t.cols) > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

func describe(ctx context.Context, conn *sql.Conn, name, createSQL string) (table, error) {
	t := table{name: name, withoutRow: strings.Contains(strings.ToUpper(createSQL), "WITHOUT ROWID")}
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA main.table_info(%s)`, security.QuoteIdent(name)))
	if err != nil {
		return t, err
	}
	defer rows.Close()
	pkCols, intPK := 0, false
	for rows.Next() {
		var (
			cid     int
			col     string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &col, &typ, &notnull, &dflt, &pk); err != nil {
			return t, err
		}
		t.cols = append(t.cols, security.QuoteIdent(col))
		if pk > 0 {
			pkCols++
			intPK = strings.EqualFold(strings.TrimSpace(typ), "INTEGER")
		}
	}
	t.rowidAlias = !t.withoutRow && pkCols == 1 && intPK
	return t, rows.Err()
}

// takeFiles records every project file (except the database itself) and keeps a
// hard link to it, so a file the replay deletes or replaces can be put back. A hard
// link costs no space; when linking is impossible the file is copied. A hard link
// shares its storage with the original, so a file rewritten in place (rather than
// replaced) cannot be put back; apps store uploads as new files.
func (s *Snapshot) takeFiles() error {
	dir, err := os.MkdirTemp("", "of-flowsnap-files-")
	if err != nil {
		return err
	}
	s.fileDir = dir
	return walkProjectFiles(s.projectDir, func(rel string, info fs.FileInfo) error {
		s.files[rel] = fileInfo{size: info.Size(), modTime: info.ModTime()}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return linkOrCopy(filepath.Join(s.projectDir, rel), dst)
	})
}

// walkProjectFiles visits every regular file under projectDir except the project
// database and its SQLite sidecars.
func walkProjectFiles(projectDir string, fn func(rel string, info fs.FileInfo) error) error {
	return filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return err
		}
		if !strings.Contains(rel, string(filepath.Separator)) && strings.HasPrefix(rel, "project.db") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return fn(rel, info)
	})
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Restore returns the tables and files to the snapshot and removes the snapshot's
// temporary files. It is safe to call once.
func (s *Snapshot) Restore(ctx context.Context) error {
	defer s.Discard()
	if err := s.restoreTables(ctx); err != nil {
		return err
	}
	return s.restoreFiles()
}

func (s *Snapshot) restoreTables(ctx context.Context) error {
	conn, err := s.pdb.Writer().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS ofsnap`, s.snapDB); err != nil {
		return fmt.Errorf("flowsnap: attach: %w", err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE ofsnap`) //nolint:errcheck
	// Foreign keys off for the restore: deleting a changed parent row must not
	// cascade into child rows the snapshot still holds. The restored state is the
	// snapshot, which was consistent. (The pragma is a no-op inside a transaction,
	// so it is set around it.)
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`) //nolint:errcheck

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	for _, t := range s.tables {
		for _, q := range restoreStatements(t) {
			if _, err := conn.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("flowsnap: restoring %s: %w", t.name, err)
			}
		}
	}
	if err := s.restoreSequences(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// restoreStatements undoes one table's changes with plain DELETE and INSERT (never
// INSERT OR REPLACE, whose implicit delete skips triggers and would leave full-text
// indexes stale): delete rows the replay added, delete rows it changed, then insert
// every snapshot row that is missing (the changed ones and the ones it deleted).
func restoreStatements(t table) []string {
	name := security.QuoteIdent(t.name)
	cols := strings.Join(t.cols, ", ")
	if t.withoutRow {
		return []string{
			fmt.Sprintf(`DELETE FROM main.%s`, name),
			fmt.Sprintf(`INSERT INTO main.%s (%s) SELECT %s FROM ofsnap.%s`, name, cols, cols, name),
		}
	}
	insertCols, selectCols := cols, cols
	if !t.rowidAlias {
		insertCols, selectCols = "rowid, "+cols, rowidCol+", "+cols
	}
	return []string{
		fmt.Sprintf(`DELETE FROM main.%s WHERE rowid NOT IN (SELECT %s FROM ofsnap.%s)`, name, rowidCol, name),
		fmt.Sprintf(`DELETE FROM main.%s WHERE rowid IN (SELECT %s FROM (SELECT %s, %s FROM ofsnap.%s EXCEPT SELECT rowid, %s FROM main.%s))`,
			name, rowidCol, rowidCol, cols, name, cols, name),
		fmt.Sprintf(`INSERT INTO main.%s (%s) SELECT %s FROM ofsnap.%s WHERE %s NOT IN (SELECT rowid FROM main.%s)`,
			name, insertCols, selectCols, name, rowidCol, name),
	}
}

// restoreSequences resets the AUTOINCREMENT counters of the restored tables, so ids
// the replay consumed are handed out again instead of leaving gaps.
func (s *Snapshot) restoreSequences(ctx context.Context, conn *sql.Conn) error {
	var hasSeq int
	_ = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.sqlite_master WHERE name = 'sqlite_sequence'`).Scan(&hasSeq)
	if hasSeq == 0 {
		return nil
	}
	for _, t := range s.tables {
		seq, had := s.seq[t.name]
		var err error
		if had {
			_, err = conn.ExecContext(ctx, `UPDATE main.sqlite_sequence SET seq = ? WHERE name = ?`, seq, t.name)
		} else {
			_, err = conn.ExecContext(ctx, `DELETE FROM main.sqlite_sequence WHERE name = ?`, t.name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// restoreFiles deletes files the replay created and puts back files it removed or
// replaced. Directories it created and left empty are removed.
func (s *Snapshot) restoreFiles() error {
	var created []string
	_ = walkProjectFiles(s.projectDir, func(rel string, info fs.FileInfo) error {
		if _, ok := s.files[rel]; !ok {
			created = append(created, rel)
		}
		return nil
	})
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, rel := range created {
		keep(os.Remove(filepath.Join(s.projectDir, rel)))
		removeEmptyParents(s.projectDir, filepath.Dir(filepath.Join(s.projectDir, rel)))
	}
	for rel, want := range s.files {
		path := filepath.Join(s.projectDir, rel)
		info, err := os.Stat(path)
		if err == nil && info.Size() == want.size && info.ModTime().Equal(want.modTime) {
			continue
		}
		keep(os.MkdirAll(filepath.Dir(path), 0o755))
		_ = os.Remove(path)
		keep(linkOrCopy(filepath.Join(s.fileDir, rel), path))
	}
	return firstErr
}

func removeEmptyParents(root, dir string) {
	root = filepath.Clean(root)
	for dir = filepath.Clean(dir); dir != root && strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		_ = os.Remove(dir)
	}
}

// Tables names the tables a snapshot covers, for diagnostics and tests.
func (s *Snapshot) Tables() []string {
	out := make([]string, 0, len(s.tables))
	for _, t := range s.tables {
		out = append(out, t.name)
	}
	sort.Strings(out)
	return out
}
