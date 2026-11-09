// Package winclose registers a Windows console control handler so closing the
// console window, logging off, or shutting down triggers graceful shutdown:
// signal.Notify only catches Ctrl+C / Ctrl+Break; CTRL_CLOSE_EVENT bypasses it.
package winclose

import (
	"syscall"
	"time"
)

const (
	ctrlCloseEvent    = 2
	ctrlLogoffEvent   = 5
	ctrlShutdownEvent = 6
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// NotifyOnClose registers cancel to be invoked when the user closes the console
// window or the OS sends a logoff / shutdown event; Ctrl+C and Ctrl+Break pass
// through to the default handler. The callback blocks ~4.5s after cancel for
// draining - Windows force-terminates the process at ~5s regardless.
func NotifyOnClose(cancel func()) error {
	cb := syscall.NewCallback(func(ctrlType uint32) uintptr {
		switch ctrlType {
		case ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent:
			cancel()
			time.Sleep(4500 * time.Millisecond)
			return 1
		}
		return 0
	})
	ret, _, err := procSetConsoleCtrlHandler.Call(cb, 1)
	if ret == 0 {
		return err
	}
	return nil
}
