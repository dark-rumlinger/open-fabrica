package selfdestruct

import (
	"os"
	"path/filepath"

	"openfabrica/internal/singleinstance"
)

// instanceLockName is the name the app passes to singleinstance.Acquire.
const instanceLockName = "OpenFabrica-SingleInstance"

// DefaultPlan fills in the environment-specific parts of a plan from the running
// process. configPath is the config file that was loaded (it may not exist).
func DefaultPlan(dataDir, dbPath, logFile, configPath string) Plan {
	p := Plan{
		DataDir: dataDir,
		DBPath:  dbPath,
		LogFile: logFile,
		TempDir: os.TempDir(),
		Docker:  InContainer(),
	}
	if configPath != "" {
		if abs, err := filepath.Abs(configPath); err == nil {
			p.ConfigPath = abs
		}
	}
	if wd, err := os.Getwd(); err == nil {
		p.WorkDirs = append(p.WorkDirs, wd)
	}
	if lock := singleinstance.LockPath(instanceLockName); lock != "" {
		p.LockFiles = append(p.LockFiles, lock)
	}
	if exe, err := os.Executable(); err == nil {
		p.Executable = exe
	}
	return p
}

// InContainer reports whether the process runs inside a Docker container.
func InContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}
