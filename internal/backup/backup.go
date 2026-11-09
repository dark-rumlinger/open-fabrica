// Package backup writes and restores a whole-instance archive: the core
// database, every project database, uploaded and generated files, the key files
// and the ACME state, as one zip.
//
// SQLite files are captured with VACUUM INTO, which is consistent with a running
// server, so a backup does not need the instance stopped. A restore does: it
// replaces files the server holds open, so the caller must make sure none is
// running. The archive contains the encryption and signing keys; guard it like
// the data directory itself.
package backup

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"openfabrica/internal/diskpath"

	_ "modernc.org/sqlite" // the sqlite driver, registered for VACUUM INTO
)

// FormatVersion is bumped when the archive layout changes incompatibly.
const FormatVersion = 1

const (
	manifestName = "manifest.json"
	coreDBName   = "openfabrica.db"
)

var projectDBRe = regexp.MustCompile(`^projects/(\d+)/project\.db$`)

// Manifest describes an archive.
type Manifest struct {
	Format    int       `json:"format"`
	CreatedAt time.Time `json:"created_at"`
	Version   string    `json:"version"`
	Projects  []int     `json:"projects"`
	Files     int       `json:"files"`
}

// Options locates the instance on disk.
type Options struct {
	DataDir string // data directory (projects, keys, certs)
	DBPath  string // core database; defaults to <DataDir>/openfabrica.db
	Version string // recorded in the manifest
	// Skip is an absolute path never to include (the archive being written, when
	// it sits inside the data directory).
	Skip string
}

func (o Options) dbPath() string {
	if o.DBPath != "" {
		return o.DBPath
	}
	return filepath.Join(o.DataDir, coreDBName)
}

// isSQLiteSidecar reports a -wal, -shm or -journal file, which a VACUUM INTO
// snapshot makes redundant.
func isSQLiteSidecar(name string) bool {
	return strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal")
}

// snapshot writes a consistent copy of the SQLite database at src to dst.
func snapshot(src, dst string) error {
	db, err := sql.Open("sqlite", src+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("snapshotting %s: %w", filepath.Base(src), err)
	}
	return nil
}

// Create writes the archive to w.
func Create(w io.Writer, o Options) (*Manifest, error) {
	if o.DataDir == "" {
		return nil, errors.New("backup: data directory is required")
	}
	tmp, err := os.MkdirTemp("", "of-backup-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	zw := zip.NewWriter(w)
	m := &Manifest{Format: FormatVersion, CreatedAt: time.Now().UTC(), Version: o.Version}

	addFile := func(name, src string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, f); err != nil {
			return err
		}
		m.Files++
		return nil
	}

	corePath := o.dbPath()
	if _, err := os.Stat(corePath); err != nil {
		return nil, fmt.Errorf("backup: core database not found at %s: %w", corePath, err)
	}
	coreSnap := filepath.Join(tmp, "core.db")
	if err := snapshot(corePath, coreSnap); err != nil {
		return nil, err
	}
	if err := addFile(coreDBName, coreSnap); err != nil {
		return nil, err
	}

	absData, _ := filepath.Abs(o.DataDir)
	absCore, _ := filepath.Abs(corePath)
	absSkip := ""
	if o.Skip != "" {
		absSkip, _ = filepath.Abs(o.Skip)
	}

	err = filepath.WalkDir(o.DataDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		abs, _ := filepath.Abs(p)
		if d.IsDir() {
			// A backups folder inside the data dir would nest every earlier backup.
			if abs != absData && d.Name() == "backups" && filepath.Dir(abs) == absData {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and devices are not archived
		}
		if abs == absSkip || isSQLiteSidecar(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(o.DataDir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if abs == absCore || name == coreDBName {
			return nil // captured above as a snapshot
		}
		if mm := projectDBRe.FindStringSubmatch(name); mm != nil {
			snap := filepath.Join(tmp, "project-"+mm[1]+".db")
			if err := snapshot(p, snap); err != nil {
				return err
			}
			id, _ := strconv.Atoi(mm[1])
			m.Projects = append(m.Projects, id)
			return addFile(name, snap)
		}
		return addFile(name, p)
	})
	if err != nil {
		return nil, err
	}

	mw, err := zw.Create(manifestName)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return m, nil
}

// readManifest finds and validates manifest.json.
func readManifest(zr *zip.Reader) (*Manifest, error) {
	for _, f := range zr.File {
		if f.Name != manifestName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var m Manifest
		if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m); err != nil {
			return nil, fmt.Errorf("backup: unreadable manifest: %w", err)
		}
		if m.Format != FormatVersion {
			return nil, fmt.Errorf("backup: archive format %d is not supported (this build reads %d)", m.Format, FormatVersion)
		}
		return &m, nil
	}
	return nil, errors.New("backup: not an Open Fabrica backup (missing manifest.json)")
}

// safeTarget maps an archive entry name to a path inside dir, refusing anything
// that would land outside it.
func safeTarget(dir, name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
		return "", fmt.Errorf("backup: unsafe path %q in archive", name)
	}
	target := filepath.Join(dir, filepath.FromSlash(clean))
	if !diskpath.Inside(dir, target) {
		return "", fmt.Errorf("backup: path %q escapes the data directory", name)
	}
	return target, nil
}

// Restore unpacks an archive into the instance described by o. It refuses to
// write over an existing instance unless force is set, and the server must not be
// running. Project file paths are re-anchored to the data directory afterwards.
func Restore(r io.ReaderAt, size int64, o Options, force bool) (*Manifest, error) {
	if o.DataDir == "" {
		return nil, errors.New("restore: data directory is required")
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("restore: not a zip archive: %w", err)
	}
	m, err := readManifest(zr)
	if err != nil {
		return nil, err
	}

	corePath := o.dbPath()
	if !force {
		if _, err := os.Stat(corePath); err == nil {
			return nil, fmt.Errorf("restore: %s already exists; restoring over a live instance would destroy it (use --force to replace it)", corePath)
		}
		if entries, err := os.ReadDir(filepath.Join(o.DataDir, "projects")); err == nil && len(entries) > 0 {
			return nil, fmt.Errorf("restore: %s already holds projects (use --force to replace them)", filepath.Join(o.DataDir, "projects"))
		}
	}
	if err := os.MkdirAll(o.DataDir, 0o750); err != nil {
		return nil, err
	}

	// Unpack and validate into a staging directory first, so a bad, truncated or
	// hostile archive fails before it has touched the live instance. Only a fully
	// extracted and relinked tree is swapped in.
	stage := filepath.Join(o.DataDir, restoreStagingName)
	if err := os.RemoveAll(stage); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	// The core database is staged beside its final path (it may live outside the
	// data directory) so the swap is a same-volume rename.
	stagedCore := corePath + ".restore"
	defer func() { _ = os.Remove(stagedCore) }() // already gone once the swap renamed it

	for _, f := range zr.File {
		if f.Name == manifestName || f.FileInfo().IsDir() {
			continue
		}
		var target string
		if f.Name == coreDBName {
			target = stagedCore
		} else if target, err = safeTarget(stage, f.Name); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, err
		}
		if err := extract(f, target); err != nil {
			return nil, err
		}
	}

	// Re-anchor stored file paths to where each project will live once swapped in.
	for _, id := range m.Projects {
		idStr := strconv.Itoa(id)
		finalDir := filepath.Join(o.DataDir, "projects", idStr)
		if err := relinkProject(filepath.Join(stage, "projects", idStr, "project.db"), id, finalDir); err != nil {
			return nil, fmt.Errorf("restore: relinking project %d: %w", id, err)
		}
	}

	if err := swapIn(stage, stagedCore, corePath); err != nil {
		return nil, err
	}
	return m, nil
}

// restoreStagingName is the scratch directory, inside the data directory, that a
// restore unpacks into before swapping the result into place.
const restoreStagingName = ".restore-staging"

// swapIn moves a validated staging tree over the live instance. projects/ is
// replaced wholesale (a project absent from the archive must not survive), other
// files overwrite their live counterparts, and the stale WAL/SHM files of the
// replaced core database are dropped so they are not applied to the restored one.
func swapIn(stage, stagedCore, corePath string) error {
	dataDir := filepath.Dir(stage)
	for _, p := range []string{corePath + "-wal", corePath + "-shm"} {
		_ = os.Remove(p)
	}
	if _, err := os.Stat(stagedCore); err == nil {
		if err := os.Rename(stagedCore, corePath); err != nil {
			return fmt.Errorf("restore: replacing core database: %w", err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "projects")); err != nil {
		return fmt.Errorf("restore: removing old projects: %w", err)
	}
	return filepath.WalkDir(stage, func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, src)
		if err != nil || rel == "." {
			return err
		}
		dst := filepath.Join(dataDir, rel)
		if d.IsDir() {
			// A whole project directory moves in one rename.
			if filepath.Dir(rel) == "projects" {
				if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
					return err
				}
				// #nosec G122 -- src is inside the staging dir this restore just extracted and validated (zip-slip checked); nothing else writes there.
				if err := os.Rename(src, dst); err != nil {
					return fmt.Errorf("restore: moving %s: %w", rel, err)
				}
				return filepath.SkipDir
			}
			return os.MkdirAll(dst, 0o750)
		}
		// #nosec G122 -- src is inside the staging dir this restore just extracted and validated (zip-slip checked); nothing else writes there.
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("restore: moving %s: %w", rel, err)
		}
		return nil
	})
}

func extract(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	mode := f.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	// The declared size bounds the copy, so a lying entry cannot fill the disk.
	n, err := io.Copy(out, io.LimitReader(rc, int64(f.UncompressedSize64)+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if uint64(n) != f.UncompressedSize64 {
		return fmt.Errorf("restore: %s is corrupt (size mismatch)", f.Name)
	}
	return nil
}

// sqlWriter adapts *sql.DB to diskpath.DB.
type sqlWriter struct{ *sql.DB }

func (w sqlWriter) ExecWrite(q string, args ...any) (sql.Result, error) { return w.DB.Exec(q, args...) }

// relinkProject re-anchors stored file paths in one restored project database to
// the directory it now lives in (the data directory may have moved).
func relinkProject(dbFile string, id int, dir string) error {
	if _, err := os.Stat(dbFile); err != nil {
		return nil // a project with no database has nothing to relink
	}
	db, err := sql.Open("sqlite", dbFile+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return err
	}
	defer db.Close()
	return diskpath.RemapProject(sqlWriter{db}, id, dir)
}
