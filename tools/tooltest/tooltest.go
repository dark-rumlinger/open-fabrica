// Package tooltest is the shared harness for tool tests. It runs tools against
// real, fully migrated databases through the same tools.Env / Executor.Invoke
// path production uses, so a test cannot pass on a hand-written schema that has
// drifted from db/project_migrations, and a handler that deadlocks on the
// single-connection writer pool fails here instead of only in production.
//
// Typical use:
//
//	h := tooltest.New(t)
//	h.MustOK("manage_secrets", map[string]any{"action": "store", "name": "k", "value": "v"})
//	var n int
//	h.DB().QueryRow("SELECT COUNT(*) FROM of_secrets").Scan(&n)
//
// The tool under test must be registered in tools.DefaultRegistry, which an
// in-package test gets from its own package's init(); use WithRegistry for a
// registry holding fixtures.
package tooltest

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/dbh"
	"openfabrica/security"
	"openfabrica/tools"
)

// ProjectID is the id every harness project database is created under.
const ProjectID = 1

// Harness is one isolated instance: a migrated global DB, one migrated project
// DB, a tools.Env over them, and an executor.
type Harness struct {
	t        testing.TB
	Global   *db.DB
	Project  *db.ProjectDB
	Env      *tools.Env
	Executor *tools.Executor
}

// New builds a harness in t's temp dir. Everything is closed on cleanup.
func New(t testing.TB) *Harness {
	t.Helper()
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("tooltest: open global db: %v", err)
	}
	t.Cleanup(func() { _ = gdb.Close() })
	mgr := db.NewProjectDBManager(dir)
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(ProjectID)
	if err != nil {
		t.Fatalf("tooltest: create project db: %v", err)
	}
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("tooltest: encryptor: %v", err)
	}
	return &Harness{
		t:       t,
		Global:  gdb,
		Project: pdb,
		Env: &tools.Env{
			GlobalDB: gdb.DB,
			DataDir:  dir,
			// Production always has an encryption key; a fixed one here keeps tools that
			// store encrypted values (webhook secrets, API keys) working under test.
			Encryptor: enc,
		},
		Executor: tools.NewExecutor(tools.DefaultRegistry()),
	}
}

// ProjectDB is the short form for tests that drive a handler or store function
// directly: a migrated project database handle and nothing else.
func ProjectDB(t testing.TB) dbh.Handle {
	t.Helper()
	return New(t).DB()
}

// WithRegistry points the harness executor at reg (for fixture tools).
func (h *Harness) WithRegistry(reg *tools.Registry) *Harness {
	h.Executor = tools.NewExecutor(reg)
	return h
}

// WithBus attaches an event bus so tools that publish (and the executor's
// tool.executed / tool.failed events) can be observed.
func (h *Harness) WithBus(bus *events.Bus) *Harness {
	h.Env.Bus = bus
	return h
}

// DB is the project database handle a tool sees: reads on the read pool, writes
// on the single-connection writer. Use it to seed and assert.
func (h *Harness) DB() dbh.Handle {
	return h.Project.ToolHandle(context.Background())
}

// Context is the ToolContext a tool call receives, for tests that drive a
// handler function directly rather than through Call.
func (h *Harness) Context() *tools.ToolContext {
	return h.Env.ForProject(context.Background(), h.Project, ProjectID, slog.Default())
}

// Call runs one tool call through Executor.Invoke, exactly as the engine does,
// and returns the decoded result. A call-level failure (panic, timeout) comes
// back as a failed Result, never as a test failure; assert on it.
func (h *Harness) Call(name string, args map[string]any) *tools.Result {
	h.t.Helper()
	res, _, _ := h.Executor.Invoke(context.Background(), h.Env, tools.Call{
		Name:      name,
		Args:      args,
		ProjectID: ProjectID,
		Project:   h.Project,
		Logger:    slog.Default(),
	})
	if res == nil {
		h.t.Fatalf("tooltest: %s returned a nil result", name)
	}
	return res
}

// MustOK runs a call and fails the test unless it succeeded.
func (h *Harness) MustOK(name string, args map[string]any) *tools.Result {
	h.t.Helper()
	res := h.Call(name, args)
	if !res.Success {
		h.t.Fatalf("%s %v: want success, got code=%q error=%q", name, args["action"], res.Code, res.Error)
	}
	return res
}

// MustFail runs a call and fails the test unless it failed; a non-empty code
// must also match the result's code.
func (h *Harness) MustFail(name string, args map[string]any, code string) *tools.Result {
	h.t.Helper()
	res := h.Call(name, args)
	if res.Success {
		h.t.Fatalf("%s %v: want failure, got success: %s", name, args["action"], DataJSON(res))
	}
	if code != "" && res.Code != code {
		h.t.Fatalf("%s %v: want code %q, got %q (error=%q)", name, args["action"], code, res.Code, res.Error)
	}
	return res
}

// DataJSON renders a result's Data for failure messages.
func DataJSON(res *tools.Result) string {
	b, err := json.Marshal(res.Data)
	if err != nil {
		return "<unmarshalable data>"
	}
	return string(b)
}
