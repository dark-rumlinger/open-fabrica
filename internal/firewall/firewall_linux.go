package firewall

import (
	"context"
	"fmt"
	"openfabrica/internal/msgcat"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type linuxManager struct{}

func newManager() Manager { return &linuxManager{} }

func hasUFW() bool       { _, err := exec.LookPath("ufw"); return err == nil }
func hasFirewalld() bool { _, err := exec.LookPath("firewall-cmd"); return err == nil }

// ufwAddArgs builds `ufw allow <port>/tcp`.
func ufwAddArgs(port int) []string {
	return []string{"allow", strconv.Itoa(port) + "/tcp"}
}

// ufwDeleteArgs builds `ufw delete allow <port>/tcp`.
func ufwDeleteArgs(port int) []string {
	return []string{"delete", "allow", strconv.Itoa(port) + "/tcp"}
}

func (m *linuxManager) Detect(_ context.Context, r Rule) (Status, error) {
	st := Status{NeedsElevation: os.Geteuid() != 0}

	switch {
	case hasUFW():
		st.Supported = true
		st.Active = ufwEnabled()
		st.RulePresent = ufwRulePresent(r.Ports) // best-effort, only readable as root
	case hasFirewalld():
		st.Supported = true
		out, _ := runCmd("firewall-cmd", "--state")
		st.Active = strings.Contains(string(out), "running")
		st.RulePresent = firewalldRulePresent(r.Ports)
	default:
		st.Supported = false
		st.Detail = msgcat.Mark("No managed firewall (ufw or firewalld) detected, nothing to configure.")
		return st, nil
	}

	switch {
	case !st.Active:
		st.Detail = msgcat.Mark("The firewall is not active, so nothing is blocking incoming connections.")
	case st.RulePresent:
		st.Detail = msgcat.Mark("The required ports are already allowed through the firewall.")
	default:
		// Same caution as the Windows branch, with an extra reason here: the rule
		// lists are only fully readable as root, so a "not found" from an
		// unprivileged process is genuinely inconclusive.
		st.Detail = msgcat.Mark("No rule allowing Open Fabrica's ports was found. That normally keeps visitors out, unless a rule this check cannot read is already allowing them.")
	}
	return st, nil
}

func (m *linuxManager) Ensure(ctx context.Context, r Rule) (Result, error) {
	// Already root (typical for a systemd service): apply directly, no prompt.
	if os.Geteuid() == 0 {
		return applyRulesElevated(r, false), nil
	}

	if _, err := exec.LookPath("pkexec"); err == nil {
		return m.viaPkexec(ctx, r, false)
	}

	return Result{Error: manualCommands(r, false)}, nil
}

func (m *linuxManager) Remove(ctx context.Context, name string) (Result, error) {
	r := Rule{Name: name}
	if os.Geteuid() == 0 {
		return applyRulesElevated(r, true), nil
	}
	if _, err := exec.LookPath("pkexec"); err == nil {
		return m.viaPkexec(ctx, r, true)
	}
	return Result{Error: manualCommands(r, true)}, nil
}

// viaPkexec re-executes this binary as root under polkit, which shows a GUI
// auth prompt once for the whole batch.
func (m *linuxManager) viaPkexec(_ context.Context, r Rule, remove bool) (Result, error) {
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

	args := []string{exe, applySubcommand, "--result", resultPath, "--name", r.Name, "--ports", portsCSV(r.Ports)}
	if remove {
		args = append(args, "--remove")
	}

	cmd := exec.Command("pkexec", args...)
	runErr := cmd.Run()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 126 {
			return Result{Cancelled: true}, nil // polkit auth dismissed / failed
		}
	}
	return readResult(resultPath), nil
}

// applyRulesElevated runs in the root child: applies (or removes) the port
// rules via ufw or firewalld.
func applyRulesElevated(r Rule, remove bool) Result {
	switch {
	case hasUFW():
		for _, p := range r.Ports {
			args := ufwAddArgs(p)
			if remove {
				args = ufwDeleteArgs(p)
			}
			if out, err := runCmd("ufw", args...); err != nil {
				return Result{Error: fmt.Sprintf("ufw failed: %v: %s", err, strings.TrimSpace(string(out)))}
			}
		}
		return Result{OK: true}
	case hasFirewalld():
		for _, p := range r.Ports {
			flag := "--add-port=" + strconv.Itoa(p) + "/tcp"
			if remove {
				flag = "--remove-port=" + strconv.Itoa(p) + "/tcp"
			}
			if out, err := runCmd("firewall-cmd", "--permanent", flag); err != nil {
				return Result{Error: fmt.Sprintf("firewall-cmd failed: %v: %s", err, strings.TrimSpace(string(out)))}
			}
		}
		if out, err := runCmd("firewall-cmd", "--reload"); err != nil {
			return Result{Error: fmt.Sprintf("firewall-cmd reload failed: %v: %s", err, strings.TrimSpace(string(out)))}
		}
		return Result{OK: true}
	default:
		return Result{Error: "no supported firewall (ufw or firewalld) found"}
	}
}

func ufwEnabled() bool {
	// /etc/ufw/ufw.conf is world-readable; avoids needing root just to detect.
	data, err := os.ReadFile("/etc/ufw/ufw.conf")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "ENABLED=yes") {
			return true
		}
	}
	return false
}

func ufwRulePresent(ports []int) bool {
	out, err := runCmd("ufw", "status") // succeeds only as root
	if err != nil {
		return false
	}
	s := string(out)
	for _, p := range ports {
		if !strings.Contains(s, strconv.Itoa(p)+"/tcp") {
			return false
		}
	}
	return len(ports) > 0
}

func firewalldRulePresent(ports []int) bool {
	out, err := runCmd("firewall-cmd", "--list-ports")
	if err != nil {
		return false
	}
	s := string(out)
	for _, p := range ports {
		if !strings.Contains(s, strconv.Itoa(p)+"/tcp") {
			return false
		}
	}
	return len(ports) > 0
}

func manualCommands(r Rule, remove bool) string {
	verb := "allow"
	if remove {
		verb = "delete allow"
	}
	var b strings.Builder
	b.WriteString("Could not find pkexec to prompt for permission. Run these as root:")
	for _, p := range r.Ports {
		b.WriteString(fmt.Sprintf("\n  sudo ufw %s %d/tcp", verb, p))
	}
	return b.String()
}

func portsCSV(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}
