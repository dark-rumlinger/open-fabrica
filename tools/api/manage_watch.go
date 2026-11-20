package api

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"openfabrica/config"
	"openfabrica/events"
	"openfabrica/internal/folderwatch"
	"openfabrica/store"
	"openfabrica/tools"
)

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_watch",
		MaxResult:          16000,
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Watch local drop-zone folders for new CSV, JSON, text, or media files and trigger server functions or events.",
		WhenToUse: `Use when building local-first drop-zone workflows: receipt/invoice processors,
automatic CSV/JSON importers, local document/photo organizers, or desktop ETL
pipelines. Actions: add, list, delete.`,
		Actions: []tools.Action{
			&WatchAdd{},
			&WatchList{},
			&WatchDelete{},
		},
	})
}

func emitWatchConfigChanged(ctx *tools.ToolContext) {
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.Event{
			Type:      events.EventWatchConfigChanged,
			ProjectID: ctx.ProjectID,
		})
	}
}

func watchProjectDir(ctx *tools.ToolContext) string {
	dataDir := ctx.DataDir
	if dataDir == "" {
		dataDir = config.DefaultDataDir
	}
	return filepath.Join(dataDir, "projects", strconv.Itoa(ctx.ProjectID))
}

// --- add --------------------------------------------------------------------

type WatchAdd struct {
	Name         string `arg:"name,required" desc:"Unique watcher name (e.g. receipts_inbox, csv_imports)."`
	Folder       string `arg:"folder" desc:"Optional local folder path to watch. Leave blank to auto-create the project's drop-zone folder (<project>/watch/<name>)."`
	Glob         string `arg:"glob" desc:"Comma-separated filename patterns to match (default '*', e.g. '*.csv,*.json,*.txt')."`
	FunctionPath string `arg:"function_path" desc:"Optional manage_function endpoint path invoked with {method:'WATCH', watcher, name, ext, size_bytes, mime_type, is_text, content}."`
	PollSec      int    `arg:"poll_sec" max:"3600" desc:"Polling interval in seconds (default 5, min 2, max 3600)."`
}

func (a *WatchAdd) Doc() string {
	return "Create or update a local folder watcher that fires folder.file_dropped events and/or invokes a server function when files are dropped."
}
func (a *WatchAdd) Example() any {
	return &WatchAdd{
		Name:         "csv_imports",
		Glob:         "*.csv,*.json",
		FunctionPath: "on-file-drop",
		PollSec:      5,
	}
}
func (a *WatchAdd) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return tools.Fail("name is required"), nil
	}
	absFolder, err := folderwatch.ValidateWatchFolder(watchProjectDir(ctx), a.Folder, name)
	if err != nil {
		return tools.Fail("%v", err), nil
	}
	if err := os.MkdirAll(absFolder, 0o750); err != nil {
		return tools.Fail("create watch folder: %v", err), nil
	}
	glob := strings.TrimSpace(a.Glob)
	if glob == "" {
		glob = "*"
	}
	poll := a.PollSec
	if poll <= 0 {
		poll = 5
	}
	if poll < 2 {
		poll = 2
	}
	w := store.FolderWatcher{
		Name:         name,
		Folder:       absFolder,
		Glob:         glob,
		FunctionPath: strings.TrimSpace(a.FunctionPath),
		PollSec:      poll,
	}
	if err := store.UpsertFolderWatcher(ctx.DB, w); err != nil {
		return tools.Fail("save folder watcher: %v", err), nil
	}
	emitWatchConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"name":          w.Name,
		"folder":        w.Folder,
		"glob":          w.Glob,
		"function_path": w.FunctionPath,
		"poll_sec":      w.PollSec,
	}), nil
}

// --- list -------------------------------------------------------------------

type WatchList struct{}

func (a *WatchList) Doc() string {
	return "List all configured local folder watchers for this project."
}
func (a *WatchList) ReadOnly() bool { return true }
func (a *WatchList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := store.ListFolderWatchers(ctx.DB)
	if err != nil {
		return tools.Fail("list folder watchers: %v", err), nil
	}
	out := make([]map[string]any, len(rows))
	for i, w := range rows {
		out[i] = map[string]any{
			"name":          w.Name,
			"folder":        w.Folder,
			"glob":          w.Glob,
			"function_path": w.FunctionPath,
			"poll_sec":      w.PollSec,
			"enabled":       w.Enabled,
		}
	}
	return tools.Ok(map[string]any{
		"count":    len(out),
		"watchers": out,
	}), nil
}

// --- delete -----------------------------------------------------------------

type WatchDelete struct {
	Name string `arg:"name,required" desc:"Watcher name to remove."`
}

func (a *WatchDelete) Doc() string  { return "Delete a local folder watcher." }
func (a *WatchDelete) Example() any { return &WatchDelete{Name: "csv_imports"} }
func (a *WatchDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	deleted, err := store.DeleteFolderWatcher(ctx.DB, strings.TrimSpace(a.Name))
	if err != nil {
		return tools.Fail("delete folder watcher: %v", err), nil
	}
	if !deleted {
		return tools.Fail("watcher not found"), nil
	}
	emitWatchConfigChanged(ctx)
	return tools.Ok(map[string]any{"deleted": a.Name}), nil
}
