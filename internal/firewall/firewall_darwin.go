package firewall

import (
	"context"
	"openfabrica/internal/msgcat"
	"os"
	"os/exec"
	"strings"
)

// socketfilterfw is the command-line front end of the macOS application
// firewall. Its read-only queries work without root.
const socketfilterfw = "/usr/libexec/ApplicationFirewall/socketfilterfw"

// macOS manages the application firewall per program, like the Windows rule:
// the rule allows this binary, whatever ports it binds. Changing it needs
// root, so Ensure re-executes this binary behind the administrator prompt,
// the same way the Linux manager goes through pkexec.
type darwinManager struct{}

func newManager() Manager { return &darwinManager{} }

func (m *darwinManager) Detect(_ context.Context, _ Rule) (Status, error) {
	if _, err := os.Stat(socketfilterfw); err != nil {
		return Status{Detail: msgcat.Mark("The macOS firewall tool was not found, nothing to configure.")}, nil
	}
	st := Status{Supported: true, NeedsElevation: os.Geteuid() != 0}

	out, _ := runCmd(socketfilterfw, "--getglobalstate")
	st.Active = parseSocketfilterEnabled(string(out))
	if !st.Active {
		st.Detail = msgcat.Mark("The macOS firewall is off, so nothing is blocking incoming connections.")
		return st, nil
	}

	if out, _ := runCmd(socketfilterfw, "--getblockall"); parseSocketfilterBlockAll(string(out)) {
		// An app rule cannot override "block all", so there is nothing a
		// button can do; say what the owner has to change instead.
		st.Supported = false
		st.Detail = msgcat.Mark("The macOS firewall blocks all incoming connections. Turn off \"Block all incoming connections\" in System Settings > Network > Firewall > Options to let visitors in.")
		return st, nil
	}

	exe, err := os.Executable()
	if err != nil {
		st.Detail = msgcat.Mark("Could not locate this program to check its firewall rule.")
		return st, nil
	}
	out, _ = runCmd(socketfilterfw, "--getappblocked", exe)
	st.RulePresent = parseSocketfilterAppAllowed(string(out))
	if st.RulePresent {
		st.Detail = msgcat.Mark("Open Fabrica is allowed through the macOS firewall.")
	} else {
		st.Detail = msgcat.Mark("Open Fabrica is not on the macOS firewall's allowed list yet.")
	}
	return st, nil
}

func (m *darwinManager) Ensure(ctx context.Context, r Rule) (Result, error) {
	return m.elevated(ctx, r, false)
}

func (m *darwinManager) Remove(ctx context.Context, name string) (Result, error) {
	return m.elevated(ctx, Rule{Name: name}, true)
}

// elevated applies the change directly when already root, and otherwise
// re-executes this binary as root through osascript, which shows the standard
// administrator prompt.
func (m *darwinManager) elevated(_ context.Context, r Rule, remove bool) (Result, error) {
	exe, err := os.Executable()
	if err != nil {
		return Result{Error: "could not locate own executable: " + err.Error()}, nil
	}
	r.Program = exe
	if os.Geteuid() == 0 {
		return applyRulesElevated(r, remove), nil
	}

	tmp, err := os.CreateTemp("", "of-fw-*.json")
	if err != nil {
		return Result{Error: "could not create temp file: " + err.Error()}, nil
	}
	resultPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(resultPath) }()

	argv := []string{exe, applySubcommand, "--result", resultPath, "--name", r.Name, "--program", exe}
	if remove {
		argv = append(argv, "--remove")
	}
	// Not under cmdTimeout: the prompt legitimately waits on the user.
	out, runErr := exec.Command("osascript", osascriptElevatedArgs(argv)...).CombinedOutput()
	if runErr != nil && osascriptCancelled(string(out)) {
		return Result{Cancelled: true}, nil
	}
	res := readResult(resultPath)
	if !res.OK && res.Error == "elevated helper produced no result" && runErr != nil {
		res.Error = "could not get administrator permission: " + strings.TrimSpace(string(out))
	}
	return res, nil
}

// applyRulesElevated runs as root: adds the program to the application
// firewall and unblocks it, or removes it.
func applyRulesElevated(r Rule, remove bool) Result {
	if r.Program == "" {
		return Result{Error: "no program given for the firewall rule"}
	}
	steps := [][]string{{"--add", r.Program}, {"--unblockapp", r.Program}}
	if remove {
		steps = [][]string{{"--remove", r.Program}}
	}
	for _, args := range steps {
		if out, err := runCmd(socketfilterfw, args...); err != nil {
			return Result{Error: "socketfilterfw failed: " + err.Error() + ": " + strings.TrimSpace(string(out))}
		}
	}
	return Result{OK: true}
}
