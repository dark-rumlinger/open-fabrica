/* Open Fabrica end-to-end encryption helper.
   PLATFORM-MANAGED: the platform owns this file and your pages load it at
   runtime - do not edit, recreate, or delete it. Import it by specifier:
     import { session } from 'e2ee';
   All crypto runs in the browser via Web Crypto (SubtleCrypto); the server relay
   only ever sees the opaque {iv,ct} envelope. Never hand-roll crypto and never
   send a plaintext message body on an e2ee endpoint. See the e2ee guide. */
const OFE2EE = (function () {
  'use strict';
  var enc = new TextEncoder();
  var dec = new TextDecoder();

  function b64(bytes) {
    var b = new Uint8Array(bytes), s = '';
    for (var i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
    return btoa(s);
  }
  function unb64(str) {
    var bin = atob(str), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }
  function cmpBytes(a, b) {
    var n = Math.min(a.length, b.length);
    for (var i = 0; i < n; i++) { if (a[i] !== b[i]) return a[i] - b[i]; }
    return a.length - b.length;
  }
  function concat(parts) {
    var total = 0, i;
    for (i = 0; i < parts.length; i++) total += parts[i].length;
    var out = new Uint8Array(total), off = 0;
    for (i = 0; i < parts.length; i++) { out.set(parts[i], off); off += parts[i].length; }
    return out;
  }
  function extractFingerprint(sdp) {
    if (!sdp) return '';
    var m = String(sdp).match(/a=fingerprint:\S+\s+([0-9A-Fa-f:]+)/);
    return m ? m[1].toUpperCase() : '';
  }

  // The relay drops any frame carrying a plaintext message body
  // (text/content/body/message/msg/plaintext/caption/note) - even one that ALSO
  // includes an {iv,ct} envelope, so dummy ciphertext cannot smuggle a cleartext body past the
  // guard. Control frames (e2ee-pub, e2ee-gkey) and WebRTC signaling
  // (hello/offer/answer/ice/bye) carry no such field and pass in the clear by
  // design. Put secret words through encrypt(); everything else may travel openly.

  // ----- Device identity & safety numbers -----
  // A long-lived ECDSA P-256 identity key, stored per-device (IndexedDB,
  // non-extractable private key), authenticates each session's EPHEMERAL ECDH key:
  // start() signs the ephemeral public key with the identity key and announces both
  // in the e2ee-pub frame; a peer verifies that signature before trusting the
  // ephemeral key (a forged key with no valid signature is rejected). The identity
  // public key is STABLE across sessions, so a contact is verified ONCE
  // (safetyNumber) and the helper warns if it later changes - trust on first use.
  // It is per-DEVICE, not per-person: a different browser/profile, or cleared
  // storage, is a new identity and an expected safety-number change.

  // memStore: non-persistent fallback (private mode / no IndexedDB / tests).
  function memStore() {
    var m = {};
    return {
      getItem: function (k) { return Promise.resolve(m[k]); },
      setItem: function (k, v) { m[k] = v; return Promise.resolve(); }
    };
  }
  // idbStore: a tiny IndexedDB key->value store (db 'of-e2ee', store 'kv').
  function idbStore() {
    var DBN = 'of-e2ee', STN = 'kv', dbP = null;
    function open() {
      if (dbP) return dbP;
      dbP = new Promise(function (resolve, reject) {
        var req = indexedDB.open(DBN, 1);
        req.onupgradeneeded = function () { req.result.createObjectStore(STN); };
        req.onsuccess = function () { resolve(req.result); };
        req.onerror = function () { reject(req.error); };
      });
      return dbP;
    }
    function tx(mode, fn) {
      return open().then(function (db) {
        return new Promise(function (resolve, reject) {
          var t = db.transaction(STN, mode), r = fn(t.objectStore(STN));
          t.oncomplete = function () { resolve(r && r.result); };
          t.onerror = function () { reject(t.error); };
          t.onabort = function () { reject(t.error); };
        });
      });
    }
    return {
      getItem: function (k) { return tx('readonly', function (st) { return st.get(k); }); },
      setItem: function (k, v) { return tx('readwrite', function (st) { return st.put(v, k); }); }
    };
  }
  var _defaultStore = null;
  function defaultStore() {
    if (_defaultStore) return _defaultStore;
    try { _defaultStore = (typeof indexedDB !== 'undefined') ? idbStore() : memStore(); }
    catch (e) { _defaultStore = memStore(); }
    return _defaultStore;
  }

  // loadIdentity: load the device identity from the store, or generate+persist a
  // fresh ECDSA P-256 keypair (non-extractable private key). Cached per store so
  // every session on a device shares one identity.
  function loadIdentity(store) {
    if (store._idP) return store._idP;
    store._idP = (async function () {
      var saved = null;
      try { saved = await store.getItem('identity'); } catch (e) {}
      var priv, pubRaw;
      if (saved && saved.priv && saved.pubRaw) {
        priv = saved.priv; pubRaw = saved.pubRaw;
      } else {
        var kp = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
        priv = kp.privateKey;
        pubRaw = await crypto.subtle.exportKey('raw', kp.publicKey);
        try { await store.setItem('identity', { priv: priv, pubRaw: pubRaw }); } catch (e) {}
      }
      var pub = new Uint8Array(pubRaw);
      return { priv: priv, pub: pub, pubB64: b64(pub) };
    })();
    return store._idP;
  }

  function idSigData(roomId, pubBytes) { return enc.encode('of-e2ee-id-v1|' + roomId + '|' + b64(pubBytes)); }
  async function signEphemeral(identity, roomId, pubBytes) {
    var sig = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, identity.priv, idSigData(roomId, pubBytes));
    return b64(new Uint8Array(sig));
  }
  async function verifyEphemeral(idpubBytes, roomId, pubBytes, sigBytes) {
    try {
      var key = await crypto.subtle.importKey('raw', idpubBytes, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['verify']);
      return await crypto.subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, key, sigBytes, idSigData(roomId, pubBytes));
    } catch (e) { return false; }
  }
  // safetyDigits: a stable, human-comparable number over a sorted list of identity
  // public keys (SHA-256 -> 8 groups of 5 digits). '' until at least two are known.
  async function safetyDigits(idPubs) {
    var list = idPubs.filter(Boolean).slice().sort(cmpBytes);
    if (list.length < 2) return '';
    var digest = new Uint8Array(await crypto.subtle.digest('SHA-256', concat(list)));
    var parts = [];
    for (var i = 0; i < 8; i++) {
      var n = ((digest[i * 2] << 8) | digest[i * 2 + 1]) % 100000;
      var s = '' + n; while (s.length < 5) s = '0' + s;
      parts.push(s);
    }
    return parts.join(' ');
  }
  // Trust-on-first-use contact store: contact:<id> -> { idPub, verified }.
  async function trustStatus(store, contactId, cur) {
    var out = { idPub: cur || '', present: !!cur, firstUse: true, changed: false, verified: false };
    if (!cur || !contactId) return out;
    var rec = null; try { rec = await store.getItem('contact:' + String(contactId)); } catch (e) {}
    if (rec && rec.idPub) { out.firstUse = false; out.changed = rec.idPub !== cur; out.verified = !!rec.verified && rec.idPub === cur; }
    return out;
  }
  async function trustPin(store, contactId, cur) {
    if (!cur || !contactId) return;
    var rec = null; try { rec = await store.getItem('contact:' + String(contactId)); } catch (e) {}
    if (!rec || rec.idPub !== cur) { try { await store.setItem('contact:' + String(contactId), { idPub: cur, verified: false }); } catch (e) {} }
  }
  async function trustVerify(store, contactId, cur) {
    if (!cur || !contactId) return;
    try { await store.setItem('contact:' + String(contactId), { idPub: cur, verified: true }); } catch (e) {}
  }

  // ----- Symmetric per-message ratchet (ephemeral group rooms) -----
  // Optional (ratchet:true). Within an epoch each member runs a hash-ratchet
  // chain: message i uses a key derived from chain key CK_i, then the chain
  // advances (CK_{i+1} = HKDF(CK_i)) and CK_i plus the message key are DELETED.
  // Old message keys cannot be re-derived from the current chain, so a state
  // compromise does not expose ALREADY-CONSUMED messages (per-message forward
  // secrecy). Because used keys are gone, decrypt cannot reload history - this is
  // for EPHEMERAL rooms only; never pair ratchet:true with a persisted
  // write_to_table. Both ends derive the same per-sender chain from the shared
  // epoch root, so no extra key distribution is needed.
  var RATCHET_SKIP_BOUND = 512;
  function zeroBytes(u) { if (u) { for (var i = 0; i < u.length; i++) u[i] = 0; } }
  async function hkdf32(rootBytes, salt, infoStr) {
    var base = await crypto.subtle.importKey('raw', rootBytes, 'HKDF', false, ['deriveBits']);
    var bits = await crypto.subtle.deriveBits({ name: 'HKDF', hash: 'SHA-256', salt: salt, info: enc.encode(infoStr) }, base, 256);
    return new Uint8Array(bits);
  }
  // chainStep: from chain key ck derive this message's AES key and the next chain
  // key. The caller zeroes ck after calling (it is spent).
  async function chainStep(ck, salt) {
    var mkBits = await hkdf32(ck, salt, 'of-e2ee-v1|msg');
    var next = await hkdf32(ck, salt, 'of-e2ee-v1|ratchet');
    var msgKey = await crypto.subtle.importKey('raw', mkBits, 'AES-GCM', false, ['encrypt', 'decrypt']);
    zeroBytes(mkBits);
    return { msgKey: msgKey, next: next };
  }

  // ----- Archive key (readable history) -----
  // The live message key is derived from a per-session ephemeral ECDH pair, so it
  // is gone the moment the page reloads and stored ciphertext can never be read
  // again. A room that persists chat (write_to_table) opts into archive:true, which
  // derives the message key from the PASSWORD and room alone - reproducible on any
  // device, in any later session, from the same two inputs the user already types.
  // The trade is explicit and one-directional: an archived room has no forward
  // secrecy, so archive:true and ratchet:true are mutually exclusive.
  // Archive frames are tagged a:1 on the wire and their AAD binds only the room, so
  // decryption does not need the relay-stamped _sender (which the durable write
  // strips) or any ephemeral material.
  async function deriveArchiveKey(password, roomId) {
    var pwKey = await crypto.subtle.importKey('raw', enc.encode(password), 'PBKDF2', false, ['deriveBits']);
    var pwBits = await crypto.subtle.deriveBits({ name: 'PBKDF2', salt: enc.encode(roomId), iterations: 200000, hash: 'SHA-256' }, pwKey, 256);
    var base = await crypto.subtle.importKey('raw', pwBits, 'HKDF', false, ['deriveKey']);
    return crypto.subtle.deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt: enc.encode(roomId), info: enc.encode('of-e2ee-v1|archive|' + roomId) },
      base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  }
  // archiveKey caches the derivation per session object (PBKDF2 at 200k iterations
  // is deliberately slow, so it must not run per message).
  function archiveKey(sess) {
    if (!sess._archiveP) sess._archiveP = deriveArchiveKey(sess.password, sess.roomId);
    return sess._archiveP;
  }
  // encArchive / decArchive: the archive envelope. No replay counter - history is
  // replayed repeatedly and out of order, and a counter would drop every row after
  // the first.
  async function encArchive(sess, text) {
    var key = await archiveKey(sess);
    var iv = crypto.getRandomValues(new Uint8Array(12));
    var aad = enc.encode(sess.roomId + '|archive');
    var payload = enc.encode(JSON.stringify({ t: String(text), ts: Date.now() }));
    var ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: iv, additionalData: aad }, key, payload);
    return { type: 'chat', v: 1, a: 1, iv: b64(iv), ct: b64(new Uint8Array(ct)) };
  }
  async function decArchive(sess, m) {
    if (!m || !m.iv || !m.ct) return null;
    try {
      var key = await archiveKey(sess);
      var aad = enc.encode(sess.roomId + '|archive');
      var pt = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(m.iv), additionalData: aad }, key, unb64(m.ct));
      var obj = JSON.parse(dec.decode(pt));
      return typeof obj.t === 'string' ? obj.t : null;
    } catch (e) {
      return null;
    }
  }

  // ----- Encrypted files / images / voice messages (blobs) -----
  // encBlob splits binary data into AES-GCM chunks under the shared key; each
  // chunk's AAD binds its position (i/n) so a relay cannot reorder or truncate
  // the file undetected. The chunks carry no plaintext body field, so they pass
  // the relay gate and are NOT persisted as chat envelopes. Send them over a
  // WebRTC DataChannel (preferred for large files) or as relay frames (set a
  // chunkSize that fits max_msg_size, or raise it).
  async function encBlob(key, roomId, data, opts) {
    opts = opts || {};
    var bytes = (data instanceof Uint8Array) ? data : new Uint8Array(data);
    var chunkSize = (opts.chunkSize && opts.chunkSize > 0) ? opts.chunkSize : 16384;
    var n = Math.max(1, Math.ceil(bytes.length / chunkSize));
    var chunks = [];
    for (var i = 0; i < n; i++) {
      var slice = bytes.subarray(i * chunkSize, Math.min((i + 1) * chunkSize, bytes.length));
      var iv = crypto.getRandomValues(new Uint8Array(12));
      var aad = enc.encode(roomId + '|blob|' + i + '/' + n);
      var ct = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: iv, additionalData: aad }, key, slice));
      chunks.push(b64(concat([iv, ct])));
    }
    return { meta: { v: 1, name: String(opts.name || ''), type: String(opts.type || ''), size: bytes.length, n: n }, chunks: chunks };
  }
  // decBlob reassembles a blob produced by encBlob. Returns a Uint8Array, or null
  // if a chunk is missing, out of order, or fails authentication (fail closed).
  async function decBlob(key, roomId, blob) {
    if (!key || !blob || !blob.meta || !blob.chunks) return null;
    var n = blob.meta.n | 0;
    if (!Array.isArray(blob.chunks) || blob.chunks.length !== n || n < 1) return null;
    var parts = [];
    for (var i = 0; i < n; i++) {
      try {
        var raw = unb64(blob.chunks[i]);
        var iv = raw.subarray(0, 12), ct = raw.subarray(12);
        var aad = enc.encode(roomId + '|blob|' + i + '/' + n);
        parts.push(new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: iv, additionalData: aad }, key, ct)));
      } catch (e) { return null; }
    }
    return concat(parts);
  }

  function Session(opts) {
    opts = opts || {};
    this.roomId = String(opts.roomId || '');
    this.password = String(opts.password || '');
    // archive: derive the message key from the password + room so persisted rows
    // stay readable. Turns forward secrecy off for this room by design.
    this.archive = !!opts.archive;
    this._archiveP = null;
    this._store = opts.store || defaultStore();
    this.identity = null;
    this.peerId = '';          // the client id we handshook with (a reload keeps it)
    this.peerIdPub = null;     // peer's identity public key (authenticated)
    this.peerIdError = false;  // a presented ephemeral key failed signature check
    this.keyPair = null;
    this.myPub = null;
    this.peerPub = null;
    this.aesKey = null;
    this.ready = false;
    this._readyCbs = [];
    this._sendCounter = 0;
    this._peerCounters = {};  // (sender|peerPub) -> last replay counter, namespaced per ephemeral key
    this.localFingerprint = '';
    this.remoteFingerprint = '';
    // ratchet is a group-only option; a two-party session ignores it. Warn rather
    // than silently downgrade, so a caller who wanted per-message forward secrecy
    // finds out they need mode:'group' instead of shipping an unratcheted room.
    if (opts.ratchet) {
      console.warn("E2EE: ratchet:true has no effect without mode:'group' - this two-party session is NOT ratcheted. Pass mode:'group' for a ratcheting room.");
    }
  }

  // onReady subscribers are RETAINED, not consumed: a peer that reloads announces a
  // fresh ephemeral key, the session re-derives, and every subscriber fires again so
  // a composer that was gated on the first derive re-enables on the second.
  Session.prototype.onReady = function (cb) {
    this._readyCbs.push(cb);
    if (this.ready) { try { cb(); } catch (e) {} }
  };
  Session.prototype._fireReady = function () {
    this.ready = true;
    for (var i = 0; i < this._readyCbs.length; i++) { try { this._readyCbs[i](); } catch (e) {} }
  };

  Session.prototype._pubFrame = function () {
    var f = { type: 'e2ee-pub', pub: b64(this.myPub) };
    if (this.identity && this._sig) { f.idpub = this.identity.pubB64; f.sig = this._sig; }
    return f;
  };

  // start: generate our ECDH keypair, sign it with our device identity, and
  // announce both. Call on ws open.
  Session.prototype.start = async function (ws) {
    if (this.keyPair) return;
    this.identity = await loadIdentity(this._store);
    this.keyPair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, false, ['deriveBits']);
    this.myPub = new Uint8Array(await crypto.subtle.exportKey('raw', this.keyPair.publicKey));
    this._sig = await signEphemeral(this.identity, this.roomId, this.myPub);
    ws.send(JSON.stringify(this._pubFrame()));
    await this._maybeDerive();
  };

  // handleSignal: consume an inbound e2ee-pub frame. Returns true when the frame
  // was an E2EE control frame the app should not process further. A frame whose
  // identity signature does not verify is rejected (peerIdError), never trusted.
  //
  // A new pub from the client id we are ALREADY paired with is that peer
  // reconnecting with a fresh ephemeral key (a reload reconnects on the same client
  // id, so the relay suppresses the leave and the roster never clears). Treating it
  // as a duplicate is what left the returning peer waiting on onReady forever, with
  // a fail-closed composer, in a room that never recovers. So re-handshake instead:
  // re-verify, adopt the new key, re-announce ours, re-derive. The old key stays
  // installed until the new one is ready, so there is never a window with no key.
  //
  // A pub from a DIFFERENT client id is a third peer arriving in a two-party room
  // (cap it with max_clients, or use mode:'group'); the established pair holds and
  // the newcomer is ignored rather than displacing one of them.
  Session.prototype.handleSignal = async function (m, ws) {
    if (!m || m.type !== 'e2ee-pub') return false;
    if (!m.pub) return true;
    var sender = m._sender || '';
    var paired = !!this.peerPub;
    if (paired && this.peerId && sender && sender !== this.peerId) return true;
    var incoming = unb64(m.pub);
    if (paired && cmpBytes(this.peerPub, incoming) === 0) return true; // duplicate announce
    if (m.idpub && m.sig) {
      var ok = await verifyEphemeral(unb64(m.idpub), this.roomId, incoming, unb64(m.sig));
      if (!ok) { this.peerIdError = true; return true; }
      this.peerIdPub = unb64(m.idpub);
    }
    this.peerPub = incoming;
    if (sender) this.peerId = sender;
    if (this.myPub && ws) ws.send(JSON.stringify(this._pubFrame()));
    if (paired) await this._rederive();
    else await this._maybeDerive();
    return true;
  };

  // safetyNumber: stable, human-comparable number over both IDENTITY keys
  // (unchanged across sessions). Compare once; if it later differs, the contact's
  // key changed. '' until both identities are known.
  Session.prototype.safetyNumber = async function () {
    if (!this.identity || !this.peerIdPub) return '';
    return safetyDigits([this.identity.pub, this.peerIdPub]);
  };
  // verifyStatus: trust state for a contact label you choose (e.g. a username).
  // { present, idError, firstUse, changed, verified, idPub }.
  Session.prototype.verifyStatus = async function (contactId) {
    var s = await trustStatus(this._store, contactId, this.peerIdPub ? b64(this.peerIdPub) : '');
    s.idError = this.peerIdError;
    return s;
  };
  // pin: trust-on-first-use - remember this contact's identity key (unverified) so
  // a later change is detectable. markVerified: record the user confirmed it.
  Session.prototype.pin = function (contactId) { return trustPin(this._store, contactId, this.peerIdPub ? b64(this.peerIdPub) : ''); };
  Session.prototype.markVerified = function (contactId) { return trustVerify(this._store, contactId, this.peerIdPub ? b64(this.peerIdPub) : ''); };

  // encryptBlob / decryptBlob: end-to-end encrypt a file, image, or recorded
  // voice/video message under the session key. data is an ArrayBuffer/Uint8Array
  // (for a File/Blob pass await blob.arrayBuffer()); decryptBlob returns a
  // Uint8Array (wrap it in new Blob([bytes], {type: meta.type})).
  // On an archive:true session blobs ride the password-derived key too, so a
  // stored attachment is readable in a later session exactly like stored chat.
  Session.prototype.encryptBlob = async function (data, opts) {
    if (this.archive) {
      var out = await encBlob(await archiveKey(this), this.roomId, data, opts);
      out.meta.a = 1;
      return out;
    }
    if (!this.ready || !this.aesKey) throw new Error('E2EE key not ready');
    return encBlob(this.aesKey, this.roomId, data, opts);
  };
  Session.prototype.decryptBlob = async function (blob) {
    // Try the archive key, then FALL THROUGH to the live key - the same order
    // decrypt() uses, and for the same reason: one peer configured without archive
    // sends live-keyed blobs into an archive room, and returning null there loses
    // the attachment for good.
    if (blob && blob.meta && (blob.meta.a || this.archive)) {
      var arc = await decBlob(await archiveKey(this), this.roomId, blob);
      if (arc != null) return arc;
    }
    if (!this.aesKey) return null;
    return decBlob(this.aesKey, this.roomId, blob);
  };

  Session.prototype._deriveShared = async function () {
    var peerKey = await crypto.subtle.importKey('raw', this.peerPub, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
    var ecdhBits = await crypto.subtle.deriveBits({ name: 'ECDH', public: peerKey }, this.keyPair.privateKey, 256);
    var pwKey = await crypto.subtle.importKey('raw', enc.encode(this.password), 'PBKDF2', false, ['deriveBits']);
    var pwBits = await crypto.subtle.deriveBits({ name: 'PBKDF2', salt: enc.encode(this.roomId), iterations: 200000, hash: 'SHA-256' }, pwKey, 256);
    var base = await crypto.subtle.importKey('raw', ecdhBits, 'HKDF', false, ['deriveKey']);
    return crypto.subtle.deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(pwBits), info: enc.encode('of-e2ee-v1|chat|' + this.roomId) },
      base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  };

  Session.prototype._maybeDerive = async function () {
    if (this.aesKey || !this.keyPair || !this.peerPub) return;
    this.aesKey = await this._deriveShared();
    this._fireReady();
  };

  // _rederive swaps in the key for a peer that reconnected (a fresh ephemeral pub).
  // Replay counters are namespaced by peer pubkey (like the group path's epoch) and
  // NOT cleared here: a genuine new pub gets a fresh counter (its sender restarts at
  // 1), while a relay replaying the peer's STALE pub lands back on that pub's old
  // high-water mark, so the replayed messages are rejected instead of re-accepted.
  Session.prototype._rederive = async function () {
    if (!this.keyPair || !this.peerPub) return;
    var key = await this._deriveShared();
    this.aesKey = key;
    this._fireReady();
  };

  // encryptArchive / decryptArchive: the password-derived lane, readable in any
  // later session. encrypt()/decrypt() use it automatically on an archive:true
  // session; these are the explicit entry points for a room that wants to archive
  // only some messages.
  Session.prototype.encryptArchive = function (text) {
    if (!this.archive) throw new Error('E2EE archive not enabled: pass archive:true to session()');
    return encArchive(this, text);
  };
  Session.prototype.decryptArchive = function (m) {
    if (!this.archive) return Promise.resolve(null);
    return decArchive(this, m);
  };

  // encrypt: build the wire envelope for a plaintext string. Pass your own
  // client id (welcome._clientId) so it binds into the AEAD as the sender.
  // Throws if the key is not ready - never fall back to plaintext. On an
  // archive:true session the payload rides the password-derived key instead, so
  // the stored row is readable later; the handshake still has to complete first,
  // because that is what authenticates the peer and backs verifyCode().
  Session.prototype.encrypt = async function (text, myClientId) {
    if (!this.ready || !this.aesKey) throw new Error('E2EE key not ready');
    if (this.archive) return encArchive(this, text);
    // decrypt binds AAD to the relay-stamped _sender; encrypt must bind the SAME
    // id or every peer's decrypt returns null (fail-closed, no error surfaced).
    // Warn once - the usual cause is encrypt(text) with the id argument dropped.
    if (!myClientId && !this._warnedNoId) {
      this._warnedNoId = true;
      console.warn('E2EE encrypt(text, myClientId): called without myClientId - peers will fail to decrypt (their AAD binds to the relay _sender). Pass net.myId.');
    }
    var iv = crypto.getRandomValues(new Uint8Array(12));
    var payload = enc.encode(JSON.stringify({ t: String(text), n: ++this._sendCounter, ts: Date.now() }));
    var aad = enc.encode(this.roomId + '|' + (myClientId || ''));
    var ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: iv, additionalData: aad }, this.aesKey, payload);
    return { type: 'chat', v: 1, iv: b64(iv), ct: b64(new Uint8Array(ct)) };
  };

  // decrypt: return the plaintext string, or null if the frame is invalid,
  // replayed, or undecryptable. Never throws (fail closed - drop, don't render).
  // An archive-tagged frame (a:1) takes the password-derived key and needs no
  // handshake and no _sender, so the SAME call reads a live frame and a row
  // replayed out of the history table. An archive session tries that key for an
  // UNtagged frame too - a history table with no `a` column would otherwise return
  // null for every row - and drops back to the live key if it does not open, so one
  // peer configured without archive does not silence the room.
  Session.prototype.decrypt = async function (m) {
    if (m && m.a) return decArchive(this, m);
    if (this.archive) {
      var arc = await decArchive(this, m);
      if (arc != null) return arc;
    }
    if (!this.ready || !this.aesKey || !m || !m.iv || !m.ct) return null;
    var sender = m._sender || '';
    try {
      var aad = enc.encode(this.roomId + '|' + sender);
      var pt = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(m.iv), additionalData: aad }, this.aesKey, unb64(m.ct));
      var obj = JSON.parse(dec.decode(pt));
      // Namespace the replay counter by the peer's current ephemeral pubkey: a
      // reconnect mints a new pub (fresh counter), but a relay replaying the peer's
      // stale pub reuses that pub's old high-water mark, so its messages are rejected.
      var ck = sender + '|' + (this.peerPub ? b64(this.peerPub) : '');
      var last = this._peerCounters[ck] || 0;
      if (typeof obj.n === 'number') {
        if (obj.n <= last) return null;
        this._peerCounters[ck] = obj.n;
      }
      return typeof obj.t === 'string' ? obj.t : null;
    } catch (e) {
      return null;
    }
  };

  // setFingerprints: feed local/remote SDP so the verify code also covers the
  // media (DTLS) path. Optional; call after setRemoteDescription on a call.
  Session.prototype.setFingerprints = function (localSdp, remoteSdp) {
    this.localFingerprint = extractFingerprint(localSdp);
    this.remoteFingerprint = extractFingerprint(remoteSdp);
  };

  // verifyCode: short human-comparable string over both public keys and (when
  // set) both DTLS fingerprints. Identical on both ends iff nobody tampered
  // with the key exchange or the SDP. '' until both public keys are known.
  Session.prototype.verifyCode = async function () {
    if (!this.myPub || !this.peerPub) return '';
    var pubs = [this.myPub, this.peerPub].sort(cmpBytes);
    var fps = [this.localFingerprint, this.remoteFingerprint].filter(Boolean).sort();
    var material = concat([pubs[0], pubs[1], enc.encode(fps.join('|'))]);
    var digest = new Uint8Array(await crypto.subtle.digest('SHA-256', material));
    var parts = [];
    for (var i = 0; i < 4; i++) {
      var s = '' + digest[i];
      while (s.length < 3) s = '0' + s;
      parts.push(s);
    }
    return parts.join(' ');
  };

  // ----- Group rooms (3+ peers): pairwise-wrapped epoch key -----
  // Every member announces an ECDH public key (e2ee-pub), as two-party Session
  // does. A deterministic distributor (lowest client id) mints a random per-epoch
  // AES-GCM key and sends it to each member wrapped under a pairwise key =
  // ECDH(distributor, member) mixed with PBKDF2(password), so a wrong-password
  // peer cannot unwrap it. The epoch is re-minted on membership change: a JOIN
  // bumps it and the joiner only receives the post-join epoch (backward secrecy);
  // a LEAVE bumps it and re-wraps only to remaining members (forward secrecy).
  // NOT a per-message ratchet - any member in an epoch shares one key and reads
  // every message under it. Envelope is the same {iv,ct} plus an 'ep' key tag;
  // the relay enforces and persists it as in the two-party case.
  var EPOCH_RETAIN = 3;

  function GroupSession(opts) {
    opts = opts || {};
    // Readable history and per-message forward secrecy are opposite guarantees:
    // the ratchet destroys each message key after use, so an archived ratchet room
    // would store ciphertext its own members could never reopen. Refuse the pair at
    // construction rather than shipping a room that quietly loses its history.
    if (opts.archive && opts.ratchet) {
      throw new Error('E2EE: archive:true and ratchet:true are mutually exclusive - the ratchet destroys the keys an archive exists to reopen');
    }
    this.roomId = String(opts.roomId || '');
    this.password = String(opts.password || '');
    this.archive = !!opts.archive;
    this._archiveP = null;
    this.self = String(opts.clientId || '');
    this._store = opts.store || defaultStore();
    this.identity = null;
    this.idError = {};        // clientId -> true (presented key failed signature check)
    this.keyPair = null;
    this.myPub = null;
    this.peers = {};          // clientId -> { pub, wrapKey, idPub } (idPub authenticated)
    this.members = {};        // clientId -> true (current roster, includes self)
    this.epochs = {};         // ep -> AES-GCM CryptoKey
    this.epOrder = [];        // installed epoch numbers, oldest first (retain last K)
    this.curEp = -1;          // epoch used for sending
    this.maxEp = -1;          // highest epoch number minted/seen
    this.ready = false;
    this._readyCbs = [];
    this._sendCounter = 0;
    this._peerCounters = {};  // (sender|ep) -> last replay counter
    this._sentPubTo = {};     // clientId -> true (we replied with our pub)
    this._sentEpochTo = {};   // ep -> { clientId: true } (distributor bookkeeping)
    this._curRaw = null;      // raw bytes of the current epoch key (distributor only)
    this._pendingGkeys = {};  // 'by|ep' -> wrapped key awaiting an agreed distributor
    this._ws = null;
    this._rosterSig = '';
    this._rosterDirty = false;
    this._recQ = null;
    this.ratchet = !!opts.ratchet;
    this.chains = {};      // (ep|clientId) -> { ck: Uint8Array, i: number }
    this.skipped = {};     // (ep|clientId) -> { index -> msgKey } bounded out-of-order cache
    this._skipOrder = [];  // FIFO of 'ep|clientId|index' bounding the skipped cache
    this._salt = enc.encode(this.roomId);
  }

  // Subscribers are RETAINED, matching Session.onReady: a group room re-derives on
  // every epoch change, and a callback that only ever fired once left anything
  // gated on "keys are ready" stuck off after the first rekey.
  GroupSession.prototype.onReady = function (cb) {
    this._readyCbs.push(cb);
    if (this.ready) { try { cb(); } catch (e) {} }
  };
  GroupSession.prototype._fireReady = function () {
    this.ready = true;
    for (var i = 0; i < this._readyCbs.length; i++) { try { this._readyCbs[i](); } catch (e) {} }
  };

  // setSelf: record our own client id (welcome._clientId). Call before onRoster.
  GroupSession.prototype.setSelf = function (id) {
    if (id) { this.self = String(id); this.members[this.self] = true; }
  };

  GroupSession.prototype._pubFrame = function (toId) {
    var f = { type: 'e2ee-pub', pub: b64(this.myPub) };
    if (this.identity && this._sig) { f.idpub = this.identity.pubB64; f.sig = this._sig; }
    if (toId) f._to = toId;
    return f;
  };

  // start: generate our ECDH keypair, sign it with our device identity, and
  // announce both. Call on ws open.
  GroupSession.prototype.start = async function (ws) {
    this._ws = ws;
    if (this.keyPair) return;
    this.identity = await loadIdentity(this._store);
    this.keyPair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, false, ['deriveBits']);
    this.myPub = new Uint8Array(await crypto.subtle.exportKey('raw', this.keyPair.publicKey));
    this._sig = await signEphemeral(this.identity, this.roomId, this.myPub);
    ws.send(JSON.stringify(this._pubFrame()));
    this._reconcile();
  };

  // onRoster: feed the FULL current member id list (welcome._clientId +
  // welcome._peers ids, then re-fed on every join/leave). The helper diffs it and,
  // when WE are the distributor, re-keys the group on any change. A missed leave
  // means the departed member is not rotated out, so keep this accurate.
  GroupSession.prototype.onRoster = function (ids) {
    var next = {};
    if (this.self) next[this.self] = true;
    if (ids && ids.length) { for (var i = 0; i < ids.length; i++) { if (ids[i]) next[String(ids[i])] = true; } }
    this.members = next;
    var gone = [];
    for (var id in this.peers) { if (this.peers.hasOwnProperty(id) && !next[id]) gone.push(id); }
    for (var j = 0; j < gone.length; j++) { delete this.peers[gone[j]]; delete this._sentPubTo[gone[j]]; }
    var sig = Object.keys(next).sort().join(',');
    if (sig !== this._rosterSig) { this._rosterSig = sig; this._rosterDirty = true; }
    this._reconcile();
  };

  // _trimPendingGkeys bounds the queue: a key for an epoch we already hold, or one
  // older than the retain window, will never be installed, so it is dead weight.
  GroupSession.prototype._trimPendingGkeys = function () {
    for (var k in this._pendingGkeys) {
      if (!this._pendingGkeys.hasOwnProperty(k)) continue;
      var ep = this._pendingGkeys[k].ep;
      if (this.epochs[ep] || (this.curEp >= 0 && ep < this.curEp - EPOCH_RETAIN)) delete this._pendingGkeys[k];
    }
  };

  GroupSession.prototype._distributor = function () {
    var min = null;
    for (var id in this.members) {
      if (!this.members.hasOwnProperty(id)) continue;
      if (min === null || id < min) min = id;
    }
    return min;
  };

  // handleSignal: consume e2ee-pub (peer key announce) and e2ee-gkey (wrapped epoch
  // key). Returns true when the frame is an E2EE control frame the app should not
  // process further.
  GroupSession.prototype.handleSignal = async function (m, ws) {
    if (!m) return false;
    if (ws) this._ws = ws;
    if (m.type === 'e2ee-pub') {
      var sender = m._sender || '';
      if (sender && sender !== this.self && m.pub) {
        // Authenticate the ephemeral key against the claimed identity; a forged
        // key with no valid signature is rejected, never added as a member key.
        var incoming = unb64(m.pub);
        if (m.idpub && m.sig) {
          var ok = await verifyEphemeral(unb64(m.idpub), this.roomId, incoming, unb64(m.sig));
          if (!ok) { this.idError[sender] = true; return true; }
        }
        var p = this.peers[sender] || (this.peers[sender] = { pub: null, wrapKey: null, idPub: null });
        // A DIFFERENT pub from a member we already know is that member reconnecting
        // with a fresh ephemeral key. Its pairwise wrap key, its ratchet chains, and
        // the record of having sent it this epoch are all keyed to the old one, so
        // reset the lot and mark the roster dirty: the distributor then re-mints, and
        // the returning member gets a key it can actually use. Ignoring the new pub
        // as a duplicate is what left a reloaded member permanently mute.
        var changed = !p.pub || cmpBytes(p.pub, incoming) !== 0;
        if (changed) {
          p.pub = incoming;
          p.wrapKey = null;
          if (m.idpub) p.idPub = unb64(m.idpub);
          delete this._sentPubTo[sender];
          for (var sep in this._sentEpochTo) {
            if (this._sentEpochTo.hasOwnProperty(sep)) delete this._sentEpochTo[sep][sender];
          }
          this._forgetMemberChains(sender);
          if (p.pub && this.maxEp >= 0) this._rosterDirty = true;
        }
        this.members[sender] = true;
        // Reply once, targeted, so a later joiner learns our key without a
        // room-wide rebroadcast storm.
        if (this.myPub && this._ws && !this._sentPubTo[sender]) {
          this._ws.send(JSON.stringify(this._pubFrame(sender)));
          this._sentPubTo[sender] = true;
        }
        if (changed) this._reconcile();
      }
      return true;
    }
    if (m.type === 'e2ee-gkey') {
      // Queue every wrapped key addressed to us, keyed by (sender, epoch), and let
      // _reconcileInner install it once we agree that sender is the distributor and
      // we hold their pairwise wrap key. Testing the distributor HERE dropped a key
      // that arrived before our own roster caught up, and the distributor records
      // having sent it, so it never resends: that epoch stayed silently unreadable.
      if (m.by && m.by !== this.self && m.wraps && m.wraps[this.self]) {
        this._pendingGkeys[m.by + '|' + m.ep] = { by: m.by, ep: m.ep, wrap: m.wraps[this.self] };
        this._trimPendingGkeys();
        this._reconcile();
      }
      return true;
    }
    return false;
  };

  // _forgetMemberChains drops every ratchet chain and cached skipped key belonging
  // to one member, across all epochs. Called when that member reconnects: its fresh
  // session restarts its send chain at index 0, so the chain we hold is spent.
  GroupSession.prototype._forgetMemberChains = function (id) {
    if (!this.ratchet) return;
    var suffix = '|' + id;
    for (var ck in this.chains) {
      if (this.chains.hasOwnProperty(ck) && ck.length > suffix.length && ck.indexOf(suffix, ck.length - suffix.length) !== -1) delete this.chains[ck];
    }
    for (var sk in this.skipped) {
      if (this.skipped.hasOwnProperty(sk) && sk.length > suffix.length && sk.indexOf(suffix, sk.length - suffix.length) !== -1) delete this.skipped[sk];
    }
  };

  GroupSession.prototype._derivePeerWrapKey = async function (peerPubBytes) {
    var peerKey = await crypto.subtle.importKey('raw', peerPubBytes, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
    var ecdhBits = await crypto.subtle.deriveBits({ name: 'ECDH', public: peerKey }, this.keyPair.privateKey, 256);
    var pwKey = await crypto.subtle.importKey('raw', enc.encode(this.password), 'PBKDF2', false, ['deriveBits']);
    var pwBits = await crypto.subtle.deriveBits({ name: 'PBKDF2', salt: enc.encode(this.roomId), iterations: 200000, hash: 'SHA-256' }, pwKey, 256);
    var base = await crypto.subtle.importKey('raw', ecdhBits, 'HKDF', false, ['deriveKey']);
    return await crypto.subtle.deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(pwBits), info: enc.encode('of-e2ee-v1|wrap|' + this.roomId) },
      base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  };

  GroupSession.prototype._wrapFor = async function (wrapKey, raw, ep, recipientId) {
    var iv = crypto.getRandomValues(new Uint8Array(12));
    var aad = enc.encode(this.roomId + '|gkey|' + ep + '|' + recipientId);
    var ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: iv, additionalData: aad }, wrapKey, raw);
    return { iv: b64(iv), ct: b64(new Uint8Array(ct)) };
  };

  GroupSession.prototype._installEpoch = function (ep, key) {
    if (!this.epochs[ep]) { this.epochs[ep] = key; this.epOrder.push(ep); }
    if (ep > this.curEp) this.curEp = ep;
    if (ep > this.maxEp) this.maxEp = ep;
    while (this.epOrder.length > EPOCH_RETAIN) {
      var old = this.epOrder.shift();
      if (old !== this.curEp) {
        delete this.epochs[old];
        if (this.ratchet) {
          var pre = old + '|';
          for (var ck in this.chains) { if (this.chains.hasOwnProperty(ck) && ck.indexOf(pre) === 0) delete this.chains[ck]; }
          for (var sk in this.skipped) { if (this.skipped.hasOwnProperty(sk) && sk.indexOf(pre) === 0) delete this.skipped[sk]; }
        }
      }
    }
  };

  GroupSession.prototype._installWrapped = async function (by, ep, wrap) {
    var p = this.peers[by];
    if (!p || !p.pub || !this.keyPair) return;
    if (!p.wrapKey) p.wrapKey = await this._derivePeerWrapKey(p.pub);
    try {
      var aad = enc.encode(this.roomId + '|gkey|' + ep + '|' + this.self);
      var rawBuf = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(wrap.iv), additionalData: aad }, p.wrapKey, unb64(wrap.ct));
      var rootBytes = new Uint8Array(rawBuf);
      var key = await crypto.subtle.importKey('raw', rootBytes, 'AES-GCM', false, ['encrypt', 'decrypt']);
      this._installEpoch(ep, key);
      if (this.ratchet) { await this._seedChains(ep, rootBytes); zeroBytes(rootBytes); }
      this._fireReady();
    } catch (e) { /* wrong password or tampered wrap: drop, stay not-ready */ }
  };

  // _seedChains derives the per-member ratchet chain roots for an epoch from the
  // epoch root, for every current member. Receivers feed rootBytes from the
  // unwrapped key and then drop it, so the epoch root is not retained.
  GroupSession.prototype._seedChains = async function (ep, rootBytes) {
    for (var id in this.members) {
      if (!this.members.hasOwnProperty(id)) continue;
      var ck = ep + '|' + id;
      if (this.chains[ck]) continue;
      this.chains[ck] = { ck: await hkdf32(rootBytes, this._salt, 'of-e2ee-v1|chain0|' + ep + '|' + id), i: 0 };
    }
  };
  GroupSession.prototype._cacheSkipped = function (key, idx, msgKey) {
    this.skipped[key] = this.skipped[key] || {};
    this.skipped[key][idx] = msgKey;
    this._skipOrder.push(key + '|' + idx);
    while (this._skipOrder.length > RATCHET_SKIP_BOUND) {
      var old = this._skipOrder.shift();
      var p = old.lastIndexOf('|');
      var k0 = old.slice(0, p), i0 = old.slice(p + 1);
      if (this.skipped[k0]) delete this.skipped[k0][i0];
    }
  };

  // _reconcile serializes reconciliation so rapid join/leave/pub events cannot
  // double-mint an epoch.
  GroupSession.prototype._reconcile = function () {
    var self = this;
    this._recQ = (this._recQ || Promise.resolve()).then(function () { return self._reconcileInner(); }).catch(function () {});
    return this._recQ;
  };

  GroupSession.prototype._reconcileInner = async function () {
    if (!this.keyPair || !this._ws) return;
    // Derive any pairwise wrap keys we can now form.
    for (var id in this.peers) {
      if (!this.peers.hasOwnProperty(id)) continue;
      var p = this.peers[id];
      if (p && p.pub && !p.wrapKey) { p.wrapKey = await this._derivePeerWrapKey(p.pub); }
    }
    // Drain any wrapped key whose sender we now agree is the distributor and whose
    // pairwise wrap key we can now form.
    var dist = this._distributor();
    for (var gk in this._pendingGkeys) {
      if (!this._pendingGkeys.hasOwnProperty(gk)) continue;
      var pg = this._pendingGkeys[gk], pp = this.peers[pg.by];
      if (pg.by !== dist || !pp || !pp.wrapKey) continue;
      delete this._pendingGkeys[gk];
      await this._installWrapped(pg.by, pg.ep, pg.wrap);
    }
    // Only the elected distributor mints/distributes keys.
    var memberCount = 0;
    for (var k in this.members) { if (this.members.hasOwnProperty(k)) memberCount++; }
    if (this._distributor() !== this.self || memberCount < 2) return;
    if (this.maxEp < 0 || this._rosterDirty) {
      this._rosterDirty = false;
      await this._mintEpoch();
    } else if (this.curEp >= 0 && this._curRaw) {
      await this._topUpCurrentEpoch();
    }
  };

  GroupSession.prototype._mintEpoch = async function () {
    var ep = ++this.maxEp;
    var raw = crypto.getRandomValues(new Uint8Array(32));
    this._curRaw = raw;
    var key = await crypto.subtle.importKey('raw', raw, 'AES-GCM', false, ['encrypt', 'decrypt']);
    this._installEpoch(ep, key);
    if (this.ratchet) await this._seedChains(ep, raw);
    this._sentEpochTo[ep] = {};
    var wraps = {};
    for (var id in this.members) {
      if (!this.members.hasOwnProperty(id) || id === this.self) continue;
      var p = this.peers[id];
      if (p && p.wrapKey) {
        wraps[id] = await this._wrapFor(p.wrapKey, raw, ep, id);
        this._sentEpochTo[ep][id] = true;
      }
    }
    this._ws.send(JSON.stringify({ type: 'e2ee-gkey', ep: ep, by: this.self, wraps: wraps }));
    this._fireReady();
  };

  // _topUpCurrentEpoch sends the current epoch key to members whose pub arrived
  // after the epoch was minted (no epoch bump - they are already current members).
  GroupSession.prototype._topUpCurrentEpoch = async function () {
    var ep = this.curEp;
    if (!this._curRaw) return;
    this._sentEpochTo[ep] = this._sentEpochTo[ep] || {};
    var wraps = {}, any = false;
    for (var id in this.members) {
      if (!this.members.hasOwnProperty(id) || id === this.self) continue;
      var p = this.peers[id];
      if (p && p.wrapKey && !this._sentEpochTo[ep][id]) {
        wraps[id] = await this._wrapFor(p.wrapKey, this._curRaw, ep, id);
        this._sentEpochTo[ep][id] = true;
        any = true;
      }
    }
    if (any) this._ws.send(JSON.stringify({ type: 'e2ee-gkey', ep: ep, by: this.self, wraps: wraps }));
  };

  // encryptArchive / decryptArchive: the password-derived lane, readable in any
  // later session. encrypt()/decrypt() use it automatically on an archive:true
  // group session; these are the explicit entry points for a room that wants to
  // archive only some messages.
  GroupSession.prototype.encryptArchive = function (text) {
    if (!this.archive) throw new Error('E2EE archive not enabled: pass archive:true to session()');
    return encArchive(this, text);
  };
  GroupSession.prototype.decryptArchive = function (m) {
    if (!this.archive) return Promise.resolve(null);
    return decArchive(this, m);
  };

  // encrypt: build the wire envelope tagged with the current epoch (and, in
  // ratchet mode, the message index). Throws if no key is ready - never fall back
  // to plaintext.
  GroupSession.prototype.encrypt = async function (text, myClientId) {
    if (myClientId && !this.self) this.self = String(myClientId);
    // Same AAD contract as the two-party session: without a self id, peers bind
    // decrypt to the relay _sender and this room's messages never open. Warn once.
    if (!this.self && !this._warnedNoSelf) {
      this._warnedNoSelf = true;
      console.warn('E2EE group encrypt: no self client id - call setSelf(net.myId) (or pass myClientId) or peers will fail to decrypt.');
    }
    if (this.curEp < 0) throw new Error('E2EE group key not ready');
    // Archive rooms key on the password, not the epoch: the row has to reopen for a
    // member who joins a later session with nothing but the room and the password.
    if (this.archive) return encArchive(this, text);
    var iv = crypto.getRandomValues(new Uint8Array(12));
    var aad = enc.encode(this.roomId + '|' + (this.self || ''));
    if (this.ratchet) {
      var ch = this.chains[this.curEp + '|' + this.self];
      if (!ch) throw new Error('E2EE group key not ready');
      var step = await chainStep(ch.ck, this._salt);
      var idx = ch.i;
      zeroBytes(ch.ck); ch.ck = step.next; ch.i = idx + 1;
      var rpayload = enc.encode(JSON.stringify({ t: String(text), ts: Date.now() }));
      var rct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: iv, additionalData: aad }, step.msgKey, rpayload);
      return { type: 'chat', v: 1, ep: this.curEp, i: idx, iv: b64(iv), ct: b64(new Uint8Array(rct)) };
    }
    if (!this.epochs[this.curEp]) throw new Error('E2EE group key not ready');
    var payload = enc.encode(JSON.stringify({ t: String(text), n: ++this._sendCounter, ts: Date.now() }));
    var ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv: iv, additionalData: aad }, this.epochs[this.curEp], payload);
    return { type: 'chat', v: 1, ep: this.curEp, iv: b64(iv), ct: b64(new Uint8Array(ct)) };
  };

  // decrypt: return the plaintext string, or null if the frame is invalid,
  // replayed, from an epoch we do not hold (e.g. sent before we joined), or
  // undecryptable. In ratchet mode a message whose key was already consumed and
  // deleted also returns null (that is the forward-secrecy guarantee). Never
  // throws (fail closed - drop, do not render).
  GroupSession.prototype.decrypt = async function (m) {
    if (!m || !m.iv || !m.ct) return null;
    // Archive frames key on the password, not the epoch. The untagged attempt
    // covers a history table with no `a` column, and falling through to the epoch
    // key covers a member configured without archive (see Session.decrypt).
    if (m.a) return decArchive(this, m);
    if (this.archive) {
      var arc = await decArchive(this, m);
      if (arc != null) return arc;
    }
    var ep = (typeof m.ep === 'number') ? m.ep : this.curEp;
    var sender = m._sender || '';
    var aad = enc.encode(this.roomId + '|' + sender);
    if (this.ratchet) {
      if (typeof m.i !== 'number') return null;
      var rkey = ep + '|' + sender;
      var ch = this.chains[rkey];
      var msgKey = null;
      if (ch && m.i >= ch.i) {
        // Cap the catch-up: m.i comes off the wire (the untrusted relay stamps
        // _sender, and this runs before AES-GCM can reject the frame), so an
        // arbitrary index must not drive an unbounded run of awaited HKDF steps.
        // A gap wider than the skip cache is unsatisfiable anyway - the earliest
        // cached keys would be evicted before they could be used - so drop it.
        if (m.i - ch.i > RATCHET_SKIP_BOUND) return null;
        // advance to m.i, caching message keys for any indices we skip over
        while (ch.i < m.i) {
          var sst = await chainStep(ch.ck, this._salt);
          this._cacheSkipped(rkey, ch.i, sst.msgKey);
          zeroBytes(ch.ck); ch.ck = sst.next; ch.i++;
        }
        var nst = await chainStep(ch.ck, this._salt);
        zeroBytes(ch.ck); ch.ck = nst.next; ch.i = m.i + 1;
        msgKey = nst.msgKey;
      } else {
        var sk = this.skipped[rkey];
        if (sk && sk[m.i]) { msgKey = sk[m.i]; delete sk[m.i]; }
      }
      if (!msgKey) return null; // old / replayed / unseeded epoch - key is gone
      try {
        var rpt = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(m.iv), additionalData: aad }, msgKey, unb64(m.ct));
        var robj = JSON.parse(dec.decode(rpt));
        return typeof robj.t === 'string' ? robj.t : null;
      } catch (e) { return null; }
    }
    var key = this.epochs[ep];
    if (!key) return null;
    try {
      var pt = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(m.iv), additionalData: aad }, key, unb64(m.ct));
      var obj = JSON.parse(dec.decode(pt));
      var ck = sender + '|' + ep;
      var lastN = this._peerCounters[ck] || 0;
      if (typeof obj.n === 'number') {
        if (obj.n <= lastN) return null;
        this._peerCounters[ck] = obj.n;
      }
      return typeof obj.t === 'string' ? obj.t : null;
    } catch (e) {
      return null;
    }
  };

  // verifyCode: short human-comparable string over every known member public key
  // (sorted). Identical for all honest members; a relay that injected a forged
  // member key shifts it. '' until at least two public keys are known.
  GroupSession.prototype.verifyCode = async function () {
    var pubs = [];
    if (this.myPub) pubs.push(this.myPub);
    for (var id in this.peers) { if (this.peers.hasOwnProperty(id) && this.peers[id].pub) pubs.push(this.peers[id].pub); }
    if (pubs.length < 2) return '';
    pubs.sort(cmpBytes);
    var digest = new Uint8Array(await crypto.subtle.digest('SHA-256', concat(pubs)));
    var parts = [];
    for (var i = 0; i < 4; i++) { var s = '' + digest[i]; while (s.length < 3) s = '0' + s; parts.push(s); }
    return parts.join(' ');
  };

  // safetyNumber: stable number over EVERY known member's identity key (sorted).
  // Same for all honest members; shifts if a forged member identity was injected.
  GroupSession.prototype.safetyNumber = async function () {
    var ids = [];
    if (this.identity) ids.push(this.identity.pub);
    for (var id in this.peers) { if (this.peers.hasOwnProperty(id) && this.peers[id].idPub) ids.push(this.peers[id].idPub); }
    return safetyDigits(ids);
  };
  // Per-member trust-on-first-use, keyed by a contact label you choose. clientId
  // selects which present member; contactId is the durable handle you pin against.
  GroupSession.prototype._memberIdB64 = function (clientId) {
    var p = this.peers[clientId];
    return (p && p.idPub) ? b64(p.idPub) : '';
  };
  GroupSession.prototype.verifyStatus = async function (clientId, contactId) {
    var s = await trustStatus(this._store, contactId, this._memberIdB64(clientId));
    s.idError = !!this.idError[clientId];
    return s;
  };
  GroupSession.prototype.pin = function (clientId, contactId) { return trustPin(this._store, contactId, this._memberIdB64(clientId)); };
  GroupSession.prototype.markVerified = function (clientId, contactId) { return trustVerify(this._store, contactId, this._memberIdB64(clientId)); };

  // encryptBlob / decryptBlob: encrypted file/image/voice-message transfer for a
  // group room, under the current epoch key (tagged so a receiver picks the right
  // one). Blobs use the epoch key directly - they are not per-message ratcheted
  // even when ratchet:true is set for text. See Session.encryptBlob for the data
  // shape.
  GroupSession.prototype.encryptBlob = async function (data, opts) {
    if (this.archive) {
      var arc = await encBlob(await archiveKey(this), this.roomId, data, opts);
      arc.meta.a = 1;
      return arc;
    }
    if (this.curEp < 0 || !this.epochs[this.curEp]) throw new Error('E2EE group key not ready');
    var out = await encBlob(this.epochs[this.curEp], this.roomId, data, opts);
    out.meta.ep = this.curEp;
    return out;
  };
  GroupSession.prototype.decryptBlob = async function (blob) {
    if (!blob || !blob.meta) return null;
    // Archive key first, then fall through to the epoch key (see Session.decryptBlob).
    if (blob.meta.a || this.archive) {
      var arc = await decBlob(await archiveKey(this), this.roomId, blob);
      if (arc != null) return arc;
    }
    var ep = (typeof blob.meta.ep === 'number') ? blob.meta.ep : this.curEp;
    var key = this.epochs[ep];
    if (!key) return null;
    return decBlob(key, this.roomId, blob);
  };

  return {
    session: function (opts) {
      opts = opts || {};
      if (opts.mode === 'group') return new GroupSession(opts);
      return new Session(opts);
    },
    groupSession: function (opts) { return new GroupSession(opts); },
    // identity: resolve the device identity (loads or generates it). Read .pubB64
    // to show the user their own key fingerprint.
    identity: function (opts) { opts = opts || {}; return loadIdentity(opts.store || defaultStore()); }
  };
})();

export const session = OFE2EE.session;
export const groupSession = OFE2EE.groupSession;
export const identity = OFE2EE.identity;
export default OFE2EE;
