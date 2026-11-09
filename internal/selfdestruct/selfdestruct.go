// Package selfdestruct erases an installation from the machine it runs on: the
// keys first, then the databases and data directory, then loose files, and
// finally the executable itself. Every step is best effort; a failure is
// recorded and the next step still runs.
package selfdestruct

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Plan lists what to erase. All fields are explicit so tests can point them at a
// scratch directory; DefaultPlan fills the environment-specific ones.
type Plan struct {
	// DataDir holds the databases, project files, keys and certificates.
	DataDir string
	// DBPath is the core database. It normally lives inside DataDir, but a config
	// can point it elsewhere.
	DBPath string
	// LogFile is the absolute path of the optional log file, or "".
	LogFile string
	// ConfigPath is the config file that was loaded, or "".
	ConfigPath string
	// WorkDirs are searched for backup archives.
	WorkDirs []string
	// TempDir is searched for the temporary files the app leaves behind.
	TempDir string
	// LockFiles are single-instance lock files to remove.
	LockFiles []string
	// Executable is the running binary, or "" to leave it alone.
	Executable string
	// Docker is true inside a container, where the data directory is a mount
	// point: its contents are removed but the directory itself may stay.
	Docker bool
}

// Actions are the steps that need live objects from the running process.
type Actions struct {
	// Quiesce stops background work and closes every database handle.
	Quiesce func()
	// RemoveFirewall deletes the OS firewall rule, if one was added.
	RemoveFirewall func(ctx context.Context) error
}

// Step is the outcome of one stage of the wipe.
type Step struct {
	Name string
	Err  error
}

// Report is the ordered outcome of Run.
type Report struct {
	Steps []Step
}

// Failed returns the steps that did not complete cleanly.
func (r Report) Failed() []Step {
	var out []Step
	for _, s := range r.Steps {
		if s.Err != nil {
			out = append(out, s)
		}
	}
	return out
}

// tempPrefixes are the names the app gives its scratch files and directories.
var tempPrefixes = []string{
	"of-backup-", "of-export-", "of-import-", "of-flowsnap-", "of-flow-uploads-", "of-fw-",
}

// overwriteCap bounds how much of one file is overwritten before removal; larger
// files are simply deleted.
const overwriteCap = 1 << 30

// sensitiveSuffixes mark files worth overwriting before they are deleted.
var sensitiveSuffixes = []string{".db", ".db-wal", ".db-shm", ".key"}

// Run executes the plan in order and returns what happened. The keys go first so
// that every encrypted value is unreadable even if a later step fails.
func Run(ctx context.Context, p Plan, a Actions) Report {
	var r Report
	step := func(name string, fn func() error) {
		r.Steps = append(r.Steps, Step{Name: name, Err: guard(fn)})
	}

	if err := p.validate(); err != nil {
		step("check installation", func() error { return err })
		return r
	}

	step("remove keys", p.removeKeys)
	if a.Quiesce != nil {
		step("stop services", func() error { a.Quiesce(); return nil })
	}
	if a.RemoveFirewall != nil {
		step("remove firewall rule", func() error { return a.RemoveFirewall(ctx) })
	}
	step("erase databases", p.eraseDatabases)
	step("erase data directory", p.eraseDataDir)
	step("erase log files", p.eraseLogs)
	step("erase loose files", p.eraseLoose)
	if p.Executable != "" {
		step("remove executable", func() error { return removeSelf(p.Executable) })
	}
	return r
}

// Describe lists, in order, what Run would do. It touches nothing.
func (p Plan) Describe() []string {
	var out []string
	out = append(out, "keys: "+filepath.Join(p.DataDir, ".security"))
	out = append(out, "firewall rule named by the app (if present)")
	for _, f := range p.databaseFiles() {
		out = append(out, "database: "+f)
	}
	if p.Docker {
		out = append(out, "data directory contents: "+p.DataDir+" (mount point is kept)")
	} else {
		out = append(out, "data directory: "+p.DataDir)
	}
	for _, f := range p.logFiles() {
		out = append(out, "log: "+f)
	}
	for _, f := range p.looseFiles() {
		out = append(out, "file: "+f)
	}
	if p.Executable != "" {
		out = append(out, "executable: "+p.Executable)
	}
	return out
}

// guard converts a panic inside a step into an error so the wipe keeps going.
func guard(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return fn()
}

// validate refuses to run against a directory that does not look like an
// installation, so a misconfigured data path can never wipe a root or home
// directory.
func (p Plan) validate() error {
	if p.DataDir == "" || !filepath.IsAbs(p.DataDir) {
		return errors.New("data directory is not an absolute path")
	}
	clean := filepath.Clean(p.DataDir)
	if filepath.Dir(clean) == clean {
		return errors.New("data directory is a filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(clean, home) {
		return errors.New("data directory is the home directory")
	}
	for _, marker := range []string{
		filepath.Join(clean, ".security"),
		filepath.Join(clean, "projects"),
		p.DBPath,
	} {
		if marker == "" {
			continue
		}
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
	}
	return errors.New("data directory does not look like an installation")
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func (p Plan) removeKeys() error {
	dir := filepath.Join(p.DataDir, ".security")
	var errs []error
	for _, name := range []string{"encryption.key", "jwt.key"} {
		overwrite(filepath.Join(dir, name))
	}
	if err := removeAllRetry(dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// databaseFiles lists every database file: the core one plus every per-project
// database found under the data directory.
func (p Plan) databaseFiles() []string {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		if path != "" && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		add(p.DBPath + suffix)
	}
	_ = filepath.WalkDir(p.DataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if hasSensitiveSuffix(path) {
			add(path)
		}
		return nil
	})
	return out
}

func hasSensitiveSuffix(path string) bool {
	lower := strings.ToLower(path)
	for _, s := range sensitiveSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

func (p Plan) eraseDatabases() error {
	var errs []error
	for _, f := range p.databaseFiles() {
		overwrite(f)
		if err := removeAllRetry(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p Plan) eraseDataDir() error {
	if err := removeContents(p.DataDir); err != nil {
		return err
	}
	// A container mount point cannot be removed, so it is left empty.
	if p.Docker {
		return nil
	}
	if err := os.Remove(p.DataDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p Plan) logFiles() []string {
	if p.LogFile == "" {
		return nil
	}
	out := []string{p.LogFile}
	for i := 1; i <= 5; i++ {
		out = append(out, fmt.Sprintf("%s.%d", p.LogFile, i))
	}
	return out
}

func (p Plan) eraseLogs() error {
	var errs []error
	for _, f := range p.logFiles() {
		if err := removeAllRetry(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// looseFiles lists the files that live outside the data directory.
func (p Plan) looseFiles() []string {
	var out []string
	add := func(path string) {
		if path != "" {
			out = append(out, path)
		}
	}
	add(p.ConfigPath)
	dirs := append([]string{}, p.WorkDirs...)
	if p.Executable != "" {
		exeDir := filepath.Dir(p.Executable)
		add(filepath.Join(exeDir, "seed.json"))
		dirs = append(dirs, exeDir)
	}
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "openfabrica-backup-*.zip"))
		out = append(out, matches...)
	}
	if p.TempDir != "" {
		for _, prefix := range tempPrefixes {
			matches, _ := filepath.Glob(filepath.Join(p.TempDir, prefix+"*"))
			out = append(out, matches...)
		}
	}
	out = append(out, p.LockFiles...)
	return out
}

func (p Plan) eraseLoose() error {
	var errs []error
	for _, f := range p.looseFiles() {
		if err := removeAllRetry(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// overwrite zeroes a regular file in place before it is deleted. It is best
// effort: journaling and copy-on-write filesystems may keep old blocks, so the
// deleted keys are the real protection and this only narrows the window.
func overwrite(path string) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > overwriteCap {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	zeros := make([]byte, 64<<10)
	for left := info.Size(); left > 0; {
		n := int64(len(zeros))
		if left < n {
			n = left
		}
		if _, err := f.Write(zeros[:n]); err != nil {
			return
		}
		left -= n
	}
	_ = f.Sync()
}

// removeAllRetry removes path, retrying briefly because antivirus scanners and
// just-closed handles can hold a file for a moment on Windows. A missing path
// is not an error.
func removeAllRetry(path string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// removeContents deletes everything inside dir but not dir itself.
func removeContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := removeAllRetry(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
