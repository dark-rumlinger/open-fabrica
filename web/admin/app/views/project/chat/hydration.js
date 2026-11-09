/**
 * Hydration controller for the chat view. The DB snapshot (history.load()) and
 * the live SSE stream race on every page-revisit: an event arriving between
 * subscribeBrainEvents() and the history fetch resolving would render twice.
 * So all live-watcher DOM mutations are buffered until history is fully
 * drained, then flushed while filtering out anything already rendered from the
 * snapshot.
 *
 * Dedup keys: brainMessages (raw text of session_id='brain' messages, which
 * uniquely identifies them within a build run), toolCallIds (stable across SSE
 * `call_id`/`id` and history `tc.id`), completedStages (stage names that
 * already have a transition card from history; live `brain.stage_change`
 * carries the just-completed stage as `prev_stage`).
 */
export function createHydration() {
  let ready = false;
  const buffered = [];
  const keys = {
    brainMessages: new Set(),
    toolCallIds: new Set(),
    completedStages: new Set(),
  };

  function bufferOrRun(fn) {
    if (ready) fn();
    else buffered.push(fn);
  }

  function markFromHistory(historyKeys) {
    if (!historyKeys) return;
    if (historyKeys.brainMessages) {
      for (const k of historyKeys.brainMessages) keys.brainMessages.add(k);
    }
    if (historyKeys.toolCallIds) {
      for (const k of historyKeys.toolCallIds) keys.toolCallIds.add(k);
    }
    if (historyKeys.completedStages) {
      for (const k of historyKeys.completedStages) keys.completedStages.add(k);
    }
  }

  function flush() {
    ready = true;
    const pending = buffered.splice(0);
    for (const fn of pending) {
      try { fn(); } catch (err) { console.warn('[hydration] buffered handler threw', err); }
    }
  }

  return {
    bufferOrRun,
    markFromHistory,
    flush,
    isReady: () => ready,
    isBrainMessageSeen: (content) => keys.brainMessages.has(content),
    isToolCallSeen: (id) => !!id && keys.toolCallIds.has(id),
    isStageSeen: (stage) => keys.completedStages.has(stage),
    // After an engine message is rendered live, remember its content so a
    // subsequent buffered duplicate (or a stray repeat) is skipped.
    rememberBrainMessage(content) { if (content) keys.brainMessages.add(content); },
    rememberToolCall(id) { if (id) keys.toolCallIds.add(id); },
    rememberStage(stage) { if (stage) keys.completedStages.add(stage); },
  };
}
