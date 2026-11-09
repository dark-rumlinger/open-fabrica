/**
 * Engine SSE -> state fanout: maps `engine.*` and related per-project events
 * into the state keys views consume via state.watch / state.select. Also
 * tracks the last seen event_id so the SSE client can backfill on reconnect.
 */

import { t } from './i18n.js';
import * as state from './state.js';
import { get } from './http.js';
import * as toast from '../ui/toast.js';

let lastEventID = 0;

function track(data) {
  if (typeof data?.event_id === 'number' && data.event_id > lastEventID) {
    lastEventID = data.event_id;
  }
}

function trackProjectActivity(projectId) {
  if (!projectId) return;
  const activity = { ...(state.get('projectActivity') || {}) };
  activity[projectId] = Date.now();
  state.set('projectActivity', activity);
}

// ---------------------------------------------------------------------------
// Event dispatchers, each takes the raw event envelope `{type, project_id,
// payload, event_id, timestamp}` and updates state. Shared by both the live
// SSE fanout and the backfill replay path.
// ---------------------------------------------------------------------------

const ENGINE_MAP = [
  ['engine.message',         'brainMessage',         true],
  ['engine.tool_start',      'brainToolStart',       true],
  ['engine.tool_result',     'brainToolResult',      false],
  ['engine.stage_change',    'brainStageChange',     true],
  ['engine.thinking',        'brainThinking',        false],
  ['engine.tasks',           'brainTasks',           true],
  ['engine.stop_landed',     'brainStopLanded',      false],
];

const dispatchers = {};
for (const [evt, key, tracksActivity] of ENGINE_MAP) {
  dispatchers[evt] = (data) => {
    state.set(key, { project_id: data.project_id, ...(data.payload || {}) });
    if (tracksActivity) trackProjectActivity(data.project_id);
  };
}

dispatchers['project.phase_changed'] = (data) => {
  if (data?.payload?.phase) {
    state.set('projectPhaseChanged', {
      project_id: data.project_id,
      mode: data.payload.mode,
      phase: data.payload.phase,
    });
  }
};

dispatchers['tool.executed'] = (data) => {
  if (data?.payload) {
    state.set('toolExecuted', {
      project_id: data.project_id,
      tool: data.payload.tool,
      success: data.payload.success,
      timestamp: Date.now(),
    });
  }
};

dispatchers['page.saved'] = (data) => {
  state.set('toolExecuted', {
    project_id: data.project_id,
    tool: 'save_page',
    success: true,
    timestamp: Date.now(),
  });
};

function dispatch(data) {
  track(data);
  const fn = dispatchers[data?.type];
  if (fn) fn(data);
}

// ---------------------------------------------------------------------------
// SSE wiring + reconnect-time backfill.
// ---------------------------------------------------------------------------

export function wireBrainBus(sse) {
  for (const evt of Object.keys(dispatchers)) {
    if (evt === 'tool.executed' || evt === 'page.saved') continue;
    sse.on(evt, (data) => {
      // Server sends events with .type missing on some payloads; backfill the
      // type so the shared dispatch path works.
      if (!data.type) data.type = evt;
      dispatch(data);
    });
  }
}

export function wireToolEvents(sse) {
  for (const evt of ['tool.executed', 'page.saved']) {
    sse.on(evt, (data) => {
      if (!data.type) data.type = evt;
      dispatch(data);
    });
  }
}

/**
 * Fetch buffered events with id > lastEventID and replay them through the
 * same dispatchers as live events. Optional `projectId` filters the backfill
 * server-side so unrelated projects' events are not replayed through state.
 */
async function backfillSinceLastEvent(projectId) {
  if (lastEventID === 0) return; // no live events yet, nothing to backfill against
  const since = lastEventID;
  let url = `/admin/api/events/backfill?since=${since}`;
  if (projectId != null && projectId !== '') {
    url += `&project_id=${encodeURIComponent(projectId)}`;
  }
  let resp;
  try {
    resp = await get(url);
  } catch (err) {
    console.warn('[engine-bus] backfill failed', err);
    return;
  }
  const events = resp?.events || [];
  if (resp?.missed) {
    toast.warning(t('chat.missedUpdates'));
  }
  for (const e of events) dispatch(e);
}

/**
 * Run a backfill whenever an SSE reconnect succeeds. Call after wireBrainBus +
 * wireToolEvents. `getProjectId` is a getter (not a fixed value) because the
 * active project changes over the SPA lifetime.
 */
export function wireReconnectBackfill(sse, getProjectId) {
  let wasDisconnected = false;
  sse.onConnectionState((stateName) => {
    if (stateName === 'reconnecting' || stateName === 'gave_up') {
      wasDisconnected = true;
    } else if (stateName === 'open' && wasDisconnected) {
      wasDisconnected = false;
      const pid = typeof getProjectId === 'function' ? getProjectId() : undefined;
      backfillSinceLastEvent(pid);
    }
  });
}
