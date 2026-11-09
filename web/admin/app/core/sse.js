/**
 * SSE client with auto-reconnect, exponential backoff, and connection-state
 * callbacks. Caps automatic reconnects after MAX_AUTO_RECONNECTS attempts;
 * subsequent retries require an explicit retry() call (typically wired to a
 * "disconnected" banner button).
 */

const MAX_AUTO_RECONNECTS = 6;

export class SSEClient {
  constructor() {
    this._source = null;
    this._url = null;
    this._handlers = {};
    this._attached = new Set(); // event types with a listener on the CURRENT source
    this._connectionListeners = [];
    this._reconnectAttempts = 0;
    this._maxReconnectDelay = 30000;
    this._baseDelay = 1000;
    this._reconnectTimer = null;
    this._closed = false;
    this._connected = false;
    this._gaveUp = false;
    this._wake = () => this._onWake();
  }

  connect(url) {
    this._url = url;
    this._closed = false;
    this._gaveUp = false;
    this._reconnectAttempts = 0;
    // A laptop waking up or a network switch is exactly when the backoff above
    // has run out; retry on those signals instead of waiting for a click.
    window.addEventListener('online', this._wake);
    document.addEventListener('visibilitychange', this._wake);
    this._doConnect();
  }

  _onWake() {
    if (this._closed || this._connected) return;
    if (document.visibilityState === 'hidden') return;
    this.retry();
  }

  retry() {
    if (this._closed) return;
    this._gaveUp = false;
    this._reconnectAttempts = 0;
    if (this._source) this._source.close();
    this._doConnect();
  }

  _doConnect() {
    if (this._closed) return;
    if (this._reconnectTimer) {
      clearTimeout(this._reconnectTimer);
      this._reconnectTimer = null;
    }

    this._source = new EventSource(this._url, { withCredentials: true });
    this._emitState('connecting');

    this._source.onopen = () => {
      this._reconnectAttempts = 0;
      this._connected = true;
      this._emitState('open');
    };

    this._source.onerror = () => {
      this._source.close();
      this._connected = false;
      if (this._reconnectAttempts >= MAX_AUTO_RECONNECTS) {
        this._gaveUp = true;
        this._emitState('gave_up');
        return;
      }
      // Schedule first so the attempt count the banner shows is already 1+.
      this._scheduleReconnect();
      this._emitState('reconnecting');
    };

    this._attached = new Set();
    for (const eventType of Object.keys(this._handlers)) {
      this._attachType(eventType);
    }

    this._source.onmessage = (e) => this._dispatch('message', e);
  }

  // Attach ONE listener for a named event type on the current source. It reads
  // the handler list at fire time, so on() calls made after connect still take
  // effect (a listener bound per-callback at connect time silently dropped
  // late subscriptions until the next reconnect).
  _attachType(eventType) {
    if (!this._source || eventType === 'message' || this._attached.has(eventType)) return;
    this._attached.add(eventType);
    this._source.addEventListener(eventType, (e) => this._dispatch(eventType, e));
  }

  _dispatch(eventType, e) {
    let data;
    try { data = JSON.parse(e.data); } catch { data = e.data; }
    for (const cb of (this._handlers[eventType] || [])) {
      try { cb(data); } catch (err) { console.error('[sse] handler threw', eventType, err); }
    }
  }

  _scheduleReconnect() {
    if (this._closed) return;
    const delay = Math.min(
      this._baseDelay * Math.pow(2, this._reconnectAttempts),
      this._maxReconnectDelay,
    );
    this._reconnectAttempts++;
    this._reconnectTimer = setTimeout(() => this._doConnect(), delay);
  }

  on(eventType, callback) {
    if (!this._handlers[eventType]) this._handlers[eventType] = [];
    if (this._handlers[eventType].includes(callback)) return;
    this._handlers[eventType].push(callback);
    this._attachType(eventType); // no-op before connect; binds live otherwise
  }

  /**
   * Subscribe to connection-state changes.
   * State is one of: 'connecting' | 'open' | 'reconnecting' | 'gave_up' | 'closed'.
   */
  onConnectionState(callback) {
    this._connectionListeners.push(callback);
    return () => {
      const i = this._connectionListeners.indexOf(callback);
      if (i !== -1) this._connectionListeners.splice(i, 1);
    };
  }

  _emitState(state) {
    for (const cb of this._connectionListeners) {
      try { cb(state, this._reconnectAttempts); } catch (e) { console.error('[sse] state listener threw', e); }
    }
  }

  get connected() { return this._connected; }
  get gaveUp() { return this._gaveUp; }

  disconnect() {
    this._closed = true;
    window.removeEventListener('online', this._wake);
    document.removeEventListener('visibilitychange', this._wake);
    if (this._reconnectTimer) {
      clearTimeout(this._reconnectTimer);
      this._reconnectTimer = null;
    }
    if (this._source) {
      this._source.close();
      this._source = null;
    }
    this._handlers = {};
    this._attached = new Set();
    // Emit 'closed' while the state listeners are still registered, then drop
    // them; clearing first meant the final state change never reached anyone.
    this._emitState('closed');
    this._connectionListeners = [];
  }
}
