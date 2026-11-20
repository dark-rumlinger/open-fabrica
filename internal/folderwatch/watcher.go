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
)

// WatchSpec defines one active folder watcher for a project.
type WatchSpec struct {
	Name         string
	Folder       string // validated absolute path on disk
	Glob         string // comma-separated filepath.Match patterns (default "*")
	FunctionPath string
	PollSec      int
}

// FileEvent describes a newly dropped or modified file in a watched folder.
type FileEvent struct {
	WatcherName  string
	FunctionPath string
	Folder       string
	RelPath      string
	Name         string
	Ext          string
	SizeBytes    int64
	ModTimeNs    int64
	MIMEType     string
	IsText       bool
	Content      string // populated when IsText && SizeBytes <= maxTextPreviewBytes
}

// ProjectHooks wires folderwatch.Hub to persistence and event/function dispatch
// without creating package import cycles.
type ProjectHooks struct {
	LoadWatchers func(projectID int) ([]WatchSpec, error)
	IsSeen       func(projectID int, watcherName, relPath string, size, modTimeNs int64) bool
	MarkSeen     func(projectID int, watcherName, relPath string, size, modTimeNs int64)
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
	entries, err := os.ReadDir(sp.Folder)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ctx.Err() != nil {
			return
		}
		if ent.IsDir() || ent.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, ".") || !MatchGlobCSV(sp.Glob, name) {
			continue
		}
		fullPath := filepath.Join(sp.Folder, name)
		fi, err := os.Lstat(fullPath)
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		size := fi.Size()
		modNs := fi.ModTime().UnixNano()
		if h.hooks.IsSeen != nil && h.hooks.IsSeen(projectID, sp.Name, name, size, modNs) {
			continue
		}

		ev := readDroppedFile(sp, fullPath, name, size, modNs)
		if h.hooks.MarkSeen != nil {
			h.hooks.MarkSeen(projectID, sp.Name, name, size, modNs)
		}
		if h.hooks.OnFile != nil {
			h.hooks.OnFile(projectID, ev)
		}
	}
}

func readDroppedFile(sp WatchSpec, fullPath, name string, size, modNs int64) FileEvent {
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
		RelPath:      name,
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
