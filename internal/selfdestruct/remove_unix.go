//go:build !windows

package selfdestruct

import "os"

// removeSelf deletes the running executable. Unix filesystems allow unlinking a
// binary that is still mapped, so the process can simply exit afterwards.
func removeSelf(exe string) error {
	if err := os.Remove(exe); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
