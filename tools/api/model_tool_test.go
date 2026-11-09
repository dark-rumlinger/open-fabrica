package api

import (
	"testing"

	"openfabrica/internal/wsendpoint"
)

// modelTool must resolve every stored model constant AND the "webrtc" plan-choice
// alias to a real, registered tool, never "manage_webrtc" or "manage_".
func TestModelTool_ResolvesEveryModelAndAlias(t *testing.T) {
	cases := map[string]string{
		wsendpoint.ModelRelay:         "manage_relay",
		wsendpoint.ModelTick:          "manage_tick",
		wsendpoint.ModelAuthoritative: "manage_authoritative",
		wsendpoint.ModelMatchmaking:   "manage_matchmaking",
		wsendpoint.ModelSignaling:     "manage_signaling",
		"webrtc":                      "manage_signaling", // plan-choice / trait alias
	}
	for model, want := range cases {
		if got := modelTool(model); got != want {
			t.Errorf("modelTool(%q) = %q, want %q", model, got, want)
		}
	}
}
