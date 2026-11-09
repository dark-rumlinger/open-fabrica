import { t } from '../../../core/i18n.js';
import * as state from '../../../core/state.js';
import { isForProject } from '../../../core/utils.js';
import { formatDuration } from '../../../core/format.js';
import { createBrainMessage, createBuildPlanCard, createBuildProgressCard, createStageTransition, createMilestone } from '../../../ui/chat/message.js';

/**
 * Subscribes the chat view to all brain.* state events for this project.
 * All watchers are registered through `scope.add` so a single scope.dispose()
 * unwires everything on view teardown.
 */
export function subscribeBrainEvents({
  scope, projectId, feed, tracker, typing, questions, toolCardMap,
  onActivity, hydration, chatInput,
}) {
  // Keeps the send/stop button in step with what the engine is doing. Two
  // reasons rather than one: 'engine' spans a whole pipeline run and holds the
  // button through the gaps between LLM calls, while 'thinking' brackets a
  // single round-trip and is the only signal a LIVE chat-wake produces.
  const setWorking = (reason, active) => chatInput?.setWorking(reason, active);
  // `defer` wraps each watcher body. While hydration is mid-flight (history
  // fetch running) the work is buffered and replayed by hydration.flush()
  // so it can't race the snapshot render.
  const defer = hydration
    ? (fn) => (data) => hydration.bufferOrRun(() => fn(data))
    : (fn) => fn;

  const sel = (key, fn) => state.select(key, (data) => {
    if (!isForProject(data, projectId)) return;
    fn(data);
  }, scope);

  // ---- Build-queue narration -------------------------------------------------
  // The build queue (of_tasks) is reported inline in the chat instead of a
  // standalone panel: the full plan once, then a line per task as it completes
  // with the remaining backlog. brain.tasks fires on every transition with the
  // whole queue, so completions are detected by diffing the done set between
  // snapshots.
  let baselineSet = false;     // first snapshot establishes the baseline silently
  let buildPlanShown = false;  // the plan is announced at most once per view
  let knownDoneIds = new Set();

  const taskName = (t) => t.title || t.id || 'task';

  function announcePlanIfFresh(data, tasks) {
    if (buildPlanShown) return;
    // Only at the start of a build (nothing done yet) - a mid-build reload
    // shouldn't replay the plan.
    if ((data.total || 0) > 0 && (data.done || 0) === 0 && tasks.length) {
      feed.append(createBuildPlanCard({ tasks: tasks.map(taskName) }));
      feed.scrollToBottom();
      buildPlanShown = true;
    }
  }

  function announceCompletions(newlyDone, tasks) {
    const remaining = tasks.filter(t => t.status !== 'done');
    feed.append(createBuildProgressCard({
      completed: newlyDone.map(taskName),
      doneCount: tasks.filter(t => t.status === 'done').length,
      totalCount: tasks.length,
      remaining: remaining.map(taskName),
    }));
    feed.scrollToBottom();
  }

  // Apply a queue snapshot. `silent` (used for the on-mount hydration seed) sets
  // the baseline and the BUILD counter without writing any chat messages, so a
  // reload can't double-report tasks that finished before the view mounted.
  function applyTaskSnapshot(data, { silent = false } = {}) {
    if (!data) return;
    const tasks = Array.isArray(data.tasks) ? data.tasks : [];

    const doneIds = new Set(tasks.filter(t => t.status === 'done').map(t => t.id));

    if (!baselineSet) {
      baselineSet = true;
      knownDoneIds = doneIds;
      if (!silent) announcePlanIfFresh(data, tasks);
      return;
    }

    if (!silent) {
      announcePlanIfFresh(data, tasks);
      const newlyDone = tasks.filter(t => t.status === 'done' && !knownDoneIds.has(t.id));
      if (newlyDone.length) announceCompletions(newlyDone, tasks);
    }
    knownDoneIds = doneIds;
  }

  sel('brainThinking', defer((data) => {
    // brainThinking brackets a real LLM round-trip, so it is safe to show even
    // in LIVE: it only fires while a chat-wake is actively working, never while
    // an idle live project sits waiting. The dots bridge the gap before each
    // narration line lands so the feed never looks frozen. (The poke() re-show
    // timer stays suppressed in LIVE - see typing.js - this is the only live
    // typing cue, driven by explicit activity rather than a silence timer.)
    if (data.state === 'start') {
      typing.stop();
      feed.showBrainTyping();
      setWorking('thinking', true);
    } else if (data.state === 'end') {
      feed.hideBrainTyping();
      setWorking('thinking', false);
    }
  }));

  sel('brainTasks', defer((data) => {
    typing.poke();
    onActivity();
    applyTaskSnapshot(data);

    // Pin the current task title into the typing indicator so the "thinking"
    // line reflects the real work in flight. The loop drains serially, so this
    // is normally a single task; the >1 branch is a defensive fallback for a
    // stale snapshot that momentarily shows more than one active row.
    const active = Array.isArray(data.tasks) ? data.tasks.filter(t => t.status === 'active') : [];
    if (active.length === 1) {
      feed.setBrainTypingLabel(t('chat.buildingTask', { task: active[0].title || active[0].id }));
    } else if (active.length > 1) {
      feed.setBrainTypingLabel(t('chat.buildingParallel', { count: active.length }));
    } else {
      feed.setBrainTypingLabel('');
    }
  }));

  sel('brainMessage', defer((data) => {
    if (!data.content) return;
    typing.poke();
    const content = data.content;

    // Stage tracking, pass-progress chips, and BUILD completion are all driven
    // by the structured brainStageChange / projectPhaseChanged events below - the
    // engine never emits "Starting stage: **X**", "X pass N/M", "Build complete",
    // or "Validation passed" as message content, so no content scanning here.

    if (hydration?.isBrainMessageSeen(content)) return;
    hydration?.rememberBrainMessage(content);

    onActivity();
    feed.append(createBrainMessage(content));
    feed.scrollToBottom();
  }));

  sel('brainStageChange', defer((data) => {
    const { prev_stage, stage, duration_ms } = data;
    typing.setLiveStage(stage === 'LIVE');
    if (!typing.inLiveStage) typing.poke();

    // A stage change only happens inside a running pipeline, so it is the
    // cheapest proof one is in flight. LIVE is the terminal stage: reaching it
    // means the pipeline is done, not that another stage started.
    setWorking('engine', !!stage && stage !== 'LIVE');

    if (prev_stage) tracker.markCompleted(prev_stage);
    if (stage) tracker.setStage(stage);

    if (prev_stage) {
      if (hydration?.isStageSeen(prev_stage)) return;
      hydration?.rememberStage(prev_stage);
      const dur = duration_ms ? formatDuration(duration_ms) : '';
      feed.append(createStageTransition(prev_stage, dur));
      feed.scrollToBottom();
    }
  }));

  sel('brainToolStart', defer((data) => {
    typing.poke();
    onActivity();
    const id = data.call_id || data.id;
    if (hydration?.isToolCallSeen(id)) return;
    hydration?.rememberToolCall(id);
    toolCardMap.start(data);
  }));

  sel('brainToolResult', defer((data) => {
    typing.poke();
    // toolCardMap.finish() is a no-op when the call_id is unknown, which is
    // exactly what we want for tools that completed before this view mounted
    // (their result is already in history). No extra dedup needed here.
    toolCardMap.finish(data);
  }));

  sel('questionAsked', defer((data) => {
    if (!data.question) return;
    onActivity();
    questions.add(data);
    feed.scrollToBottom();
  }));

  sel('brainStopLanded', () => {
    // The stop really settled, so release the "Stopping…" hold. Undeferred like
    // projectPhaseChanged: this drives the button, not the feed. It is the ONLY
    // release for a live or scheduled stop, neither of which changes the phase.
    chatInput?.setStopping(false);
  });

  // The pipeline is in flight only while the phase is building. Needs you, live
  // and paused all end it, and the phase is what tells "building" from "parked on
  // the owner", which share a stored mode. This is also what clears the button
  // after a stop: the interrupt parks the run and the phase follows.
  let prevPhaseKey = null;
  sel('projectPhaseChanged', (data) => {
    const phase = data.phase;
    if (!phase) return;
    const wasBuilding = prevPhaseKey === 'building' || prevPhaseKey === 'needs_you'
      || (prevPhaseKey === null && !!tracker.currentStageKey && tracker.currentStageKey !== 'LIVE');
    prevPhaseKey = phase.key;

    const inFlight = phase.key === 'building' && !phase.stalled;
    setWorking('engine', inFlight);
    if (!inFlight) {
      setWorking('thinking', false);
      chatInput?.setStopping(false);
    }

    if (phase.key === 'live') {
      typing.setPaused(false);
      typing.setLiveStage(true);
      tracker.markCompleted('PLAN');
      tracker.markCompleted('BUILD');
      tracker.setStage('LIVE');
      // Celebrate the moment the project goes live, not every time a worker
      // starts on an already-live project. The line is picked from a small pool
      // so the banner doesn't read identically every build.
      if (wasBuilding) {
        onActivity();
        const liveLines = [t('chat.live1'), t('chat.live2'), t('chat.live3')];
        feed.append(createMilestone(liveLines[Math.floor(Math.random() * liveLines.length)]));
        feed.scrollToBottom();
      }
    } else if (phase.key === 'paused') {
      // Pausing stops the pipeline: kill the typing indicator and suppress its
      // re-show timer so the feed doesn't keep cycling "thinking" lines.
      typing.setPaused(true);
      if (tracker.currentStageKey) tracker.setError(tracker.currentStageKey);
    } else if (phase.key === 'needs_you') {
      // Parked on an answer: nothing is thinking, so the feed goes quiet too.
      typing.setPaused(true);
    } else {
      // Building again, from a pause or from an answer: let the indicator
      // track activity again.
      typing.setPaused(false);
    }
  });

  state.select('questionAnswered', (data) => {
    if (!data) return;
    questions.remove(data.question_id);
  }, scope);

  return {
    // Seed the build-queue baseline from the on-mount snapshot. Silent so a
    // mid-build reload doesn't re-narrate tasks that finished before mount.
    // Skipped once a live brain.tasks event has already established the baseline.
    seedBuildQueue(data) {
      if (!baselineSet) {
        applyTaskSnapshot(data, { silent: true });
        // The silent seed swallows the genuine BUILD-start snapshot (done=0),
        // so without this the "Build plan queued" card waits for the next live
        // manage_tasks publish and lands minutes into the build. Render it here
        // when the seed is genuinely fresh; announcePlanIfFresh self-guards on
        // done=0 && total>0, so a mid-build reload (done>0) won't replay it.
        announcePlanIfFresh(data, Array.isArray(data?.tasks) ? data.tasks : []);
      }
    },
  };
}
