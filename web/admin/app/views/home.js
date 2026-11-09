/**
 * Home: the prompt is the app.
 *
 * Replaces the old dashboard at the same route, and deliberately shows nothing
 * else but the composer. Anything that needs attention (startup failures,
 * pending questions) sits above it; the project list and system stats live on
 * /projects, which is where you go when you want to look at what exists rather
 * than make something new.
 */

import { h, clear } from '../core/dom.js';
import { t, tn } from '../core/i18n.js';
import { get } from '../core/http.js';
import { navigate } from '../core/router.js';
import { defineView } from '../core/view.js';
import { icon } from '../ui/icon.js';
import * as state from '../core/state.js';
import { warningCard } from '../ui/warning-card.js';
import { clickable } from '../ui/helpers.js';
import { humanizeError } from '../ui/errors.js';
import { createHomeComposer } from './home/composer.js';
import { createMotes } from '../ui/motes.js';

export const renderHome = defineView(async (ctx, container) => {
  clear(container);

  const banners = h('div', { className: 'home-banners' });
  const hero = h('div', { className: 'home-hero-slot' }, [
    h('div', { className: 'skeleton skeleton--card' }),
  ]);

  container.appendChild(h('div', { className: 'home' }, [createMotes(), banners, hero]));

  try {
    const composer = await createHomeComposer();
    clear(hero);
    hero.appendChild(composer.element);
    composer.focus();
  } catch (err) {
    clear(hero);
    hero.appendChild(h('div', { className: 'home-hero__error' },
      t('home.loadFailed', { error: humanizeError(err.message) })));
  }

  // Banners are secondary to the composer, so they load after it and their
  // failures are silent: a missing warning must not cost you the prompt box.
  const [status, questions] = await Promise.all([
    get('/admin/api/system/status').catch(() => null),
    get('/admin/api/questions?status=pending').catch(() => []),
  ]);

  if (status) {
    state.set('systemStatus', status);
  }

  if (questions.length > 0) {
    // A real button for keyboard and screen-reader users, not a clickable div.
    banners.appendChild(clickable(h('div', {
      className: 'attention-banner attention-banner--clickable',
    }, [
      h('span', { innerHTML: icon('alert-circle') }),
      h('span', { className: 'attention-banner__text' },
        tn('home.pendingQuestions', questions.length)
      ),
      h('span', { className: 'ml-auto', innerHTML: icon('chevron-right') }),
    ]), () => navigate('/questions')));
  }

  // Non-fatal startup failures (engine auto-start, jobs). Returned by the
  // status endpoint since boot; dismissing hides them until the next boot.
  for (const w of (status && status.boot_warnings) || []) {
    const card = warningCard('boot:' + w, t('home.bootWarning', { error: humanizeError(w) }));
    if (card) banners.appendChild(card);
  }

  // A public domain with wide-open defaults (any-origin CORS, permissive CSP):
  // fine for a local box, worth tightening before serving untrusted visitors.
  if (state.isAdmin() && status && (status.exposure_warnings || []).length > 0) {
    const issues = status.exposure_warnings.join('; ');
    const card = warningCard(
      'exposure:' + issues,
      t('home.exposure', { issues })
    );
    if (card) banners.appendChild(card);
  }

  // One-time reminder that there is no password reset. Shown until
  // dismissed; the setup wizard mentions it once, easy to miss.
  if (state.isAdmin()) {
    const card = warningCard(
      'password-reminder',
      t('home.passwordReminder'),
      { kind: 'info', iconName: 'lock' }
    );
    if (card) banners.appendChild(card);
  }
});
