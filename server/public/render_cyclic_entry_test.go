package public

import (
	"strings"
	"testing"
)

// TestInjectPageAssets_CyclicEntryStillEmitsScript guards the spine+leaf split
// pattern: the entry imports its leaves and the leaves import shared helpers back
// from the entry, so the import graph is a cycle and every node (the entry
// included) is "imported". Every attached JS file gets its own
// <script type="module"> tag unconditionally - the browser de-dupes module
// fetch+evaluation by URL, so a redundant tag is harmless, while skipping
// "imported" files would leave a cyclic page with an importmap but no entry
// script, booting nothing.
func TestInjectPageAssets_CyclicEntryStillEmitsScript(t *testing.T) {
	_, js := injectPageAssets([]string{"editor.js"}, nil)
	if !strings.Contains(js, `<script type="module" src="/files/editor.js"`) {
		t.Errorf("attached entry lost its module <script>; the page would boot nothing. got: %q", js)
	}
}
