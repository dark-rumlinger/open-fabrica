package firewall

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"openfabrica/internal/msgcat"
)

// runCmdRun is runCmd for calls that only care about success/failure, not output
// (e.g. the idempotent delete-before-add). Windows-only: the netsh delete path.
func runCmdRun(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}

// netshDeleteArgs builds argv to delete every rule with the given name.
func netshDeleteArgs(name string) []string {
	return []string{"advfirewall", "firewall", "delete", "rule", "name=" + name}
}

// netshShowArgs builds argv to show a rule by name (read-only, no elevation).
func netshShowArgs(name string) []string {
	return []string{"advfirewall", "firewall", "show", "rule", "name=" + name}
}

// Windows API surface. Raw syscall.NewLazyDLL to match internal/winclose and
// avoid adding golang.org/x/sys as a dependency.
var (
	shell32             = syscall.NewLazyDLL("shell32.dll")
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")
	procWaitForSingle   = kernel32.NewProc("WaitForSingleObject")
	procCloseHandle     = kernel32.NewProc("CloseHandle")
)

const (
	seeMaskNoCloseProcess = 0x00000040
	swHide                = 0
	infinite              = 0xFFFFFFFF
	waitTimeoutMs         = 60000 // bound the wait on the elevated child's netsh work
	errorCancelled        = 1223  // user dismissed the UAC prompt
)

// shellExecuteInfoW mirrors SHELLEXECUTEINFOW. Field order and sizes must match
// the Win32 struct exactly; the kernel reads cbSize to validate.
type shellExecuteInfoW struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       uintptr
}

type windowsManager struct{}

func newManager() Manager { return &windowsManager{} }

func (m *windowsManager) Detect(_ context.Context, r Rule) (Status, error) {
	st := Status{Supported: true, NeedsElevation: true}

	cp, _ := runCmd("netsh", "advfirewall", "show", "currentprofile")
	st.Active = firewallActive(string(cp))

	show, _ := runCmd("netsh", netshShowArgs(r.Name)...)
	st.RulePresent = ruleAllowsInbound(string(show))

	switch {
	case !st.Active:
		st.Detail = msgcat.Mark("Windows Firewall is off, so nothing is blocking incoming connections.")
	case st.RulePresent:
		st.Detail = msgcat.Mark("Open Fabrica is already allowed through Windows Firewall.")
	default:
		// Deliberately not "you are blocked": this branch only knows that no rule
		// named RuleName exists. Windows may already permit the traffic through a
		// rule it created from its own prompt, a port rule, or one a host image
		// shipped with - which is common on machines that are already reachable.
		st.Detail = msgcat.Mark("Open Fabrica has no allow-rule of its own in Windows Firewall. That normally keeps other devices out, unless another rule is already letting them in.")
	}
	return st, nil
}

func (m *windowsManager) Ensure(ctx context.Context, r Rule) (Result, error) {
	profiles := r.Profiles
	if len(profiles) == 0 {
		profiles = m.activeProfiles()
	}

	tmp, err := os.CreateTemp("", "of-fw-*.json")
	if err != nil {
		return Result{Error: "could not create temp file: " + err.Error()}, nil
	}
	resultPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(resultPath) }()

	exe, err := os.Executable()
	if err != nil {
		return Result{Error: "could not locate own executable: " + err.Error()}, nil
	}

	params := buildParamString([]string{
		applySubcommand,
		"--result", resultPath,
		"--name", r.Name,
		"--program", exe,
		"--profiles", strings.Join(profiles, ","),
	})

	if err := runElevated(exe, params); err != nil {
		if isCancelled(err) {
			return Result{Cancelled: true}, nil
		}
		return Result{Error: "could not launch elevated helper: " + err.Error()}, nil
	}

	res := readResult(resultPath)

	// Re-verify independently rather than trusting the child's exit code.
	if st, derr := m.Detect(ctx, r); derr == nil && st.RulePresent {
		return Result{OK: true}, nil
	}
	if res.Error == "" {
		res.Error = "the firewall rule was not added"
	}
	return res, nil
}

func (m *windowsManager) Remove(ctx context.Context, name string) (Result, error) {
	tmp, err := os.CreateTemp("", "of-fw-*.json")
	if err != nil {
		return Result{Error: "could not create temp file: " + err.Error()}, nil
	}
	resultPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(resultPath) }()

	exe, err := os.Executable()
	if err != nil {
		return Result{Error: "could not locate own executable: " + err.Error()}, nil
	}

	params := buildParamString([]string{
		applySubcommand,
		"--remove",
		"--result", resultPath,
		"--name", name,
	})

	if err := runElevated(exe, params); err != nil {
		if isCancelled(err) {
			return Result{Cancelled: true}, nil
		}
		return Result{Error: err.Error()}, nil
	}
	return readResult(resultPath), nil
}

// activeProfiles returns the currently-applied Windows profiles, falling back
// to "private" so a rule is never silently scoped to untrusted networks.
func (m *windowsManager) activeProfiles() []string {
	out, _ := runCmd("netsh", "advfirewall", "show", "currentprofile")
	if profs := parseActiveProfiles(string(out)); len(profs) > 0 {
		return profs
	}
	return []string{"private"}
}

// applyRulesElevated runs in the elevated child. It performs the actual netsh
// writes (delete-then-add for idempotency) and returns the outcome.
func applyRulesElevated(r Rule, remove bool) Result {
	// Always delete first so re-running never stacks duplicate rules. A missing
	// rule makes delete exit non-zero; that is fine and ignored.
	_ = runCmdRun("netsh", netshDeleteArgs(r.Name)...)
	if remove {
		return Result{OK: true}
	}

	out, err := runCmd("netsh", netshAddArgs(r.Name, r.Program, r.Profiles)...)
	if err != nil {
		return Result{Error: fmt.Sprintf("netsh add rule failed: %v: %s", err, strings.TrimSpace(string(out)))}
	}
	return Result{OK: true}
}

// runElevated launches file with params under the UAC "runas" verb and waits
// for the elevated child to finish. Returns a cancelled error if the user
// dismisses the prompt.
func runElevated(file, params string) error {
	verbPtr, _ := syscall.UTF16PtrFromString("runas")
	filePtr, _ := syscall.UTF16PtrFromString(file)
	paramsPtr, _ := syscall.UTF16PtrFromString(params)

	info := shellExecuteInfoW{
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       verbPtr,
		lpFile:       filePtr,
		lpParameters: paramsPtr,
		nShow:        swHide,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		// ShellExecuteExW returned FALSE; callErr carries GetLastError.
		if errno, ok := callErr.(syscall.Errno); ok {
			return errno
		}
		return fmt.Errorf("ShellExecuteExW failed")
	}

	if info.hProcess != 0 {
		_, _, _ = procWaitForSingle.Call(info.hProcess, uintptr(waitTimeoutMs))
		_, _, _ = procCloseHandle.Call(info.hProcess)
	}
	return nil
}

func isCancelled(err error) bool {
	errno, ok := err.(syscall.Errno)
	return ok && errno == errorCancelled
}

// buildParamString joins argv into a single command-line string with Windows
// quoting, for ShellExecuteExW's lpParameters (which is one string, not argv).
func buildParamString(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = syscall.EscapeArg(a)
	}
	return strings.Join(parts, " ")
}
