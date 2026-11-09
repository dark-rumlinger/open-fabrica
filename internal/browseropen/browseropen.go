// Package browseropen launches the user's default web browser to a URL,
// detached from the current process. Used at startup so the admin UI opens
// automatically once the server is listening.
package browseropen

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

var (
	// ErrDisabled is returned when OPENFABRICA_NO_BROWSER is set.
	ErrDisabled = errors.New("browser auto-open disabled via OPENFABRICA_NO_BROWSER")
	// ErrNoDisplay is returned on Linux/BSD when no graphical session is detected.
	ErrNoDisplay = errors.New("no graphical display detected")
	// ErrUnsupported is returned when the current GOOS has no known opener.
	ErrUnsupported = errors.New("browser auto-open not supported on this platform")
	// ErrNoOpener is returned when no candidate opener command exists on PATH.
	ErrNoOpener = errors.New("no browser-opener command found on PATH")
)

// Seams for testing. Production code uses the real os/exec functions.
var (
	lookPath = exec.LookPath
	getenv   = os.Getenv
	startCmd = func(c *exec.Cmd) error { return c.Start() }
	goos     = runtime.GOOS
)

// Open launches the default browser pointed at url. It returns immediately;
// the browser process is detached and we never wait for it. A non-nil error
// means the browser was not launched (the caller should log at debug and
// continue, never abort startup over a missing GUI).
func Open(url string) error {
	if v := getenv("OPENFABRICA_NO_BROWSER"); v != "" {
		return ErrDisabled
	}

	cmd, err := opener(url)
	if err != nil {
		return err
	}
	if err := startCmd(cmd); err != nil {
		return fmt.Errorf("launching %s: %w", cmd.Path, err)
	}
	return nil
}

func opener(url string) (*exec.Cmd, error) {
	switch goos {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url), nil
	case "darwin":
		return exec.Command("open", url), nil
	case "linux", "freebsd", "openbsd", "netbsd":
		if !hasDisplay() {
			return nil, ErrNoDisplay
		}
		for _, candidate := range []string{"xdg-open", "wslview", "gio", "gnome-open", "kde-open"} {
			path, err := lookPath(candidate)
			if err != nil {
				continue
			}
			if candidate == "gio" {
				return exec.Command(path, "open", url), nil
			}
			return exec.Command(path, url), nil
		}
		return nil, ErrNoOpener
	default:
		return nil, ErrUnsupported
	}
}

func hasDisplay() bool {
	if getenv("DISPLAY") != "" {
		return true
	}
	if getenv("WAYLAND_DISPLAY") != "" {
		return true
	}
	// WSL: Windows hosts the browser; wslview bridges to it.
	if getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	return false
}
