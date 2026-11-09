package firewall

import (
	"reflect"
	"strings"
	"testing"
)

// Sample `netsh advfirewall show currentprofile` output (Windows 11).
const currentProfilePublic = `
Public Profile Settings:
----------------------------------------------------------------------
State                                 ON
Firewall Policy                       BlockInbound,AllowOutbound
`

const currentProfileMultiHomed = `
Domain Profile Settings:
----------------------------------------------------------------------
State                                 ON

Private Profile Settings:
----------------------------------------------------------------------
State                                 ON
`

const currentProfileOff = `
Private Profile Settings:
----------------------------------------------------------------------
State                                 OFF
`

func TestParseActiveProfiles(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"single public", currentProfilePublic, []string{"public"}},
		{"multi homed", currentProfileMultiHomed, []string{"domain", "private"}},
		{"none", "no headers here", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseActiveProfiles(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseActiveProfiles() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFirewallActive(t *testing.T) {
	if !firewallActive(currentProfilePublic) {
		t.Error("expected active for State ON")
	}
	if firewallActive(currentProfileOff) {
		t.Error("expected inactive for State OFF")
	}
	if firewallActive("no state line") {
		t.Error("expected inactive when no State line")
	}
}

func TestRuleAbsent(t *testing.T) {
	if !ruleAbsent("\nNo rules match the specified criteria.\n") {
		t.Error("expected absent")
	}
	if ruleAbsent("Rule Name: Open Fabrica\nEnabled: Yes\n") {
		t.Error("expected present")
	}
}

// Real shape from `netsh advfirewall firewall show rule name="Open Fabrica"`:
// Windows piles up duplicate allow rules (TCP+UDP) when the prompt is approved.
const ruleAllowDuplicates = `
Rule Name:                            Open Fabrica
----------------------------------------------------------------------
Enabled:                              Yes
Direction:                            In
Profiles:                             Public
Protocol:                             TCP
Action:                               Allow

Rule Name:                            Open Fabrica
----------------------------------------------------------------------
Enabled:                              Yes
Direction:                            In
Profiles:                             Public
Protocol:                             UDP
Action:                               Allow
`

const ruleBlock = `
Rule Name:                            Open Fabrica
----------------------------------------------------------------------
Enabled:                              Yes
Direction:                            In
Action:                               Block
`

const ruleAllowAndBlock = ruleAllowDuplicates + ruleBlock

func TestRuleAllowsInbound(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"allow duplicates", ruleAllowDuplicates, true},
		{"block only", ruleBlock, false},
		{"block shadows allow", ruleAllowAndBlock, false},
		{"absent", "No rules match the specified criteria.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ruleAllowsInbound(tt.in); got != tt.want {
				t.Errorf("ruleAllowsInbound() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNetshAddArgsEmptyProfilesFallsBackToPrivate(t *testing.T) {
	got := netshAddArgs("Open Fabrica", "of.exe", nil)
	if got[len(got)-1] != "profile=private" {
		t.Errorf("expected fallback profile=private, got %q", got[len(got)-1])
	}
}

func TestParsePortsCSV(t *testing.T) {
	got := parsePortsCSV("80, 443 ,5733,,x")
	want := []int{80, 443, 5733}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePortsCSV() = %v, want %v", got, want)
	}
}

func TestParseSocketfilterEnabled(t *testing.T) {
	for out, want := range map[string]bool{
		"Firewall is enabled. (State = 1)\n":                                       true,
		"Firewall is disabled. (State = 0)\n":                                      false,
		"Firewall is blocking all non-essential incoming connections. (State = 2)": true,
		"Firewall is enabled.":                                                     true,
		"Firewall is disabled.":                                                    false,
	} {
		if got := parseSocketfilterEnabled(out); got != want {
			t.Errorf("parseSocketfilterEnabled(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestParseSocketfilterBlockAll(t *testing.T) {
	for out, want := range map[string]bool{
		"Firewall has block all state set to enabled.\n":  true,
		"Firewall has block all state set to disabled.\n": false,
		"Block all ENABLED!":                              true,
		"Block all DISABLED!":                             false,
	} {
		if got := parseSocketfilterBlockAll(out); got != want {
			t.Errorf("parseSocketfilterBlockAll(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestParseSocketfilterAppAllowed(t *testing.T) {
	if !parseSocketfilterAppAllowed("The application /Applications/openfabrica is permitted\n") {
		t.Error("expected permitted")
	}
	if parseSocketfilterAppAllowed("The application /Applications/openfabrica is blocked\n") {
		t.Error("expected blocked")
	}
	if parseSocketfilterAppAllowed("The application is not part of the firewall\n") {
		t.Error("an app missing from the list is not allowed")
	}
}

func TestOsascriptElevatedArgs(t *testing.T) {
	argv := []string{"/Users/a b/open'fabrica", applySubcommand, "--remove"}
	got := osascriptElevatedArgs(argv)
	// The program and its arguments trail the script as argv items, never
	// spliced into the script text.
	tail := got[len(got)-len(argv):]
	if !reflect.DeepEqual(tail, argv) {
		t.Errorf("argv tail = %v, want %v", tail, argv)
	}
	for _, a := range got[:len(got)-len(argv)] {
		if strings.Contains(a, "open'fabrica") {
			t.Errorf("program path leaked into the script: %q", a)
		}
	}
}

func TestOsascriptCancelled(t *testing.T) {
	if !osascriptCancelled("0:98: execution error: User canceled. (-128)") {
		t.Error("expected cancelled")
	}
	if osascriptCancelled("0:98: execution error: some other failure (-60005)") {
		t.Error("expected not cancelled")
	}
}
