import { createToolCall } from '../../../ui/chat/message.js';

/**
 * Shared tool-card lifecycle map. Used identically by user-initiated streams
 * (send.js) and live brain events (brain-events.js): start a tool card with
 * status "running", then promote to "success"/"error" when the result lands.
 */
export function createToolCardMap(feed) {
  const cards = {};

  function isQuestionTool(name, args) {
    return name === 'manage_chat' && args?.action === 'ask';
  }

  function start(data) {
    const toolName = data.tool || data.name || 'tool';
    const args = data.args || {};
    if (isQuestionTool(toolName, args)) return null;
    const key = data.call_id || data.id || data.name || toolName;
    const tc = createToolCall(toolName, 'running', null, args);
    cards[key] = tc;
    feed.append(tc.element);
    feed.scrollToBottom();
    return tc;
  }

  function finish(data) {
    const toolName = data.tool || data.name || 'tool';
    if (isQuestionTool(toolName, data.args)) return;
    const key = data.call_id || data.id || data.name || toolName;
    const tc = cards[key];
    if (!tc) return;
    tc.updateStatus(
      data.error ? 'error' : 'success',
      data.result || data.error,
    );
    feed.scrollToBottom();
  }

  return { start, finish, isQuestionTool };
}
