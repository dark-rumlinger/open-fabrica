(function () {
  if (window.__ofx) return 'ok';
  var s = { mut: 0, req: 0, fx: 0 };
  try {
    new MutationObserver(function (list) {
      for (var i = 0; i < list.length; i++) {
        var m = list[i];
        if (m.type === 'attributes' && m.attributeName && m.attributeName.indexOf('data-ofx') === 0) continue;
        s.mut++;
      }
    }).observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
  } catch (e) {}
  var wrap = function (obj, name, counter, ret) {
    try {
      var orig = obj[name];
      if (typeof orig !== 'function') return;
      obj[name] = function () {
        s[counter]++;
        if (ret !== undefined) return ret;
        return orig.apply(this, arguments);
      };
    } catch (e) {}
  };
  wrap(window, 'fetch', 'req');
  if (window.XMLHttpRequest) wrap(XMLHttpRequest.prototype, 'send', 'req');
  if (window.WebSocket) wrap(WebSocket.prototype, 'send', 'req');
  wrap(window, 'alert', 'fx', null);
  wrap(window, 'confirm', 'fx', true);
  wrap(window, 'prompt', 'fx', '');
  wrap(window, 'print', 'fx', null);
  wrap(window, 'open', 'fx', null);
  if (window.HTMLMediaElement) wrap(HTMLMediaElement.prototype, 'play', 'fx');
  if (window.AudioScheduledSourceNode) wrap(AudioScheduledSourceNode.prototype, 'start', 'fx');
  if (window.speechSynthesis) wrap(window.speechSynthesis, 'speak', 'fx');
  if (navigator.clipboard) wrap(navigator.clipboard, 'writeText', 'fx');
  if (navigator.vibrate) wrap(navigator, 'vibrate', 'fx');

  // A file input opens the browser's native chooser, which is no DOM, request or sound
  // but is plainly a working control. Count it and keep the chooser from opening.
  try {
    if (window.HTMLInputElement) wrap(HTMLInputElement.prototype, 'showPicker', 'fx', null);
    document.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      var f = t.closest('input[type="file"]');
      if (!f) {
        var lb = t.closest('label');
        if (lb && lb.control && lb.control.type === 'file') f = lb.control;
      }
      if (f) { s.fx++; e.preventDefault(); }
    }, true);
    if (window.HTMLInputElement) {
      var origClick = HTMLInputElement.prototype.click;
      HTMLInputElement.prototype.click = function () {
        if (this.type === 'file') { s.fx++; return; }
        return origClick.apply(this, arguments);
      };
    }
  } catch (e) {}

  function visible(el) {
    try {
      var cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden' || cs.visibility === 'collapse' || Number(cs.opacity) === 0) return false;
      if (unpainted(el)) return false;
      var b = el.getBoundingClientRect();
      return b.width > 0 && b.height > 0;
    } catch (e) { return false; }
  }
  // unpainted: content Chrome lays out when asked but never paints - a closed
  // <details> body, a content-visibility:hidden subtree. It reports a box, and a tap
  // at that box lands on whatever is drawn there, so it is not a control to click.
  function unpainted(el) {
    if (el.checkVisibility && !el.checkVisibility()) return true;
    var d = el.closest('details:not([open])');
    if (!d) return false;
    var s = el.closest('summary');
    return !(s && s.parentElement === d);
  }
  function label(el) {
    var t = (el.getAttribute('aria-label') || el.textContent || el.value || el.getAttribute('title') || '').trim().replace(/\s+/g, ' ');
    return t.slice(0, 60);
  }
  function path(el) {
    if (el.id) return el.tagName.toLowerCase() + '#' + el.id;
    var cls = (typeof el.className === 'string' ? el.className : '').trim().split(/\s+/).filter(Boolean)
      .filter(function (c) { return !/^(is-)?(active|selected|current|open|hidden|show|on)$/.test(c); }).slice(0, 2);
    return el.tagName.toLowerCase() + (cls.length ? '.' + cls.join('.') : '');
  }
  // A control already in its "on" state (the current tab, the active nav item)
  // legitimately does nothing when clicked again.
  function alreadyCurrent(el) {
    if (el.getAttribute('aria-selected') === 'true' || el.getAttribute('aria-current') && el.getAttribute('aria-current') !== 'false') return true;
    var c = ' ' + (typeof el.className === 'string' ? el.className : '') + ' ';
    return /\s(is-)?(active|selected|current)\s/.test(c);
  }
  function canvasSig() {
    var out = '';
    var cs = document.querySelectorAll('canvas');
    for (var i = 0, n = 0; i < cs.length && n < 3; i++) {
      var c = cs[i];
      if (!visible(c) || c.width * c.height > 4000000) continue;
      n++;
      try {
        var d = c.toDataURL('image/jpeg', 0.2), h = 0;
        for (var j = 0; j < d.length; j += 37) h = (h * 31 + d.charCodeAt(j)) | 0;
        out += d.length + ':' + h + ';';
      } catch (e) { out += 'x;'; }
    }
    return out;
  }
  // Values written by a handler (a slider moved by a replay button, a toggled checkbox)
  // change a property, not the DOM, so the mutation counter never sees them.
  function valSig() {
    var out = '', els = document.querySelectorAll('input, select, textarea');
    for (var i = 0; i < els.length && i < 80; i++) {
      var el = els[i];
      out += (el.type === 'checkbox' || el.type === 'radio' ? (el.checked ? '1' : '0') : String(el.value || '').slice(0, 40)) + '|';
    }
    return out;
  }
  function canvasCount() {
    var cs = document.querySelectorAll('canvas'), n = 0;
    for (var i = 0; i < cs.length; i++) if (visible(cs[i])) n++;
    return n;
  }
  // A full-viewport layer that sits on top of the page and carries its own button is a
  // first-run gate (an intro card, a cookie notice, a start screen). It blocks the page
  // until dismissed by design, so the explorer dismisses it the way a user would.
  function dismissGate() {
    var all = document.body ? document.body.getElementsByTagName('*') : [], vw = innerWidth, vh = innerHeight;
    for (var i = 0; i < all.length && i < 3000; i++) {
      var el = all[i];
      try {
        var cs = getComputedStyle(el);
        // Fixed only: an absolutely positioned full-viewport wrapper is usually the app
        // itself, and clicking its first button would be clicking arbitrary app controls.
        if (cs.position !== 'fixed' || cs.pointerEvents === 'none' || !visible(el)) continue;
        var r = el.getBoundingClientRect();
        if (r.width < vw * 0.9 || r.height < vh * 0.9) continue;
        // A gate is a small card on a layer: a few words and one to three buttons. A
        // fixed application shell has far more of both and is left alone.
        if ((el.innerText || '').length > 600) continue;
        var btns = el.querySelectorAll('button, [role="button"], input[type="button"], input[type="submit"]');
        var pick = null, shown = 0;
        for (var k = 0; k < btns.length; k++) if (visible(btns[k]) && !btns[k].disabled) { shown++; if (!pick) pick = btns[k]; }
        if (!pick || shown > 3) continue;
        var top = document.elementFromPoint(vw / 2, vh / 2);
        if (!top || !(el === top || el.contains(top))) continue;
        pick.click();
        return label(pick) || pick.tagName.toLowerCase();
      } catch (e) {}
    }
    return '';
  }
  var SEL = 'button, [role="button"], [role="tab"], [role="menuitem"], [role="switch"], [role="checkbox"], [role="option"], ' +
    'summary, [onclick], input[type="button"], input[type="submit"], input[type="checkbox"], input[type="radio"], a[href^="#"]';

  window.__ofx = {
    state: function () {
      return { mut: s.mut, req: s.req, fx: s.fx, href: location.href, sx: scrollX, sy: scrollY, canv: canvasSig(), vals: valSig(), ncv: canvasCount() };
    },
    dismissGate: dismissGate,
    path: function () { return location.pathname; },
    // enumerate tags every distinct, visible, enabled control with data-ofx=<i>.
    enumerate: function () {
      var old = document.querySelectorAll('[data-ofx]');
      for (var k = 0; k < old.length; k++) old[k].removeAttribute('data-ofx');
      var els = document.querySelectorAll(SEL), out = [], seen = {};
      for (var i = 0; i < els.length; i++) {
        var el = els[i];
        try {
          if (!visible(el) || el.disabled || el.getAttribute('aria-disabled') === 'true') continue;
          if (el.closest('[inert], [aria-hidden="true"]')) continue;
          if (el.tagName === 'A' && (el.getAttribute('target') === '_blank')) continue;
          if (alreadyCurrent(el)) continue;
          // A wrapper with its own control inside: click the inner one.
          var inner = el.querySelector(SEL);
          if (inner && inner !== el && visible(inner)) continue;
          // A submit of a form that would fail native validation does nothing until
          // the form is filled; explore never fills forms.
          var type = (el.getAttribute('type') || '').toLowerCase();
          if (el.form && (type === 'submit' || (el.tagName === 'BUTTON' && type === ''))) {
            if (!el.form.checkValidity()) continue;
            // A form with an empty text field submits nothing worth observing either.
            var fields = el.form.querySelectorAll('input:not([type]), input[type="text"], input[type="email"], input[type="search"], input[type="url"], input[type="tel"], input[type="password"], input[type="number"], textarea');
            var emptyField = false;
            for (var q = 0; q < fields.length; q++) if (visible(fields[q]) && !fields[q].value) { emptyField = true; break; }
            if (emptyField) continue;
          }
          var text = label(el), sel = path(el);
          var key = el.tagName + '|' + text + '|' + sel;
          if (seen[key]) continue;
          seen[key] = true;
          el.setAttribute('data-ofx', String(out.length));
          out.push({ i: out.length, tag: el.tagName.toLowerCase(), text: text, sel: sel, key: key });
        } catch (e) {}
      }
      return JSON.stringify(out);
    },
    // aim scrolls control i into view and returns where to click it, or what sits on top.
    aim: function (i) {
      var el = document.querySelector('[data-ofx="' + i + '"]');
      // A control listed while shown can be hidden by the time it is aimed at (a
      // fresh load closed the panel it sits in): a tap at its box would land on
      // whatever is drawn there now, which is not this control being covered.
      if (!el || !visible(el)) return JSON.stringify({ gone: true });
      el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
      var r = el.getBoundingClientRect();
      var x = r.left + r.width / 2, y = r.top + r.height / 2;
      if (x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) return JSON.stringify({ gone: true });
      var top = document.elementFromPoint(x, y);
      var within = function (a, b) { for (var n = a; n; n = n.parentNode || n.host) if (n === b) return true; return false; };
      var cover = '';
      if (top && !within(top, el) && !within(el, top) && !(top.tagName === 'LABEL' && top.control === el)) {
        var cs = getComputedStyle(top);
        cover = '<' + path(top) + '> (position:' + cs.position + ', z-index:' + cs.zIndex + ', pointer-events:' + cs.pointerEvents + ')';
      }
      return JSON.stringify({ x: x, y: y, cover: cover });
    },
    layout: function () {
      var vw = document.documentElement.clientWidth || innerWidth;
      var sw = Math.max(document.documentElement.scrollWidth, document.body ? document.body.scrollWidth : 0);
      // Only a page the user can actually scroll sideways is a defect: overflow
      // clipped by an overflow-x:hidden wrapper is invisible. A phone shows it one of
      // two ways: the page scrolls, or the browser widens the layout viewport to fit
      // the content and zooms the whole page out (body{overflow-x:hidden} does not
      // stop that on mobile).
      var sx = scrollX;
      window.scrollTo(100000, scrollY);
      var scrolls = scrollX > 1 || innerWidth > vw + 2;
      window.scrollTo(sx, scrollY);
      var off = [];
      if (scrolls && document.body) {
        var all = document.body.getElementsByTagName('*'), hits = [];
        for (var i = 0; i < all.length && i < 4000; i++) {
          var el = all[i], r = el.getBoundingClientRect();
          if (r.width > 0 && r.right > vw + 2 && visible(el)) hits.push(el);
        }
        // Report the innermost offenders: an element whose child also overflows
        // only overflows because of that child.
        for (var j = 0; j < hits.length && off.length < 3; j++) {
          var leaf = true;
          for (var k = 0; k < hits.length; k++) if (k !== j && hits[j].contains(hits[k])) { leaf = false; break; }
          if (leaf) off.push(path(hits[j]) + ' ends at ' + Math.round(hits[j].getBoundingClientRect().right) + 'px');
        }
      }
      var text = document.body ? (document.body.innerText || '').replace(/\s+/g, ' ').trim().length : 0;
      var media = 0, ms = document.querySelectorAll('canvas, img, svg, video, iframe');
      for (var m = 0; m < ms.length; m++) {
        var b = ms[m].getBoundingClientRect();
        if (b.width * b.height >= 10000 && visible(ms[m])) media++;
      }
      return JSON.stringify({ vw: vw, sw: sw, scrolls: scrolls, off: off, text: text, media: media });
    }
  };
  return 'ok';
})()