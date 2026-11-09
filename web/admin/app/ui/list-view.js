/**
 * List-view helper: collapses the clear -> fetch -> render -> empty/error loop
 * that admin CRUD views all share. Opts: scope (optional lifecycle scope,
 * auto-cancels in-flight loads), container, fetch, renderItem, wrap, empty,
 * errorMsg, onLoaded. Returns { reload, showEmpty, showItems }.
 * reload({ quiet: true }) refreshes in place (no skeleton flash, scroll kept);
 * use it for background refreshes such as a tool run finishing.
 * Optional key(item) names each row (default: item.id, then item.name). When
 * every row has a unique key, a reload animates only the rows that appeared or
 * disappeared; lists without usable keys simply swap.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { emptyState, errorState, listSkeleton } from './helpers.js';
import { humanizeError } from './errors.js';
import { latest } from '../core/http.js';

export function createListView(opts) {
  const {
    scope,
    container,
    fetch: fetchFn,
    renderItem,
    wrap,
    empty = t('list.empty'),
    errorMsg = t('list.loadFailed'),
    onLoaded,
    filter,
  } = opts;

  if (!container) throw new Error('createListView: container required');
  if (typeof fetchFn !== 'function') throw new Error('createListView: fetch required');
  if (typeof renderItem !== 'function') throw new Error('createListView: renderItem required');

  // Optional in-view filter: a search box above the list that narrows the
  // already-fetched items in place, with no refetch. listEl is the element the
  // show* helpers clear and fill, so the filter box itself survives a reload.
  // Without a filter, listEl is the container and nothing changes.
  const filterCfg = normalizeFilter(filter);
  let listEl = container;
  let query = '';
  let lastItems = null;
  if (filterCfg) {
    const input = h('input', {
      type: 'search',
      className: 'list-filter',
      placeholder: filterCfg.placeholder,
      'aria-label': filterCfg.placeholder,
    });
    let debounce;
    input.addEventListener('input', () => {
      clearTimeout(debounce);
      debounce = setTimeout(() => {
        const next = input.value.trim().toLowerCase();
        if (next === query) return;
        query = next;
        if (lastItems) renderList(lastItems);
      }, 150);
    });
    listEl = h('div');
    container.appendChild(h('div', { className: 'list-filter-bar' }, [input]));
    container.appendChild(listEl);
  }

  const guarded = latest(async () => fetchFn());
  // latest() only tags resolved results as stale; rejections carry no such
  // signal, so track our own sequence to ignore failures of superseded reloads.
  let reloadSeq = 0;

  // The last rendered set, for diffing: { keys: [...], nodes: Map<key, node> }.
  // Null whenever the container holds anything but a keyed list (skeleton,
  // empty state, error, or items without stable ids), which turns diffing off.
  let prev = null;

  function keyOf(item) {
    if (typeof opts.key === 'function') return opts.key(item);
    if (item && typeof item === 'object') return item.id ?? item.name ?? null;
    return null;
  }

  function showLoading() {
    prev = null;
    clear(listEl);
    listEl.appendChild(listSkeleton());
  }

  function showEmpty() {
    prev = null;
    clear(listEl);
    let node;
    if (typeof empty === 'string') {
      node = emptyState({ iconName: 'list', title: t('list.emptyTitle'), description: empty });
    } else if (empty && empty.nodeType === 1) {
      node = empty;
    } else if (empty && typeof empty === 'object') {
      node = emptyState(empty);
    } else {
      node = emptyState({ iconName: 'list', title: t('list.emptyTitle') });
    }
    listEl.appendChild(node);
  }

  // Shown when a filter query excludes every row (distinct from a genuinely
  // empty list, which keeps the caller's own empty state).
  function showNoMatches() {
    prev = null;
    clear(listEl);
    listEl.appendChild(emptyState({
      iconName: 'search',
      title: t('list.noMatchesTitle'),
      description: t('list.noMatches', { query }),
    }));
  }

  // Render the current query against a full item set: filtered items, the
  // no-matches state, or the caller's empty state when there is no data at all.
  function renderList(items) {
    const shown = filterCfg && query
      ? items.filter((it) => filterCfg.predicate(it, query))
      : items;
    if (!shown || shown.length === 0) {
      if (filterCfg && query) showNoMatches();
      else showEmpty();
      return;
    }
    showItems(shown);
  }

  /**
   * A removed row's last frame: a clone of its old node that fades and collapses
   * in place, then disposes of itself. Inert, since the real row is already gone.
   */
  function leaveGhost(node, height) {
    const ghost = node.cloneNode(true);
    ghost.classList.remove('enter-item');
    ghost.querySelectorAll('[id]').forEach(el => el.removeAttribute('id'));
    ghost.setAttribute('aria-hidden', 'true');
    ghost.inert = true;
    ghost.style.setProperty('--leave-h', `${height}px`);
    ghost.classList.add('leave-item');
    const done = () => ghost.remove();
    ghost.addEventListener('animationend', done, { once: true });
    setTimeout(done, 600); // backstop if the animation never runs
    return ghost;
  }

  function showItems(items) {
    const pairs = items
      .map(item => ({ item, node: renderItem(item, { reload }) }))
      .filter(p => p.node);
    // A first load (a skeleton on screen) staggers everything in.
    const fromSkeleton = !!listEl.querySelector('.skeleton-list');
    const keys = pairs.map(p => keyOf(p.item));
    const keyed = keys.length > 0 && keys.every(k => k != null) && new Set(keys).size === keys.length;
    const old = fromSkeleton || !keyed ? null : prev;

    const ghosts = [];
    if (fromSkeleton) {
      pairs.forEach(({ node }, i) => {
        if (!node.classList) return;
        node.classList.add('enter-item');
        // Past the first handful everything arrives together.
        node.style.setProperty('--enter-i', String(Math.min(i, 8)));
      });
    } else if (old) {
      // Only what actually appeared or disappeared animates; rows that were
      // already there re-render silently, as a plain swap always did.
      pairs.forEach(({ node }, i) => {
        if (!old.nodes.has(keys[i]) && node.classList) node.classList.add('enter-item');
      });
      const kept = new Set(keys);
      old.keys.forEach((k, idx) => {
        const node = old.nodes.get(k);
        // Measure now: clear() below detaches it and its height with it.
        if (!kept.has(k) && node && node.isConnected) ghosts.push({ idx, node, height: node.offsetHeight });
      });
    }

    clear(listEl);
    const nodes = pairs.map(p => p.node);
    if (wrap) {
      listEl.appendChild(wrap(nodes));
    } else {
      for (const node of nodes) listEl.appendChild(node);
    }

    // Put each ghost back where its row was: before the next surviving row, or
    // after the last surviving one. With no survivors there is no anchor and
    // the swap stays plain.
    if (old && ghosts.length) {
      const fresh = new Map(pairs.map((p, i) => [keys[i], p.node]));
      for (const g of ghosts) {
        let anchor = null;
        for (let j = g.idx + 1; j < old.keys.length && !anchor; j++) anchor = fresh.get(old.keys[j]) || null;
        let after = null;
        for (let j = g.idx - 1; j >= 0 && !anchor && !after; j--) after = fresh.get(old.keys[j]) || null;
        const host = anchor || after;
        if (!host || !host.parentNode) continue;
        const ghost = leaveGhost(g.node, g.height);
        if (anchor) host.parentNode.insertBefore(ghost, anchor);
        else host.after(ghost);
        // Flex/grid gaps would leave a gap-sized jump once the ghost is gone,
        // so the collapse eats the gap as well.
        const gap = parseFloat(getComputedStyle(host.parentNode).rowGap);
        if (gap > 0) ghost.style.setProperty('--leave-gap', `${gap}px`);
      }
    }

    prev = keyed ? { keys, nodes: new Map(pairs.map((p, i) => [keys[i], p.node])) } : null;
  }

  async function reload(reloadOpts) {
    const quiet = !!(reloadOpts && reloadOpts.quiet === true);
    if (scope && scope.disposed) return;
    const seq = ++reloadSeq;
    // A quiet reload keeps what is on screen until the new data lands, but only
    // if there is something to keep; first load still shows the skeleton. A
    // keyed list already on screen is kept too, so a create or delete animates
    // just the row that changed instead of flashing the whole list.
    if (!listEl.firstChild || (!quiet && !prev)) showLoading();
    let result;
    try {
      result = await guarded();
    } catch (err) {
      // A newer reload owns the list now; its result (or error) will land on its
      // own, so a stale failure must not clobber it or toast.
      if (seq !== reloadSeq) return;
      if (scope && scope.disposed) return;
      // A quiet (background) failure leaves the last good content in place.
      if (quiet && listEl.firstChild && !listEl.querySelector('.skeleton-list')) return;
      prev = null;
      clear(listEl);
      listEl.appendChild(errorState({
        title: errorMsg,
        message: humanizeError(err.message),
        retry: () => reload(),
        reference: err.requestId,
      }));
      return;
    }
    if (result.stale) return;
    if (scope && scope.disposed) return;
    const items = result.data;
    // Keep the full set so the filter box can re-narrow it without a refetch.
    lastItems = Array.isArray(items) ? items : (items || []);
    renderList(lastItems);
    if (onLoaded) {
      try { onLoaded(items); } catch (e) { console.error('[list-view] onLoaded threw', e); }
    }
  }

  return { reload, showEmpty, showItems };
}

// normalizeFilter turns the `filter` option into a config, or null when off.
// `true` uses the default placeholder and predicate; an object may override
// either. Returns { placeholder, predicate }.
export function normalizeFilter(filter) {
  if (!filter) return null;
  const cfg = filter === true ? {} : filter;
  return {
    placeholder: cfg.placeholder || t('list.filterPlaceholder'),
    predicate: typeof cfg.predicate === 'function' ? cfg.predicate : defaultPredicate,
  };
}

// defaultPredicate matches the lowercased query against any string or number
// field of the item, so it works across every list shape (name, username, key,
// description, ...) without each view naming its own fields. A view that needs
// precise matching passes its own predicate.
export function defaultPredicate(item, q) {
  if (item == null) return false;
  if (typeof item !== 'object') return String(item).toLowerCase().includes(q);
  return Object.values(item).some(
    (v) => (typeof v === 'string' || typeof v === 'number') && String(v).toLowerCase().includes(q),
  );
}
