package probe

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Low-level browser input and DOM primitives: typing, keys, pointer, drag,
// touch, screenshots, and the JS/DOM pollers the step interpreter drives.

// dropFile drops a fixture file onto the marked drop zone the way a user dragging
// it from the desktop does: dragenter, dragover and drop, each carrying a
// DataTransfer that holds a real File. Headless Chrome cannot drag an OS file, so
// the events are dispatched in the page; handlers see e.dataTransfer.files as usual.
func dropFile(ctx context.Context, timeout time.Duration, path, name string) *flowFailure {
	data, err := os.ReadFile(path)
	if err != nil {
		return &flowFailure{"navigation", "could not read the upload fixture: " + err.Error()}
	}
	args, _ := json.Marshal([]string{base64.StdEncoding.EncodeToString(data), name, flowUploadTypes[name]})
	expr := `(function (b64, name, type) {
  var zone = document.querySelector('[data-of-flow-target]');
  if (!zone) return false;
  var bin = atob(b64), bytes = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  var dt = new DataTransfer();
  dt.items.add(new File([bytes], name, { type: type }));
  ['dragenter', 'dragover', 'drop'].forEach(function (t) {
    zone.dispatchEvent(new DragEvent(t, { bubbles: true, cancelable: true, dataTransfer: dt }));
  });
  return true;
}).apply(null, ` + string(args) + `)`
	var ok bool
	if err := runWithin(ctx, timeout, chromedp.Evaluate(expr, &ok)); err != nil || !ok {
		return &flowFailure{"not_found", fmt.Sprintf("could not drop %s onto the drop zone: %v", name, err)}
	}
	return nil
}

func settle(ctx context.Context) *flowFailure {
	_ = chromedp.Run(ctx, chromedp.Sleep(flowSettle))
	return nil
}

func runWithin(ctx context.Context, timeout time.Duration, act chromedp.Action) error {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return chromedp.Run(c, act)
}

// insertText puts text into the focused element the way an input method commits
// it, replacing the current selection; "" deletes the selection instead.
func insertText(ctx context.Context, text string) error {
	if text == "" {
		return typeKeys(ctx, true, "")
	}
	return chromedp.Run(ctx, input.InsertText(text))
}

// typeKeys types text into the focused element one key at a time (keydown,
// keypress, input, keyup per character), after deleting the selected content when
// clear is set. Per-key handlers (one-time-code boxes that jump to the next box,
// search-as-you-type, masks) see exactly what a person's typing produces, and a
// maxlength stops the extra characters as it does for them.
func typeKeys(ctx context.Context, clear bool, text string) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var runes []rune
		if clear {
			runes = append(runes, []rune(kb.Backspace)[0])
		}
		runes = append(runes, []rune(text)...)
		for _, r := range runes {
			for _, ev := range kb.Encode(r) {
				if err := ev.Do(ctx); err != nil {
					return err
				}
			}
			select {
			case <-time.After(8 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}))
}

// modifierEvent builds one key event for a modifier key.
func modifierEvent(typ input.KeyType, m flowModifierKey, held int64) *input.DispatchKeyEventParams {
	return &input.DispatchKeyEventParams{Type: typ, Key: m.name, Code: m.code,
		WindowsVirtualKeyCode: m.vk, NativeVirtualKeyCode: m.vk, Modifiers: input.Modifier(held)}
}

// pressKey sends a key to the focused element (or the page). A chord's modifiers
// ("Shift+Enter") go down as real key events first and stay set on the key's own
// events, so handlers see both e.shiftKey and a keydown with e.code "ShiftLeft" (games
// poll held keys by code). A bare modifier ("Shift") is a key of its own. With hold > 0
// everything stays down that long before it is released, for games that poll held keys.
func pressKey(ctx context.Context, arg string, hold time.Duration) error {
	mods, key, err := parsePressChord(arg)
	if err != nil {
		return err
	}
	var downs, ups []*input.DispatchKeyEventParams
	held := int64(0)
	press := func(m flowModifierKey) {
		held |= m.bit
		downs = append(downs, modifierEvent(input.KeyDown, m, held))
		ups = append([]*input.DispatchKeyEventParams{modifierEvent(input.KeyUp, m, held&^m.bit)}, ups...)
	}
	bare := false
	for _, m := range flowModifierKeys {
		if m.name == key {
			bare = true
		} else if mods&m.bit != 0 {
			press(m)
		}
	}
	if bare {
		for _, m := range flowModifierKeys {
			if m.name == key {
				press(m)
			}
		}
	} else {
		if k, ok := flowNamedKeys[key]; ok {
			key = k
		}
		var keyUps []*input.DispatchKeyEventParams
		for _, ev := range kb.Encode([]rune(key)[0]) {
			ev.Modifiers |= input.Modifier(held)
			if ev.Type == input.KeyUp {
				keyUps = append(keyUps, ev)
				continue
			}
			downs = append(downs, ev)
		}
		ups = append(keyUps, ups...)
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, ev := range downs {
			if err := ev.Do(ctx); err != nil {
				return err
			}
		}
		if hold > 0 {
			select {
			case <-time.After(hold):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, ev := range ups {
			if err := ev.Do(ctx); err != nil {
				return err
			}
		}
		return nil
	}))
}

// touch reports whether the flow runs on a touch device (a phone or tablet).
func (r *flowRun) touch() bool {
	return strings.HasPrefix(r.flow.Viewport, "mobile") || strings.HasPrefix(r.flow.Viewport, "tablet")
}

// touchTap taps a point n times as a finger does.
func touchTap(x, y float64, n int) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for i := 0; i < n; i++ {
			pt := []*input.TouchPoint{{X: x, Y: y}}
			if err := input.DispatchTouchEvent(input.TouchStart, pt).Do(ctx); err != nil {
				return err
			}
			if err := input.DispatchTouchEvent(input.TouchEnd, []*input.TouchPoint{}).Do(ctx); err != nil {
				return err
			}
			time.Sleep(60 * time.Millisecond)
		}
		return nil
	})
}

// touchDrag puts a finger down at the first point, slides it through the rest in
// small steps and lifts it at the last: a swipe, a joystick push, a drawn stroke.
func touchDrag(ctx context.Context, pts [][2]float64) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		at := func(p [2]float64) []*input.TouchPoint { return []*input.TouchPoint{{X: p[0], Y: p[1]}} }
		if err := input.DispatchTouchEvent(input.TouchStart, at(pts[0])).Do(ctx); err != nil {
			return err
		}
		for i := 1; i < len(pts); i++ {
			a, b := pts[i-1], pts[i]
			for k := 1; k <= 8; k++ {
				t := float64(k) / 8
				if err := input.DispatchTouchEvent(input.TouchMove, at([2]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t})).Do(ctx); err != nil {
					return err
				}
				time.Sleep(12 * time.Millisecond)
			}
		}
		return input.DispatchTouchEvent(input.TouchEnd, []*input.TouchPoint{}).Do(ctx)
	}))
}

// mouseDrag presses at the first point, moves through the rest in small steps (so
// pointermove handlers see a real stroke, not a teleport) and releases at the last.
func mouseDrag(ctx context.Context, pts [][2]float64) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		p := pts[0]
		if err := input.DispatchMouseEvent(input.MouseMoved, p[0], p[1]).Do(ctx); err != nil {
			return err
		}
		if err := input.DispatchMouseEvent(input.MousePressed, p[0], p[1]).
			WithButton(input.Left).WithButtons(1).WithClickCount(1).Do(ctx); err != nil {
			return err
		}
		for i := 1; i < len(pts); i++ {
			a, b := pts[i-1], pts[i]
			for k := 1; k <= 8; k++ {
				t := float64(k) / 8
				if err := input.DispatchMouseEvent(input.MouseMoved, a[0]+(b[0]-a[0])*t, a[1]+(b[1]-a[1])*t).
					WithButton(input.Left).WithButtons(1).Do(ctx); err != nil {
					return err
				}
				time.Sleep(12 * time.Millisecond)
			}
		}
		last := pts[len(pts)-1]
		return input.DispatchMouseEvent(input.MouseReleased, last[0], last[1]).
			WithButton(input.Left).WithClickCount(1).Do(ctx)
	}))
}

// screenshotHash resolves sel and returns a hash of its rendered pixels. Screenshots
// see what the compositor painted, so this works for canvas, WebGL, SVG, video and
// plain DOM alike.
func screenshotHash(ctx context.Context, timeout time.Duration, sel string) (string, *flowFailure) {
	if _, f := pollDOM(ctx, timeout, "mark", sel, "", ""); f != nil {
		return "", f
	}
	var buf []byte
	if err := runWithin(ctx, timeout, chromedp.Screenshot(flowMarker, &buf, chromedp.ByQuery)); err != nil {
		return "", &flowFailure{"not_found", fmt.Sprintf("could not capture %q: %v", sel, err)}
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// expectAnimating passes once requestAnimationFrame is being called continuously
// (the injected instrumentation counts calls): the render loop of a game, 3D scene
// or animation is alive. Retries until timeout, since a loop may start late.
func expectAnimating(ctx context.Context, timeout time.Duration) *flowFailure {
	deadline := time.Now().Add(timeout)
	for {
		var a, b float64
		_ = chromedp.Run(ctx, chromedp.Evaluate(`window.__ofRAF || 0`, &a))
		_ = chromedp.Run(ctx, chromedp.Sleep(flowFrameWindow))
		_ = chromedp.Run(ctx, chromedp.Evaluate(`window.__ofRAF || 0`, &b))
		if b-a >= flowMinFrames {
			return nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return &flowFailure{"assert", fmt.Sprintf("the page is not animating: %.0f requestAnimationFrame calls in %s, so its render/game loop is not running (waited %s)", max(b-a, 0), flowFrameWindow, timeout)}
		}
	}
}

// pollJS retries a JS expression until it is truthy. A failure reports the value the
// expression last produced, so a red expectJs says what the app state actually was
// (0 players, an empty string, null) instead of only that it was falsy.
//
// A promise is awaited and its resolved value tested, so an async check
// (navigator.clipboard.readText().then(...), a fetch) means what it says instead of
// passing because a pending promise is truthy.
func pollJS(ctx context.Context, timeout time.Duration, expr string) *flowFailure {
	wrapped := `(async () => { try { const v = await (` + expr + `); let s; try { s = v === undefined ? 'undefined' : JSON.stringify(v); } catch (e) { s = String(v); } ` +
		`return { ok: !!v, detail: '', value: String(s).slice(0, 200) }; } catch (e) { return { ok: false, detail: 'threw ' + (e && e.message || e) }; } })()`
	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	deadline := time.Now().Add(timeout)
	last := "stayed falsy"
	for {
		var r flowDOMResult
		attempt, cancel := context.WithTimeout(ctx, max(time.Until(deadline), 250*time.Millisecond))
		err := chromedp.Run(attempt, chromedp.Evaluate(wrapped, &r, awaitPromise))
		timedOut := attempt.Err() != nil && ctx.Err() == nil
		cancel()
		if timedOut {
			last = "returned a promise that never settled"
		}
		if err == nil {
			if r.OK {
				return nil
			}
			last = "stayed falsy"
			if r.Value != "" {
				last = "stayed falsy (last value: " + r.Value + ")"
			}
			if r.Detail != "" {
				last = r.Detail
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return &flowFailure{"assert", fmt.Sprintf("expectJs %q %s (waited %s)", expr, last, timeout)}
		}
		_ = chromedp.Run(ctx, chromedp.Sleep(flowPoll))
	}
}

func (r *flowRect) center() (float64, float64) {
	if r == nil {
		return 0, 0
	}
	return r.X + r.W/2, r.Y + r.H/2
}

// point returns the viewport coordinates of a fractional position inside the
// resolved element (its center when at is nil).
func (m *flowDOMResult) point(at *[2]float64) (float64, float64) {
	if m.Rect == nil {
		return 0, 0
	}
	if at == nil {
		return m.Rect.center()
	}
	return m.Rect.X + m.Rect.W*at[0], m.Rect.Y + m.Rect.H*at[1]
}

// pollDOM retries one in-page operation until it succeeds or timeout elapses, then
// reports the last attempt's reason. Evaluation errors (the page is mid-navigation
// and its execution context was just destroyed) count as "not yet".
func pollDOM(ctx context.Context, timeout time.Duration, op, arg, value, scope string) (*flowDOMResult, *flowFailure) {
	opJSON, _ := json.Marshal([]string{op, arg, value, scope})
	expr := "(" + flowDOMJS + ").apply(null, " + string(opJSON) + ")"
	deadline := time.Now().Add(timeout)
	last := flowDOMResult{Kind: "not_found", Detail: "the page never became ready"}
	for {
		var r flowDOMResult
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &r)); err == nil {
			if r.OK {
				return &r, nil
			}
			last = r
		}
		if ctx.Err() != nil {
			return nil, &flowFailure{"navigation", "flow ran out of time (" + flowTimeout.String() + " per flow)"}
		}
		if time.Now().After(deadline) {
			return nil, &flowFailure{last.Kind, fmt.Sprintf("%s (waited %s)", last.Detail, timeout)}
		}
		_ = chromedp.Run(ctx, chromedp.Sleep(flowPoll))
	}
}
