package public

import (
	"context"
	"time"

	"openfabrica/events"
	"openfabrica/internal/folderwatch"
	"openfabrica/internal/funcexec"
	"openfabrica/store"
)

const watchDefaultFnBudget = 15 * time.Second

// NewWatchHub creates the shared multi-project local folder watcher hub wired
// to project SQLite DBs, the event bus, and server function execution.
func NewWatchHub(deps *Deps) *folderwatch.Hub {
	return folderwatch.NewHub(folderwatch.ProjectHooks{
		LoadWatchers: func(projectID int) ([]folderwatch.WatchSpec, error) {
			pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
			if pdb == nil {
				return nil, nil
			}
			rows, err := store.ListFolderWatchers(pdb.Reader())
			if err != nil {
				return nil, err
			}
			out := make([]folderwatch.WatchSpec, 0, len(rows))
			for _, r := range rows {
				if !r.Enabled {
					continue
				}
				out = append(out, folderwatch.WatchSpec{
					Name:         r.Name,
					Folder:       r.Folder,
					Glob:         r.Glob,
					FunctionPath: r.FunctionPath,
					PollSec:      r.PollSec,
				})
			}
			return out, nil
		},
		IsSeen: func(projectID int, watcherName, relPath string, size, modTimeNs int64) bool {
			pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
			if pdb == nil {
				return false
			}
			return store.IsWatchFileSeen(pdb.Reader(), watcherName, relPath, size, modTimeNs)
		},
		MarkSeen: func(projectID int, watcherName, relPath string, size, modTimeNs int64) {
			pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
			if pdb == nil {
				return
			}
			store.MarkWatchFileSeen(pdb.Writer(), watcherName, relPath, size, modTimeNs)
		},
		OnFile: func(projectID int, ev folderwatch.FileEvent) {
			pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
			if pdb == nil {
				return
			}
			if deps.Bus != nil {
				deps.Bus.Publish(events.Event{
					Type:      events.EventFolderFileDropped,
					ProjectID: projectID,
					Payload: map[string]any{
						"watcher":    ev.WatcherName,
						"folder":     ev.Folder,
						"name":       ev.Name,
						"ext":        ev.Ext,
						"size_bytes": ev.SizeBytes,
						"mime_type":  ev.MIMEType,
						"is_text":    ev.IsText,
					},
				})
			}
			if ev.FunctionPath != "" {
				ep, err := store.LoadFunctionEndpoint(pdb.Reader(), ev.FunctionPath)
				if err != nil || ep == nil {
					return
				}
				rt := funcexec.NewRuntime(projectID, pdb.Reader(), pdb.Writer(), deps.completeDeps())
				req := map[string]any{
					"method":     "WATCH",
					"path":       ev.FunctionPath,
					"watcher":    ev.WatcherName,
					"folder":     ev.Folder,
					"name":       ev.Name,
					"ext":        ev.Ext,
					"size_bytes": ev.SizeBytes,
					"mime_type":  ev.MIMEType,
					"is_text":    ev.IsText,
					"content":    ev.Content,
				}
				timeout := watchDefaultFnBudget
				if ep.TimeoutMS > 0 {
					timeout = time.Duration(ep.TimeoutMS) * time.Millisecond
				}
				spec := funcexec.Spec{
					Code:        ep.Code,
					SecretsJSON: ep.Secrets,
					Timeout:     timeout,
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				_, _ = funcexec.ExecuteContext(ctx, rt, spec, req)
			}
		},
	})
}

// StartWatchers boots folder watchers for every project that has configured them.
func StartWatchers(deps *Deps, hub *folderwatch.Hub) {
	if deps == nil || deps.DB == nil || hub == nil {
		return
	}
	ids, err := store.ListProjectIDs(deps.DB.DB)
	if err != nil || len(ids) == 0 {
		return
	}
	hub.StartProjects(ids)
}
