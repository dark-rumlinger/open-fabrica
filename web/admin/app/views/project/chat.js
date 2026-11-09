import { h, render } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { displayName } from '../../core/state.js';
import { createScope } from '../../core/lifecycle.js';
import { createWelcomeBack } from '../../ui/chat/message.js';
import { createFeed } from '../../ui/chat/feed.js';
import { createPipelineTracker } from '../../ui/chat/pipeline-tracker.js';
import { createChatInput } from '../../ui/chat/input.js';
import { errorState } from '../../ui/helpers.js';
import { createBrainTyping } from './chat/typing.js';
import { createQuestionManager } from './chat/questions.js';
import { createEmptyStateController } from './chat/empty-state.js';
import { createToolCardMap } from './chat/tool-cards.js';
import { createHistory } from './chat/history.js';
import { createSender } from './chat/send.js';
import { createStopper } from './chat/stop.js';
import { subscribeBrainEvents } from './chat/brain-events.js';
import { createHydration } from './chat/hydration.js';

const SCROLL_KEY_PREFIX = 'of:chat-scroll:';

function readScrollState(projectId) {
  try {
    const raw = sessionStorage.getItem(SCROLL_KEY_PREFIX + projectId);
    return raw ? JSON.parse(raw) : null;
  } catch { return null; }
}

function writeScrollState(projectId, value) {
  try {
    if (value == null) sessionStorage.removeItem(SCROLL_KEY_PREFIX + projectId);
    else sessionStorage.setItem(SCROLL_KEY_PREFIX + projectId, JSON.stringify(value));
  } catch {
    // sessionStorage may be disabled, fall back to default scroll-to-bottom.
  }
}

export function renderProjectChat(container, projectId) {
  const scope = createScope();
  const feed = createFeed();
  const tracker = createPipelineTracker();
  scope.add(() => tracker.cleanup());

  const typing = createBrainTyping(feed, scope);
  const empty = createEmptyStateController(feed);
  const questions = createQuestionManager(feed, scope, projectId);
  const toolCardMap = createToolCardMap(feed);
  const hydration = createHydration();

  const onActivity = () => empty.remove();

  // Send and stop share one button, so the three collaborators are mutually
  // recursive by design: the input calls the stopper, the stopper reads the
  // sender, the sender drives the input. Both callbacks run on click, long
  // after all three exist.
  const chatInput = createChatInput(
    (text) => sender.send(text),
    () => stopper.stop(),
    { draftKey: `of_draft:chat:${projectId}` },
  );
  const sender = createSender({ scope, projectId, feed, chatInput, toolCardMap, onActivity });
  const stopper = createStopper({ projectId, chatInput, sender, scope });
  stopper.seed();

  const history = createHistory({ feed, projectId, tracker, typing });

  render(container, h('div', { className: 'chat-container' }, [
    tracker.element,
    feed.element,
    questions.banner,
    chatInput.element,
  ]));

  // Subscribe to live events BEFORE history.load() resolves. Watchers buffer
  // their DOM mutations into the hydration controller until both history and
  // pending-questions have finished loading, at which point flush() drains
  // the buffer (skipping anything already rendered from the DB snapshot).
  const brain = subscribeBrainEvents({
    scope, projectId, feed, tracker, typing, questions, toolCardMap,
    onActivity, hydration, chatInput,
  });

  // Seed the build-queue baseline from the current queue. SSE select() doesn't
  // replay, so without this a mid-build reload could re-narrate tasks that
  // already finished. Silent seed: it sets the baseline without writing chat
  // messages (see brain.seedBuildQueue).
  (async () => {
    try {
      const queue = await get(`/admin/api/projects/${projectId}/build-queue`);
      brain.seedBuildQueue(queue);
    } catch {
      // Build-queue endpoint failures are non-fatal; narration just waits for SSE.
    }
  })();

  let historyError = null;
  const loadConversation = async () => {
    const r = await history.load();
    if (historyError) { historyError.remove(); historyError = null; }
    if (r.failed) {
      // A failed load is not an empty project: say so, with a way to retry,
      // instead of showing the first-run welcome over an existing conversation.
      historyError = errorState({
        title: t('chat.loadFailed'),
        message: t('chat.safe'),
        retry: loadConversation,
      });
      feed.append(historyError);
      return;
    }
    hydration.markFromHistory(r.keys);
    if (r.empty) {
      empty.show();
    } else {
      // Returning owner with existing history, greet them warmly so the feed
      // doesn't just dump them mid-conversation.
      feed.append(createWelcomeBack(displayName()));
    }
    try {
      const pending = await get(`/admin/api/projects/${projectId}/questions?status=pending`);
      if (questions.loadInitial(pending)) onActivity();
    } catch {
      // Pending-questions endpoint failures are non-fatal, chat still works.
    }
    hydration.flush();

    // Restore scroll position if the user was reading mid-history when they
    // navigated away; otherwise scroll to bottom (default for first visit /
    // user was already at the bottom).
    const saved = readScrollState(projectId);
    if (saved && !saved.atBottom) {
      requestAnimationFrame(() => { feed.element.scrollTop = saved.scrollTop; });
    } else {
      feed.scrollToBottom(true);
    }
  };
  loadConversation();
  chatInput.focus();

  scope.add(() => {
    const el = feed.element;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 100;
    writeScrollState(projectId, { scrollTop: el.scrollTop, atBottom });
  });

  return function cleanup() {
    scope.dispose();
  };
}
