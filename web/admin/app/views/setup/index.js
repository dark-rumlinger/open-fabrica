/**
 * Setup orchestrator - manages the multi-step onboarding flow (see STEPS).
 *
 * The card is a modal-anatomy auth card (ui/auth-shell.js): the orchestrator
 * owns its header (title + step count + progress rule) and footer (actions),
 * and each step renders only its body. Steps talk to the chrome through the
 * ctx they are handed:
 *
 *   render({ body, setHeader(title, lede, { why }?), setActions(nodes), data, next, back })
 */

import { h, render, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { renderLanguage, takeResume } from './step-language.js';
import { renderPreview } from './step-preview.js';
import { renderWelcome } from './step-welcome.js';
import { renderAccount } from './step-account.js';
import { renderProvider } from './step-provider.js';
import { renderLaunching } from './step-launching.js';
import { runSplash } from '../../ui/splash.js';
import { createAuthShell, authCard } from '../../ui/auth-shell.js';

const STEPS = [
  {
    id: 'language',
    label: t('setup.step.language'),
    why: null,
  },
  {
    id: 'preview',
    label: t('preview.title'),
    why: null,
  },
  {
    id: 'welcome',
    label: t('setup.step.welcome'),
    why: null,
  },
  {
    id: 'account',
    label: t('account.title'),
    why: t('setup.why.account'),
  },
  {
    id: 'provider',
    label: t('setup.step.provider'),
    why: t('setup.why.provider'),
  },
  {
    id: 'launching',
    label: t('setup.step.launching'),
    why: null,
  },
];

const STEP_RENDERERS = {
  language: renderLanguage,
  preview: renderPreview,
  welcome: renderWelcome,
  account: renderAccount,
  provider: renderProvider,
  launching: renderLaunching,
};

const EXIT_MS = 180;

// `needsSetupToken` is true when the admin panel is reachable from the network and
// no admin exists yet: the server then wants the token it printed to its console.
export function renderSetup(container, { needsSetupToken = false } = {}) {
  let currentStep = 0;
  let direction = 'forward';
  let stepEl = null;

  const setupData = {
    displayName: '',
    username: '',
    password: '',
    providerId: 0,
    providerName: '',
    modelId: '',
    apiKey: '',
    needsSetupToken,
    setupToken: '',
  };

  // Header, progress rule and footer are built once so the bar animates
  // smoothly between steps; only the body swaps.
  const count = h('span', { className: 'auth-card__count' });
  const progressFill = h('div', { className: 'auth-card__progress-fill' });
  const progress = h('div', {
    className: 'auth-card__progress',
    role: 'progressbar',
    'aria-valuemin': '1',
    'aria-valuemax': String(STEPS.length - 1),
  }, [progressFill]);

  const { card, title, body, footer } = authCard({
    // Mounted hidden behind the splash; runSplash() lifts the preboot state
    // once the brand moment resolves.
    className: 'auth-card--stepped auth-card--preboot',
    aside: count,
    header: [progress],
  });

  // The language step is where the language is chosen, so the bar's picker
  // would only duplicate it.
  const shell = createAuthShell({ children: [card], languagePicker: false });
  render(container, shell.root);

  // The language step is a lead-in: it has no title, count or progress rule,
  // and the numbered steps start after it.
  function updateProgress() {
    const step = STEPS[currentStep];
    const intro = step.id === 'language';
    card.classList.toggle('auth-card--intro', intro);
    if (intro) return;
    const n = currentStep;
    const total = STEPS.length - 1;
    count.textContent = t('setup.count', { n, total });
    progressFill.style.width = `${(n / total) * 100}%`;
    progress.setAttribute('aria-valuenow', String(n));
    progress.setAttribute('aria-valuemax', String(total));
    progress.setAttribute('aria-label', t('setup.stepLabel', { label: step.label, n, total }));
  }

  function setActions(nodes) {
    clear(footer);
    const list = (nodes || []).filter(Boolean);
    list.forEach((n) => footer.appendChild(n));
    // Back on the left and the forward action on the right; a lone action
    // sits right, like a dialog's.
    footer.classList.toggle('modal__footer--split', list.length > 1);
  }

  // Each step gets its own header setter so a late call from a step that has
  // already been left can't repaint the one now showing.
  function makeCtx(el, step) {
    let headNodes = [];
    function setHeader(text, lede, opts = {}) {
      if (stepEl !== el) return;
      title.textContent = text;
      headNodes.forEach((n) => n.remove());
      headNodes = [];
      if (lede) headNodes.push(h('p', { className: 'auth-card__lede' }, lede));
      // "Why we ask this" copy is centralized in STEPS rather than scattered
      // through the step renderers.
      if (step.why && opts.why !== false) {
        headNodes.push(h('p', { className: 'form-hint form-hint--info' }, [
          h('span', {}, [h('strong', {}, t('setup.whyLabel') + ' '), step.why]),
        ]));
      }
      el.prepend(...headNodes);
    }
    return {
      body: el,
      setHeader,
      setActions: (nodes) => { if (stepEl === el) setActions(nodes); },
      data: setupData,
      next,
      back,
    };
  }

  // Awaits the renderer because the decoration below reads the DOM the
  // renderer appends. Every step is synchronous today, but running the
  // decoration before an async one settled would silently drop the staggered
  // field entrance.
  async function mountStep() {
    const step = STEPS[currentStep];
    const newStep = h('div', {
      className: `setup-step${direction === 'back' ? ' is-back' : ''}`,
    });
    body.appendChild(newStep);
    stepEl = newStep;
    updateProgress();
    setActions([]);
    await STEP_RENDERERS[step.id](makeCtx(newStep, step));
    // A later step may have mounted while we were waiting.
    if (stepEl !== newStep) return;
    decorateFields(newStep);
  }

  // currentStep moves immediately, so a double click or a second Enter during
  // the exit animation would otherwise step again and skip a screen (e.g. the
  // provider step, creating an account with no AI configured).
  let transitioning = false;

  // Steps differ in height. The card keeps its old height while the body swaps,
  // then eases to the new step's natural height so the footer glides instead of
  // jumping. A newer swap cancels an older one's cleanup via the token.
  const HEIGHT_MS = 280;
  const reduceMotion = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  let heightToken = 0;

  function holdHeight(px) {
    heightToken++;
    card.style.transition = 'none';
    card.style.height = `${px}px`;
    card.classList.add('is-resizing');
  }

  function easeHeightFrom(fromPx) {
    const token = ++heightToken;
    function done() {
      if (token !== heightToken) return;
      card.style.transition = '';
      card.style.height = '';
      card.classList.remove('is-resizing');
    }
    card.style.height = 'auto';
    const toPx = card.offsetHeight;
    if (toPx === fromPx) { done(); return; }
    card.style.height = `${fromPx}px`;
    void card.offsetHeight; // commit the starting height before animating
    card.style.transition = `height ${HEIGHT_MS}ms cubic-bezier(0.2, 0.7, 0.2, 1)`;
    card.style.height = `${toPx}px`;
    setTimeout(done, HEIGHT_MS + 40);
  }

  function go(toIndex, dir) {
    if (transitioning) return;
    if (toIndex < 0 || toIndex >= STEPS.length || toIndex === currentStep) return;
    transitioning = true;
    direction = dir;
    const fromHeight = card.offsetHeight;
    const outgoing = stepEl;
    if (outgoing) {
      outgoing.classList.add('is-leaving');
      if (dir === 'back') outgoing.classList.add('is-back');
      setTimeout(() => {
        if (outgoing.parentNode) outgoing.parentNode.removeChild(outgoing);
      }, EXIT_MS);
    }
    currentStep = toIndex;
    // Mount slightly after exit-start so the two animations overlap nicely.
    setTimeout(() => {
      if (!reduceMotion) holdHeight(fromHeight);
      clear(body); // safety: drop any leftover nodes
      transitioning = false;
      const mounted = mountStep();
      if (!reduceMotion) mounted.then(() => easeHeightFrom(fromHeight), () => easeHeightFrom(fromHeight));
    }, Math.max(EXIT_MS - 60, 0));
  }

  function next() { go(currentStep + 1, 'forward'); }
  function back() { go(currentStep - 1, 'back'); }

  // Initial mount (no exit animation), then play the loom-weave splash over the
  // hidden card and morph the logo into the bar. The weave is reserved for
  // first-run setup; login uses the brisk logo reveal. Choosing a language
  // reloads the page, and that return trip gets the brisk reveal too.
  mountStep();
  const resumed = takeResume();
  runSplash(shell.root, card, resumed
    ? { weave: false, holdMs: 500, target: shell.brandMark }
    : { weave: true, target: shell.brandMark });
}

/**
 * Tag the step's top-level children so they pick up the staggered
 * .setup-step__field entrance. Steps don't need to know about this class
 * themselves - we apply it after the renderer runs.
 */
function decorateFields(stepEl) {
  let i = 0;
  for (const child of Array.from(stepEl.children)) {
    if (!(child instanceof HTMLElement)) continue;
    child.classList.add('setup-step__field');
    child.style.setProperty('--i', String(i++));
  }
}
