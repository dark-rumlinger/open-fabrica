package tools

import (
	"context"
	"database/sql"
	"log/slog"

	"openfabrica/events"
	"openfabrica/internal/dbh"
	"openfabrica/security"
)

// Env is the instance-wide dependency set every tool call shares, built once by
// the composition root and handed to each caller that runs tools (the engine,
// live chat, scheduled jobs, event triggers). ForProject is the only place a
// ToolContext is shaped, so a field added here reaches every call path at once
// instead of being set on some hand-built literals and forgotten on others.
type Env struct {
	GlobalDB   *sql.DB // global database (users, providers, projects)
	Bus        *events.Bus
	Encryptor  *security.Encryptor
	JWTManager *security.JWTManager
	PublicPort int    // public server port for self-testing (0 = config.DefaultPublicPort)
	DataDir    string // instance data directory ("" = config.DefaultDataDir)

	// EmbedFunc runs the configured embeddings provider; the semantic-search
	// actions and find_tool's semantic ranking read it. Nil disables them.
	EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)
}

// Headless returns a copy of e with no event bus, for tool calls that event-driven
// actions (triggers, scheduled jobs) make. A tool.executed event raised from inside
// a trigger action could match a trigger subscribed to that event name and re-fire
// the action that raised it, looping; headless calls therefore publish nothing.
func (e *Env) Headless() *Env {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Bus = nil
	return &cp
}

// ProjectStore is the per-project database a tool call runs against.
// *db.ProjectDB satisfies it: ToolHandle routes reads to the read pool and
// writes to the single-connection writer, bound to the call's context.
type ProjectStore interface {
	ToolHandle(ctx context.Context) dbh.Handle
}

// ForProject shapes the ToolContext for one call against one project. The DB
// handle is bound to ctx, so none of the tool's statements outlive the call's
// deadline. Per-caller extras (the engine's ReadTracker, SeedGuides) are set on
// the returned value.
func (e *Env) ForProject(ctx context.Context, pdb ProjectStore, projectID int, logger *slog.Logger) *ToolContext {
	if logger == nil {
		logger = slog.Default()
	}
	return &ToolContext{
		Ctx:        ctx,
		DB:         pdb.ToolHandle(ctx),
		GlobalDB:   e.GlobalDB,
		ProjectID:  projectID,
		Logger:     logger,
		Bus:        e.Bus,
		Encryptor:  e.Encryptor,
		JWTManager: e.JWTManager,
		PublicPort: e.PublicPort,
		DataDir:    e.DataDir,
		EmbedFunc:  e.EmbedFunc,
	}
}
