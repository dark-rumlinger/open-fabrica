// Package firewall adds a local OS firewall "allow inbound" rule for Open
// Fabrica so projects are reachable from other devices with one click. It opens
// the LOCAL OS firewall only; internet access still needs router port-forwarding.
// Writing a rule needs elevation the main process lacks, so Ensure re-executes
// this binary under the OS elevation prompt; the parent then re-verifies the rule
// with an independent read-only query rather than trusting the child's exit code.
package firewall

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RuleName is the stable display name of the rule we manage. Used both to add
// the rule and to detect/delete it, so applying is idempotent.
const RuleName = "Open Fabrica"

// cmdTimeout bounds each non-interactive firewall CLI shell-out so a wedged
// firewall service cannot hang detection or a rule change forever. Interactive
// elevation prompts are NOT run through this - they legitimately block on the user.
const cmdTimeout = 15 * time.Second

// runCmd runs name+args under cmdTimeout and returns combined output. On timeout
// exec returns a context-deadline error, so callers surface a clear failure
// instead of blocking indefinitely.
func runCmd(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// applySubcommand is the hidden first-argument sentinel that marks an elevated
// child invocation. It is intentionally ugly so it cannot collide with a real
// user argument.
const applySubcommand = "__apply_firewall_rules"

// Rule describes the allow-rule to ensure. Windows uses a program-scoped rule
// (Program), Linux uses port-scoped rules (Ports). Profiles is the resolved set
// of Windows network profiles the rule applies to (empty on non-Windows).
type Rule struct {
	Name     string
	Program  string   // absolute path to the binary (Windows program rule)
	Ports    []int    // e.g. 80, 443, 5733 (Linux port rules)
	Profiles []string // windows: subset of {domain, private, public}
}

// Status is the read-only result of Detect. It never triggers a prompt.
type Status struct {
	Supported bool `json:"supported"` // is there a manageable firewall on this OS?
	Active    bool `json:"active"`    // is the firewall actually on / blocking inbound?

	// RulePresent answers one narrow question: does OUR rule exist - matched by
	// rule name on Windows, by port on Linux. True is conclusive. FALSE IS NOT:
	// the traffic may already be permitted by a rule Windows created from its own
	// prompt, a port rule, one a host image shipped with, or (on Linux) a rule
	// only root can read. Callers must not turn a false into "you are blocked";
	// the going-live card also drops the prompt entirely once HTTPS is serving,
	// which proves inbound works whatever this says.
	RulePresent bool `json:"rule_present"`

	NeedsElevation bool   `json:"needs_elevation"` // will applying prompt the user?
	Recommended    bool   `json:"recommended"`     // set by the caller from context (domain/LAN-share)
	Detail         string `json:"detail"`          // plain-language one-liner for the UI
}

// Result is the outcome of Ensure/Remove.
type Result struct {
	OK        bool   `json:"ok"`
	Cancelled bool   `json:"cancelled"` // user dismissed the elevation prompt
	Error     string `json:"error,omitempty"`
}

// Manager is implemented per OS.
type Manager interface {
	// Detect reads firewall state for the given rule without elevating.
	Detect(ctx context.Context, r Rule) (Status, error)
	// Ensure adds the rule, prompting for elevation if needed.
	Ensure(ctx context.Context, r Rule) (Result, error)
	// Remove deletes the rule by name (the "close" half of the feature).
	Remove(ctx context.Context, name string) (Result, error)
}

// New returns the platform Manager.
func New() Manager { return newManager() }

// readResult reads the JSON Result an elevated child wrote to path. Shared by
// the Windows and Linux parents.
func readResult(path string) Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Error: "elevated helper produced no result"}
	}
	var res Result
	if jerr := json.Unmarshal(data, &res); jerr != nil {
		return Result{Error: "could not read elevated helper result"}
	}
	return res
}

// RunSubcommandIfPresent handles the elevated child invocation. It must be
// called as the very first thing in main(), before single-instance acquisition
// and any DB/server boot: the child must do nothing but apply the rule and exit.
// Returns true when it handled the invocation (caller should return immediately).
func RunSubcommandIfPresent() bool {
	if len(os.Args) < 2 || os.Args[1] != applySubcommand {
		return false
	}

	fs := flag.NewFlagSet(applySubcommand, flag.ContinueOnError)
	var resultPath, name, program, portsCSV, profilesCSV string
	var remove bool
	fs.StringVar(&resultPath, "result", "", "path to write the JSON result to")
	fs.StringVar(&name, "name", RuleName, "rule name")
	fs.StringVar(&program, "program", "", "program path for the rule")
	fs.StringVar(&portsCSV, "ports", "", "comma-separated TCP ports")
	fs.StringVar(&profilesCSV, "profiles", "", "comma-separated windows profiles")
	fs.BoolVar(&remove, "remove", false, "delete the rule instead of adding it")
	_ = fs.Parse(os.Args[2:])

	rule := Rule{
		Name:     name,
		Program:  program,
		Ports:    parsePortsCSV(portsCSV),
		Profiles: splitCSV(profilesCSV),
	}

	res := applyRulesElevated(rule, remove)
	if resultPath != "" {
		if data, err := json.Marshal(res); err == nil {
			_ = os.WriteFile(resultPath, data, 0o600)
		}
	}
	if res.OK {
		os.Exit(0)
	}
	os.Exit(1)
	return true // unreachable; keeps the signature honest
}

// --- pure helpers (OS-agnostic, unit-tested on any platform) ---

var profileHeaderRe = regexp.MustCompile(`(?m)^(Domain|Private|Public) Profile Settings:`)

// parseActiveProfiles extracts the active Windows profile names from the output
// of `netsh advfirewall show currentprofile`. netsh prints one
// "<Name> Profile Settings:" header per currently-applied profile. Returns
// lowercased names suitable for `profile=` (e.g. ["public"]).
func parseActiveProfiles(netshOutput string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, m := range profileHeaderRe.FindAllStringSubmatch(netshOutput, -1) {
		name := strings.ToLower(m[1])
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// firewallActive reports whether the firewall is on for the currently-applied
// profile, parsed from `netsh advfirewall show currentprofile`. A disabled
// firewall prints "State OFF", so absence of an ON state means no rule is
// needed.
func firewallActive(netshOutput string) bool {
	for _, line := range strings.Split(netshOutput, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "State" {
			return strings.EqualFold(f[1], "ON")
		}
	}
	return false
}

// ruleAbsent reports whether `netsh advfirewall firewall show rule name=<n>`
// indicates the rule does not exist.
func ruleAbsent(netshOutput string) bool {
	return strings.Contains(netshOutput, "No rules match the specified criteria")
}

// ruleAllowsInbound reports whether the rule output describes an effective
// inbound allow: at least one Allow action and no Block action. A Block rule
// wins on Windows - existence alone is not enough, since Windows creates Block
// rules when a user clicks "Cancel" on the firewall prompt.
func ruleAllowsInbound(netshOutput string) bool {
	if ruleAbsent(netshOutput) {
		return false
	}
	hasAllow, hasBlock := false, false
	for _, line := range strings.Split(netshOutput, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "Action:" {
			switch strings.ToLower(f[1]) {
			case "allow":
				hasAllow = true
			case "block":
				hasBlock = true
			}
		}
	}
	return hasAllow && !hasBlock
}

// netshAddArgs builds the argv (no shell) for adding a program-scoped inbound
// allow rule. Profiles is joined into a comma list; an empty list falls back to
// "private" so the rule is never silently applied to untrusted networks.
func netshAddArgs(name, program string, profiles []string) []string {
	prof := strings.Join(profiles, ",")
	if prof == "" {
		prof = "private"
	}
	return []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + name,
		"dir=in",
		"action=allow",
		"program=" + program,
		"enable=yes",
		"profile=" + prof,
	}
}

// socketfilterStateRe matches the "(State = N)" suffix macOS's socketfilterfw
// prints for --getglobalstate. 0 is off; 1 and 2 (block all) are on.
var socketfilterStateRe = regexp.MustCompile(`State = (\d)`)

// parseSocketfilterEnabled reports whether `socketfilterfw --getglobalstate`
// output says the macOS application firewall is on.
func parseSocketfilterEnabled(out string) bool {
	if m := socketfilterStateRe.FindStringSubmatch(out); m != nil {
		return m[1] != "0"
	}
	lower := strings.ToLower(out)
	return strings.Contains(lower, "enabled") && !strings.Contains(lower, "disabled")
}

// parseSocketfilterBlockAll reports whether `socketfilterfw --getblockall`
// says all incoming connections are blocked. Wording differs across macOS
// releases ("Block all ENABLED!", "Firewall has block all state set to
// enabled."), so this keys on enabled versus disabled.
func parseSocketfilterBlockAll(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "enabled") && !strings.Contains(lower, "disabled")
}

// parseSocketfilterAppAllowed reports whether `socketfilterfw --getappblocked
// <path>` says the app is permitted. An app missing from the list is not.
func parseSocketfilterAppAllowed(out string) bool {
	return strings.Contains(strings.ToLower(out), "permitted")
}

// osascriptElevatedArgs builds the osascript argv that runs argv as root
// behind the macOS administrator prompt. Every argument is passed through
// `quoted form of`, so a path with spaces or quotes stays one argument and
// cannot inject shell syntax.
func osascriptElevatedArgs(argv []string) []string {
	script := []string{
		"on run argv",
		`set cmd to ""`,
		"repeat with a in argv",
		`set cmd to cmd & quoted form of (a as text) & " "`,
		"end repeat",
		"do shell script cmd with administrator privileges",
		"end run",
	}
	var out []string
	for _, line := range script {
		out = append(out, "-e", line)
	}
	return append(out, argv...)
}

// osascriptCancelled reports whether osascript output means the user dismissed
// the administrator prompt (AppleScript error -128).
func osascriptCancelled(out string) bool {
	return strings.Contains(out, "-128") || strings.Contains(strings.ToLower(out), "user canceled")
}

func parsePortsCSV(s string) []int {
	var out []int
	for _, p := range splitCSV(s) {
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}
