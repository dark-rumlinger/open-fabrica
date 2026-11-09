/**
 * Synchronous chat send. POSTs the user message and resolves with the full
 * assistant reply once the backend tool loop completes. No streaming.
 */

import { rawFetch } from '../../core/http.js';
import { humanizeError } from '../errors.js';

/**
 * POST a chat message and wait for the full response.
 * @param {string} url
 * @param {string} body - JSON body to POST
 * @param {Object} callbacks - { onResult({text, tools}), onError(msg) }
 * @returns {{ abort: Function }}
 */
export function startStream(url, body, callbacks = {}) {
  const controller = new AbortController();

  rawFetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body,
    signal: controller.signal,
  }).then(async (response) => {
    const text = await response.text();
    let result;
    try {
      result = JSON.parse(text);
    } catch {
      if (callbacks.onError) callbacks.onError('invalid JSON response');
      return;
    }
    if (callbacks.onResult) callbacks.onResult(result || { text: '', tools: [] });
  }).catch((err) => {
    if (err.name === 'AbortError') return;
    // rawFetch throws with the server's { error } already extracted; humanize it
    // here so the chat surfaces the same friendly copy as every other call.
    if (callbacks.onError) callbacks.onError(humanizeError(err.message));
  });

  return {
    abort() {
      controller.abort();
    },
  };
}
