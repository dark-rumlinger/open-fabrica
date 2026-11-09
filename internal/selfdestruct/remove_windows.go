//go:build windows

package selfdestruct

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// removeSelf schedules deletion of the running executable. Windows locks a
// running image, so a detached helper waits a few seconds for this process to
// exit and then deletes the file.
func removeSelf(exe string) error {
	// The path is placed inside a cmd command line, where % is still expanded.
	if strings.ContainsAny(exe, "%\"") {
		return errors.New("executable path has characters that cannot be deleted safely by the helper")
	}
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | detachedProcess,
		CmdLine:       `cmd.exe /c ping -n 6 127.0.0.1 >nul & del /f /q "` + exe + `"`,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}
