package pack

import (
	"strings"
	"testing"
)

func TestApplyConditions(t *testing.T) {
	body := "a\n<!-- when:auth -->\nAUTH\n<!-- /when -->\nb\n<!-- when:!headless -->\nUI\n<!-- /when -->\n<!-- when:headless -->\nHEADLESS\n<!-- /when -->\nc\n"

	got := applyConditions(body, map[string]bool{"auth": false, "headless": false})
	if got != "a\nb\nUI\nc\n" {
		t.Errorf("auth off, ui project: %q", got)
	}
	got = applyConditions(body, map[string]bool{"auth": true, "headless": true})
	if got != "a\nAUTH\nb\nHEADLESS\nc\n" {
		t.Errorf("auth on, headless project: %q", got)
	}
	// Nothing known about the project: every block stays.
	got = applyConditions(body, nil)
	for _, want := range []string{"AUTH", "UI", "HEADLESS"} {
		if !strings.Contains(got, want) {
			t.Errorf("unset conditions must keep %s: %q", want, got)
		}
	}
	if strings.Contains(got, "<!--") {
		t.Errorf("markers never reach the prompt: %q", got)
	}
}

func TestOmitLaws(t *testing.T) {
	body := "## Platform laws\n\n### REST response envelope\n{{contract:api-response}}\n\n### Routing\n{{contract:routing}}\n"
	got := omitLaws(body, map[string]bool{"api-response": true})
	if strings.Contains(got, "REST response envelope") || strings.Contains(got, "api-response") {
		t.Errorf("the omitted law must go with its heading: %q", got)
	}
	if !strings.Contains(got, "### Routing\n{{contract:routing}}") {
		t.Errorf("other laws stay: %q", got)
	}
	if omitLaws(body, nil) != body {
		t.Error("no omissions leaves the body byte-identical")
	}
}
