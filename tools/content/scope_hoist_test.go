package content

import "testing"

func TestResolveSaveScope(t *testing.T) {
	cases := []struct {
		name        string
		argScope    string
		meta        map[string]any
		wantScope   string
		wantHoisted bool
		wantMetaKey bool // whether "scope" should remain in meta afterwards
	}{
		{
			name:        "no scope anywhere defers to ArtifactSave (empty)",
			argScope:    "",
			meta:        map[string]any{"is_module": true},
			wantScope:   "",
			wantHoisted: false,
		},
		{
			name:        "scope nested in metadata is hoisted and stripped",
			argScope:    "",
			meta:        map[string]any{"is_module": true, "scope": "page"},
			wantScope:   "page",
			wantHoisted: true,
			wantMetaKey: false,
		},
		{
			name:        "explicit top-level scope wins over metadata",
			argScope:    "global",
			meta:        map[string]any{"scope": "page"},
			wantScope:   "global",
			wantHoisted: false,
			wantMetaKey: false, // stray key still stripped
		},
		{
			name:        "explicit top-level scope, no metadata scope",
			argScope:    "page",
			meta:        map[string]any{"is_module": true},
			wantScope:   "page",
			wantHoisted: false,
		},
		{
			name:        "empty metadata scope ignored, defers (empty)",
			argScope:    "",
			meta:        map[string]any{"scope": "  "},
			wantScope:   "",
			wantHoisted: false,
			wantMetaKey: false,
		},
		{
			name:        "nil metadata is safe, defers (empty)",
			argScope:    "",
			meta:        nil,
			wantScope:   "",
			wantHoisted: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotScope, gotHoisted := resolveSaveScope(c.argScope, c.meta)
			if gotScope != c.wantScope {
				t.Errorf("scope = %q, want %q", gotScope, c.wantScope)
			}
			if gotHoisted != c.wantHoisted {
				t.Errorf("hoisted = %v, want %v", gotHoisted, c.wantHoisted)
			}
			if c.meta != nil {
				if _, present := c.meta["scope"]; present != c.wantMetaKey {
					t.Errorf("meta scope present = %v, want %v", present, c.wantMetaKey)
				}
			}
		})
	}
}
