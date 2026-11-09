package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Flow step interpreter: one verb per case, plus goto/selector resolution.

// step performs one step in its session and returns why it failed, or nil.
func (r *flowRun) step(s *flowSession, st FlowStep) *flowFailure {
	ctx, cancel := context.WithDeadline(s.ctx, r.deadline)
	defer cancel()
	verb, arg := st.verb()
	arg = r.vars.expand(arg, s.name)
	value := r.vars.expand(derefStr(st.Value), s.name)
	scope := r.vars.expand(st.In, s.name)
	timeout := st.timeout()

	switch verb {
	case "goto":
		target, ok := sitePath(arg)
		if !ok {
			return &flowFailure{"assert", fmt.Sprintf("goto %s resolved to %q, which is not a URL or site path", st.Goto, arg)}
		}
		navCtx, cancelNav := context.WithTimeout(ctx, maxStepTimeout)
		defer cancelNav()
		if err := chromedp.Run(navCtx, chromedp.Navigate(r.origin+target)); err != nil {
			return &flowFailure{"navigation", fmt.Sprintf("could not load %s: %v", arg, err)}
		}
		return settle(ctx)

	case "leave":
		// Close the tab; a later goto as the same name opens a new browser.
		s.cancel()
		delete(r.sessions, s.name)
		r.order = slices.DeleteFunc(r.order, func(n string) bool { return n == s.name })
		return nil

	case "reload":
		navCtx, cancelNav := context.WithTimeout(ctx, maxStepTimeout)
		defer cancelNav()
		if err := chromedp.Run(navCtx, chromedp.Reload()); err != nil {
			return &flowFailure{"navigation", fmt.Sprintf("could not reload the page: %v", err)}
		}
		return settle(ctx)

	case "click", "dblclick", "hover":
		aim, _ := json.Marshal(map[string]any{"verb": verb, "at": st.At})
		m, f := pollDOM(ctx, timeout, "mark", arg, string(aim), scope)
		if f != nil {
			return f
		}
		x, y := m.point(st.At)
		var act chromedp.Action
		switch {
		case r.touch() && verb != "hover":
			// A phone or tablet is tapped, not clicked: touchstart/touchend, and the
			// click the browser derives from the tap. A double tap for dblclick.
			taps := 1
			if verb == "dblclick" {
				taps = 2
			}
			act = touchTap(x, y, taps)
		case verb == "hover":
			act = chromedp.MouseEvent(input.MouseMoved, x, y)
		case verb == "dblclick" && st.At == nil && !m.Shadow:
			act = chromedp.DoubleClick(flowMarker, chromedp.ByQuery)
		case verb == "dblclick":
			act = chromedp.MouseClickXY(x, y, chromedp.ClickCount(2))
		case st.At == nil && !m.Shadow:
			act = chromedp.Click(flowMarker, chromedp.ByQuery)
		default:
			act = chromedp.MouseClickXY(x, y)
		}
		if err := runWithin(ctx, timeout, act); err != nil {
			return &flowFailure{"not_found", fmt.Sprintf("found %q but could not %s it: %v", arg, verb, err)}
		}
		return settle(ctx)

	case "drag":
		src, f := pollDOM(ctx, timeout, "mark", arg, "", "")
		if f != nil {
			return f
		}
		var pts [][2]float64
		if st.Path != nil {
			for _, p := range st.Path {
				x, y := src.point(&p)
				pts = append(pts, [2]float64{x, y})
			}
		} else {
			to := r.vars.expand(st.To, s.name)
			if _, f := pollDOM(ctx, timeout, "markdrop", to, "", ""); f != nil {
				return f
			}
			// A native HTML5 draggable ignores synthetic mouse moves in headless
			// Chrome; replay the drag-and-drop events it listens for instead.
			if src.Draggable {
				if _, f := pollDOM(ctx, timeout, "dnd", "", "", ""); f != nil {
					return f
				}
				return settle(ctx)
			}
			both, f := pollDOM(ctx, timeout, "rects", "", "", "")
			if f != nil {
				return f
			}
			sx, sy := both.point(nil)
			tx, ty := both.Drop.center()
			pts = [][2]float64{{sx, sy}, {tx, ty}}
		}
		drag := mouseDrag
		if r.touch() {
			drag = touchDrag
		}
		if err := drag(ctx, pts); err != nil {
			return &flowFailure{"not_found", fmt.Sprintf("could not drag %q: %v", arg, err)}
		}
		return settle(ctx)

	case "scroll":
		if _, f := pollDOM(ctx, timeout, verb, arg, value, ""); f != nil {
			return f
		}
		return settle(ctx)

	case "select", "check", "uncheck":
		if _, f := pollDOM(ctx, timeout, verb, arg, value, scope); f != nil {
			return f
		}
		return settle(ctx)

	case "fill":
		m, f := pollDOM(ctx, timeout, "fill", arg, value, scope)
		if f != nil {
			return f
		}
		if m.Note != "" {
			r.note(m.Note)
		}
		// A rich-text editor is left focused with its content selected: the text goes
		// in through the browser's own editing (beforeinput/input), which editors read.
		if m.Field == "editable" {
			if err := insertText(ctx, value); err != nil {
				return &flowFailure{"not_found", fmt.Sprintf("could not type into %q: %v", arg, err)}
			}
		}
		return settle(ctx)

	case "type":
		m, f := pollDOM(ctx, timeout, "markfield", arg, value, scope)
		if f != nil {
			return f
		}
		if m.Note != "" {
			r.note(m.Note)
		}
		if err := typeKeys(ctx, m.Value != "", value); err != nil {
			return &flowFailure{"not_found", fmt.Sprintf("could not type into %q: %v", arg, err)}
		}
		return settle(ctx)

	case "press":
		if err := pressKey(ctx, arg, time.Duration(st.Hold)*time.Millisecond); err != nil {
			return &flowFailure{"not_found", fmt.Sprintf("could not press %s: %v", arg, err)}
		}
		return settle(ctx)

	case "upload":
		target, f := pollDOM(ctx, timeout, "markupload", arg, "", "")
		if f != nil {
			return f
		}
		path, err := r.fixtures.path(value)
		if err != nil {
			return &flowFailure{"navigation", "could not create the upload fixture: " + err.Error()}
		}
		if target.Value == "drop" {
			if f := dropFile(ctx, timeout, path, value); f != nil {
				return f
			}
			return settle(ctx)
		}
		if err := runWithin(ctx, timeout, chromedp.SetUploadFiles(flowMarker, []string{path}, chromedp.ByQuery, chromedp.NodeReady)); err != nil {
			return &flowFailure{"not_found", fmt.Sprintf("could not upload to %q: %v", arg, err)}
		}
		return settle(ctx)

	case "download":
		before := r.downloads.count()
		aim, _ := json.Marshal(map[string]any{"verb": "click", "at": nil})
		m, f := pollDOM(ctx, timeout, "mark", arg, string(aim), scope)
		if f != nil {
			return f
		}
		x, y := m.point(nil)
		click := chromedp.MouseClickXY(x, y)
		if r.touch() {
			click = touchTap(x, y, 1)
		}
		if err := runWithin(ctx, timeout, click); err != nil {
			return &flowFailure{"not_found", fmt.Sprintf("found %q but could not click it: %v", arg, err)}
		}
		deadline := time.Now().Add(timeout)
		for {
			name, state, size := r.downloads.after(before)
			switch state {
			case "completed":
				if size <= 0 {
					return &flowFailure{"assert", fmt.Sprintf("%q downloaded %q, but the file is empty", arg, name)}
				}
				if value != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(value)) {
					return &flowFailure{"assert", fmt.Sprintf("%q downloaded %q, not a file whose name contains %q", arg, name, value)}
				}
				return settle(ctx)
			case "canceled":
				return &flowFailure{"assert", fmt.Sprintf("the download %q started failed or was cancelled (%s)", arg, name)}
			}
			if ctx.Err() != nil || time.Now().After(deadline) {
				if state == "" {
					return &flowFailure{"assert", fmt.Sprintf("clicking %q started no download (waited %s): a download is a link with a download attribute, or a response sent as an attachment (Content-Disposition: attachment); a page that shows the file instead is not one", arg, timeout)}
				}
				return &flowFailure{"assert", fmt.Sprintf("the download %q started (%s) did not finish in %s", arg, name, timeout)}
			}
			_ = chromedp.Run(ctx, chromedp.Sleep(flowPoll))
		}

	case "offline":
		off := *st.Offline
		if err := chromedp.Run(ctx, offlineAction(off)); err != nil {
			return &flowFailure{"navigation", fmt.Sprintf("could not switch the network %s: %v", map[bool]string{true: "off", false: "on"}[off], err)}
		}
		r.setWorkersOffline(s, off)
		s.offline = off
		return settle(ctx)

	case "dialog":
		s.dialog.arm(arg == "accept", value)
		return nil

	case "clock":
		rel, abs, _ := parseFlowClock(arg)
		if abs.IsZero() {
			s.clockOffset += rel
		} else {
			s.clockOffset = time.Until(abs)
		}
		if err := r.applyClock(ctx, s); err != nil {
			return &flowFailure{"navigation", fmt.Sprintf("could not set the page clock: %v", err)}
		}
		return nil

	case "request":
		method, path, _ := strings.Cut(arg, " ")
		headers := map[string]string{}
		for k, v := range st.Headers {
			headers[k] = r.vars.expand(v, s.name)
		}
		resp, f := r.fetch(ctx, s, timeout, method, strings.TrimSpace(path), r.expandAny(st.Body, s.name), headers)
		if f != nil {
			return f
		}
		s.lastResp = resp
		return checkStatus(resp, st.Status)

	case "webhook":
		if r.hooks == nil {
			return &flowFailure{"navigation", "this replay cannot sign webhooks (no project access here)"}
		}
		body := webhookBody(r.expandAny(st.Body, s.name))
		sig, err := r.hooks.WebhookSignature(arg, body)
		if err != nil {
			return &flowFailure{"assert", fmt.Sprintf("webhook %q: %v", arg, err)}
		}
		resp, f := r.fetch(ctx, s, timeout, "POST", "/webhooks/"+arg, string(body),
			map[string]string{"Content-Type": "application/json", "X-Webhook-Signature": sig})
		if f != nil {
			return f
		}
		s.lastResp = resp
		if f := checkStatus(resp, st.Status); f != nil {
			return f
		}
		return settle(ctx)

	case "expectResponse":
		want := r.expandAny(st.ExpectResponse, s.name)
		if s.lastResp == nil {
			return &flowFailure{"assert", "there is no response to check yet"}
		}
		if !matchResponse(want, s.lastResp.Body) {
			return &flowFailure{"assert", fmt.Sprintf("the last response (%s) does not contain %s: %s", s.lastResp.label(), scalarString(want), clipBody(s.lastResp.Body))}
		}
		return nil

	case "expectEmail":
		to := r.vars.expand(st.To, s.name)
		if to == "" {
			to = r.vars.get(s.name).Email
		}
		return r.expectMessage(ctx, s, "email", arg, to, timeout)

	case "runSchedule":
		if r.hooks == nil {
			return &flowFailure{"navigation", "this replay cannot run the project's scheduled tasks (no project access here)"}
		}
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := r.hooks.RunSchedule(c, arg); err != nil {
			return &flowFailure{"assert", fmt.Sprintf("scheduled task %q: %v", arg, err)}
		}
		return settle(ctx)

	case "remember":
		if v, special, f := r.rememberSpecial(s, st.From); special {
			if f != nil {
				return f
			}
			r.vars.remembered[arg] = v
			return nil
		}
		from := r.vars.expand(st.From, s.name)
		m, f := pollDOM(ctx, timeout, "read", from, st.Attr, "")
		if f != nil {
			return f
		}
		r.vars.remembered[arg] = m.Value
		return nil

	case "wait":
		_ = chromedp.Run(ctx, chromedp.Sleep(time.Duration(st.Wait)*time.Millisecond))
		return nil
	case "waitFor", "expectVisible":
		_, f := pollDOM(ctx, timeout, "visible", arg, "", scope)
		return f
	case "expectHidden":
		_, f := pollDOM(ctx, timeout, "hidden", arg, "", scope)
		return f
	case "expectText", "expectNoText":
		op := "text"
		if verb == "expectNoText" {
			op = "notext"
		}
		_, f := pollDOM(ctx, timeout, op, arg, "", scope)
		return f
	case "expectUrl":
		_, f := pollDOM(ctx, timeout, "url", arg, "", "")
		return f
	case "expectCount":
		_, f := pollDOM(ctx, timeout, "count", arg, fmt.Sprint(*st.Count), scope)
		return f
	case "expectJs":
		return pollJS(ctx, timeout, arg)
	case "expectAnimating":
		return expectAnimating(ctx, timeout)

	case "snapshot":
		h, f := screenshotHash(ctx, timeout, arg)
		if f != nil {
			return f
		}
		s.snaps[arg] = h
		// Look again after an idle moment: a canvas that changes with no input satisfies
		// expectChanged whatever the input did.
		_ = chromedp.Run(ctx, chromedp.Sleep(flowPixelPoll))
		if again, f := screenshotHash(ctx, timeout, arg); f == nil {
			if s.drifts == nil {
				s.drifts = map[string]bool{}
			}
			s.drifts[arg] = again != h
		}
		return nil
	case "expectChanged":
		before := s.snaps[arg]
		deadline := time.Now().Add(timeout)
		for {
			h, f := screenshotHash(ctx, timeout, arg)
			if f != nil {
				return f
			}
			if h != before {
				s.snaps[arg] = h
				if s.drifts[arg] {
					r.weak = append(r.weak, fmt.Sprintf("flow %q step %d: %q also changes by itself with no input (an animation or render loop), so expectChanged cannot show that the input had an effect. "+
						"Assert something the input causes instead: HUD text, a score or position readout, a visible state.", r.flow.Name, r.stepIdx+1, arg))
				}
				return nil
			}
			if ctx.Err() != nil || time.Now().After(deadline) {
				return &flowFailure{"assert", fmt.Sprintf("%q looks exactly as it did at the snapshot: the input had no visible effect (waited %s)", arg, timeout)}
			}
			_ = chromedp.Run(ctx, chromedp.Sleep(flowPixelPoll))
		}
	}
	return &flowFailure{"assert", "unknown step"}
}

// sitePath turns a goto target into a path on the project: a site path is used as
// is; a remembered full link ("https://myapp.example/f/x7Kq", or "myapp.example/f/x7Kq"
// as apps often print it) keeps only its path, query and fragment, because the flow
// browser reaches the project through the probe origin, not the public host.
func sitePath(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		return v, true
	}
	if !strings.Contains(v, "://") {
		v = "http://" + strings.TrimPrefix(v, "//")
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", false
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		p += "#" + u.EscapedFragment()
	}
	return p, true
}

// stepSelector returns the element selector a step targets ("" for steps without one).
func stepSelector(st FlowStep) string {
	verb, arg := st.verb()
	switch verb {
	case "click", "dblclick", "hover", "drag", "scroll", "fill", "type", "select", "check", "uncheck", "upload", "download",
		"waitFor", "expectVisible", "expectHidden", "expectCount", "snapshot", "expectChanged":
		if verb == "scroll" && (arg == "top" || arg == "bottom") {
			return ""
		}
		return arg
	case "remember":
		if flowSpecialFromRe.MatchString(st.From) {
			return ""
		}
		return st.From
	}
	return ""
}
