/**
 * View controller contract: the router calls destroy() before mounting the
 * next view. Async mounts resolve after defineView returns, so the resolved
 * controller is adopted late and destroy() may land before the mount settles;
 * scope.dispose() runs on destroy() either way.
 */

import { createScope } from './lifecycle.js';

export function defineView(mount) {
  return function (...args) {
    const scope = createScope();
    const ctx = { scope };
    const result = mount(ctx, ...args);

    let inner = null;
    let destroyed = false;

    const runDestroy = (r) => {
      try { r.destroy(); } catch (e) { console.error('[view] destroy threw', e); }
    };

    const adopt = (r) => {
      if (!r || typeof r.destroy !== 'function') return;
      // destroy() already ran while the mount was in flight: the view is gone,
      // so tear the late controller down at once rather than leaking it.
      if (destroyed) runDestroy(r);
      else inner = r;
    };

    if (result && typeof result.then === 'function') {
      // Not Promise.resolve(...).then for the sync case: that would defer adopt
      // to a microtask, so a destroy() called synchronously after mount would
      // miss the controller.
      result.then(adopt, (e) => console.error('[view] mount failed', e));
    } else {
      adopt(result);
    }

    return {
      destroy() {
        destroyed = true;
        if (inner) {
          runDestroy(inner);
          inner = null;
        }
        scope.dispose();
      },
    };
  };
}
