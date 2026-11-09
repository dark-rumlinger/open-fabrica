package engine

import (
	"strings"

	"openfabrica/analysis"
)

// knownLibraryGlobals maps a classic library's normalised name (lowercase, no
// punctuation, no ".js" suffix) to the window global its script defines. The plan
// prompt asks the planner for global_name on every classic library, but an omitted one
// silently drops the undefined-reference gate for the whole project to non-blocking
// (the gate cannot tell a typo from the library's own global), so a name the platform
// already knows is filled in rather than left to the planner's memory.
var knownLibraryGlobals = map[string]string{
	"chart": "Chart", "chartjs": "Chart", "marked": "marked", "gsap": "gsap", "d3": "d3",
	"p5": "p5", "phaser": "Phaser", "pixi": "PIXI", "pixijs": "PIXI",
	"howler": "Howl", "howlerjs": "Howl", "jquery": "jQuery", "lodash": "_",
	"moment": "moment", "momentjs": "moment", "dayjs": "dayjs", "axios": "axios",
	"dompurify": "DOMPurify", "confetti": "confetti", "canvasconfetti": "confetti",
	"tone": "Tone", "tonejs": "Tone", "matter": "Matter", "matterjs": "Matter",
	"anime": "anime", "animejs": "anime", "three": "THREE", "threejs": "THREE",
	"leaflet": "L", "vue": "Vue", "react": "React", "reactdom": "ReactDOM",
	"hammer": "Hammer", "hammerjs": "Hammer", "sortable": "Sortable", "sortablejs": "Sortable",
	"papaparse": "Papa", "xlsx": "XLSX", "html2canvas": "html2canvas", "jspdf": "jspdf",
	"highlight": "hljs", "highlightjs": "hljs", "mermaid": "mermaid", "katex": "katex",
	"echarts": "echarts", "plotly": "Plotly", "plotlyjs": "Plotly", "cannon": "CANNON", "cannonjs": "CANNON",
}

// fillGlobalNames sets global_name on a classic script library that omitted it when the
// library is a well-known one. Runs after fillModuleNames so a library promoted to a
// module (which defines no global) is skipped, and never overwrites a name the planner
// gave. An unknown library is left empty: guessing one would make the gate block on a
// name the library never defines.
func fillGlobalNames(p *Plan) {
	for i := range p.ExternalLibraries {
		lib := &p.ExternalLibraries[i]
		if strings.TrimSpace(lib.GlobalName) != "" {
			continue
		}
		ext := analysis.ExternalLib{Name: lib.Name, URL: lib.URL, Kind: lib.Kind, ModuleName: lib.ModuleName}
		if !analysis.LibraryDefinesGlobal(ext) {
			continue
		}
		if g, ok := knownLibraryGlobals[normalizeLibraryName(lib.Name)]; ok {
			lib.GlobalName = g
		}
	}
}

// normalizeLibraryName lowercases a library name and drops everything but letters and
// digits, so "Chart.js", "chart-js" and "chartjs" meet, then trims a trailing "js".
func normalizeLibraryName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
