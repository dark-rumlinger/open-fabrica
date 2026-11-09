/**
 * Scoped cleanup helper. Views/components register listeners, timers,
 * abortable requests, and ad-hoc teardown through a scope; dispose()
 * unwinds everything in reverse registration order.
 */

export function createScope() {
  const cleanups = [];
  let disposed = false;

  function add(fn) {
    if (disposed) {
      try { fn(); } catch (e) { console.error('[lifecycle] cleanup after dispose threw', e); }
      return () => {};
    }
    cleanups.push(fn);
    return () => {
      const i = cleanups.indexOf(fn);
      if (i !== -1) cleanups.splice(i, 1);
    };
  }

  function on(target, evt, handler, opts) {
    target.addEventListener(evt, handler, opts);
    return add(() => target.removeEventListener(evt, handler, opts));
  }

  function timeout(fn, ms) {
    const id = setTimeout(() => {
      const i = cleanups.indexOf(cancel);
      if (i !== -1) cleanups.splice(i, 1);
      fn();
    }, ms);
    const cancel = () => clearTimeout(id);
    cleanups.push(cancel);
    return cancel;
  }

  function interval(fn, ms) {
    const id = setInterval(fn, ms);
    return add(() => clearInterval(id));
  }

  function abort(controller) {
    return add(() => {
      try { controller.abort(); } catch {}
    });
  }

  function controller() {
    const c = new AbortController();
    abort(c);
    return c;
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    while (cleanups.length) {
      const fn = cleanups.pop();
      try { fn(); } catch (e) { console.error('[lifecycle] cleanup threw', e); }
    }
  }

  return {
    add, on, timeout, interval, abort, controller, dispose,
    get disposed() { return disposed; },
  };
}
