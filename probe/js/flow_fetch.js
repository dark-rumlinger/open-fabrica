(async function (method, path, body, headers) {
  var init = { method: method, headers: headers || {}, credentials: 'same-origin' };
  var has = function (name) { return Object.keys(init.headers).some(function (k) { return k.toLowerCase() === name; }); };
  if (body !== null && body !== undefined) {
    init.body = typeof body === 'string' ? body : JSON.stringify(body);
    if (!has('content-type')) init.headers['Content-Type'] = typeof body === 'string' ? 'text/plain' : 'application/json';
  }
  try { var t = localStorage.getItem('auth_token'); if (t && !has('authorization')) init.headers['Authorization'] = 'Bearer ' + t; } catch (e) {}
  var r = await fetch(path, init);
  var text = await r.text();
  return { status: r.status, body: text.slice(0, 20000) };
})