import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { get } from '../../../core/http.js';
import { latest } from '../../../core/http.js';
import { formatDuration } from '../../../core/format.js';
import {
  createUserMessage, createAutomatedMessage, createAssistantMessage,
  createBrainMessage, createToolCall, createStageTransition,
} from '../../../ui/chat/message.js';

// EXPAND leads the order so the "infer next stage" math below ranks it
// correctly. It only ever appears in `completed` for opt-in projects; for
// everyone else it's simply absent and the order behaves as PLAN-first.
const STAGE_ORDER = ['EXPAND', 'PLAN', 'BUILD', 'LIVE'];

function renderMessages(feed, messages, prepend = false, keys = null) {
  const toolResults = {};
  for (const msg of messages) {
    if (msg.role === 'tool' && msg.tool_call_id) {
      toolResults[msg.tool_call_id] = msg.content;
    }
  }

  const elements = [];
  for (const msg of messages) {
    if (msg.role === 'user') {
      elements.push(msg.session_id === 'brain'
        ? createAutomatedMessage(msg.content, msg.created_at)
        : createUserMessage(msg.content, msg.created_at));
      if (keys && msg.content) keys.brainMessages.add(msg.content);
    } else if (msg.role === 'assistant') {
      const isBrain = msg.session_id === 'brain';
      if (msg.content) {
        elements.push(isBrain
          ? createBrainMessage(msg.content, msg.created_at)
          : createAssistantMessage(msg.content, msg.created_at));
        if (keys && isBrain) keys.brainMessages.add(msg.content);
      }
      if (msg.tool_calls?.length) {
        for (const tc of msg.tool_calls) {
          const name = tc.name || tc.function?.name || 'tool';
          let args = {};
          try {
            if (tc.arguments) args = typeof tc.arguments === 'string' ? JSON.parse(tc.arguments) : tc.arguments;
          } catch {
            // Malformed tool args from a historic message; render with empty args.
          }
          if (name === 'manage_chat' && args.action === 'ask') continue;
          const result = toolResults[tc.id];
          const toolEl = createToolCall(name, 'success', result, args);
          elements.push(toolEl.element);
          if (keys && tc.id) keys.toolCallIds.add(tc.id);
        }
      }
    }
  }

  if (prepend) {
    feed.element.querySelector('.load-earlier-btn')?.remove();
    const firstChild = feed.element.firstChild;
    for (const el of elements) feed.element.insertBefore(el, firstChild);
  } else {
    for (const el of elements) feed.append(el);
  }
}

function renderTimeline(feed, messages, stages, keys) {
  const entries = [];
  for (const m of messages) entries.push({ ts: m.created_at, kind: 'message', data: m });
  for (const s of stages) entries.push({ ts: s.ended_at || s.started_at, kind: 'stage', data: s });
  entries.sort((a, b) => (a.ts < b.ts ? -1 : a.ts > b.ts ? 1 : 0));

  let batch = [];
  const flush = () => {
    if (batch.length) {
      renderMessages(feed, batch, false, keys);
      batch = [];
    }
  };
  for (const e of entries) {
    if (e.kind === 'message') {
      batch.push(e.data);
    } else {
      flush();
      const dur = e.data.duration_ms ? formatDuration(e.data.duration_ms) : '';
      feed.append(createStageTransition(e.data.stage, dur));
      if (keys) keys.completedStages.add(e.data.stage);
    }
  }
  flush();
}

/**
 * loadHistory + load-earlier pagination, with sequence guard.
 */
export function createHistory({ feed, projectId, tracker, typing }) {
  // Cursor is the row id of the oldest message we hold, not its created_at.
  // created_at is second-granularity, so paginating on it drops same-second
  // messages straddling a page boundary (see LoadMergedHistory in history.go).
  let oldestId = null;
  let hasOlder = true;
  let loadingEarlier = false;

  const guardedEarlier = latest(async (id) =>
    get(`/admin/api/chat/${projectId}/history?before=${encodeURIComponent(id)}`));

  async function load() {
    const keys = {
      brainMessages: new Set(),
      toolCallIds: new Set(),
      completedStages: new Set(),
    };
    let payload;
    try {
      payload = await get(`/admin/api/chat/${projectId}/history`);
    } catch {
      return { empty: true, failed: true, keys };
    }

    const messages = Array.isArray(payload) ? payload : (payload?.messages || []);
    const stages = Array.isArray(payload) ? [] : (payload?.stages || []);
    const pipeline = Array.isArray(payload) ? null : payload?.pipeline;

    if (messages.length === 0 && stages.length === 0) {
      hasOlder = false;
      return { empty: true, failed: false, keys };
    }

    if (messages.length > 0) {
      // Backend orders ascending, so messages[0] is the oldest in this batch.
      // Reordering the backend response would break this paginator.
      oldestId = messages[0].id;
      // Page size lives in chat/session.go (historyLimit). When a session has
      // exactly N*50 messages this triggers one dead-click on "Load earlier"
      // whose response is empty and then hides the button - acceptable.
      hasOlder = messages.length >= 50;
    } else {
      hasOlder = false;
    }

    // The `stages` array is windowed to the visible message page (so old chat
    // dividers don't pile at the top), but the tracker needs every completed
    // stage or an optional leading stage like EXPAND vanishes from the tracker
    // on reload. pipeline.completed_stages carries the full, unwindowed set.
    const completed = new Set(stages.filter(s => s.status === 'completed').map(s => s.stage));
    if (pipeline?.completed_stages) for (const s of pipeline.completed_stages) completed.add(s);
    for (const s of completed) tracker.markCompleted(s);
    if (pipeline?.stage) {
      tracker.setStage(pipeline.stage);
      if (pipeline.paused) tracker.setError(pipeline.stage);
      typing.setPaused(!!pipeline.paused);
      typing.setLiveStage(pipeline.stage === 'LIVE');
    } else if (stages.length > 0) {
      const lastIdx = Math.max(...[...completed].map(s => STAGE_ORDER.indexOf(s)));
      const next = STAGE_ORDER[lastIdx + 1];
      if (next) tracker.setStage(next);
    }

    renderTimeline(feed, messages, stages, keys);
    if (hasOlder) addLoadEarlierButton();
    return { empty: false, failed: false, keys };
  }

  function addLoadEarlierButton() {
    const btn = h('div', { className: 'load-earlier-btn' }, [
      h('button', {
        className: 'btn btn--ghost btn--sm',
        onClick: loadEarlier,
      }, t('chat.loadEarlier')),
    ]);
    // If the empty-state card is still mounted (race during initial render)
    // its presence as firstChild would push the button to an odd position.
    feed.element.querySelector('.chat-empty')?.remove();
    feed.element.insertBefore(btn, feed.element.firstChild);
  }

  async function loadEarlier() {
    if (!oldestId || !hasOlder || loadingEarlier) return;
    loadingEarlier = true;
    const btnEl = feed.element.querySelector('.load-earlier-btn button');
    if (btnEl) {
      btnEl.textContent = t('common.loading');
      btnEl.disabled = true;
    }
    try {
      const r = await guardedEarlier(oldestId);
      if (r.stale) return;
      const messages = r.data;
      if (!messages || messages.length === 0) {
        hasOlder = false;
        feed.element.querySelector('.load-earlier-btn')?.remove();
        return;
      }
      oldestId = messages[0].id;
      hasOlder = messages.length >= 50;
      const prevHeight = feed.element.scrollHeight;
      renderMessages(feed, messages, true);
      feed.element.scrollTop += feed.element.scrollHeight - prevHeight;
      if (hasOlder) addLoadEarlierButton();
    } catch {
      if (btnEl) {
        btnEl.textContent = t('chat.loadEarlier');
        btnEl.disabled = false;
      }
    } finally {
      loadingEarlier = false;
    }
  }

  return { load, loadEarlier };
}
