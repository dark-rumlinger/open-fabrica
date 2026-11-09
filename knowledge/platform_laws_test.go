package knowledge

import (
	"strings"
	"testing"
)

// PlatformLawIDs scans only {{contract:}} tokens, so a law authored as inline
// prose under "## Platform laws" is invisible to every surface that reads the
// set (owner chat renders laws by id). The Module scope law shipped inline once
// and silently missed chat; this pins the structure: one contract token per
// `###` law, no inline-prose laws.
func TestEveryPlatformLawIsAContract(t *testing.T) {
	body, err := MentalModel()
	if err != nil {
		t.Fatalf("MentalModel: %v", err)
	}
	const heading = "## Platform laws"
	i := strings.Index(body, heading)
	if i < 0 {
		t.Fatal("mental-model.md lost its Platform laws heading")
	}
	section := body[i+len(heading):]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}

	laws := 0
	for _, block := range strings.Split(section, "\n### ")[1:] {
		laws++
		title := block
		if nl := strings.IndexByte(title, '\n'); nl >= 0 {
			title = title[:nl]
		}
		if !strings.Contains(block, "{{contract:") {
			t.Errorf("platform law %q is inline prose - PlatformLawIDs cannot see it, so owner chat "+
				"silently drops it. Author it as knowledge/contracts/<id>.contract.md and transclude it.", title)
		}
	}
	if laws == 0 {
		t.Fatal("no ### laws found under Platform laws; the section structure changed and this guard is blind")
	}

	ids, err := PlatformLawIDs()
	if err != nil {
		t.Fatalf("PlatformLawIDs: %v", err)
	}
	if len(ids) != laws {
		t.Errorf("PlatformLawIDs returned %d ids for %d ### laws - a law's token sits outside its "+
			"heading block or a heading carries two tokens", len(ids), laws)
	}
}
