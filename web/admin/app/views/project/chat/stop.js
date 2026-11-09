import { t } from '../../../core/i18n.js';
import { post, get } from '../../../core/http.js';
import * as toast from '../../../ui/toast.js';

// How long the button may sit at "Stopping…" without hearing that the stop
// landed. NOT a deadline for the engine: a stop settles at a turn boundary and a
// BUILD turn can run for minutes, which is exactly what the hold is there to
// show. It covers the one case the engine cannot announce - a stop accepted just
// as the last session ended, leaving nothing to consume the request. A held
// button is disabled AND suppresses Enter, so a hold that never released would
// leave no way to send a message either; releasing re-arms a clickable Stop,
// which is honest (still running, ask again) and a repeat request is idempotent.
const STOP_HOLD_MS = 60000;

/**
 * The chat stop button's dispatcher. Two independent things can be running: a
 * chat turn (abort the request; cancels it server-side) and the build pipeline
 * (POST interrupt; the engine parks the run at the next turn boundary and
 * stays resumable). The chat turn wins when both are live: it is the thing
 * they started last.
 */
export function createStopper({ projectId, chatInput, sender, scope }) {
  // The "stopping" hold lives in chatInput and nowhere else. It is what the
  // button renders from, and the engine.stop_landed / project.phase_changed
  // watchers release it. A second copy here would never see those events, so the
  // re-entrancy guard below would latch on forever and kill the button.

  // Seed the button from authoritative engine state. The SSE stream only
  // carries transitions, so a view mounted mid-build would otherwise show Send
  // until the next event happened to arrive.
  async function seed() {
    try {
      const project = await get(`/admin/api/projects/${projectId}`);
      const phase = project?.phase;
      chatInput.setWorking('engine', phase?.key === 'building' && !phase?.stalled);
    } catch {
      // Status is a convenience seed; SSE still drives the button from here on.
    }
  }

  async function interrupt() {
    if (chatInput.isStopping()) return;
    chatInput.setStopping(true);
    // Guarded rather than cancelled, so releasing the hold needs no wiring back
    // into here: whoever released it already won, and this fires into a no-op.
    scope.timeout(() => {
      if (!chatInput.isStopping()) return;
      chatInput.setStopping(false);
      seed();
    }, STOP_HOLD_MS);

    try {
      await post(`/admin/api/engine/${projectId}/interrupt`, {});
    } catch {
      // 409 means nothing was running after all, so the button was stale.
      // Re-seed from the engine rather than guessing.
      chatInput.setStopping(false);
      await seed();
      toast.info(t('chat.nothingRunning'));
      return;
    }
    // The hold deliberately stays: the request only set a flag, and the engine
    // is still winding up the turn in flight. Clearing it here would put a live
    // Stop button back under the owner's cursor while tool calls keep streaming,
    // which reads as the stop having done nothing at all.
  }

  function stop() {
    if (sender.isSending()) {
      sender.abort();
      return;
    }
    interrupt();
  }

  return { stop, seed };
}
