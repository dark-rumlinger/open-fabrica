package selfdestruct

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeInstall builds a scratch installation and returns its plan.
func makeInstall(t *testing.T) Plan {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	write := func(rel, body string) string {
		path := filepath.Join(data, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(".security/encryption.key", "secret-encryption-key")
	write(".security/jwt.key", "secret-jwt-key")
	write("openfabrica.db", "core")
	write("openfabrica.db-wal", "wal")
	write("projects/1/project.db", "project")
	write("projects/1/files/uploads/a.txt", "upload")
	write("certs/acme/account.key", "acme")

	work := filepath.Join(root, "work")
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{work, tmp} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	zip := filepath.Join(work, "openfabrica-backup-20260101-000000.zip")
	cfg := filepath.Join(work, "config.json")
	scratch := filepath.Join(tmp, "of-export-123")
	keep := filepath.Join(tmp, "unrelated.txt")
	logf := filepath.Join(work, "app.log")
	for _, f := range []string{zip, cfg, scratch, keep, logf, logf + ".1"} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Plan{
		DataDir:    data,
		DBPath:     filepath.Join(data, "openfabrica.db"),
		LogFile:    logf,
		ConfigPath: cfg,
		WorkDirs:   []string{work},
		TempDir:    tmp,
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRunErasesEverything(t *testing.T) {
	p := makeInstall(t)
	rep := Run(context.Background(), p, Actions{})

	if failed := rep.Failed(); len(failed) != 0 {
		t.Fatalf("unexpected failures: %+v", failed)
	}
	if exists(p.DataDir) {
		t.Errorf("data dir still exists")
	}
	for _, f := range []string{p.ConfigPath, p.LogFile, p.LogFile + ".1",
		filepath.Join(p.WorkDirs[0], "openfabrica-backup-20260101-000000.zip"),
		filepath.Join(p.TempDir, "of-export-123")} {
		if exists(f) {
			t.Errorf("%s still exists", f)
		}
	}
	if !exists(filepath.Join(p.TempDir, "unrelated.txt")) {
		t.Errorf("unrelated temp file was removed")
	}
}

func TestRunOrderAndActions(t *testing.T) {
	p := makeInstall(t)
	keyPath := filepath.Join(p.DataDir, ".security", "encryption.key")

	var keysGoneAtQuiesce, dbPresentAtQuiesce, firewallCalled bool
	rep := Run(context.Background(), p, Actions{
		Quiesce: func() {
			keysGoneAtQuiesce = !exists(keyPath)
			dbPresentAtQuiesce = exists(p.DBPath)
		},
		RemoveFirewall: func(context.Context) error { firewallCalled = true; return nil },
	})

	if !keysGoneAtQuiesce {
		t.Error("keys must be removed before services stop")
	}
	if !dbPresentAtQuiesce {
		t.Error("databases must still exist while services stop")
	}
	if !firewallCalled {
		t.Error("firewall action was not called")
	}
	var names []string
	for _, s := range rep.Steps {
		names = append(names, s.Name)
	}
	got := strings.Join(names, ",")
	want := "remove keys,stop services,remove firewall rule,erase databases,erase data directory,erase log files,erase loose files"
	if got != want {
		t.Errorf("step order = %q, want %q", got, want)
	}
}

func TestRunContinuesAfterFailure(t *testing.T) {
	p := makeInstall(t)
	rep := Run(context.Background(), p, Actions{
		Quiesce:        func() { panic("boom") },
		RemoveFirewall: func(context.Context) error { return errors.New("no rights") },
	})

	failed := rep.Failed()
	if len(failed) != 2 {
		t.Fatalf("want 2 failed steps, got %+v", failed)
	}
	if exists(p.DataDir) {
		t.Error("later steps must still run after earlier failures")
	}
}

func TestRunDockerKeepsMountPoint(t *testing.T) {
	p := makeInstall(t)
	p.Docker = true
	rep := Run(context.Background(), p, Actions{})
	if failed := rep.Failed(); len(failed) != 0 {
		t.Fatalf("unexpected failures: %+v", failed)
	}
	entries, err := os.ReadDir(p.DataDir)
	if err != nil {
		t.Fatalf("mount point should remain: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("mount point not empty: %d entries", len(entries))
	}
}

func TestValidateRefusesUnsafeDirs(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := map[string]Plan{
		"empty":    {},
		"relative": {DataDir: "data"},
		"root":     {DataDir: filepath.VolumeName(home) + string(filepath.Separator)},
		"home":     {DataDir: home},
		"unmarked": {DataDir: t.TempDir()},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			rep := Run(context.Background(), p, Actions{})
			if len(rep.Failed()) != 1 || rep.Steps[0].Name != "check installation" {
				t.Fatalf("want a refused plan, got %+v", rep.Steps)
			}
			if len(rep.Steps) != 1 {
				t.Errorf("no step may run after refusal, got %d", len(rep.Steps))
			}
		})
	}
}

func TestDescribeTouchesNothing(t *testing.T) {
	p := makeInstall(t)
	lines := p.Describe()
	if len(lines) == 0 {
		t.Fatal("empty description")
	}
	if !exists(filepath.Join(p.DataDir, ".security", "encryption.key")) {
		t.Error("Describe must not delete anything")
	}
}

func TestOverwriteZeroesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	overwrite(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "\x00\x00\x00\x00\x00\x00" {
		t.Errorf("file not zeroed: %q", data)
	}
}
