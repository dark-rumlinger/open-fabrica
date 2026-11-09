import { createGameHost } from '../../../ui/chat/games/host.js';

/**
 * Engine typing indicator manager. Hides the indicator the instant any engine
 * event arrives (poke); re-shows it after 1.5s of silence so the chat doesn't
 * look frozen during long LLM round-trips. Suppressed entirely once the
 * pipeline reaches LIVE.
 *
 * Also drives the game host (see games/host.js): every poke counts as the engine
 * being alive, and stop()/LIVE count as idle, so a long-running action can offer
 * a game while a normal turn never does.
 */
export function createBrainTyping(feed, scope) {
  let timer = null;
  let inLiveStage = false;
  let paused = false;
  const games = createGameHost(feed, scope);

  function poke() {
    feed.hideBrainTyping();
    if (timer) clearTimeout(timer);
    if (inLiveStage || paused) return;
    games.onActivity();
    timer = setTimeout(() => feed.showBrainTyping(), 1500);
  }

  function stop() {
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
    feed.hideBrainTyping();
  }

  function setLiveStage(v) {
    inLiveStage = v;
    // Only a genuine go-live ends the "working" span for the game host. stop()
    // is also called mid-build (e.g. at the start of each thinking round), so
    // it must NOT reset the boredom timer or the threshold never accrues.
    if (v) { games.onIdle(); stop(); }
  }

  // A paused pipeline isn't working, so the indicator must stay hidden and the
  // re-show timer suppressed until it resumes. Mirrors setLiveStage but is
  // driven by the explicit paused/resumed mode rather than reaching LIVE.
  function setPaused(v) {
    paused = v;
    if (v) { games.onIdle(); stop(); }
  }

  if (scope) scope.add(stop);

  return {
    poke,
    stop,
    setLiveStage,
    setPaused,
    get inLiveStage() { return inLiveStage; },
  };
}
