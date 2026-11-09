package analysis

// Undefined-reference (forgotten-symbol) analysis: the source-level check the
// import gate cannot do. esbuild treats an unresolved free identifier as an
// assumed runtime global, so a module reaching for a name nothing gives it
// parses and links clean and only throws `<name> is not defined` when that path
// runs - possibly on interaction, which the page-load probe never exercises.
// Both entry points read the original source, never minified output.
//
// UndefinedRefs resolves names against enclosing scopes (js_scope.go) for
// module bodies; UndefinedCallRefs uses a flat whole-body reading that holds
// for classic scripts. Both are biased hard toward zero false positives: a
// missed reference is acceptable, a build wrongly blocked is not. Every
// uncertainty treats a name as bound.

import (
	"regexp"
	"sort"
	"strings"
)

// UndefinedRef is one identifier the module neither binds, imports, nor shares
// with the known-globals allowlist - a guaranteed `<Name> is not defined`
// ReferenceError the instant that code path runs.
type UndefinedRef struct {
	Name string  // the undeclared identifier
	Line int     // 1-based line of the reference Kind describes
	Kind RefKind // how the module uses the name; "" from UndefinedCallRefs
}

// RefKind is the position an unresolved name is used in. A read can be satisfied
// by an import; an assignment cannot, because an import binding is read-only.
type RefKind string

const (
	RefCall   RefKind = "call"   // `foo(...)`
	RefMember RefKind = "member" // `foo.bar`, `foo?.bar`
	RefAssign RefKind = "assign" // `foo = ...`
)

// newNameSet builds a lookup set from a name list.
func newNameSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// jsKeywords are reserved words that read like an identifier before a `(` or `=>`
// but never name a callable binding, so they are never flagged or collected.
var jsKeywords = newNameSet(
	"if", "else", "for", "while", "do", "switch", "case", "default", "break",
	"continue", "return", "function", "var", "let", "const", "new", "delete",
	"typeof", "instanceof", "in", "of", "void", "yield", "await", "async",
	"class", "extends", "super", "this", "import", "export", "from", "as",
	"try", "catch", "finally", "throw", "with", "debugger", "true", "false",
	"null", "undefined",
)

// jsGlobals is the curated allowlist of ambient JS + browser globals a module may
// reference without declaring or importing. Broad by rule: an ambient global
// wrongly read as undefined blocks a correct build, which costs more than the
// finding it would have produced. A missing entry surfaces in the corpus sweep
// before the check gates at error severity.
var jsGlobals = newNameSet(
	// language builtins
	"Object", "Array", "String", "Number", "Boolean", "BigInt", "Symbol",
	"Math", "JSON", "Date", "RegExp", "Function", "Promise", "Proxy", "Reflect",
	"Map", "Set", "WeakMap", "WeakSet", "WeakRef", "FinalizationRegistry",
	"Error", "EvalError", "RangeError", "ReferenceError", "SyntaxError",
	"TypeError", "URIError", "AggregateError", "Intl", "globalThis", "eval",
	"parseInt", "parseFloat", "isNaN", "isFinite", "encodeURIComponent",
	"decodeURIComponent", "encodeURI", "decodeURI", "escape", "unescape",
	"structuredClone", "ArrayBuffer", "SharedArrayBuffer", "DataView",
	"Int8Array", "Uint8Array", "Uint8ClampedArray", "Int16Array", "Uint16Array",
	"Int32Array", "Uint32Array", "Float32Array", "Float64Array",
	"BigInt64Array", "BigUint64Array",
	// timers and microtasks
	"setTimeout", "clearTimeout", "setInterval", "clearInterval",
	"requestAnimationFrame", "cancelAnimationFrame", "requestIdleCallback",
	"cancelIdleCallback", "queueMicrotask", "reportError",
	// window / bom
	"window", "self", "document", "navigator", "location", "history", "screen",
	"console", "alert", "confirm", "prompt", "localStorage", "sessionStorage",
	"indexedDB", "caches", "crypto", "performance", "matchMedia",
	"getComputedStyle", "scrollTo", "scrollBy", "postMessage", "addEventListener",
	"removeEventListener", "dispatchEvent", "getSelection",
	// networking / data
	"fetch", "XMLHttpRequest", "FormData", "Headers", "Request", "Response",
	"URL", "URLSearchParams", "Blob", "File", "FileReader", "FileList",
	"WebSocket", "EventSource", "AbortController", "AbortSignal", "TextEncoder",
	"TextDecoder", "btoa", "atob", "Notification", "DOMParser", "XMLSerializer",
	// observers / workers
	"MutationObserver", "ResizeObserver", "IntersectionObserver",
	"PerformanceObserver", "Worker", "SharedWorker",
	// events and nodes (used with `new` or as instanceof targets)
	"Event", "CustomEvent", "MouseEvent", "PointerEvent", "KeyboardEvent",
	"WheelEvent", "TouchEvent", "DragEvent", "InputEvent", "FocusEvent",
	"UIEvent", "ProgressEvent", "MessageEvent", "CloseEvent", "StorageEvent",
	// (HashChangeEvent's absence failed a correct `new HashChangeEvent('hashchange')`
	// and cost a fix-pass rewriting working code.)
	"HashChangeEvent", "PopStateEvent", "PageTransitionEvent", "BeforeUnloadEvent",
	"SubmitEvent", "FormDataEvent", "AnimationEvent", "TransitionEvent", "CompositionEvent",
	"ErrorEvent", "PromiseRejectionEvent", "ToggleEvent", "MediaQueryListEvent",
	"SecurityPolicyViolationEvent", "BlobEvent", "EventTarget",
	"Node", "Element", "HTMLElement", "HTMLCanvasElement", "HTMLImageElement",
	"HTMLInputElement", "DocumentFragment", "Text", "Range", "NodeList",
	"Image", "Audio", "Option",
	// canvas / graphics (this build class leans on these)
	"Path2D", "DOMMatrix", "DOMPoint", "DOMRect", "ImageData", "ImageBitmap",
	"OffscreenCanvas", "createImageBitmap", "CanvasRenderingContext2D",
	"WebGLRenderingContext", "WebGL2RenderingContext",
	// audio
	"AudioContext", "webkitAudioContext", "OscillatorNode", "GainNode",
	"AnalyserNode", "AudioBuffer", "AudioBufferSourceNode", "BiquadFilterNode",
	"DynamicsCompressorNode", "AudioWorkletNode",
	// webrtc (peer-to-peer media / data - a whole build class hand-writes these)
	"RTCPeerConnection", "RTCSessionDescription", "RTCIceCandidate", "RTCDataChannel",
	"RTCRtpSender", "RTCRtpReceiver", "RTCRtpTransceiver", "RTCDtlsTransport",
	"RTCCertificate", "RTCPeerConnectionIceEvent", "RTCDataChannelEvent",
	"RTCTrackEvent",
	// media capture / playback
	"MediaStream", "MediaStreamTrack", "MediaRecorder", "MediaSource", "MediaDevices",
	"MediaStreamTrackEvent", "SourceBuffer",
	// web crypto types (the `crypto` global is above; these are the type/ctor names)
	"CryptoKey", "SubtleCrypto",
	// channels / streams
	"BroadcastChannel", "MessageChannel", "MessagePort",
	"ReadableStream", "WritableStream", "TransformStream",
	"ReadableStreamDefaultReader", "WritableStreamDefaultWriter",
	// clipboard (ClipboardItem in particular: a real Clipboard API global whose
	// absence here blocked a correct write)
	"ClipboardItem", "ClipboardEvent", "Clipboard",
	// speech, viewport, fonts, device
	"speechSynthesis", "SpeechSynthesisUtterance", "SpeechRecognition",
	"visualViewport", "VisualViewport", "FontFace", "FontFaceSet", "CSS",
	"ScreenOrientation", "DeviceOrientationEvent", "DeviceMotionEvent",
	"GamepadEvent", "Gamepad",
	// service worker / push (a PWA build class reaches for these)
	"ServiceWorker", "ServiceWorkerRegistration", "ServiceWorkerContainer",
	"PushManager", "PushSubscription", "PermissionStatus",
	// more element types used with `new` or as instanceof targets
	"HTMLVideoElement", "HTMLAudioElement", "HTMLMediaElement", "HTMLFormElement",
	"HTMLSelectElement", "HTMLTextAreaElement", "HTMLButtonElement",
	"HTMLTemplateElement", "HTMLDivElement", "HTMLSpanElement", "HTMLAnchorElement",
	"HTMLDialogElement", "HTMLLabelElement", "HTMLOptionElement", "HTMLTableElement",
	"SVGElement", "SVGSVGElement", "HTMLCollection", "DOMTokenList", "NamedNodeMap",
	// language / platform builtins and window aliases
	"WebAssembly", "Atomics", "CompressionStream", "DecompressionStream",
	"top", "parent", "frames", "origin", "isSecureContext", "devicePixelRatio",
	"innerWidth", "innerHeight", "outerWidth", "outerHeight", "scrollX", "scrollY",
)

// IsJSGlobal reports whether name is a known ambient JS / DOM / BOM global from the
// curated allowlist above. Exported so other write gates - the window-global bridge
// in particular - share this one corpus-swept notion of "a name the platform or
// browser provides" instead of maintaining a second copy that drifts out of sync.
func IsJSGlobal(name string) bool { return jsGlobals[name] }

var (
	// A default binding: `import Def from 'x'` or `import Def, { a } from 'x'`.
	// Named-only (`import {`), namespace (`import *`), and side-effect
	// (`import 'x'`) forms have no leading identifier and never match.
	defaultImportRe = regexp.MustCompile(`\bimport\s+([A-Za-z_$][\w$]*)`)
	// A namespace binding: `import * as ns from 'x'` (optionally after a default).
	namespaceImportRe = regexp.MustCompile(`\bimport\s+(?:[A-Za-z_$][\w$]*\s*,\s*)?\*\s+as\s+([A-Za-z_$][\w$]*)`)
	// A named function declaration or expression (generators and anonymous forms
	// too; anonymous has no name group so it simply does not match).
	funcDeclRe = regexp.MustCompile(`\bfunction\b\s*\*?\s*([A-Za-z_$][\w$]*)`)
	// A class declaration or named class expression.
	classDeclRe = regexp.MustCompile(`\bclass\s+([A-Za-z_$][\w$]*)`)
	// The head of a var/let/const declaration; the declarator names that follow are
	// mined by appendDeclaratorNames (destructuring and multi-declarator aware).
	varDeclKwRe = regexp.MustCompile(`\b(?:var|let|const)\s+`)
	// A function header's parameter list (declaration, expression, generator).
	funcParamsRe = regexp.MustCompile(`\bfunction\b\s*\*?\s*[A-Za-z_$]?[\w$]*\s*\(([^()]*)\)`)
	// An arrow with a parenthesized parameter list.
	arrowParenRe = regexp.MustCompile(`\(([^()]*)\)\s*=>`)
	// An arrow with a single bare-identifier parameter (`x => ...`).
	arrowIdentRe = regexp.MustCompile(`(^|[^.\w$])([A-Za-z_$][\w$]*)\s*=>`)
	// A method / getter / setter head whose body follows (`name(params) {`). Group 1
	// is the name (skipped when it is a keyword, i.e. `if (...) {`, `for (...) {`),
	// group 2 the parameter list.
	methodParamsRe = regexp.MustCompile(`(?:^|[^.\w$])([A-Za-z_$][\w$]*)\s*\(([^()]*)\)\s*\{`)
	// A catch binding, which may itself be a destructuring pattern.
	catchParamRe = regexp.MustCompile(`\bcatch\s*\(([^()]*)\)`)
	// An assignment target (`foo = ...`), so an implicit-global assignment that
	// genuinely defines a name at runtime is never flagged. Excludes `==`/`===`
	// (group 2 rejects a following `=`), `=>` (rejects a following `>`), and the
	// comparison operators (an operator, not a bare ident, sits before their `=`).
	assignLHSRe = regexp.MustCompile(`(?:^|[^.\w$])([A-Za-z_$][\w$]*)[ \t]*=([^=>]|$)`)
)

// UndefinedCallRefs returns the identifiers used in call position in body that the
// module neither declares, imports, nor shares with the JS/DOM globals allowlist -
// each a `<Name> is not defined` runtime throw. Pure and deterministic: same body
// in, same result out (sorted by name). path is accepted for caller symmetry with
// the other store analyzers; the analysis is body-only.
func UndefinedCallRefs(path, body string) []UndefinedRef {
	masked := maskJS(body)
	declared := collectDeclared(masked)
	first := map[string]int{}
	var order []string
	n := len(masked)
	for i := 0; i < n; {
		c := masked[i]
		if !isIdentStartByte(c) {
			i++
			continue
		}
		j := i + 1
		for j < n && isIdentByte(masked[j]) {
			j++
		}
		name := masked[i:j]
		// A reference is a call only when the next significant byte is `(`.
		k := j
		for k < n && isSpaceByte(masked[k]) {
			k++
		}
		if k >= n || masked[k] != '(' {
			i = j
			continue
		}
		// Property access (`a.foo(...)`, `a?.foo(...)`) is not a free reference.
		if lastNonSpaceBefore(masked, i) == '.' {
			i = j
			continue
		}
		// A definition site - `name(params) {` for a function/method/getter - is a
		// binding, not a call. Skip it so the definition itself is never flagged.
		if closeIdx := skipBalancedJS(masked, k); closeIdx < n {
			m := closeIdx
			for m < n && isSpaceByte(masked[m]) {
				m++
			}
			if m < n && masked[m] == '{' {
				i = j
				continue
			}
		}
		if jsKeywords[name] || jsGlobals[name] || declared[name] {
			i = j
			continue
		}
		if _, seen := first[name]; !seen {
			first[name] = 1 + strings.Count(body[:i], "\n")
			order = append(order, name)
		}
		i = j
	}
	sort.Strings(order)
	out := make([]UndefinedRef, 0, len(order))
	for _, nm := range order {
		out = append(out, UndefinedRef{Name: nm, Line: first[nm]})
	}
	return out
}

// UndefinedRefs returns the identifiers in body that no enclosing scope binds -
// each a `<Name> is not defined` ReferenceError the instant that code path runs.
// Pure and deterministic: same body in, same result out (sorted by name). path is
// accepted for caller symmetry with the other store analyzers; the analysis is
// body-only.
//
// Only three positions are reported - a call (`foo(...)`), a member base
// (`foo.bar`), and an assignment target (`foo = ...`). Each is unambiguous
// without a parser. A bare read is not: it cannot be told apart from an object
// key, a shorthand property, or a label, so reporting one costs far more false
// positives than the findings are worth.
//
// Every project JS body is analyzed: the renderer serves ALL project JS as
// `<script type="module">`, and a module always runs strict - `foo = 1` with no
// declaration throws rather than defining a global - so the analysis agrees
// with how every body runs.
func UndefinedRefs(path, body string) []UndefinedRef {
	if body == "" {
		return nil
	}
	masked := maskJS(body)
	scopes := analyzeScopes(masked)
	lineStarts := computeLineStarts(body)
	found := map[string]UndefinedRef{}
	var order []string
	n := len(masked)
	for i := 0; i < n; {
		if !isIdentStartByte(masked[i]) || identContinues(masked, i) {
			i++
			continue
		}
		j := i + 1
		for j < n && isIdentByte(masked[j]) {
			j++
		}
		name := masked[i:j]
		kind, ok := refKindAt(masked, j)
		if !ok || jsKeywords[name] || jsGlobals[name] || scopes.resolves(name, i) {
			i = j
			continue
		}
		ref := UndefinedRef{Name: name, Line: lineOf(lineStarts, i), Kind: kind}
		if prev, seen := found[name]; !seen {
			found[name] = ref
			order = append(order, name)
		} else if prev.Kind != RefAssign && kind == RefAssign {
			// One finding per name, and an assignment outranks a read: it is the
			// reference an import cannot satisfy. Line follows Kind.
			found[name] = ref
		}
		i = j
	}
	sort.Strings(order)
	out := make([]UndefinedRef, 0, len(order))
	for _, nm := range order {
		out = append(out, found[nm])
	}
	return out
}

// refKindAt classifies the reference whose identifier ends at j. ok is false when
// the position is not one of the three reported shapes, or when the identifier is
// a definition site (`name(params) {`) rather than a call.
func refKindAt(masked string, j int) (RefKind, bool) {
	k := skipSpaceAnd(masked, j, "")
	if k >= len(masked) {
		return "", false
	}
	switch c := masked[k]; {
	case c == '(':
		if closeIdx := skipBalancedJS(masked, k); closeIdx < len(masked) {
			if m := skipSpaceAnd(masked, closeIdx, ""); m < len(masked) && masked[m] == '{' {
				return "", false // a definition, not a call
			}
		}
		return RefCall, true
	case c == '.':
		return RefMember, true
	case c == '?' && k+1 < len(masked) && masked[k+1] == '.':
		return RefMember, true
	case c == '=' && k+1 < len(masked) && masked[k+1] != '=' && masked[k+1] != '>':
		return RefAssign, true
	}
	return "", false
}

// collectDeclared builds the over-inclusive set of names in scope in a module:
// every imported local, function/class declaration, var/let/const binding
// (destructuring included), function/arrow/method/catch parameter, and assignment
// target. Over-collection is deliberate - it can only suppress a finding, never
// invent one. masked must be maskJS output (comment and string interiors blanked)
// so a name inside a string or comment never registers.
func collectDeclared(masked string) map[string]bool {
	d := map[string]bool{}
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			d[n] = true
		}
	}
	for _, ni := range ExtractNamedImports(masked) {
		add(ni.Local)
	}
	for _, m := range defaultImportRe.FindAllStringSubmatch(masked, -1) {
		add(m[1])
	}
	for _, m := range namespaceImportRe.FindAllStringSubmatch(masked, -1) {
		add(m[1])
	}
	for _, m := range funcDeclRe.FindAllStringSubmatch(masked, -1) {
		add(m[1])
	}
	for _, m := range classDeclRe.FindAllStringSubmatch(masked, -1) {
		add(m[1])
	}
	for _, loc := range varDeclKwRe.FindAllStringIndex(masked, -1) {
		appendDeclaratorNames(add, masked[loc[1]:])
	}
	collectParams(masked, add)
	for _, m := range assignLHSRe.FindAllStringSubmatch(masked, -1) {
		if !jsKeywords[m[1]] {
			add(m[1])
		}
	}
	return d
}

// collectParams adds every parameter name bound by a function, arrow, method, or
// catch clause. A parameter list is treated as an array-destructuring pattern
// (each entry a binding, defaults dropped, rest and nested patterns recursed) by
// wrapping it in [] and reusing appendPatternNames.
func collectParams(masked string, add func(string)) {
	addList := func(list string) { appendPatternNames(add, "["+list+"]") }
	for _, m := range funcParamsRe.FindAllStringSubmatch(masked, -1) {
		addList(m[1])
	}
	for _, m := range arrowParenRe.FindAllStringSubmatch(masked, -1) {
		addList(m[1])
	}
	for _, m := range arrowIdentRe.FindAllStringSubmatch(masked, -1) {
		if !jsKeywords[m[2]] {
			add(m[2])
		}
	}
	for _, m := range methodParamsRe.FindAllStringSubmatch(masked, -1) {
		if jsKeywords[m[1]] {
			continue // `if (...) {`, `for (...) {`, `switch (...) {`, etc.
		}
		addList(m[2])
	}
	for _, m := range catchParamRe.FindAllStringSubmatch(masked, -1) {
		addList(m[1])
	}
}

// lastNonSpaceBefore returns the last non-space byte before index i, or 0 when i
// is at or before the first non-space byte. Used to spot property access (a `.`
// immediately preceding an identifier), so a member call is never a free ref.
func lastNonSpaceBefore(s string, i int) byte {
	for k := i - 1; k >= 0; k-- {
		if !isSpaceByte(s[k]) {
			return s[k]
		}
	}
	return 0
}

// DeclaredTopLevelNames maps each name declared by a named function, class, or
// var/let/const in any module to the sorted, deduped module paths that declare it,
// whether or not it is exported. It is the "declared but not exported" companion
// to ExportIndex: when a forgotten call names a symbol no module exports, this
// points at the module that defines it privately, so the fix message can say
// "export it there and import it, or define it locally". Bodies are masked so a
// declaration inside a string or comment never registers.
func DeclaredTopLevelNames(bodies map[string]string) map[string][]string {
	tmp := map[string]map[string]bool{}
	for p, b := range bodies {
		masked := maskJS(b)
		names := map[string]bool{}
		collect := func(n string) {
			if n = strings.TrimSpace(n); n != "" {
				names[n] = true
			}
		}
		for _, m := range funcDeclRe.FindAllStringSubmatch(masked, -1) {
			collect(m[1])
		}
		for _, m := range classDeclRe.FindAllStringSubmatch(masked, -1) {
			collect(m[1])
		}
		for _, loc := range varDeclKwRe.FindAllStringIndex(masked, -1) {
			appendDeclaratorNames(collect, masked[loc[1]:])
		}
		for name := range names {
			if tmp[name] == nil {
				tmp[name] = map[string]bool{}
			}
			tmp[name][p] = true
		}
	}
	out := make(map[string][]string, len(tmp))
	for name, set := range tmp {
		ps := make([]string, 0, len(set))
		for p := range set {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		out[name] = ps
	}
	return out
}
