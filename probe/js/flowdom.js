function (op, sel, value, scope, info) {
  var outerSel = sel;
  var norm = function (s) { return String(s == null ? '' : s).replace(/\s+/g, ' ').trim(); };
  // fold compares text the way a person reads it. innerText carries CSS
  // text-transform (a "Sound: on" styled uppercase reads "SOUND: ON"), and pages set
  // typographic quotes, dashes and ellipses that a flow script writes as ASCII.
  var fold = function (s) {
    return norm(s).replace(/[‘’‚′]/g, "'").replace(/[“”„″]/g, '"')
      .replace(/[–—]/g, '-').replace(/…/g, '...').toLowerCase();
  };
  var visible = function (el) {
    if (!el || !el.isConnected) return false;
    var cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none') return false;
    return el.getClientRects().length > 0;
  };
  // Elements under root, descending into open shadow roots, so web components are
  // reachable like any other markup.
  var allEls = function (root) {
    var out = [];
    var walk = function (r) {
      var els = r.querySelectorAll('*');
      for (var i = 0; i < els.length; i++) {
        out.push(els[i]);
        if (els[i].shadowRoot) walk(els[i].shadowRoot);
      }
    };
    if (root.shadowRoot) walk(root.shadowRoot);
    walk(root);
    return out;
  };
  var deepQuery = function (root, css) {
    var out = Array.prototype.slice.call(root.querySelectorAll(css));
    var hosts = allEls(root);
    for (var i = 0; i < hosts.length; i++) {
      if (hosts[i].shadowRoot) out = out.concat(Array.prototype.slice.call(hosts[i].shadowRoot.querySelectorAll(css)));
    }
    if (root.shadowRoot) out = out.concat(Array.prototype.slice.call(root.shadowRoot.querySelectorAll(css)));
    return out.filter(function (el, i) { return out.indexOf(el) === i; });
  };
  // textOf is what an element shows, including the text inside open shadow roots
  // below it (innerText stops at a shadow host).
  var textOf = function (el) {
    var t = el.innerText || '';
    var hosts = allEls(el).filter(function (h) { return h.shadowRoot; });
    if (el.shadowRoot) hosts.unshift(el);
    hosts.forEach(function (h) {
      Array.prototype.forEach.call(h.shadowRoot.children, function (c) {
        if (c.tagName !== 'STYLE' && c.tagName !== 'SCRIPT') t += ' ' + (c.innerText || '');
      });
    });
    return t;
  };
  // Keep the innermost matches: a wrapper whose text is only its button's text
  // also matches, but the button is what a user clicks.
  var innermost = function (hits) {
    return hits.filter(function (el) {
      return !hits.some(function (o) { return o !== el && el.contains(o); });
    });
  };
  // splitTop splits a selector list on the commas between selectors, not those
  // inside (), [] or quotes.
  var splitTop = function (s) {
    var parts = [], depth = 0, q = '', start = 0;
    for (var i = 0; i < s.length; i++) {
      var c = s[i];
      if (q) { if (c === '\\') i++; else if (c === q) q = ''; continue; }
      if (c === '"' || c === "'") q = c;
      else if (c === '(' || c === '[') depth++;
      else if (c === ')' || c === ']') depth--;
      else if (c === ',' && depth === 0) { parts.push(s.slice(start, i)); start = i + 1; }
    }
    parts.push(s.slice(start));
    return parts;
  };
  var hasTextRe = /:has-text\(\s*(["'])((?:\\.|(?!\1)[^\\])*)\1\s*\)/g;
  // below finds css under each of els: descendants (or children, for a css that
  // starts with ">") of the element itself, and inside open shadow roots below it.
  var below = function (els, css) {
    css = css.trim();
    var out = [];
    els.forEach(function (el) {
      var found = el.nodeType === 1 ? el.querySelectorAll(':scope ' + css) : el.querySelectorAll(css.replace(/^>\s*/, ''));
      Array.prototype.forEach.call(found, function (x) { if (out.indexOf(x) < 0) out.push(x); });
      allEls(el).forEach(function (h) {
        if (!h.shadowRoot) return;
        Array.prototype.forEach.call(h.shadowRoot.querySelectorAll(css.replace(/^>\s*/, '')), function (x) { if (out.indexOf(x) < 0) out.push(x); });
      });
    });
    return out;
  };
  // queryCSS is querySelectorAll plus Playwright's :has-text("...") (the element's
  // text contains that text), anywhere in each selector of the list:
  // ".card:has-text('Lena') .btn" is the .btn inside Lena's card.
  var queryCSS = function (root, s) {
    if (s.indexOf(':has-text(') < 0) return deepQuery(root, s);
    var out = [];
    splitTop(s).forEach(function (part) {
      var segs = [], wants = [], last = 0, m;
      hasTextRe.lastIndex = 0;
      while ((m = hasTextRe.exec(part))) {
        segs.push(part.slice(last, m.index));
        wants.push(fold(m[2].replace(/\\(.)/g, '$1')));
        last = hasTextRe.lastIndex;
      }
      segs.push(part.slice(last));
      var hits = null;
      segs.forEach(function (seg, i) {
        var want = wants[i];
        if (i === segs.length - 1 && !seg.trim()) return; // ends with :has-text
        var bare = /^\s*$|[\s>+~]$/.test(seg);
        var css = bare ? seg + '*' : seg;
        var found = hits === null ? deepQuery(root, css) : below(hits, css);
        if (want !== undefined) {
          found = found.filter(function (el) { return fold(textOf(el) || el.textContent).indexOf(want) >= 0; });
          if (bare) found = innermost(found);
        }
        hits = found;
      });
      (hits || []).forEach(function (el) { if (out.indexOf(el) < 0) out.push(el); });
    });
    return out;
  };
  // findIn resolves a selector under root: CSS (with :has-text), or text=<label>,
  // which picks the innermost elements whose folded text (or value / aria-label for
  // inputs and icon buttons) equals the label.
  var findIn = function (root, q) {
    if (q.indexOf('text=') === 0) {
      var want = fold(q.slice(5));
      var hits = [];
      var base = root.nodeType === 9 ? document.body : root;
      var cands = base ? allEls(base) : [];
      if (root.nodeType === 1) cands.unshift(root);
      for (var i = 0; i < cands.length; i++) {
        var el = cands[i];
        var t = fold(el.innerText);
        if (!t && (el.tagName === 'INPUT' || el.tagName === 'BUTTON')) t = fold(el.value);
        if (t !== want && fold(el.getAttribute('aria-label')) !== want) continue;
        hits.push(el);
      }
      return innermost(hits);
    }
    return queryCSS(root, q);
  };
  // scopeRoots are the visible elements a step is scoped to with in: (a row, a
  // card, a column); without in the whole document is the one root.
  var scopeRoots = function () {
    if (!scope) return [document];
    return findIn(document, scope).filter(visible);
  };
  var findAll = function (q) {
    var s = q === undefined ? outerSel : q;
    var roots;
    try { roots = scopeRoots(); } catch (e) { return { invalid: e.message, where: 'in' }; }
    if (!roots.length) return { noScope: true };
    try {
      var out = [];
      roots.forEach(function (r) {
        findIn(r, s).forEach(function (el) { if (out.indexOf(el) < 0) out.push(el); });
      });
      return out;
    } catch (e) { return { invalid: e.message }; }
  };
  var badSel = function (all) {
    if (all.invalid) {
      var inScope = all.where === 'in';
      return { ok: false, kind: 'assert', detail: 'invalid ' + (inScope ? 'in ' : '') + 'selector ' + JSON.stringify(inScope ? scope : sel) + ': ' + all.invalid };
    }
    if (all.noScope) return { ok: false, kind: 'not_found', detail: 'no visible element matches in: ' + JSON.stringify(scope) };
    return null;
  };
  var find = function () {
    var all = findAll();
    var bad = badSel(all);
    if (bad) return { error: bad };
    var vis = all.filter(visible);
    if (vis.length) return { el: vis[0] };
    // A checkbox, radio or file input styled away behind its label is used through
    // that label, so the label stands in for it: pointer steps act on the label,
    // visibility checks pass, and value steps still act on the input itself.
    for (var i = 0; i < all.length; i++) {
      var inp = all[i];
      if (inp.tagName !== 'INPUT' || ['checkbox', 'radio', 'file'].indexOf(inp.type) < 0) continue;
      var lab = (inp.labels && Array.prototype.find.call(inp.labels, visible)) || null;
      if (lab) return { el: inp, via: lab };
    }
    var where = scope ? ' in ' + JSON.stringify(scope) : '';
    if (all.length) return { error: { ok: false, kind: 'not_found', detail: JSON.stringify(sel) + where + ' exists but is not visible' } };
    return { error: { ok: false, kind: 'not_found', detail: 'no element matches ' + JSON.stringify(sel) + where } };
  };
  var rectOf = function (el) { var r = el.getBoundingClientRect(); return { X: r.left, Y: r.top, W: r.width, H: r.height }; };
  var tag = function (el, attr) {
    var old = allEls(document).filter(function (o) { return o.hasAttribute(attr); });
    for (var i = 0; i < old.length; i++) old[i].removeAttribute(attr);
    el.setAttribute(attr, '1');
  };
  var fire = function (el, type) { el.dispatchEvent(new Event(type, { bubbles: true })); };
  var body = function () { return document.body ? fold(textOf(document.body)) : ''; };
  var clip = function (t, n) { t = norm(t); return t.length > n ? t.slice(0, n - 3) + '...' : t; };

  // coverOf returns null when el, or something inside or around it, would receive a pointer
  // action, else a failure naming the element on top. A real user meets the same defect.
  var coverOf = function (el, aim) {
    var r = el.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    var at = aim.at || [0.5, 0.5];
    var x = r.left + r.width * at[0], y = r.top + r.height * at[1];
    if (x < 0 || y < 0 || x >= window.innerWidth || y >= window.innerHeight) return null;
    var top = document.elementFromPoint(x, y);
    while (top && top.shadowRoot) {
      var inner = top.shadowRoot.elementFromPoint(x, y);
      if (!inner || inner === top) break;
      top = inner;
    }
    if (!top) return null;
    var within = function (a, b) {
      for (var n = a; n; n = n.parentNode || n.host) if (n === b) return true;
      return false;
    };
    if (within(top, el) || within(el, top)) return null;
    if (top.tagName === 'LABEL' && top.control === el) return null;
    var cs = getComputedStyle(top);
    var cls = typeof top.className === 'string' ? top.className.trim().split(/\s+/).filter(Boolean).slice(0, 3).join('.') : '';
    var name = top.tagName.toLowerCase() + (top.id ? '#' + top.id : '') + (cls ? '.' + cls : '');
    return { ok: false, kind: 'covered', detail: aim.verb + ' on ' + JSON.stringify(outerSel) + ' would land on <' + name +
      '> instead, which sits over it (display:' + cs.display + ', position:' + cs.position + ', pointer-events:' + cs.pointerEvents +
      ', z-index:' + cs.zIndex + ', opacity:' + cs.opacity + '): ' + clip(top.outerHTML, 160) };
  };

  switch (op) {
  case 'mark': {
    var f = find(); if (f.error) return f.error;
    var target = f.via || f.el;
    tag(target, 'data-of-flow-target');
    target.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    // A pointer step passes its aim as value; any other caller marks without the check.
    if (value) {
      var cov = coverOf(target, JSON.parse(value));
      if (cov) return cov;
    }
    return { ok: true, rect: rectOf(target), draggable: target.getAttribute('draggable') === 'true', shadow: target.getRootNode() !== document };
  }
  case 'markdrop': {
    var f = find(); if (f.error) return f.error;
    tag(f.el, 'data-of-flow-drop');
    return { ok: true, rect: rectOf(f.el) };
  }
  case 'rects': {
    var s = document.querySelector('[data-of-flow-target]'), d = document.querySelector('[data-of-flow-drop]');
    if (!s || !d) return { ok: false, kind: 'not_found', detail: 'drag source or target disappeared' };
    return { ok: true, rect: rectOf(s), drop: rectOf(d) };
  }
  case 'dnd': {
    var s = document.querySelector('[data-of-flow-target]'), d = document.querySelector('[data-of-flow-drop]');
    if (!s || !d) return { ok: false, kind: 'not_found', detail: 'drag source or target disappeared' };
    var dt = new DataTransfer();
    var drag = function (el, type) { el.dispatchEvent(new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: dt })); };
    drag(s, 'dragstart'); drag(d, 'dragenter'); drag(d, 'dragover'); drag(d, 'drop'); drag(s, 'dragend');
    return { ok: true };
  }
  case 'markupload': {
    // A file input (or a label for one) receives the file directly; file inputs are
    // routinely hidden behind a styled label, so visibility is not required there.
    // Any other element is a drop zone: it must be visible, and the file is dropped
    // onto it. The result says which, so the caller picks the delivery.
    var all = findAll();
    var bad = badSel(all); if (bad) return bad;
    var el = all[0];
    if (el && el.tagName === 'LABEL' && el.control) el = el.control;
    if (!el) return { ok: false, kind: 'not_found', detail: 'no element matches ' + JSON.stringify(sel) };
    if (el.tagName === 'INPUT' && el.type === 'file') {
      tag(el, 'data-of-flow-target');
      return { ok: true, value: 'input' };
    }
    var zone = all.filter(visible)[0];
    if (!zone) return { ok: false, kind: 'not_found', detail: JSON.stringify(sel) + ' exists but is not visible, so nothing can be dropped onto it' };
    tag(zone, 'data-of-flow-target');
    zone.scrollIntoView({ block: 'center' });
    return { ok: true, value: 'drop' };
  }
  case 'visible': {
    var f = find(); return f.error || { ok: true };
  }
  case 'context': {
    // What the page looked like when a step failed: where it is, the state of the
    // step's target and scope, where the text a text step wanted actually is, the
    // visible controls with selectors a step can use, and the controls closest to a
    // selector that matched nothing.
    var out = { ok: true, url: location.pathname + location.search + location.hash, title: document.title || '' };
    var best = function (el) {
      if (el.id && document.querySelectorAll('#' + CSS.escape(el.id)).length === 1) return '#' + el.id;
      var tagName = el.tagName.toLowerCase(), nm = el.getAttribute('name');
      if (nm) return tagName + '[name="' + nm + '"]';
      var t = norm(el.innerText || el.value || el.getAttribute('aria-label') || '');
      if (t && t.length <= 40) return 'text=' + t;
      var cls = typeof el.className === 'string' ? el.className.trim().split(' ')[0] : '';
      return tagName + (cls ? '.' + cls : '');
    };
    var label = function (el) {
      var t = norm(el.innerText || el.value || el.getAttribute('aria-label') || el.getAttribute('placeholder') || el.getAttribute('title') || '');
      if (!t && el.tagName === 'INPUT') t = el.type;
      return t.length > 40 ? t.slice(0, 37) + '...' : t;
    };
    // snip names an element and quotes what it shows.
    var snip = function (el) {
      var bs = best(el), t = norm(el.innerText || el.value || el.textContent || '');
      if (bs.indexOf('text=') === 0) return bs;
      return bs + (t ? ' "' + clip(t, 160) + '"' : ' (empty)');
    };
    var verb = (info && info.verb) || '', want = info && info.want ? fold(info.want) : '';
    if (sel) {
      var all = findAll();
      if (all.invalid) out.target = 'invalid selector';
      else if (all.noScope) out.target = '';
      else if (!all.length) out.target = 'missing';
      else {
        var vis = all.filter(visible);
        out.target = vis.length ? (vis[0].disabled ? 'disabled' : 'visible') : (find().error ? 'hidden' : 'visible');
        if (verb === 'expectCount' || verb === 'expectHidden' || verb === 'waitFor' || verb === 'expectVisible') {
          out.matches = vis.slice(0, 4).map(snip);
          out.matchCount = vis.length;
          out.hiddenCount = all.length - vis.length;
        }
      }
    }
    var roots = null;
    if (scope) {
      try {
        var sr = findIn(document, scope);
        roots = sr.filter(visible);
        out.scopeState = roots.length ? 'visible' : (sr.length ? 'hidden' : 'missing');
        out.scope = roots.slice(0, 3).map(snip);
        out.scopeCount = roots.length;
      } catch (e) { out.scopeState = 'invalid selector'; }
    }
    if (want && (verb === 'expectText' || verb === 'expectNoText')) {
      var els = document.body ? allEls(document.body) : [];
      var holders = innermost(els.filter(function (el) { return visible(el) && fold(el.innerText).indexOf(want) >= 0; }));
      if (verb === 'expectNoText' && roots) {
        holders = holders.filter(function (h) { return roots.some(function (r) { return r === h || r.contains(h); }); });
      }
      if (verb === 'expectText' && roots) {
        out.foundInScope = holders.some(function (h) { return roots.some(function (r) { return r === h || r.contains(h); }); });
      }
      out.foundIn = holders.slice(0, 3).map(snip);
      if (!holders.length && verb === 'expectText') {
        var hid = innermost(els.filter(function (el) { return fold(el.textContent).indexOf(want) >= 0; }));
        if (hid.length) out.hiddenIn = best(hid[0]);
        // The longest start of the wanted text that the page does show: catches a
        // name the app shortened, a count that differs, a label reworded at the end.
        var text = body();
        for (var n = want.length - 1; !hid.length && n >= Math.max(4, Math.ceil(want.length * 0.6)); n--) {
          var pre = want.slice(0, n);
          if (text.indexOf(pre) < 0) continue;
          var near = innermost(els.filter(function (el) { return visible(el) && fold(el.innerText).indexOf(pre) >= 0; }));
          if (near.length) out.near = snip(near[0]);
          break;
        }
      }
    }
    var nodes = Array.prototype.slice.call(document.querySelectorAll(
      'a[href], button, input:not([type=hidden]), select, textarea, [role=button], [onclick], [contenteditable=true], canvas, label'));
    var controls = [], seen = {};
    for (var i = 0; i < nodes.length && controls.length < 25; i++) {
      var el = nodes[i];
      var hiddenFile = el.tagName === 'INPUT' && el.type === 'file' && !visible(el);
      if (!visible(el) && !hiddenFile) continue;
      var bs = best(el);
      if (seen[bs]) continue;
      seen[bs] = 1;
      var l = label(el);
      controls.push(bs + (hiddenFile ? ' (hidden file input)' : (l && bs.indexOf('text=') !== 0 ? ' "' + l + '"' : '')));
    }
    out.controls = controls;
    if (sel && out.target === 'missing') {
      var stop = ['text', 'div', 'span', 'button', 'input', 'not', 'hidden', 'last', 'first', 'type', 'child', 'has'];
      var words = (sel.toLowerCase().match(/[a-z0-9]{3,}/g) || []).filter(function (w) { return stop.indexOf(w) < 0; });
      var pool = nodes.concat(Array.prototype.slice.call(document.querySelectorAll('[id]')));
      var scored = [], taken = {};
      pool.forEach(function (n) {
        if (!visible(n)) return;
        var bs = best(n);
        if (taken[bs]) return;
        taken[bs] = 1;
        var hay = ((n.id || '') + ' ' + (typeof n.className === 'string' ? n.className : '') + ' ' + norm(n.innerText || n.value || '').slice(0, 80)).toLowerCase();
        var sc = 0;
        words.forEach(function (w) { if (hay.indexOf(w) >= 0) sc++; });
        if (sc) scored.push([sc, bs]);
      });
      scored.sort(function (a, b) { return b[0] - a[0]; });
      out.closest = scored.slice(0, 3).map(function (x) { return x[1]; });
      // text= matches a WHOLE label, so "text=Morning walk" misses a chip that
      // reads "Morning walk + add". Name the visible labels that contain the
      // wanted text (or that it contains) as the exact selectors to use.
      if (sel.indexOf('text=') === 0) {
        var wantLab = fold(sel.slice(5)), nearLabs = [], nearSeen = {};
        var cands = document.body ? allEls(document.body) : [];
        for (var j = 0; j < cands.length && nearLabs.length < 3; j++) {
          var c = cands[j];
          if (!visible(c)) continue;
          var raw = norm(c.innerText || c.value || c.getAttribute('aria-label') || '');
          var lab = fold(raw);
          if (!lab || lab === wantLab || lab.length > 60) continue;
          if (lab.indexOf(wantLab) < 0 && wantLab.indexOf(lab) < 0) continue;
          if (wantLab.indexOf(lab) >= 0 && lab.length < 3) continue;
          // innermost only: skip a wrapper when a child carries the same label
          var inner = false;
          for (var k = 0; k < c.children.length; k++) {
            if (fold(c.children[k].innerText) === lab) { inner = true; break; }
          }
          if (inner || nearSeen[lab]) continue;
          nearSeen[lab] = 1;
          nearLabs.push('text=' + raw);
        }
        out.textMatches = nearLabs;
      }
    }
    return out;
  }
  case 'read': {
    // A form field reads its value, anything else its visible text, unless an
    // attribute is named (href of a share link, data-id of a row). Hidden elements
    // count: apps often keep a generated link in a hidden input.
    var all = findAll();
    var bad = badSel(all); if (bad) return bad;
    var el = all.filter(visible)[0] || all[0];
    if (!el) return { ok: false, kind: 'not_found', detail: 'no element matches ' + JSON.stringify(sel) };
    var v;
    if (value) v = el.getAttribute(value);
    else if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT') v = el.value;
    else v = el.innerText;
    v = String(v == null ? '' : v).trim();
    if (!v) return { ok: false, kind: 'not_found', detail: JSON.stringify(sel) + (value ? ' has no ' + value + ' attribute value' : ' is empty') + ' - nothing to remember yet' };
    return { ok: true, value: v };
  }
  case 'hidden': {
    var all = findAll();
    if (all.noScope) return { ok: true };
    var bad = badSel(all); if (bad) return bad;
    return all.filter(visible).length === 0 ? { ok: true } : { ok: false, kind: 'assert', detail: JSON.stringify(sel) + ' is still visible' };
  }
  case 'scroll': {
    if (sel === 'top') { window.scrollTo(0, 0); return { ok: true }; }
    if (sel === 'bottom') { window.scrollTo(0, document.documentElement.scrollHeight); return { ok: true }; }
    var f = find(); if (f.error) return f.error;
    f.el.scrollIntoView({ block: 'center' });
    return { ok: true };
  }
  case 'fill':
  case 'markfield': {
    // fill sets a text field's value at once, as pasting does; markfield focuses the
    // field with its content selected so the caller can type over it key by key. A
    // rich-text editor (contenteditable) is left focused and selected for either, so
    // the caller inserts the text through the browser's own editing, which editors
    // listen to. A field's maxlength holds as it does for a person: the extra
    // characters never arrive, and the note says so.
    var f = find(); if (f.error) return f.error;
    var el = f.el;
    var selectAll = function () {
      el.focus();
      if (el.isContentEditable) {
        var r = document.createRange(); r.selectNodeContents(el);
        var s = window.getSelection(); s.removeAllRanges(); s.addRange(r);
      } else if (typeof el.select === 'function') {
        try { el.select(); } catch (e) {}
      }
    };
    if (el.isContentEditable) {
      tag(el, 'data-of-flow-target');
      selectAll();
      return { ok: true, field: 'editable', value: norm(el.innerText) };
    }
    if (el.tagName !== 'INPUT' && el.tagName !== 'TEXTAREA') return { ok: false, kind: 'assert', detail: JSON.stringify(sel) + ' is a <' + el.tagName.toLowerCase() + '>, not a text field' };
    if (el.disabled || el.readOnly) return { ok: false, kind: 'not_found', detail: JSON.stringify(sel) + ' is disabled or read-only' };
    tag(el, 'data-of-flow-target');
    var note = '';
    if (el.maxLength > 0 && value.length > el.maxLength) {
      note = JSON.stringify(sel) + ' accepts at most ' + el.maxLength + ' characters (maxlength), so of the ' + value.length +
        ' in ' + JSON.stringify(value) + ' it holds ' + JSON.stringify(value.slice(0, el.maxLength));
    }
    if (op === 'markfield') {
      selectAll();
      return { ok: true, field: 'text', value: el.value, maxLength: el.maxLength > 0 ? el.maxLength : 0, note: note };
    }
    if (note) value = value.slice(0, el.maxLength);
    el.focus();
    // The native setter, so frameworks that shadow .value still see the change.
    var proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, value);
    fire(el, 'input'); fire(el, 'change');
    return { ok: true, field: 'text', note: note };
  }
  case 'fieldvalue': {
    var el = document.querySelector('[data-of-flow-target]');
    if (!el) return { ok: false, kind: 'not_found', detail: 'the field disappeared' };
    return { ok: true, value: el.isContentEditable ? norm(el.innerText) : String(el.value) };
  }
  case 'select': {
    var f = find(); if (f.error) return f.error;
    var el = f.el;
    if (el.tagName !== 'SELECT') return { ok: false, kind: 'assert', detail: JSON.stringify(sel) + ' is not a <select>' };
    var opt = Array.prototype.find.call(el.options, function (o) { return o.value === value; }) ||
              Array.prototype.find.call(el.options, function (o) { return norm(o.text) === norm(value); });
    if (!opt) return { ok: false, kind: 'not_found', detail: JSON.stringify(sel) + ' has no option ' + JSON.stringify(value) };
    el.value = opt.value; fire(el, 'input'); fire(el, 'change');
    return { ok: true };
  }
  case 'check':
  case 'uncheck': {
    var f = find(); if (f.error) return f.error;
    var el = f.el, want = op === 'check';
    if (!('checked' in el)) return { ok: false, kind: 'assert', detail: JSON.stringify(sel) + ' is not a checkbox or radio' };
    if (el.checked !== want) el.click();
    return el.checked === want ? { ok: true } : { ok: false, kind: 'assert', detail: JSON.stringify(sel) + ' did not become ' + (want ? 'checked' : 'unchecked') };
  }
  case 'text':
  case 'notext': {
    // sel is the text; with in:, only the visible elements matching scope count.
    var want = fold(sel), shows;
    var where = scope ? ' in ' + JSON.stringify(scope) : '';
    if (scope) {
      var roots;
      try { roots = scopeRoots(); } catch (e) { return { ok: false, kind: 'assert', detail: 'invalid in selector ' + JSON.stringify(scope) + ': ' + e.message }; }
      if (op === 'text' && !roots.length) return { ok: false, kind: 'not_found', detail: 'no visible element matches in: ' + JSON.stringify(scope) };
      shows = roots.some(function (el) { return fold(textOf(el)).indexOf(want) >= 0; });
    } else {
      shows = body().indexOf(want) >= 0;
    }
    if (op === 'text') return shows ? { ok: true } : { ok: false, kind: 'assert', detail: 'page does not show ' + JSON.stringify(sel) + where };
    return !shows ? { ok: true } : { ok: false, kind: 'assert', detail: 'page still shows ' + JSON.stringify(sel) + where };
  }
  case 'url': {
    // The query and the hash count only when the expectation names them, so
    // "/about" still matches "/about?ref=x" while "/#chat" checks the fragment.
    var strip = function (p) { return p.length > 1 ? p.replace(/\/+$/, '') : p; };
    var full = location.pathname + location.search + location.hash;
    var here = location.pathname + (sel.indexOf('?') >= 0 ? location.search : '') + (sel.indexOf('#') >= 0 ? location.hash : '');
    var ok = sel.slice(-1) === '*' ? full.indexOf(sel.slice(0, -1)) === 0 : strip(here) === strip(sel);
    return ok ? { ok: true } : { ok: false, kind: 'assert', detail: 'URL is ' + full + ', expected ' + sel };
  }
  case 'count': {
    var all = findAll();
    var n = 0;
    if (!all.noScope) {
      var bad = badSel(all); if (bad) return bad;
      n = all.filter(visible).length;
    }
    var where = scope ? ' in ' + JSON.stringify(scope) : '';
    return String(n) === value ? { ok: true } : { ok: false, kind: 'assert', detail: JSON.stringify(sel) + where + ' matches ' + n + ' visible elements, expected ' + value };
  }
  }
  return { ok: false, kind: 'assert', detail: 'unknown op ' + op };
}