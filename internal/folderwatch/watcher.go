// Package folderwatch implements a pure-Go, cross-platform local drop-zone
// folder watcher for Open Fabrica projects. It polls configured local folders
// safely (blocking symlinks and sensitive system/credential paths), deduplicates
// processed files via SQLite, and fires project events / server functions when
// new or updated files arrive.
package folderwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxTextPreviewBytes = 256 << 10 // 256 KiB cap on text content passed to handlers
	minPollSec          = 2
	maxPollSec          = 3600
	defaultPollSec      = 5
	maxScanDepth        = 8
	maxScanEntries      = 2000
)

// WatchSpec defines one active folder watcher for a project.
type WatchSpec struct {
	Name         string
	Folder       string // validated absolute path on disk
	Glob         string // comma-separated filepath.Match patterns (default "*")
	FunctionPath string
	PollSec      int
}

// SeenFile holds the previously processed size and mtime of a relative path.
type SeenFile struct {
	SizeBytes int64
	ModTimeNs int64
}

// FileEvent describes a newly dropped, modified, or deleted file in a watched folder.
type FileEvent struct {
	WatcherName  string
	FunctionPath string
	Folder       string
	Event        string // "created", "modified", or "deleted"
	Kind         string // "created", "changed", or "deleted" (alias for UI/feed schemas)
	RelPath      string
	Name         string
	Ext          string
	SizeBytes    int64
	ModTimeNs    int64
	MIMEType     string
	IsText       bool
	Content      string // populated when IsText && SizeBytes <= maxTextPreviewBytes
}

// DirEntry describes one file or directory inside a watched folder (returned by ListWatchDir).
type DirEntry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Ext       string `json:"ext"`
	SizeBytes int64  `json:"size_bytes"`
	Size      int64  `json:"size"`
	ModTimeMS int64  `json:"mtime_ms"`
	ModTime   string `json:"mtime"`
	IsDir     bool   `json:"is_dir"`
}

// ProjectHooks wires folderwatch.Hub to persistence and event/function dispatch
// without creating package import cycles.
type ProjectHooks struct {
	LoadWatchers func(projectID int) ([]WatchSpec, error)
	IsSeen       func(projectID int, watcherName, relPath string, size, modTimeNs int64) bool
	MarkSeen     func(projectID int, watcherName, relPath string, size, modTimeNs int64)
	ListSeen     func(projectID int, watcherName string) (map[string]SeenFile, error)
	DeleteSeen   func(projectID int, watcherName, relPath string)
	OnFile       func(projectID int, ev FileEvent)
}

var blockedSegments = []string{
	".security", ".ssh", ".aws", ".gnupg", ".kube",
}

// ValidateWatchFolder resolves rawFolder into a safe absolute directory path.
// Empty or relative paths resolve inside <projectDir>/watch/<subpath> with
// strict containment. Absolute paths are permitted for local-first workflows
// unless they target sensitive OS or credential directories.
func ValidateWatchFolder(projectDir, rawFolder, watcherName string) (string, error) {
	raw := strings.TrimSpace(rawFolder)
	if raw == "" {
		safeName := sanitizeSubdir(watcherName)
		if safeName == "" {
			safeName = "inbox"
		}
		if projectDir == "" {
			return "", errors.New("project directory is unavailable")
		}
		return filepath.Clean(filepath.Join(projectDir, "watch", safeName)), nil
	}

	var abs string
	if filepath.IsAbs(raw) {
		abs = filepath.Clean(raw)
	} else {
		if projectDir == "" {
			return "", errors.New("project directory is unavailable for relative folder")
		}
		base := filepath.Clean(filepath.Join(projectDir, "watch"))
		abs = filepath.Clean(filepath.Join(base, raw))
		rel, err := filepath.Rel(base, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", fmt.Errorf("relative folder %q escapes project watch root", raw)
		}
	}

	if isBlockedWatchPath(abs) {
		return "", fmt.Errorf("folder %q points to a protected system or credential path", abs)
	}
	return abs, nil
}

func sanitizeSubdir(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isBlockedWatchPath(abs string) bool {
	norm := strings.ToLower(filepath.ToSlash(filepath.Clean(abs)))
	// Block UNC / SMB network paths (prevents Windows NTLM hash leakage).
	if strings.HasPrefix(norm, "//") {
		return true
	}
	for _, prefix := range []string{"/etc", "/proc", "/sys", "/dev", "/root", "c:/windows", "c:/program files"} {
		if norm == prefix || strings.HasPrefix(norm, prefix+"/") {
			return true
		}
	}
	for _, part := range strings.Split(norm, "/") {
		for _, blocked := range blockedSegments {
			if part == blocked {
				return true
			}
		}
	}
	return false
}

// MatchGlobCSV reports whether name matches any comma-separated filepath.Match pattern.
func MatchGlobCSV(globCSV, name string) bool {
	if strings.TrimSpace(globCSV) == "" {
		return true
	}
	lowName := strings.ToLower(name)
	for _, raw := range strings.Split(globCSV, ",") {
		pat := strings.TrimSpace(raw)
		if pat == "" {
			continue
		}
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
		if ok, _ := filepath.Match(strings.ToLower(pat), lowName); ok {
			return true
		}
	}
	return false
}

type projectRunner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Hub manages per-project folder watcher loops.
type Hub struct {
	mu      sync.Mutex
	hooks   ProjectHooks
	runners map[int]*projectRunner
	closed  bool
}

// NewHub creates a multi-project folder watcher hub.
func NewHub(hooks ProjectHooks) *Hub {
	return &Hub{
		hooks:   hooks,
		runners: make(map[int]*projectRunner),
	}
}

// Reload reloads the active watchers for projectID, starting or stopping its loop.
func (h *Hub) Reload(projectID int) {
	if h.hooks.LoadWatchers == nil {
		return
	}
	specs, err := h.hooks.LoadWatchers(projectID)
	if err != nil {
		return
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	prev := h.runners[projectID]
	delete(h.runners, projectID)
	var next *projectRunner
	if len(specs) > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		next = &projectRunner{cancel: cancel, done: make(chan struct{})}
		h.runners[projectID] = next
		go h.runProject(ctx, projectID, specs, next.done)
	}
	h.mu.Unlock()

	if prev != nil {
		prev.cancel()
		<-prev.done
	}
}

// StartProjects boots watcher loops for all projects that have active watchers.
func (h *Hub) StartProjects(projectIDs []int) {
	for _, pid := range projectIDs {
		h.Reload(pid)
	}
}

// CloseProject stops the watcher loop for projectID.
func (h *Hub) CloseProject(projectID int) {
	h.mu.Lock()
	prev := h.runners[projectID]
	delete(h.runners, projectID)
	h.mu.Unlock()
	if prev != nil {
		prev.cancel()
		<-prev.done
	}
}

// CloseAll stops every active watcher loop.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	h.closed = true
	all := make([]*projectRunner, 0, len(h.runners))
	for _, r := range h.runners {
		all = append(all, r)
	}
	h.runners = make(map[int]*projectRunner)
	h.mu.Unlock()
	for _, r := range all {
		r.cancel()
	}
	for _, r := range all {
		<-r.done
	}
}

func (h *Hub) runProject(ctx context.Context, projectID int, specs []WatchSpec, done chan struct{}) {
	defer close(done)
	lastScan := make(map[string]time.Time, len(specs))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// Run one immediate scan on start, then tick every second honoring each watcher's PollSec.
	h.scanSpecs(ctx, projectID, specs, lastScan)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.scanSpecs(ctx, projectID, specs, lastScan)
		}
	}
}

func (h *Hub) scanSpecs(ctx context.Context, projectID int, specs []WatchSpec, lastScan map[string]time.Time) {
	now := time.Now()
	for _, sp := range specs {
		if ctx.Err() != nil {
			return
		}
		poll := sp.PollSec
		if poll < minPollSec {
			poll = defaultPollSec
		}
		if poll > maxPollSec {
			poll = maxPollSec
		}
		if prev, ok := lastScan[sp.Name]; ok && now.Sub(prev) < time.Duration(poll)*time.Second {
			continue
		}
		lastScan[sp.Name] = now
		h.scanOne(ctx, projectID, sp)
	}
}

func (h *Hub) scanOne(ctx context.Context, projectID int, sp WatchSpec) {
	var prevSeen map[string]SeenFile
	if h.hooks.ListSeen != nil {
		prevSeen, _ = h.hooks.ListSeen(projectID, sp.Name)
	}
	present := make(map[string]bool)
	scanned := 0

	var walk func(relDir string, depth int) bool
	walk = func(relDir string, depth int) bool {
		if ctx.Err() != nil || depth > maxScanDepth || scanned >= maxScanEntries {
			return false
		}
		absDir := sp.Folder
		if relDir != "" {
			absDir = filepath.Join(sp.Folder, filepath.FromSlash(relDir))
		}
		entries, err := os.ReadDir(absDir)
		if err != nil {
			return relDir != "" // top-level read error aborts deletion sweep
		}
		for _, ent := range entries {
			if ctx.Err() != nil || scanned >= maxScanEntries {
				return false
			}
			if ent.Type()&os.ModeSymlink != 0 {
				continue
			}
			name := ent.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			relPath := name
			if relDir != "" {
				relPath = relDir + "/" + name
			}
			fullPath := filepath.Join(absDir, name)
			fi, err := os.Lstat(fullPath)
			if err != nil || fi.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if fi.IsDir() {
				walk(relPath, depth+1)
				continue
			}
			if !MatchGlobCSV(sp.Glob, name) && !MatchGlobCSV(sp.Glob, relPath) {
				continue
			}
			scanned++
			present[relPath] = true
			size := fi.Size()
			modNs := fi.ModTime().UnixNano()

			eventKind := "created"
			uiKind := "created"
			if prevSeen != nil {
				if prev, existed := prevSeen[relPath]; existed {
					if prev.SizeBytes == size && prev.ModTimeNs == modNs {
						continue
					}
					eventKind = "modified"
					uiKind = "changed"
				}
			} else if h.hooks.IsSeen != nil && h.hooks.IsSeen(projectID, sp.Name, relPath, size, modNs) {
				continue
			}

			ev := readDroppedFile(sp, fullPath, relPath, name, size, modNs, eventKind, uiKind)
			if h.hooks.MarkSeen != nil {
				h.hooks.MarkSeen(projectID, sp.Name, relPath, size, modNs)
			}
			if h.hooks.OnFile != nil {
				h.hooks.OnFile(projectID, ev)
			}
		}
		return true
	}

	completed := walk("", 0)
	if completed && prevSeen != nil {
		nowNs := time.Now().UnixNano()
		for relPath, prev := range prevSeen {
			if present[relPath] {
				continue
			}
			if h.hooks.DeleteSeen != nil {
				h.hooks.DeleteSeen(projectID, sp.Name, relPath)
			}
			if h.hooks.OnFile != nil {
				base := filepath.Base(filepath.FromSlash(relPath))
				ext := strings.ToLower(filepath.Ext(base))
				h.hooks.OnFile(projectID, FileEvent{
					WatcherName:  sp.Name,
					FunctionPath: sp.FunctionPath,
					Folder:       sp.Folder,
					Event:        "deleted",
					Kind:         "deleted",
					RelPath:      relPath,
					Name:         base,
					Ext:          ext,
					SizeBytes:    prev.SizeBytes,
					ModTimeNs:    nowNs,
				})
			}
		}
	}
}

// ListWatchDir returns the current directory and file inventory of a validated watch folder,
// honouring glob (for files) and skipping hidden files and symlinks.
func ListWatchDir(folder, glob string) ([]DirEntry, bool, error) {
	if isBlockedWatchPath(folder) {
		return nil, false, fmt.Errorf("folder %q points to a protected path", folder)
	}
	fi, err := os.Stat(folder)
	if err != nil || !fi.IsDir() {
		return nil, false, err
	}
	var out []DirEntry
	var walk func(relDir string, depth int)
	walk = func(relDir string, depth int) {
		if depth > maxScanDepth || len(out) >= maxScanEntries {
			return
		}
		absDir := folder
		if relDir != "" {
			absDir = filepath.Join(folder, filepath.FromSlash(relDir))
		}
		entries, err := os.ReadDir(absDir)
		if err != nil {
			return
		}
		for _, ent := range entries {
			if len(out) >= maxScanEntries {
				return
			}
			if ent.Type()&os.ModeSymlink != 0 {
				continue
			}
			name := ent.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			relPath := name
			if relDir != "" {
				relPath = relDir + "/" + name
			}
			fullPath := filepath.Join(absDir, name)
			info, err := os.Lstat(fullPath)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			isDir := info.IsDir()
			if !isDir && !MatchGlobCSV(glob, name) && !MatchGlobCSV(glob, relPath) {
				continue
			}
			mtime := info.ModTime().UTC()
			out = append(out, DirEntry{
				Path:      relPath,
				Name:      name,
				Ext:       strings.ToLower(filepath.Ext(name)),
				SizeBytes: info.Size(),
				Size:      info.Size(),
				ModTimeMS: mtime.UnixMilli(),
				ModTime:   mtime.Format(time.RFC3339),
				IsDir:     isDir,
			})
			if isDir {
				walk(relPath, depth+1)
			}
		}
	}
	walk("", 0)
	if out == nil {
		out = []DirEntry{}
	}
	return out, true, nil
}

func readDroppedFile(sp WatchSpec, fullPath, relPath, name string, size, modNs int64, eventKind, uiKind string) FileEvent {
	ext := strings.ToLower(filepath.Ext(name))
	mimeType := mime.TypeByExtension(ext)
	var content string
	var isText bool

	f, err := os.Open(fullPath)
	if err == nil {
		defer func() { _ = f.Close() }()
		buf, rerr := io.ReadAll(io.LimitReader(f, maxTextPreviewBytes+1))
		if rerr == nil {
			if mimeType == "" && len(buf) > 0 {
				mimeType = http.DetectContentType(buf[:min(len(buf), 512)])
			}
			if len(buf) <= maxTextPreviewBytes && utf8.Valid(buf) && !bytesHasNul(buf) {
				isText = true
				content = string(buf)
			}
		}
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	return FileEvent{
		WatcherName:  sp.Name,
		FunctionPath: sp.FunctionPath,
		Folder:       sp.Folder,
		Event:        eventKind,
		Kind:         uiKind,
		RelPath:      relPath,
		Name:         name,
		Ext:          ext,
		SizeBytes:    size,
		ModTimeNs:    modNs,
		MIMEType:     mimeType,
		IsText:       isText,
		Content:      content,
	}
}

func bytesHasNul(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
