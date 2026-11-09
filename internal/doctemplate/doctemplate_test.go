package doctemplate

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderFillsAndEscapesValues(t *testing.T) {
	out, err := Render(`<h1>Invoice {{number}}</h1><p>{{customer.name}} owes {{total}}</p>{{missing}}`, map[string]any{
		"number":   float64(42),
		"total":    12.5,
		"customer": map[string]any{"name": `<script>alert("x")</script> & Co`},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `<h1>Invoice 42</h1><p>&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; Co owes 12.5</p>`
	if out != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

func TestRenderEachBlocks(t *testing.T) {
	data := map[string]any{
		"currency": "EUR",
		"items": []any{
			map[string]any{"name": "Pen", "qty": float64(2)},
			map[string]any{"name": "Ink", "qty": float64(1)},
		},
		"tags": []any{"a", "b"},
	}
	out, err := Render(`{{#each items}}<li>{{@index}}. {{name}} x{{qty}} ({{../currency}})</li>{{/each}}|{{#each tags}}{{this}},{{/each}}`, data)
	if err != nil {
		t.Fatal(err)
	}
	want := `<li>1. Pen x2 (EUR)</li><li>2. Ink x1 (EUR)</li>|a,b,`
	if out != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

func TestRenderNestedEach(t *testing.T) {
	data := map[string]any{"rows": []any{
		map[string]any{"cells": []any{"a", "b"}},
		map[string]any{"cells": []any{"c"}},
	}}
	out, err := Render(`{{#each rows}}[{{#each cells}}{{this}}{{/each}}]{{/each}}`, data)
	if err != nil || out != "[ab][c]" {
		t.Errorf("nested: %q %v", out, err)
	}
}

func TestRenderRejectsMalformedAndOversizedTemplates(t *testing.T) {
	for name, tmpl := range map[string]string{
		"unclosed tag":    "Hello {{name",
		"unclosed block":  "{{#each items}}x",
		"stray close":     "x{{/each}}",
		"too deeply nest": "{{#each a}}{{#each b}}{{#each c}}{{#each d}}{{#each e}}x{{/each}}{{/each}}{{/each}}{{/each}}{{/each}}",
	} {
		if _, err := Render(tmpl, map[string]any{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	big := make([]any, MaxEachItems+500)
	for i := range big {
		big[i] = "x"
	}
	out, err := Render(`{{#each items}}x{{/each}}`, map[string]any{"items": big})
	if err != nil || len(out) != MaxEachItems {
		t.Errorf("each must be capped at %d items, got %d (%v)", MaxEachItems, len(out), err)
	}

	huge := strings.Repeat("{{v}}", 1000)
	if _, err := Render(huge, map[string]any{"v": strings.Repeat("y", 10_000)}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a runaway document must hit the output cap, got %v", err)
	}
}
