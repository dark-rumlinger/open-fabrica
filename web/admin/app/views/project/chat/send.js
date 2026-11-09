import { createUserMessage, createStreamingMessage } from '../../../ui/chat/message.js';
import { startStream } from '../../../ui/chat/stream.js';
import { humanizeError } from '../../../ui/errors.js';
import { t } from '../../../core/i18n.js';

/**
 * User-initiated chat send. Owns the streaming message and the SSE stream
 * controller; both are torn down via the lifecycle scope.
 */
export function createSender({ scope, projectId, feed, chatInput, toolCardMap, onActivity }) {
  let currentStream = null;
  let streamingMsg = null;

  // Settle the in-flight turn's UI. Called from every exit (result, error,
  // abort) so the button can never be left showing stop with nothing running.
  function clearInFlight() {
    currentStream = null;
    chatInput.setWorking('chat', false);
  }

  function abort() {
    if (!currentStream) return;
    try { currentStream.abort(); } catch {}
    // Aborting cancels the request context server-side, which ends the chat
    // tool loop there too. The reply is gone with it, so close the placeholder
    // rather than leaving it spinning.
    if (streamingMsg) {
      const note = t('chat.stopped');
      streamingMsg.appendText(`\n\n*${note}*`);
      streamingMsg.finish();
      streamingMsg = null;
    }
    clearInFlight();
  }
  scope.add(abort);

  function send(text) {
    onActivity();
    feed.append(createUserMessage(text));
    chatInput.setWorking('chat', true);

    streamingMsg = createStreamingMessage();
    feed.append(streamingMsg.element);

    currentStream = startStream(
      `/admin/api/chat/${projectId}/stream`,
      JSON.stringify({ message: text, session_id: 'admin' }),
      {
        onResult(result) {
          for (const tool of result.tools || []) {
            toolCardMap.start({ id: tool.id, name: tool.name });
            toolCardMap.finish({
              id: tool.id,
              name: tool.name,
              result: tool.result,
              is_error: tool.is_error,
            });
          }
          if (result.text) {
            streamingMsg?.appendText(result.text);
          }
          streamingMsg?.finish();
          streamingMsg = null;
          feed.scrollToBottom();
          clearInFlight();
          chatInput.focus();
        },
        onError(err) {
          if (streamingMsg) {
            const note = t('chat.sendFailed', { reason: humanizeError(String(err)) });
            streamingMsg.appendText(`\n\n*${note}*`);
            streamingMsg.finish();
            streamingMsg = null;
          }
          // The text was cleared on send; hand it back so a failure never costs
          // the owner what they typed.
          chatInput.restore(text);
          clearInFlight();
          chatInput.focus();
        },
      },
    );
  }

  return {
    send,
    abort,
    // Whether a chat turn is in flight. The stop button prefers aborting this
    // over interrupting the pipeline: it is the thing the owner just started.
    isSending() { return currentStream !== null; },
  };
}
