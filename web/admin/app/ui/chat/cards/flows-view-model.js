/**
 * View model for the user flow test cards. Turns a manage_testing result for
 * write_flows / test_flows (or the go-live flow_check payload) into the plain
 * data the card renders: a state, a title and subtitle, one row per flow and a
 * few footnotes. Kept free of DOM imports so the wording can be unit-tested
 * under node.
 *
 * A write_flows call saves the script AND replays it, so a red replay is a
 * saved script with failing flows, never a rejected one. Only a refusal that
 * saved nothing is "rejected".
 */

import { t, tn } from '../../../core/i18n.js';

// These two patterns read the English text the server writes; they are not UI copy.
const FAILED_HEAD = /^(\d+) of (\d+) flows failed(?::\s*(.*?))?(?:\s*\((\d+) more blocked by them\))?$/;
const FAILURE_HEAD = /^user flow "(.+?)" \(of_test\.js\) failed at step (\d+) of (\d+)/;

const SKIP_KINDS = ['no_browser', 'visitors', 'snapshot', 'cancelled'];

/** [title, sub] for a skipped replay, by the backend's skip_kind. */
function skipCopy(kind) {
  const k = SKIP_KINDS.includes(kind) ? kind : 'default';
  return [t(`fv.skip.${k}.title`), t(`fv.skip.${k}.sub`)];
}

function list(v) {
  return Array.isArray(v) ? v.filter((s) => typeof s === 'string') : [];
}

function clip(s, max) {
  const text = String(s || '').replace(/\s+/g, ' ').trim();
  return text.length > max ? text.slice(0, max - 3) + '...' : text;
}

function joinNames(names, max = 3) {
  const shown = names.slice(0, max).join(', ');
  return names.length > max ? t('fv.andMore', { names: shown, count: names.length - max }) : shown;
}

/** Splits a tool result (JSON string or object, with or without the envelope). */
function unpack(result) {
  let parsed = result;
  if (typeof result === 'string') {
    try {
      parsed = JSON.parse(result);
    } catch {
      // Cut-off JSON says nothing; plain text is an error message.
      const text = result.trim();
      return { ok: null, code: '', error: text.startsWith('{') ? '' : text, d: {} };
    }
  }
  if (!parsed || typeof parsed !== 'object') return { ok: null, code: '', error: '', d: {} };
  const d = parsed.data && typeof parsed.data === 'object' && !Array.isArray(parsed.data) ? parsed.data : parsed;
  return {
    ok: typeof parsed.success === 'boolean' ? parsed.success : null,
    code: typeof parsed.code === 'string' ? parsed.code : '',
    error: typeof parsed.error === 'string' ? parsed.error : '',
    d,
  };
}

// Lines of a failure report written for the model, not the owner.
const MODEL_ADVICE = /^(Verdict:|A real user doing this journey)/;

/**
 * Finds the JSON object that starts at `from` (the failing step as the runner
 * prints it) and returns its end index, or -1. Quotes and escapes are honoured,
 * because steps hold scripts full of braces.
 */
function jsonEnd(text, from) {
  if (text[from] !== '{') return -1;
  let depth = 0;
  let inString = false;
  for (let i = from; i < text.length; i++) {
    const c = text[i];
    if (inString) {
      if (c === '\\') i++;
      else if (c === '"') inString = false;
    } else if (c === '"') inString = true;
    else if (c === '{') depth++;
    else if (c === '}' && --depth === 0) return i + 1;
  }
  return -1;
}

/** Shortens long quoted strings (scripts, selectors) so a reason fits on a line. */
function shortenQuoted(text) {
  return text.replace(/"((?:[^"\\]|\\.)*)"/g, (all, inner) => (inner.length > 40 ? `"${inner.slice(0, 37)}..."` : all));
}

/** The string values of a printed step, or [] when it does not parse. */
function stepValues(stepText) {
  try {
    return Object.values(JSON.parse(stepText)).filter((v) => typeof v === 'string');
  } catch {
    return [];
  }
}

/**
 * Splits one runner failure message into a one-line reason for the row and a
 * report for the "Full report" disclosure. The report holds only what the row
 * does not: the whole step, the whole failure, and the page state, JS errors and
 * console output that follow. It is '' when the row already says it all.
 */
function parseFailure(msg) {
  const head = FAILURE_HEAD.exec(msg);
  const lines = msg.split('\n');
  const afterHead = head ? lines[0].slice(head[0].length + 1) : lines[0];
  const end = jsonEnd(afterHead, 0);
  if (end < 0 || afterHead.slice(end, end + 2) !== ': ') {
    return { reason: clip(shortenQuoted(afterHead), 160), report: '' };
  }
  const stepText = afterHead.slice(0, end);
  const detail = afterHead.slice(end + 2);
  let step = stepText;
  try {
    step = JSON.stringify(JSON.parse(stepText), null, 2);
  } catch {
    // Keep the step as printed.
  }
  const extra = lines.slice(1).filter((l) => !MODEL_ADVICE.test(l)).join('\n').trim();
  const reason = clip(shortenQuoted(detail), 160);
  const rowSaysItAll = !extra && step.length <= 80 && detail.length <= 160;
  if (rowSaysItAll) return { reason, report: '' };
  // The failure often quotes the step's own script back; the step block above
  // already shows it in full, so the failure keeps just the rest.
  const stepStrings = stepValues(stepText).filter((v) => v.length > 40);
  const failure = detail.replace(/"((?:[^"\\]|\\.)*)"/g, (all, inner) => (
    stepStrings.some((v) => v.includes(inner) || inner.includes(v)) ? '"..."' : all));
  const parts = [`${t('fv.reportStep')}\n${step}`, `${t('fv.reportFailure')}\n${failure}`];
  if (extra) parts.push(extra);
  return { reason, report: parts.join('\n\n') };
}

/** Failure messages keyed by flow name, with the failing step when the text names it. */
function failureIndex(messages) {
  const byName = new Map();
  for (const msg of messages) {
    const m = FAILURE_HEAD.exec(msg);
    if (!m) continue;
    const { reason, report } = parseFailure(msg);
    byName.set(m[1], { step: Number(m[2]), of: Number(m[3]), line: reason, full: report });
  }
  return byName;
}

function buildRows(names, passed, failed, blocked, failures, ran) {
  const failedSet = new Set(failed);
  const passedSet = new Set(passed);
  const blockedNames = blocked.map((b) => b.replace(/\s*\(needs .*\)$/, ''));
  const blockedSet = new Set(blockedNames);
  const order = names.length ? names : [...passed, ...failed, ...blockedNames];
  return order.map((name) => {
    if (failedSet.has(name)) {
      const f = failures.get(name);
      return {
        name,
        status: 'failed',
        detail: f ? t('fv.stepOf', { step: f.step, of: f.of }) : t('fv.failedDetail'),
        reason: f ? f.line : '',
        full: f ? f.full : '',
      };
    }
    if (blockedSet.has(name)) return { name, status: 'blocked', detail: t('fv.waitingFlow'), reason: '', full: '' };
    if (passedSet.has(name)) return { name, status: 'passed', detail: t('fv.passedDetail'), reason: '', full: '' };
    return { name, status: 'pending', detail: ran ? '' : t('fv.notRun'), reason: '', full: '' };
  });
}

function view(state, tone, title, sub, extra = {}) {
  const badge = state === 'unknown' ? '' : t(`fv.badge.${state}`);
  return { state, tone, title, sub, badge, saved: null, rows: [], notes: [], expanded: false, ...extra };
}

/**
 * The card for one write_flows / test_flows call. `status` is the tool-call
 * status ('running' | 'success' | 'error'); history always replays 'success',
 * so the outcome is read from the result, never from the status.
 */
export function flowsView(action, args, result, status) {
  const writing = action === 'write_flows';
  if (status === 'running') {
    const title = writing
      ? t('fv.savingReplaying')
      : (args && args.flow ? t('fv.replayingOne', { flow: args.flow }) : t('fv.replayingAll'));
    const sub = t('fv.runsHeadless');
    return view('running', 'info', title, sub, { expanded: true });
  }

  const { ok, code, error, d } = unpack(result);
  const passed = list(d.passed);
  const failedNames = list(d.failed);
  const blocked = list(d.blocked);
  const scriptNames = list(d.flows);
  const messages = list(d.failures);

  // The live event may carry only the error text ("2 of 4 flows failed: a, b").
  let failed = failedNames;
  let total = passed.length + failed.length;
  const head = FAILED_HEAD.exec(error);
  if (!failed.length && head) {
    failed = head[3] ? head[3].split(/,\s*/) : [];
    total = Number(head[2]);
  }
  const failedCount = failed.length || (head ? Number(head[1]) : 0);

  const saved = writing && typeof d.saved === 'string'
    ? { flows: scriptNames.length || null, steps: typeof d.steps === 'number' ? d.steps : null }
    : null;

  const rows = buildRows(scriptNames, passed, failed, blocked, failureIndex(messages), true);
  const notes = [];
  const weak = list(d.weak_checks);
  if (weak.length) notes.push(tn('fv.weakSteps', weak.length));
  if (typeof d.rollback_error === 'string' && d.rollback_error) notes.push(clip(d.rollback_error, 200));
  const warnings = list(d.warnings);
  if (warnings.length) notes.push(tn('fv.scriptWarnings', warnings.length));
  if (typeof d.replays_left === 'number') notes.push(tn('fv.replaysLeft', d.replays_left));
  const extra = { saved, rows, notes };

  if (code === 'user_flows_off') {
    return view('off', 'neutral', t('fv.offTitle'), t('fv.offSub'), extra);
  }
  const refused = ok === false || (ok === null && error);
  if (refused && !failedCount && !passed.length && !d.saved && !d.skipped && !d.budget_exhausted && !d.stalled) {
    const reasonKey = code === 'diagnostic_suite' || code === 'flows_dropped' ? `fv.reason.${code}` : '';
    const sub = (reasonKey && t(reasonKey)) || clip(error.split('\n')[0], 160) || t('fv.refused');
    return view('rejected', 'warning', writing ? t('fv.notSaved') : t('fv.notReplayed'), sub, { ...extra, expanded: true });
  }
  if (d.budget_exhausted) {
    return view('limit', 'warning', t('fv.limitTitle'), t('fv.limitSub'), { ...extra, expanded: true });
  }
  if (d.stalled) {
    return view('stalled', 'warning', t('fv.stalledTitle'), t('fv.stalledSub'), { ...extra, expanded: true });
  }
  if (typeof d.skipped === 'string' && d.skipped) {
    const [title, sub] = skipCopy(d.skip_kind);
    return view('skipped', 'warning', title, sub, { ...extra, rows: rows.map((r) => ({ ...r, status: 'pending', detail: t('fv.notRun') })), expanded: true });
  }
  if (writing && typeof d.replay_skipped === 'string' && d.replay_skipped) {
    return view('skipped', 'warning', t('fv.savedNotReplayed'), t('fv.savedNotReplayedSub'), { ...extra, expanded: true });
  }
  if (failedCount > 0) {
    const all = total || failedCount + passed.length;
    const title = tn('fv.failedOf', all, { failed: failedCount });
    const names = joinNames(failed);
    const waiting = blocked.length || (head && head[4] ? Number(head[4]) : 0);
    const sub = [
      writing ? t('fv.savedDot') : '',
      names ? t('fv.failedNames', { names }) : t('fv.seeBelow'),
      waiting ? t('fv.waitingOnThem', { count: waiting }) : '',
    ].filter(Boolean).join(' ');
    return view('failed', 'danger', title, sub, { ...extra, expanded: true });
  }
  if (passed.length) {
    const title = passed.length === 1 ? t('fv.flowPassed') : tn('fv.allPassed', passed.length);
    return view('passed', 'success', title, writing ? t('fv.savedReplayed') : t('fv.replayedHeadless'), extra);
  }
  if (ok === true && saved) {
    return view('saved', 'neutral', t('fv.flowsSaved'), saved.flows ? tn('fv.flowsSavedSub', saved.flows) : '', extra);
  }
  return view('unknown', 'neutral', writing ? t('fv.flowTestsSaved') : t('fv.replayFinished'), '', extra);
}

/**
 * The go-live "Flow tests" block, from the flow_check payload the backend adds
 * to the completion message ({total, failed[], skipped, skip_kind}). Returns
 * null when there is nothing to say (no script, or tests turned off).
 */
export function goLiveFlowView(fc) {
  if (!fc || typeof fc !== 'object') return null;
  const failed = list(fc.failed);
  const total = typeof fc.total === 'number' ? fc.total : 0;
  if (fc.skipped) {
    const [title, sub] = skipCopy(fc.skip_kind);
    return { state: 'skipped', tone: 'warning', icon: 'clock', title, sub, failed: [] };
  }
  if (failed.length) {
    // Payloads from before the total was sent carry only the failed names.
    return {
      state: 'failed', tone: 'warning', icon: 'alert-triangle',
      title: total >= failed.length ? tn('fv.didNotPassOf', total, { failed: failed.length }) : tn('fv.didNotPass', failed.length),
      sub: t('fv.glFailedSub'),
      failed,
    };
  }
  if (total > 0) {
    return {
      state: 'passed', tone: 'success', icon: 'check',
      title: total === 1 ? t('fv.flowPassed') : tn('fv.allPassed', total),
      sub: t('fv.glPassedSub'),
      failed: [],
    };
  }
  return null;
}
