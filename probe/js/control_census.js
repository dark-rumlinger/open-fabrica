(function () {
  function visible(el) {
    try {
      var cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden' || cs.visibility === 'collapse') return false;
      // A closed <details> body (or a content-visibility:hidden subtree) reports a box
      // but is never painted: nobody can see or use those controls yet.
      if (el.checkVisibility && !el.checkVisibility()) return false;
      var d = el.closest('details:not([open])'), s = el.closest('summary');
      if (d && !(s && s.parentElement === d)) return false;
      var rects = el.getClientRects();
      if (!rects || rects.length === 0) return false;
      var b = el.getBoundingClientRect();
      return b.width > 0 && b.height > 0;
    } catch (e) { return false; }
  }
  function nativeLive(el) {
    var tag = el.tagName;
    if (tag === 'A') { var h = el.getAttribute('href'); return h !== null && h !== ''; }
    if (tag === 'SUMMARY') return true;
    if (tag === 'LABEL') return el.hasAttribute('for') || !!el.querySelector('input,select,textarea');
    if (tag === 'BUTTON') { var bt = (el.getAttribute('type') || '').toLowerCase();
      return (bt === '' || bt === 'submit' || bt === 'reset') && !!el.form; }
    if (tag === 'INPUT') { var it = (el.getAttribute('type') || '').toLowerCase();
      return (it === 'submit' || it === 'image' || it === 'reset') && !!el.form; }
    return false;
  }
  function ancestorLive(el) {
    for (var n = el.parentElement; n && n !== document.documentElement; n = n.parentElement) {
      try { if (n.hasAttribute('onclick') || nativeLive(n)) return true; } catch (e) {}
    }
    return false;
  }
  // classify: 'live' (actionable from markup), 'candidate' (needs a listener),
  // or 'skip' (not a control we judge - a bare anchor, a text input).
  function classify(el) {
    if (el.hasAttribute('onclick') || ancestorLive(el)) return 'live';
    var tag = el.tagName;
    var role = (el.getAttribute('role') || '').toLowerCase();
    if (tag === 'A') { var h = el.getAttribute('href');
      if (h !== null && h !== '') return 'live';
      return role === 'button' ? 'candidate' : 'skip'; }
    if (tag === 'SUMMARY') return 'live';
    if (tag === 'LABEL') return nativeLive(el) ? 'live' : 'candidate';
    if (tag === 'BUTTON') { var bt = (el.getAttribute('type') || '').toLowerCase();
      if (bt === '' || bt === 'submit' || bt === 'reset') return el.form ? 'live' : 'candidate';
      return 'candidate'; }
    if (tag === 'INPUT') { var it = (el.getAttribute('type') || '').toLowerCase();
      if (it === 'submit' || it === 'image' || it === 'reset') return el.form ? 'live' : 'candidate';
      if (it === 'button') return 'candidate';
      return 'skip'; }
    if (role === 'button') return 'candidate';
    return 'skip';
  }
  function label(el) {
    var t = (el.textContent || el.value || el.getAttribute('aria-label') || '').trim().replace(/\s+/g, ' ');
    return t.slice(0, 60);
  }
  function path(el) {
    if (el.id) return el.tagName.toLowerCase() + '#' + el.id;
    var cls = (el.getAttribute('class') || '').trim().split(/\s+/).filter(Boolean).slice(0, 2);
    return el.tagName.toLowerCase() + (cls.length ? '.' + cls.join('.') : '');
  }
  var sel = 'button, [role="button"], a, [onclick], input[type="submit"], input[type="button"], input[type="image"], summary, label';
  var els = document.querySelectorAll(sel);
  var total = 0, out = [], idx = 0, hidden = 0, hiddenSel = [];
  for (var i = 0; i < els.length; i++) {
    var el = els[i];
    var kind = classify(el);
    if (kind === 'skip') continue;
    if (!visible(el)) {
      // A control that exists but cannot be seen. Counted separately: on its own
      // this is ordinary (a modal, a collapsed menu), but a page where EVERY
      // control is one of these has no reachable UI at all.
      hidden++;
      if (hiddenSel.length < 8) hiddenSel.push(path(el));
      continue;
    }
    total++;
    if (kind === 'live') continue;
    try { el.setAttribute('data-ofdeadctl', String(idx)); } catch (e) { continue; }
    out.push({ i: idx, tag: el.tagName.toLowerCase(), text: label(el), sel: path(el) });
    idx++;
  }
  var canvas = 0, cvs = document.querySelectorAll('canvas');
  for (var c = 0; c < cvs.length; c++) {
    try { var cr = cvs[c].getBoundingClientRect(); if (visible(cvs[c]) && cr.width * cr.height >= 40000) canvas++; } catch (e) {}
  }
  return JSON.stringify({ total: total, candidates: out, hidden: hidden, hiddenSel: hiddenSel, canvas: canvas });
})()