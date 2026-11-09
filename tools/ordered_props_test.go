package tools

import (
	"encoding/json"
	"testing"
)

func TestOrderedPropsKeepsAuthoredOrder(t *testing.T) {
	b, err := json.Marshal(map[string]any{"properties": OrderedProps{
		Keys:  []string{"search", "replace"},
		Props: map[string]any{"count": 1, "replace": "r", "search": "s", "near": "n"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"properties":{"search":"s","replace":"r","count":1,"near":"n"}}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}
