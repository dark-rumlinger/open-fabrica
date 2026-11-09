package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/jobs"
	"openfabrica/providers/llm"
	"openfabrica/security"
)

// TestNewToolEnvSetsEveryField holds the composition root to its one job for
// tools: every tools.Env field is populated, so no tool call path runs with a
// dependency silently nil. A field added to tools.Env fails here until wired
// (EmbedFunc went unset in production for exactly this reason).
func TestNewToolEnvSetsEveryField(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	a := &appDeps{
		cfg:          &config.Config{PublicPort: 5733},
		database:     database,
		projectDBMgr: db.NewProjectDBManager(dir),
		encryptor:    enc,
		jwtMgr:       security.NewJWTManager(strings.Repeat("s", 32), time.Hour),
		bus:          events.NewBus(),
		llmRegistry:  llm.NewRegistry(),
	}
	env := reflect.ValueOf(*newToolEnv(a))
	for i := 0; i < env.NumField(); i++ {
		if env.Field(i).IsZero() {
			t.Errorf("newToolEnv leaves tools.Env.%s unset", env.Type().Field(i).Name)
		}
	}
}

// Deleting a project by any route (the admin API, the Telegram bot) publishes
// project.deleted; the job worker for it must stop. A bot-side delete used to leave
// the worker polling a closed database until the next restart.
func TestProjectDeletedStopsTheJobWorker(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	pm := db.NewProjectDBManager(dir)
	t.Cleanup(func() { pm.CloseAll() })
	if _, err := pm.Create(3); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	jobsMgr := jobs.NewManager(&jobs.Deps{DB: database, ProjectDBManager: pm}, ctx)
	t.Cleanup(jobsMgr.StopAll)
	if err := jobsMgr.StartProject(3); err != nil {
		t.Fatal(err)
	}
	if !jobsMgr.HasWorker(3) {
		t.Fatal("the worker did not start")
	}

	bus := events.NewBus()
	// Only the deleted-event subscriber is exercised, so no engine is needed.
	wireEngineWakeSubscribers(bus, nil, jobsMgr)
	bus.Publish(events.NewEvent(events.EventProjectDeleted, 3, nil))

	if jobsMgr.HasWorker(3) {
		t.Error("the job worker is still running after the project was deleted")
	}
}
