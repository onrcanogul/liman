// The liman panel: what the daemon knows, and what it can be asked to do
// from a browser. Anything that can lose something (rolling back, stopping
// an app, removing a secret) is confirmed in a dialog first.
//
// Three rules hold everywhere in this file:
//
//   1. Data is never turned into markup. Every element is created with h() or
//      s() and every value is set as text. A field that contains "<script>"
//      is shown as those eight characters.
//   2. Nothing from the API is trusted to have the right shape. Values pass
//      through str(), num() and time() before they are used; a field that is
//      missing or odd becomes a dash or "Unknown", not an exception.
//   3. A failure is drawn, not thrown. A request that fails turns into a
//      banner, a chart that cannot be drawn into an empty box, and anything
//      unforeseen into a message instead of a blank page.
(function () {
'use strict';

// ---------- DOM builders ----------
function h(tag, attrs) {
  var el = document.createElement(tag);
  setAttrs(el, attrs);
  for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
  return el;
}
var SVGNS = 'http://www.w3.org/2000/svg';
function s(tag, attrs) {
  var el = document.createElementNS(SVGNS, tag);
  setAttrs(el, attrs);
  for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
  return el;
}
function setAttrs(el, attrs) {
  if (!attrs) return;
  Object.keys(attrs).forEach(function (k) {
    var v = attrs[k];
    if (v === null || v === undefined || v === false) return;
    if (k === 'class') el.setAttribute('class', v);
    else if (k.slice(0, 2) === 'on' && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'text') el.textContent = String(v);
    // The content security policy forbids style attributes; setting
    // properties on el.style is allowed, because only script can do it.
    else if (k === 'css') Object.keys(v).forEach(function (p) { el.style[p] = v[p]; });
    else el.setAttribute(k, v === true ? '' : String(v));
  });
}
function add(el, c) {
  if (c === null || c === undefined || c === false) return;
  if (Array.isArray(c)) { c.forEach(function (x) { add(el, x); }); return; }
  el.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
}
function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); }

// Icons are fixed path data written here, never data from the server.
var ICONS = {
  overview: 'M3 3h7v7H3zM14 3h7v4h-7zM14 11h7v10h-7zM3 14h7v7H3z',
  apps: 'M12 2l9 5-9 5-9-5 9-5zM3 12l9 5 9-5M3 17l9 5 9-5',
  activity: 'M3 12h4l3-8 4 16 3-8h4',
  secrets: 'M7 11V8a5 5 0 0110 0v3M5 11h14v10H5z',
  daemon: 'M4 4h16v6H4zM4 14h16v6H4zM8 7h.01M8 17h.01',
  tokens: 'M19 9a4 4 0 11-8 0 4 4 0 018 0zM12 12l-9 9M6 18l3 3M9 15l2 2',
  backups: 'M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3zM4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6',
  chevron: 'M6 9l6 6 6-6',
  collapse: 'M15 6l-6 6 6 6', expand: 'M9 6l6 6-6 6', menu: 'M4 6h16M4 12h16M4 18h16',
  signout: 'M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10'
};
function icon(name) {
  return s('svg', { viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '1.8', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' },
    s('path', { d: ICONS[name] || '' }));
}

// ---------- formatting that never throws on odd input ----------
var DASH = '—';
function str(v, max) {
  if (v === null || v === undefined) return '';
  var t = String(v).replace(/[\u0000-\u001f\u007f]/g, ' ').trim();
  return max && t.length > max ? t.slice(0, max - 1) + '…' : t;
}
function orDash(v, max) { var t = str(v, max); return t === '' ? DASH : t; }
function num(v) { var n = Number(v); return v !== null && v !== '' && typeof v !== 'boolean' && Number.isFinite(n) && n >= 0 ? Math.floor(n) : null; }
function fmtNum(v) { var n = num(v); if (n === null) return DASH; try { return n.toLocaleString('en-US'); } catch (e) { return String(n); } }
// Go encodes a time that was never set as year 1; anything before 2000 is
// treated as "unknown" rather than shown as a date nobody meant.
var EPOCH_FLOOR = Date.UTC(2000, 0, 1);
function time(ts) { var t = typeof ts === 'number' ? ts : Date.parse(ts); return Number.isFinite(t) && t > EPOCH_FLOOR ? t : null; }
function ago(ts) {
  var t = time(ts); if (t === null) return DASH;
  var d = Math.round((Date.now() - t) / 1000);
  if (d < -60) return 'in the future';
  if (d < 5) return 'just now';
  if (d < 60) return d + 's ago';
  if (d < 3600) return Math.floor(d / 60) + 'm ago';
  if (d < 86400) return Math.floor(d / 3600) + 'h ago';
  return Math.floor(d / 86400) + 'd ago';
}
function abs(ts) { var t = time(ts); if (t === null) return 'Time unknown'; try { return new Date(t).toLocaleString(); } catch (e) { return 'Time unknown'; } }
function clock(ts, withSec) { try { return new Date(ts).toLocaleTimeString([], withSec ? { hour: '2-digit', minute: '2-digit', second: '2-digit' } : { hour: '2-digit', minute: '2-digit' }); } catch (e) { return DASH; } }
function day(ts) { try { return new Date(ts).toLocaleDateString([], { month: 'short', day: 'numeric' }); } catch (e) { return DASH; } }
function when(ts) { return h('span', { title: abs(ts), text: ago(ts) }); }
function took(ms) {
  var n = num(ms); if (n === null) return DASH;
  if (n < 1000) return n + ' ms';
  if (n < 60000) return (Math.round(n / 100) / 10) + ' s';
  return Math.floor(n / 60000) + 'm ' + Math.round((n % 60000) / 1000) + 's';
}
function size(bytes) {
  var n = num(bytes); if (n === null) return DASH;
  if (n >= 1048576) return (Math.round(n / 104857.6) / 10) + ' MiB';
  if (n >= 1024) return Math.round(n / 1024) + ' KiB';
  return n + ' bytes';
}
function span(seconds) {
  var n = num(seconds); if (n === null || n === 0) return DASH;
  if (n % 86400 === 0) return (n / 86400) + (n === 86400 ? ' day' : ' days');
  if (n % 3600 === 0) return (n / 3600) + 'h';
  if (n % 60 === 0) return (n / 60) + 'm';
  return n + 's';
}

// Status is derived in one place. Anything unrecognised becomes "Unknown"
// (and asks for attention) instead of breaking a row or looking fine.
function status(app) {
  var st = str(app && app.state).toLowerCase(), hl = str(app && app.health).toLowerCase();
  if (hl === 'down') return { key: 'crit', label: 'Down', attn: true };
  if (hl === 'restarting') return { key: 'warn', label: 'Restarting', attn: true };
  if (st === 'running') {
    if (hl === 'unhealthy') return { key: 'crit', label: 'Unhealthy', attn: true };
    if (hl === 'healthy') return { key: 'ok', label: 'Healthy', attn: false };
    if (hl === '') return { key: 'ok', label: 'Running', attn: false };
    return { key: 'none', label: 'Unknown', attn: true };
  }
  // liman has no "stopped on purpose": stopping an app removes it. A
  // container that is not running is one that should be.
  if (st === 'exited' || st === 'created' || st === 'dead') return { key: 'crit', label: 'Stopped', attn: true };
  return { key: 'none', label: 'Unknown', attn: true };
}
function pill(p) { return h('span', { class: 'pill ' + p.key }, h('i', { 'aria-hidden': 'true' }), p.label); }
function okPill(ok) { return ok === true ? pill({ key: 'ok', label: 'Succeeded' }) : ok === false ? pill({ key: 'crit', label: 'Failed' }) : pill({ key: 'none', label: 'Unknown' }); }

// ---------- state ----------
var HOUR = 3600000, DAY = 86400000;
var RANGES = { '6h': 'last 6 hours', '24h': 'last 24 hours', '7d': 'last 7 days' };
var BUCKETS = 48;
// How long derived data (health history, secret names) is reused before it
// is asked for again. The main listing is refreshed far more often; these
// change slowly and cost one request per app.
var EXTRA_TTL = 60000;

var S = {
  mode: 'signin',        // signin | loading | ready | offline
  token: '', gateError: '', gateBusy: false,
  info: null, apps: [], events: [], releases: [],
  engineError: '', offlineError: '', updated: 0,
  health: new Map(),     // "app|range" -> { at, hist }
  secrets: new Map(),    // app -> { at, list, error }
  allSecrets: null,      // { at, list, error }: GET /v1/secrets, every app's
  tokens: null,          // { at, list, error }: GET /v1/tokens
  logs: { app: '', text: '', error: '', loading: false, cut: false, following: false, ended: false },
  backups: null,         // { at, list, error }
  certs: null,           // { at, data, error }: GET /v1/certs
  keys: null,            // { at, data, error }: GET /v1/keys
  backupBusy: false, backupNote: null,   // note: { kind, text }
  note: null,            // { kind, text }: the outcome of the last action
  checks: new Map(),     // backup name -> { busy, report, error }
  q: '', only: 'all', actApp: '', actResult: '', tab: 'overview', range: '24h', every: 10, collapsed: false, drawer: false
};
// Maps, not plain objects, wherever a key comes from the server: an app
// called "constructor" must be an ordinary key.

function recall(key) { try { return window.localStorage.getItem(key); } catch (e) { return null; } }
function remember(key, value) { try { window.localStorage.setItem(key, value); } catch (e) { /* storage is optional */ } }
S.collapsed = recall('liman-side') === 'collapsed';

// The token lives in sessionStorage: it survives a reload, is never shared
// with another tab and is gone when this tab closes. If storage is not
// available it lives in memory only, and a reload asks for it again.
var TOKEN_KEY = 'liman-token';
function loadToken() { try { return str(window.sessionStorage.getItem(TOKEN_KEY)); } catch (e) { return ''; } }
function saveToken(t) { try { if (t) window.sessionStorage.setItem(TOKEN_KEY, t); else window.sessionStorage.removeItem(TOKEN_KEY); } catch (e) { /* memory only */ } }

var $ = function (id) { return document.getElementById(id); };
var $app = $('app'), $gate = $('gate'), $main = $('main'), $nav = $('nav'), $foot = $('foot'), $crumbs = $('crumbs'), $tip = $('tip'), $dlg = $('dialog');
var afterMount = [];

// ---------- talking to the daemon ----------
// Every request goes to the daemon the page came from, with the token in a
// header. Almost all of them are GETs; the exceptions are listed where
// send() is called, and each is something that cannot lose data.
function fail(status, message) { return { apiError: true, status: status, message: message }; }
function request(path, timeoutMs, verb, body) {
  var ctl = new AbortController(), timer = setTimeout(function () { ctl.abort(); }, timeoutMs || 15000);
  var opts = { method: verb || 'GET', headers: { Authorization: 'Bearer ' + S.token }, cache: 'no-store', credentials: 'omit', redirect: 'error', signal: ctl.signal };
  // A body is only ever a secret's value. It goes out as raw bytes and is
  // held nowhere else.
  if (body !== undefined && body !== null) { opts.body = body; opts.headers['Content-Type'] = 'application/octet-stream'; }
  return fetch(path, opts)
    .then(function (res) { return { res: res, done: function () { clearTimeout(timer); }, abort: function () { ctl.abort(); } }; },
      function () { clearTimeout(timer); throw fail(0, ctl.signal.aborted ? 'The daemon took too long to answer.' : 'The daemon cannot be reached.'); });
}
function errorOf(res) {
  return res.json().then(function (body) { return str(body && body.error, 300); }, function () { return ''; })
    .then(function (msg) { return fail(res.status, msg || 'The daemon answered with status ' + res.status + '.'); });
}
function get(path) {
  return request(path).then(function (r) {
    if (!r.res.ok) return errorOf(r.res).then(function (e) { r.done(); throw e; });
    return r.res.json().then(function (v) { r.done(); return v; }, function () { r.done(); throw fail(0, 'The daemon sent an answer that could not be read.'); });
  });
}
// send asks the daemon to do something. The browser adds an Origin header
// to such a request by itself; the daemon accepts it only from its own
// pages. Every request that changes anything goes through here or through
// stream(), and a test counts the places that call them.
function send(verb, path, body, timeoutMs) {
  return request(path, timeoutMs || 60000, verb, body).then(function (r) {
    if (!r.res.ok) return errorOf(r.res).then(function (e) { r.done(); throw e; });
    // 204 has no body to read.
    if (r.res.status === 204) { r.done(); return null; }
    return r.res.json().then(function (v) { r.done(); return v; }, function () { r.done(); throw fail(0, 'The daemon sent an answer that could not be read.'); });
  });
}
// stream sends a request whose answer arrives line by line while the work
// is going on (a rollback). onText gets each piece of progress output; the
// promise resolves with the final result.
function stream(verb, path, onText) {
  return request(path, 600000, verb).then(function (r) {
    if (!r.res.ok) return errorOf(r.res).then(function (e) { r.done(); throw e; });
    if (!r.res.body || !r.res.body.getReader) { r.done(); throw fail(0, 'This browser cannot follow the progress of the operation.'); }
    var reader = r.res.body.getReader(), dec = new TextDecoder(), buf = '', result = null;
    function take(line) {
      if (line.trim() === '') return;
      var ev; try { ev = JSON.parse(line); } catch (e) { return; }
      if (!ev || typeof ev !== 'object') return;
      if (ev.type === 'output' && typeof ev.text === 'string') onText(ev.text);
      if (ev.type === 'result' && ev.result && typeof ev.result === 'object') result = ev.result;
    }
    function pump() {
      return reader.read().then(function (chunk) {
        if (chunk.done) {
          take(buf); r.done();
          if (!result) throw fail(0, 'The connection ended before the daemon reported the outcome. Look at the app to see where it stands.');
          return result;
        }
        buf += dec.decode(chunk.value, { stream: true });
        var at; while ((at = buf.indexOf('\n')) !== -1) { take(buf.slice(0, at)); buf = buf.slice(at + 1); }
        return pump();
      }, function () { r.done(); throw fail(0, 'The connection was lost while the operation was running. Look at the app to see where it stands.'); });
    }
    return pump();
  });
}
// settle turns a promise into one that never rejects, so several requests
// can be waited for together without one failure hiding the others.
function settle(p) { return p.then(function (v) { return { ok: true, value: v }; }, function (e) { return { ok: false, error: e && e.apiError ? e : fail(0, 'Unexpected error.') }; }); }
function appPath(name, rest) { return '/v1/apps/' + encodeURIComponent(name) + rest; }

// seq numbers the refreshes. An answer that arrives after a newer refresh
// started, or after signing out, is dropped.
var seq = 0, refreshing = false;

function refresh() {
  if (refreshing || !S.token) return Promise.resolve();
  refreshing = true;
  var my = ++seq;
  $('refresh').disabled = true; $('refresh').className = 'btn busy';

  // /v1/info first and alone: it proves the token. If the token is wrong
  // this is the only request that fails, and failed authentications are
  // counted by the daemon.
  return settle(get('/v1/info')).then(function (info) {
    if (my !== seq) return null;
    if (!info.ok) { connectionFailed(info.error); return null; }
    S.info = info.value && typeof info.value === 'object' ? info.value : {};
    return Promise.all([settle(get('/v1/apps')), settle(get('/v1/history?limit=300')), settle(get('/v1/releases?limit=500'))]);
  }).then(function (parts) {
    if (!parts || my !== seq) return;
    var denied = parts.filter(function (p) { return !p.ok && p.error.status === 401; })[0];
    if (denied) { connectionFailed(denied.error); return; }

    // The app listing needs Docker; the rest does not. With Docker down the
    // panel still shows the history, and says what is wrong.
    if (parts[0].ok) { S.apps = buildApps(parts[0].value); S.engineError = ''; }
    else S.engineError = parts[0].error.message;
    if (parts[1].ok) S.events = cleanList(parts[1].value);
    if (parts[2].ok) S.releases = cleanList(parts[2].value);

    S.mode = 'ready'; S.offlineError = ''; S.updated = Date.now();
    render();
    return ensureExtras();
  }).then(null, function (e) { crash(e); }).then(function () {
    refreshing = false;
    $('refresh').disabled = false; $('refresh').className = 'btn';
  });
}

// rejected says why the daemon refused the token. It answers "token
// expired" for a token that has run out and nothing more specific for any
// other, so expiry is the one reason worth telling apart.
function rejected(err) {
  return /^token expired/.test(str(err && err.message, 100))
    ? 'This token has expired. Create a new one on the server with limand token create.'
    : 'The daemon rejected this token. It may have been revoked.';
}

// tokenLeft is how long the token has left, in milliseconds, or null when
// it never expires. It is measured with the daemon's clock, not this
// computer's: the daemon decides when the token stops working.
var EXPIRY_WARNING = 14 * 86400000;
function tokenLeft() {
  var end = S.info ? time(S.info.token_expires_at) : null, now = S.info ? time(S.info.now) : null;
  return end === null || now === null ? null : end - now;
}
// roughly says a span the way limand token list does: days and hours to
// the nearest, each only from two of it upwards, so that 47 hours is never
// "1 day" or "2 days".
function roughly(ms) {
  function n(v, unit) { return v + ' ' + unit + (v === 1 ? '' : 's'); }
  if (ms >= 2 * 86400000) return n(Math.round(ms / 86400000), 'day');
  if (ms >= 2 * 3600000) return n(Math.round(ms / 3600000), 'hour');
  if (ms >= 60000) return n(Math.floor(ms / 60000), 'minute');
  return 'less than a minute';
}

function connectionFailed(err) {
  if (err.status === 401) { signOut(rejected(err)); return; }
  if (err.status === 429) { signOut(err.message); return; }
  // Unreachable, or an error of the daemon's own: keep what is on screen,
  // mark it as stale and keep trying.
  if (S.mode === 'loading' && !S.info) { signOut(err.message, true); return; }
  S.mode = 'offline'; S.offlineError = err.message;
  render();
}

function cleanList(v) { return Array.isArray(v) ? v.filter(function (x) { return x && typeof x === 'object'; }) : []; }

// buildApps turns the container listing into one entry per app: its
// container in service plus the ones kept for a rollback.
function buildApps(list) {
  var by = new Map();
  cleanList(list).forEach(function (c) {
    var name = str(c.app); if (name === '') return;
    if (!by.has(name)) by.set(name, { current: null, previous: [] });
    if (str(c.role) === 'previous') by.get(name).previous.push(c); else by.get(name).current = c;
  });
  var out = [];
  by.forEach(function (g, name) {
    var c = g.current || g.previous[g.previous.length - 1], port = num(c.host_port);
    out.push({
      name: name, image: c.image, domain: c.domain, state: c.state, health: c.health, note: c.health_detail,
      restarts: num(c.restarts) || 0, release: c.release, container: c.container, uptime: c.status,
      address: port ? '127.0.0.1:' + port : '', healthcheck: c.healthcheck, deployed: c.created,
      previous: g.current ? g.previous : []
    });
  });
  return out;
}

// ensureExtras loads what the current view needs beyond the main listing
// and draws again when something new arrived.
function ensureExtras() {
  if (S.mode !== 'ready') return Promise.resolve();
  var r = route(), my = seq, now = Date.now(), jobs = [];

  function wantHealth(name) {
    var key = name + '|' + S.range, have = S.health.get(key);
    if (have && now - have.at < EXTRA_TTL) return;
    jobs.push(settle(get(appPath(name, '/health?range=' + encodeURIComponent(S.range)))).then(function (res) {
      if (my !== seq && !S.token) return;
      S.health.set(key, { at: Date.now(), hist: res.ok ? res.value : null, error: res.ok ? '' : res.error.message });
    }));
  }
  function wantSecrets(name) {
    var have = S.secrets.get(name);
    if (have && now - have.at < EXTRA_TTL) return;
    jobs.push(settle(get(appPath(name, '/secrets'))).then(function (res) {
      if (my !== seq && !S.token) return;
      S.secrets.set(name, { at: Date.now(), list: res.ok ? cleanList(res.value) : null, error: res.ok ? '' : res.error.message });
    }));
  }

  var names = S.apps.map(function (a) { return a.name; });
  var monitored = !!(S.info && S.info.monitor), vault = !!(S.info && S.info.secrets);
  if (r.view === 'overview' && monitored) names.slice(0, 20).forEach(wantHealth);
  if (r.view === 'app' && names.indexOf(r.name) !== -1) {
    if (S.tab === 'overview' && monitored) wantHealth(r.name);
    if (S.tab === 'secrets' && vault) wantSecrets(r.name);
  }
  // The Secrets page lists every app's secrets in one request, apps that
  // are not deployed included: those are the ones nobody would see
  // otherwise.
  if (r.view === 'secrets' && vault && !(S.allSecrets && now - S.allSecrets.at < EXTRA_TTL)) {
    jobs.push(settle(get('/v1/secrets')).then(function (res) {
      if (!S.token) return;
      S.allSecrets = { at: Date.now(), list: res.ok ? cleanList(res.value) : null, error: res.ok ? '' : res.error.message };
    }));
  }
  if (r.view === 'tokens' && !tokenApps() && !(S.tokens && now - S.tokens.at < EXTRA_TTL)) {
    jobs.push(settle(get('/v1/tokens')).then(function (res) {
      if (!S.token) return;
      S.tokens = { at: Date.now(), list: res.ok ? cleanList(res.value) : null, error: res.ok ? '' : res.error.message };
    }));
  }
  // Certificates are wanted everywhere: an expired one is a banner on
  // every page. Reading them is cheap. The keys are not (the daemon checks
  // every backup's signature), so only the Daemon page asks for them.
  if (S.info && S.info.certificates && !tokenApps() && !(S.certs && now - S.certs.at < EXTRA_TTL)) {
    jobs.push(settle(get('/v1/certs')).then(function (res) {
      if (!S.token) return;
      S.certs = { at: Date.now(), data: res.ok && res.value && typeof res.value === 'object' ? res.value : null, error: res.ok ? '' : res.error.message };
    }));
  }
  if (r.view === 'daemon' && S.info && S.info.secrets && !tokenApps() && !(S.keys && now - S.keys.at < EXTRA_TTL)) {
    jobs.push(settle(get('/v1/keys')).then(function (res) {
      if (!S.token) return;
      S.keys = { at: Date.now(), data: res.ok && res.value && typeof res.value === 'object' ? res.value : null, error: res.ok ? '' : res.error.message };
    }));
  }
  if (r.view === 'backups' && S.info && S.info.backups && !tokenApps() && !(S.backups && now - S.backups.at < EXTRA_TTL)) {
    jobs.push(settle(get('/v1/backups')).then(function (res) {
      if (!S.token) return;
      S.backups = { at: Date.now(), list: res.ok && res.value && typeof res.value === 'object' ? res.value : null, error: res.ok ? '' : res.error.message };
    }));
  }

  if (jobs.length === 0) return Promise.resolve();
  return Promise.all(jobs).then(function () { if (S.token) render(); });
}

// ---------- backups: the two things the panel can ask the daemon to do ----------
function reportText(rep) {
  var sig = str(rep && rep.signature);
  if (sig === 'invalid') return 'its signature does not match: the file was changed after it was written, or it belongs to another master key. Do not restore it';
  if (sig === 'unknown-key') return 'it was signed with master key ' + str(rep.signed_by, 32) + ', which this server does not have (retired, or from another server). It cannot be checked or restored without that key';
  return (sig === 'valid' ? 'signed, ' : sig === 'missing' ? 'not signed, ' : sig === 'unchecked' ? 'signature not checked, ' : '') + contentText(rep);
}
function contentText(rep) {
  var n = num(rep && rep.secrets) || 0, bad = Array.isArray(rep && rep.undecryptable) ? rep.undecryptable : [];
  if (bad.length) return bad.length + ' of ' + n + (n === 1 ? ' secret' : ' secrets') + ' cannot be decrypted with the server\u2019s master key: ' + bad.map(function (x) { return str(x, 80); }).slice(0, 5).join(', ');
  if (n > 0 && !(rep && rep.key_checked)) return 'intact, but ' + (n === 1 ? 'its secret was' : 'its ' + n + ' secrets were') + ' not tested: the server has no master key';
  return 'intact' + (n === 1 ? '; its secret can be decrypted' : n > 1 ? '; all ' + n + ' secrets can be decrypted' : '');
}
function backUpNow() {
  if (S.backupBusy) return;
  S.backupBusy = true; S.backupNote = null; render();
  send('POST', '/v1/backups', null, 120000).then(function (res) {
    var rep = res && res.report, b = (res && res.backup) || {};
    var removed = num(res && res.removed) || 0;
    S.backupNote = { kind: rep && rep.ok ? 'ok' : 'warn', text: 'Wrote ' + orDash(b.name, 80) + ' (' + size(b.size_bytes) + '): ' + reportText(rep) + '.' + (removed ? ' Removed ' + removed + ' older ' + (removed === 1 ? 'backup.' : 'backups.') : '') };
    if (b.name && rep) S.checks.set(str(b.name), { busy: false, report: rep, error: '' });
  }, function (e) {
    if (e && e.status === 401) { signOut(rejected(e)); return; }
    S.backupNote = { kind: 'crit', text: 'No backup was made. ' + str(e && e.message, 300) };
  }).then(function () {
    S.backupBusy = false; S.backups = null;
    if (!S.token) return;
    render(); return ensureExtras();
  });
}
function checkBackup(name) {
  S.checks.set(name, { busy: true, report: null, error: '' }); render();
  send('POST', '/v1/backups/' + encodeURIComponent(name) + '/verify', null, 120000).then(function (rep) {
    S.checks.set(name, { busy: false, report: rep, error: '' });
  }, function (e) {
    if (e && e.status === 401) { signOut(rejected(e)); return; }
    S.checks.set(name, { busy: false, report: null, error: str(e && e.message, 300) || 'The check failed.' });
  }).then(function () { if (S.token) render(); });
}

// Logs are read on demand, or followed: then the connection stays open
// and what the container writes is appended as it comes. The daemon starts
// with the last 200 lines either way. Only the end is kept, so a chatty app
// cannot fill the tab's memory.
var LOG_KEEP = 200000, LOG_LINES = 400, logRun = 0, logStop = null;
// FOLLOW_LIMIT ends a followed stream after a while; Follow starts it
// again. A tab left open overnight should not hold a connection forever.
var FOLLOW_LIMIT = 6 * 3600000;
function emptyLogs(name) { return { app: name || '', text: '', error: '', loading: false, cut: false, following: false, ended: false }; }
// lastLines keeps the last LOG_LINES lines of text.
function lastLines(text) {
  var lines = text.replace(/\r\n?/g, '\n').split('\n');
  return lines.length > LOG_LINES ? { text: lines.slice(-LOG_LINES).join('\n'), cut: true } : { text: lines.join('\n'), cut: false };
}
// stopFollowing closes a followed stream, if there is one. Whatever it had
// shown stays on screen.
function stopFollowing() {
  logRun++;
  if (logStop) { logStop(); logStop = null; }
  if (S.logs.following) { S.logs.following = false; S.logs.loading = false; }
}
function loadLogs(name, follow) {
  stopFollowing();
  var my = logRun;
  S.logs = emptyLogs(name); S.logs.loading = true; S.logs.following = !!follow;
  render();
  request(appPath(name, '/logs') + (follow ? '?follow=true' : ''), follow ? FOLLOW_LIMIT : 20000).then(function (r) {
    if (my !== logRun) { r.abort(); r.done(); return null; }
    if (!r.res.ok) return errorOf(r.res).then(function (e) { r.done(); throw e; });
    if (!r.res.body || !r.res.body.getReader) {
      if (follow) { r.abort(); r.done(); throw fail(0, 'This browser cannot follow a stream. The command line can: liman logs -f ' + name); }
      return r.res.text().then(function (t) { r.done(); return { text: t.slice(-LOG_KEEP), cut: t.length > LOG_KEEP }; });
    }
    var reader = r.res.body.getReader(), dec = new TextDecoder(), text = '', cut = false, paintTimer = null;
    if (follow) {
      logStop = function () { clearTimeout(paintTimer); r.abort(); r.done(); };
      // The stream is open: what there is to show is shown, even before
      // the container writes anything.
      S.logs.loading = false; render();
    }
    // paint puts the latest text on screen at most a few times a second;
    // a busy app would otherwise redraw for every chunk.
    function paint() {
      paintTimer = null;
      if (my !== logRun) return;
      var kept = lastLines(text);
      S.logs.text = kept.text; S.logs.cut = cut || kept.cut;
      var el = $('logs');
      if (!el) { render(); scrollLogsToEnd(); return; }
      // Only keep up with the end when the reader is there; someone who
      // scrolled up to read is left where they are.
      var atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
      el.textContent = S.logs.text;
      if (atEnd) scrollLogsToEnd();
    }
    function pump() {
      return reader.read().then(function (chunk) {
        if (chunk.done) { r.done(); return { text: text, cut: cut, ended: follow }; }
        text += dec.decode(chunk.value, { stream: true });
        if (text.length > LOG_KEEP * 2) { text = text.slice(-LOG_KEEP); cut = true; }
        if (follow && !paintTimer) paintTimer = setTimeout(paint, 200);
        return pump();
      }, function () { r.done(); return { text: text, cut: true, stopped: true }; });
    }
    return pump();
  }).then(function (out) {
    if (!out || my !== logRun) return;
    logStop = null;
    var kept = lastLines(out.text);
    S.logs = emptyLogs(name);
    S.logs.text = kept.text; S.logs.cut = out.cut || kept.cut; S.logs.ended = !!out.ended;
    if (out.stopped) S.logs.error = follow ? 'Following stopped: the connection to the daemon ended. Follow again to go on.' : 'Reading stopped before the end; the log is very large. The command line shows all of it.';
    render(); scrollLogsToEnd();
  }, function (e) {
    if (my !== logRun) return;
    logStop = null;
    if (e && e.status === 401) { signOut(rejected(e)); return; }
    S.logs = emptyLogs(name); S.logs.error = e && e.apiError ? e.message : 'The logs could not be read.';
    render();
  });
}
function scrollLogsToEnd() { var el = $('logs'); if (el) el.scrollTop = el.scrollHeight; }

// ---------- signing in and out ----------
function signIn(token) {
  lastActive = Date.now();
  token = str(token);
  if (token === '') { S.gateError = 'Paste a token first.'; renderGate(); return; }
  // A header value cannot contain these; refusing here gives a clear
  // message instead of a failed request.
  if (/[^\x21-\x7e]/.test(token)) { S.gateError = 'That does not look like a token: it contains spaces or other characters a token never has.'; renderGate(); return; }
  S.token = token; S.gateError = ''; S.gateBusy = true; S.mode = 'loading'; S.info = null;
  renderGate();
  refresh().then(function () {
    S.gateBusy = false;
    if (S.mode === 'ready') saveToken(S.token);
    render();
  });
}
function signOut(message, keepToken) {
  seq++; stopFollowing();
  var token = keepToken ? S.token : '';
  if (!keepToken) saveToken('');
  S.token = ''; S.mode = 'signin'; S.gateError = str(message, 300); S.gateBusy = false;
  S.info = null; S.apps = []; S.events = []; S.releases = []; S.engineError = ''; S.offlineError = ''; S.updated = 0;
  S.health = new Map(); S.secrets = new Map(); S.logs = emptyLogs();
  S.backups = null; S.backupBusy = false; S.backupNote = null; S.checks = new Map(); S.note = null;
  S.certs = null; S.keys = null; S.allSecrets = null; S.tokens = null;
  // What the last page showed is not merely hidden behind the sign-in
  // form: it is taken out of the page.
  clear($main);
  closeDialog();
  S.pending = token;
  render();
}
function renderGate() {
  clear($gate);
  var $err = h('div', { class: 'err', id: 'gate-err', role: 'alert', text: S.gateError });
  var $in = h('input', { class: 'input mono', id: 'token', type: 'password', autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false', maxlength: '512', 'aria-describedby': 'gate-err gate-hint',
    'aria-invalid': S.gateError ? 'true' : null, disabled: S.gateBusy, value: S.pending || null });
  var form = h('form', { class: 'gate-card', novalidate: true, onsubmit: function (e) { e.preventDefault(); S.pending = ''; signIn($in.value); } },
    h('div', { class: 'gate-brand' }, h('div', { class: 'logo', 'aria-hidden': 'true', text: 'li' }), h('div', { class: 'brand', text: 'liman' })),
    h('h1', { text: 'Sign in to the panel' }),
    h('p', null, 'The panel reads from the daemon at ', h('code', { text: str(window.location.host, 80) }), ' with an API token, the same kind the command line uses.'),
    h('div', { class: 'field' }, h('label', { for: 'token' }, 'API token'), $in, $err),
    h('button', { class: 'btn primary', type: 'submit', disabled: S.gateBusy }, S.gateBusy ? 'Checking…' : 'Sign in'),
    h('p', { class: 'hint', id: 'gate-hint' }, 'Create one on the server with ', h('code', { text: 'limand token create panel' }), ', or with ', h('code', { text: '--read-only' }), ' for a panel that can only look. It is kept in this tab only and forgotten when the tab closes. From the panel you can look at everything, roll back or stop an app, manage its secrets, revoke tokens and make backups. Deploying a new version is done with the command line.'));
  $gate.appendChild(form);
  if (!S.gateBusy) { try { $in.focus(); } catch (e) { /* not focusable yet */ } }
}

// ---------- routing: plain #name hashes only ----------
function route() {
  var hash = ''; try { hash = decodeURIComponent((window.location.hash || '').replace(/^#/, '')); } catch (e) { hash = ''; }
  if (['apps', 'activity', 'secrets', 'tokens', 'daemon', 'backups'].indexOf(hash) !== -1) return { view: hash };
  if (hash.indexOf('app-') === 0) return { view: 'app', name: hash.slice(4) };
  return { view: 'overview' };
}
function appHref(name) { return '#app-' + encodeURIComponent(name); }
function findApp(name) { return S.apps.filter(function (a) { return a.name === name; })[0] || null; }
function byAttention(a, b) { return (status(b).attn - status(a).attn) || a.name.localeCompare(b.name); }
function sortedEvents() { return S.events.slice().sort(function (a, b) { return (time(b.time) || 0) - (time(a.time) || 0); }); }

// ---------- sidebar and top bar ----------
function navItem(view, label, ic, current, badge, attn) {
  return h('a', { class: 'nav', href: '#' + view, title: S.collapsed ? label : null, 'aria-current': current ? 'page' : null }, icon(ic), h('span', { class: 'lbl trunc', text: label }),
    badge ? h('span', { class: 'badge' + (attn ? ' attn' : ''), text: badge }) : null);
}
function renderSide(r) {
  clear($nav);
  var list = S.apps.slice().sort(byAttention), attn = list.filter(function (a) { return status(a).attn; }).length;
  add($nav, [h('div', { class: 'side-label', text: 'Operate' }),
    navItem('overview', 'Overview', 'overview', r.view === 'overview'),
    navItem('apps', 'Apps', 'apps', r.view === 'apps', S.info ? (attn ? attn + ' of ' + list.length : String(list.length)) : '', attn > 0)]);
  if (list.length) {
    var shown = list.slice(0, 12);
    add($nav, h('div', { class: 'sub' }, shown.map(function (a) {
      var st = status(a);
      return h('a', { class: 'nav', href: appHref(a.name), title: a.name + ' · ' + st.label, 'aria-current': r.view === 'app' && r.name === a.name ? 'page' : null },
        h('i', { class: 'dot ' + st.key, 'aria-hidden': 'true' }), h('span', { class: 'lbl trunc', text: a.name }), h('span', { class: 'sr', text: st.label }));
    }), list.length > shown.length ? h('a', { class: 'nav muted', href: '#apps' }, h('span', { class: 'lbl', text: 'All ' + list.length + ' apps' })) : null));
  }
  add($nav, [navItem('activity', 'Activity', 'activity', r.view === 'activity'), navItem('secrets', 'Secrets', 'secrets', r.view === 'secrets'),
    h('div', { class: 'side-label', text: 'System' }), tokenApps() ? null : navItem('tokens', 'Tokens', 'tokens', r.view === 'tokens'), tokenApps() ? null : navItem('backups', 'Backups', 'backups', r.view === 'backups'), navItem('daemon', 'Daemon', 'daemon', r.view === 'daemon')]);
  renderFoot();
  $app.className = 'app' + (S.collapsed ? ' collapsed' : '') + (S.drawer ? ' drawer' : '');
}
function renderFoot() {
  clear($foot);
  var p = S.mode === 'offline' ? ['crit', 'Unreachable'] : S.mode === 'loading' ? ['info', 'Connecting'] : ['ok', readOnly() ? 'Connected, read-only' : 'Connected'];
  add($foot, [h('div', { class: 'conn', title: p[1] + ' · ' + str(window.location.host, 80) }, h('i', { class: 'dot ' + p[0], 'aria-hidden': 'true' }),
      h('span', { class: 'lbl trunc' }, p[1], S.updated ? h('span', { class: 'muted', id: 'updated', text: ' · updated ' + ago(S.updated) }) : null)),
    h('button', { class: 'nav', id: 'signout', title: S.collapsed ? 'Sign out' : null, onclick: function () { signOut(''); } }, icon('signout'), h('span', { class: 'lbl', text: 'Sign out' })),
    h('button', { class: 'nav', id: 'collapse', 'aria-label': S.collapsed ? 'Expand navigation' : 'Collapse navigation', onclick: function () {
      S.collapsed = !S.collapsed; remember('liman-side', S.collapsed ? 'collapsed' : 'open'); render(); } },
      icon(S.collapsed ? 'expand' : 'collapse'), h('span', { class: 'lbl', text: 'Collapse' }))]);
}
function renderCrumbs(r) {
  clear($crumbs);
  var names = { overview: 'Overview', apps: 'Apps', activity: 'Activity', secrets: 'Secrets', tokens: 'Tokens', daemon: 'Daemon', backups: 'Backups' };
  var parts = r.view === 'app' ? [['#apps', 'Apps'], [null, str(r.name, 80)]] : [[null, names[r.view]]];
  add($crumbs, [h('a', { href: '#overview', text: 'liman' })]);
  parts.forEach(function (p) { add($crumbs, [h('span', { class: 'sep', 'aria-hidden': 'true', text: '/' }), p[0] ? h('a', { href: p[0], text: p[1] }) : h('span', { class: 'here trunc', title: p[1], text: p[1] })]); });
}

// ---------- shared blocks ----------
function panel(title, body, opts) {
  opts = opts || {};
  return h('section', { class: 'panel ' + (opts.span || 'c12') }, h('div', { class: 'panel-h' }, h('h2', { text: title }), opts.side || null), h('div', { class: 'panel-b' + (opts.flush ? ' flush' : '') }, body));
}
function stat(title, value, caption, opts) {
  opts = opts || {};
  return h('section', { class: 'panel c3' }, h('div', { class: 'panel-h' }, h('h2', { text: title })), h('div', { class: 'stat ' + (opts.tone || '') }, h('b', { class: opts.small ? 'small' : null, title: opts.small ? value : null, text: value }), h('span', { text: caption })));
}
function stateBlock(title, body, action, tight) { return h('div', { class: 'state' + (tight ? ' tight' : '') }, h('h3', { text: title }), body || null, action || null); }
function fullState(title, body, action) { return h('div', { class: 'panel' }, stateBlock(title, body, action)); }
function banners() {
  var out = [];
  if (S.note) out.push(h('div', { class: 'banner ' + (S.note.kind === 'ok' ? 'info' : 'crit'), role: 'status' }, h('div', { class: 'grow wrap-any', text: S.note.text }),
    h('button', { class: 'btn small', onclick: function () { S.note = null; render(); } }, 'Dismiss')));
  if (S.mode === 'offline') out.push(h('div', { class: 'banner crit', role: 'alert' }, h('div', { class: 'grow' }, h('b', { text: 'The daemon is not answering. ' }),
    'This is the status from ' + clock(S.updated, true) + ' and may be out of date. ' + str(S.offlineError, 200)),
    h('button', { class: 'btn small', onclick: function () { refresh(); } }, 'Try again')));
  else if (S.engineError) out.push(h('div', { class: 'banner crit', role: 'alert' }, h('div', { class: 'grow' }, h('b', { text: 'The daemon cannot reach Docker. ' }),
    'The app list below is the last one it could read. ' + str(S.engineError, 240))));
  var left = tokenLeft();
  if (S.mode === 'ready' && left !== null && left < EXPIRY_WARNING) out.push(h('div', { class: 'banner warn', role: 'status' }, h('div', { class: 'grow' },
    h('b', { text: 'This token expires in ' + roughly(Math.max(left, 0)) + '. ' }),
    'The panel signs out when it does. Create a new one on the server with ', h('code', { text: 'limand token create' }), ' and revoke this one.')));
  // A certificate is renewed when a third of its life is left (30 days of
  // Let's Encrypt's 90), twice a day. One still due with two weeks left
  // means renewing keeps failing; one that expired is broken HTTPS. Only
  // certificates an app uses count: the rest are not renewed, on purpose.
  var certList = S.certs && S.certs.data && Array.isArray(S.certs.data.certificates) ? S.certs.data.certificates.filter(function (c) { return c && c.in_use; }) : [];
  var expired = certList.filter(function (c) { return c && c.status === 'expired'; });
  var failing = certList.filter(function (c) { var end = c && time(c.not_after); return c && c.status === 'renewal due' && end !== null && end - Date.now() < 14 * DAY; });
  if (S.mode === 'ready' && expired.length) out.push(h('div', { class: 'banner crit', role: 'alert' }, h('div', { class: 'grow wrap-any' },
    h('b', { text: (expired.length === 1 ? 'The certificate for ' : expired.length + ' certificates have expired: ') + expired.slice(0, 3).map(function (c) { return str(c.domain, 80); }).join(', ') + (expired.length === 1 ? ' has expired. ' : '. ') }),
    'Browsers refuse HTTPS for it. ', S.certs.data.acme ? 'liman keeps trying to renew; the reason is in the daemon log (journalctl -u limand | grep certificate), and limand cert renew <domain> tries at once.' : 'acme is not set up, so liman does not renew certificates; replace the files in cert_dir.')));
  if (S.mode === 'ready' && failing.length) out.push(h('div', { class: 'banner warn', role: 'status' }, h('div', { class: 'grow wrap-any' },
    h('b', { text: 'Renewing ' + failing.slice(0, 3).map(function (c) { return str(c.domain, 80); }).join(', ') + ' keeps failing. ' }),
    'It expires in ' + roughly(Math.max(time(failing[0].not_after) - Date.now(), 0)) + '. ', S.certs.data.acme ? 'The reason is in the daemon log (journalctl -u limand | grep certificate); limand cert renew <domain> tries at once.' : 'acme is not set up, so liman does not renew certificates.')));
  if (S.mode === 'ready' && S.info && S.info.key_rotation_unfinished) out.push(h('div', { class: 'banner warn', role: 'status' }, h('div', { class: 'grow' },
    h('b', { text: 'A master key rotation did not finish. ' }), 'Every secret still opens. Finish it on the server with ', h('code', { text: 'limand key rotate' }), ' while the daemon is stopped.')));
  var skew = S.info ? time(S.info.now) : null;
  if (S.mode === 'ready' && skew !== null && Math.abs(skew - S.updated) > 120000) out.push(h('div', { class: 'banner warn' }, h('div', { class: 'grow' },
    'The clock of the server and the clock of this computer are ' + Math.round(Math.abs(skew - S.updated) / 60000) + ' minutes apart. Times such as “5m ago” are off by that much.')));
  return out;
}
function skeleton() {
  var cells = [];
  for (var i = 0; i < 4; i++) cells.push(h('div', { class: 'panel c3' }, h('div', { class: 'panel-h' }, h('div', { class: 'skel', css: { width: '45%' } })), h('div', { class: 'stat' }, h('div', { class: 'skel', css: { width: '30%', height: '30px' } }))));
  var lines = []; for (var j = 0; j < 6; j++) lines.push(h('div', { class: 'skel', css: { width: (96 - j * 7) + '%' } }));
  cells.push(h('div', { class: 'panel c12' }, h('div', { class: 'panel-h' }, h('div', { class: 'skel', css: { width: '160px' } })), h('div', { class: 'panel-b skel-stack' }, lines)));
  return h('div', { class: 'grid', 'aria-busy': 'true', 'aria-label': 'Loading' }, cells);
}

// ---------- tooltip shared by every chart ----------
function showTip(text, x, y) {
  $tip.textContent = text; $tip.hidden = false;
  var w = $tip.offsetWidth, hh = $tip.offsetHeight, vw = window.innerWidth;
  $tip.style.left = Math.max(8, Math.min(vw - w - 8, x + 12)) + 'px';
  $tip.style.top = Math.max(8, y - hh - 12) + 'px';
}
function hideTip() { $tip.hidden = true; }

// ---------- health history, shared by the timeline and the line chart ----------
// frame returns the stretch of time a history covers, in the daemon's own
// numbers, or null if the answer does not make sense.
function frame(hist) {
  if (!hist || typeof hist !== 'object') return null;
  var since = time(hist.since), until = time(hist.until), step = num(hist.bucket_seconds);
  if (since === null || until === null || !step || until <= since) return null;
  return { since: since, until: until, step: step * 1000, buckets: cleanList(hist.buckets) };
}
// states spreads the buckets the daemon sent over the fixed number of
// slots. A slot without a bucket stays "none": nothing was measured then.
function states(f) {
  var out = [], n = Math.min(400, Math.max(1, Math.round((f.until - f.since) / f.step)));
  for (var i = 0; i < n; i++) out.push({ key: 'none', ok: 0, failed: 0 });
  f.buckets.forEach(function (b) {
    var t = time(b.start); if (t === null) return;
    var i = Math.floor((t - f.since) / f.step); if (i < 0 || i >= n) return;
    var ok = num(b.ok) || 0, failed = num(b.failed) || 0;
    out[i] = { key: failed > 0 ? (ok > 0 ? 'warn' : 'crit') : ok > 0 ? 'ok' : 'none', ok: ok, failed: failed };
  });
  return out;
}

// ---------- chart: health timeline ----------
// Status colours carry meaning here, so the legend names each one and the
// tooltip spells the state out: colour is never the only signal.
var TL_LABEL = { ok: 'Healthy', warn: 'Some checks failed', crit: 'All checks failed', none: 'No data' };
function timelinePanel(list, spanClass) {
  var title = 'Health over the ' + RANGES[S.range];
  if (!(S.info && S.info.monitor)) return panel(title, stateBlock('The health monitor is turned off', h('p', null, 'Without it the daemon does not check the apps, so there is no history to show. It is switched on in ', h('code', { text: 'config.yaml' }), '.'), null, true), { span: spanClass });

  var axisFrame = null;
  var rows = list.map(function (a) {
    var have = S.health.get(a.name + '|' + S.range), f = have ? frame(have.hist) : null, label = list.length > 1
      ? h('a', { class: 'trunc', href: appHref(a.name), title: a.name, text: a.name }) : h('span', { class: 'muted', text: 'Health checks' });
    if (!f) {
      var empty = []; for (var i = 0; i < BUCKETS; i++) empty.push(h('i'));
      return h('div', { class: 'tl-row' }, label, h('div', { class: 'tl-band', role: 'img', 'aria-label': a.name + ': ' + (have ? 'history could not be read' : 'loading'), title: have ? str(have.error, 200) || 'The history could not be read.' : 'Loading…' }, empty));
    }
    axisFrame = axisFrame || f;
    var st = states(f);
    var band = h('div', { class: 'tl-band', role: 'img', 'aria-label': str(a.name, 60) + ': ' + summarise(st) }, st.map(function (v) { return h('i', { class: v.key }); }));
    function move(e) {
      var rect = band.getBoundingClientRect(), i = Math.max(0, Math.min(st.length - 1, Math.floor(((e.clientX - rect.left) / Math.max(1, rect.width)) * st.length)));
      var from = f.since + i * f.step, v = st[i], counts = v.key === 'none' ? '' : ' (' + v.ok + ' ok, ' + v.failed + ' failed)';
      showTip(str(a.name, 40) + ' · ' + (S.range === '7d' ? day(from) + ' ' : '') + clock(from) + '–' + clock(from + f.step) + ' · ' + TL_LABEL[v.key] + counts, e.clientX, rect.top);
    }
    band.addEventListener('pointermove', move); band.addEventListener('pointerdown', move); band.addEventListener('pointerleave', hideTip);
    return h('div', { class: 'tl-row' }, label, band);
  });
  var far = S.range === '7d';
  var axis = axisFrame ? h('div', { class: 'tl-row' }, h('span'), h('div', { class: 'tl-axis' },
    h('span', { text: far ? day(axisFrame.since) : clock(axisFrame.since) }), h('span', { text: far ? day((axisFrame.since + axisFrame.until) / 2) : clock((axisFrame.since + axisFrame.until) / 2) }), h('span', { text: 'now' }))) : null;
  var legend = h('div', { class: 'legend' }, ['ok', 'warn', 'crit', 'none'].map(function (k) { return h('span', null, h('i', { class: k }), TL_LABEL[k]); }));
  return panel(title, h('div', { class: 'tl' }, rows, axis), { span: spanClass, side: legend });
}
function summarise(st) {
  var c = { ok: 0, warn: 0, crit: 0, none: 0 }; st.forEach(function (v) { c[v.key]++; });
  return ['ok', 'warn', 'crit', 'none'].filter(function (k) { return c[k]; }).map(function (k) { return Math.round((c[k] / st.length) * 100) + '% ' + TL_LABEL[k].toLowerCase(); }).join(', ');
}

// ---------- chart: one line (response time) ----------
// A single series: the panel title names it, so there is no legend. Where
// nothing was measured the line stops instead of bridging the gap.
function niceMax(v) { if (!(v > 0)) return 1; var p = Math.pow(10, Math.floor(Math.log10(v))), n = v / p; return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p; }
function latencyChart(a) {
  var box = h('div', { class: 'chart' });
  if (!(S.info && S.info.monitor)) { box.appendChild(stateBlock('The health monitor is turned off', null, null, true)); return box; }
  if (str(a.healthcheck) === '') { box.appendChild(stateBlock('This app defines no health check', h('p', null, 'Add a ', h('code', { text: 'healthcheck:' }), ' section to its app.yaml and the daemon will measure how long each check takes.'), null, true)); return box; }
  var have = S.health.get(a.name + '|' + S.range), f = have ? frame(have.hist) : null;
  if (!have) { box.appendChild(h('div', { class: 'skel-stack', 'aria-busy': 'true' }, h('div', { class: 'skel', css: { height: '120px' } }))); return box; }
  if (!f) { box.appendChild(stateBlock('The history could not be read', h('p', { text: str(have.error, 200) }), null, true)); return box; }

  var pts = [];
  f.buckets.forEach(function (b) {
    var t = time(b.start), avg = Number(b.avg_ms), max = Number(b.max_ms);
    if (t === null || !(num(b.probed) > 0) || !Number.isFinite(avg) || avg < 0) return;
    pts.push({ t: t + f.step / 2, v: Math.round(avg * 10) / 10, max: Number.isFinite(max) ? max : avg });
  });
  pts.sort(function (x, y) { return x.t - y.t; });
  if (pts.length === 0) { box.appendChild(stateBlock('No measurements in this range', h('p', { text: 'Response times appear here once the app has been checked a few times.' }), null, true)); return box; }

  afterMount.push(function () {
    var vals = pts.map(function (p) { return p.v; }), top = Math.max.apply(null, vals), last = pts[pts.length - 1];
    var W = Math.max(260, box.clientWidth), H = 190, L = 56, R = 12, T = 10, B = 24, max = niceMax(top);
    var X = function (t) { return L + ((t - f.since) / (f.until - f.since)) * (W - L - R); }, Y = function (v) { return T + (1 - v / max) * (H - T - B); };
    var svg = s('svg', { viewBox: '0 0 ' + W + ' ' + H, role: 'img', 'aria-label': 'Average response time of the health check, latest ' + last.v + ' ms, highest ' + top + ' ms' });
    [0, 0.5, 1].forEach(function (fr) { var y = Y(max * fr); add(svg, [s('line', { x1: L, x2: W - R, y1: y, y2: y, stroke: 'var(--line)', 'stroke-width': 1 }), s('text', { x: L - 8, y: y + 4, 'text-anchor': 'end', text: Math.round(max * fr * 10) / 10 + ' ms' })]); });
    var far = S.range === '7d';
    [[f.since, 'start'], [(f.since + f.until) / 2, 'middle'], [f.until, 'end']].forEach(function (p) { svg.appendChild(s('text', { x: X(p[0]), y: H - 6, 'text-anchor': p[1], text: far ? day(p[0]) : clock(p[0]) })); });

    // Split the points into runs of neighbouring buckets.
    var runs = [], run = [];
    pts.forEach(function (p, i) { if (i > 0 && p.t - pts[i - 1].t > f.step * 1.5) { runs.push(run); run = []; } run.push(p); });
    runs.push(run);
    runs.forEach(function (rn) {
      if (rn.length === 1) { svg.appendChild(s('circle', { r: 2.5, fill: 'var(--series)', cx: X(rn[0].t), cy: Y(rn[0].v) })); return; }
      var d = rn.map(function (p, i) { return (i ? 'L' : 'M') + X(p.t).toFixed(1) + ' ' + Y(p.v).toFixed(1); }).join(' ');
      add(svg, [s('path', { d: d + ' L' + X(rn[rn.length - 1].t).toFixed(1) + ' ' + Y(0) + ' L' + X(rn[0].t).toFixed(1) + ' ' + Y(0) + ' Z', fill: 'var(--series)', opacity: 0.12, stroke: 'none' }),
        s('path', { d: d, fill: 'none', stroke: 'var(--series)', 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' })]);
    });

    var cross = s('line', { y1: T, y2: H - B, stroke: 'var(--fg-3)', 'stroke-width': 1, visibility: 'hidden' });
    var dot = s('circle', { r: 4.5, fill: 'var(--series)', stroke: 'var(--panel)', 'stroke-width': 2, cx: X(last.t), cy: Y(last.v) });
    var hit = s('rect', { class: 'hit', x: L, y: T, width: W - L - R, height: H - T - B, fill: 'transparent' });
    function move(e) {
      var rect = svg.getBoundingClientRect(), x = ((e.clientX - rect.left) / Math.max(1, rect.width)) * W, best = 0, bd = Infinity;
      pts.forEach(function (p, i) { var dd = Math.abs(X(p.t) - x); if (dd < bd) { bd = dd; best = i; } });
      var p = pts[best]; cross.setAttribute('x1', X(p.t)); cross.setAttribute('x2', X(p.t)); cross.setAttribute('visibility', 'visible'); dot.setAttribute('cx', X(p.t)); dot.setAttribute('cy', Y(p.v));
      showTip((far ? day(p.t) + ' ' : '') + clock(p.t) + ' · average ' + p.v + ' ms, slowest ' + Math.round(p.max) + ' ms', e.clientX, rect.top + (Y(p.v) / H) * rect.height);
    }
    function leave() { hideTip(); cross.setAttribute('visibility', 'hidden'); dot.setAttribute('cx', X(last.t)); dot.setAttribute('cy', Y(last.v)); }
    hit.addEventListener('pointermove', move); hit.addEventListener('pointerdown', move); hit.addEventListener('pointerleave', leave);
    add(svg, [cross, dot, hit]); clear(box); box.appendChild(svg);
  });
  return box;
}

// ---------- chart: deploys per day, stacked succeeded + failed ----------
var DEPLOY_DAYS = 14;
function deploysPerDay() {
  var days = [], index = new Map(), today = new Date(); today.setHours(0, 0, 0, 0);
  for (var i = DEPLOY_DAYS - 1; i >= 0; i--) {
    var d = new Date(today.getFullYear(), today.getMonth(), today.getDate() - i);
    var entry = { t: d.getTime(), ok: 0, failed: 0 }; days.push(entry); index.set(d.toDateString(), entry);
  }
  S.releases.forEach(function (r) {
    var t = time(r.started_at); if (t === null) return;
    var entry = index.get(new Date(t).toDateString()); if (!entry) return;
    if (str(r.status) === 'succeeded') entry.ok++; else entry.failed++;
  });
  return days;
}
function topRound(x, y, w, hh, r) { r = Math.min(r, hh, w / 2); return 'M' + x + ' ' + (y + hh) + ' V' + (y + r) + ' Q' + x + ' ' + y + ' ' + (x + r) + ' ' + y + ' H' + (x + w - r) + ' Q' + (x + w) + ' ' + y + ' ' + (x + w) + ' ' + (y + r) + ' V' + (y + hh) + ' Z'; }
function barChart(days) {
  var box = h('div', { class: 'chart' }), total = days.reduce(function (n, d) { return n + d.ok + d.failed; }, 0);
  if (total === 0) { box.appendChild(stateBlock('No deploys in the last ' + DEPLOY_DAYS + ' days', h('p', { text: 'Each deploy is counted here on the day it ran.' }), null, true)); return box; }
  afterMount.push(function () {
    var W = Math.max(260, box.clientWidth), H = 190, L = 28, R = 8, T = 10, B = 24, max = Math.max(2, Math.max.apply(null, days.map(function (d) { return d.ok + d.failed; })));
    max = max % 2 ? max + 1 : max;
    var slot = (W - L - R) / days.length, bw = Math.max(4, Math.min(24, slot - 6)), Y = function (v) { return T + (1 - v / max) * (H - T - B); }, base = Y(0);
    var svg = s('svg', { viewBox: '0 0 ' + W + ' ' + H, role: 'img', 'aria-label': 'Deploys per day for ' + DEPLOY_DAYS + ' days, ' + total + ' in total' });
    [0, max / 2, max].forEach(function (v) { add(svg, [s('line', { x1: L, x2: W - R, y1: Y(v), y2: Y(v), stroke: 'var(--line)', 'stroke-width': 1 }), s('text', { x: L - 8, y: Y(v) + 4, 'text-anchor': 'end', text: v })]); });
    days.forEach(function (d, i) {
      var x = L + i * slot + (slot - bw) / 2, hOk = base - Y(d.ok), hF = base - Y(d.failed), g = s('g');
      if (d.ok > 0) g.appendChild(d.failed > 0 ? s('rect', { x: x, y: base - hOk, width: bw, height: hOk, fill: 'var(--series)' }) : s('path', { d: topRound(x, base - hOk, bw, hOk, 4), fill: 'var(--series)' }));
      if (d.failed > 0) g.appendChild(s('path', { d: topRound(x, base - hOk - hF - (d.ok > 0 ? 2 : 0), bw, hF, 4), fill: 'var(--st-crit)' }));
      var hit = s('rect', { x: L + i * slot, y: T, width: slot, height: H - T - B, fill: 'transparent' });
      function move(e) { showTip(day(d.t) + ' · ' + d.ok + ' succeeded' + (d.failed ? ', ' + d.failed + ' failed' : ''), e.clientX, svg.getBoundingClientRect().top + 20); }
      hit.addEventListener('pointermove', move); hit.addEventListener('pointerdown', move); hit.addEventListener('pointerleave', hideTip);
      add(svg, [g, hit]);
      if (i === 0 || i === days.length - 1 || i === Math.floor(days.length / 2)) svg.appendChild(s('text', { x: x + bw / 2, y: H - 6, 'text-anchor': i === 0 ? 'start' : i === days.length - 1 ? 'end' : 'middle', text: day(d.t) }));
    });
    clear(box); box.appendChild(svg);
  });
  return box;
}

// ---------- view: overview ----------
function noApps() {
  return fullState('No apps on this server yet', h('p', null, 'Deploy the first one from your machine with ', h('code', { text: 'liman deploy app.yaml' }), '. It shows up here as soon as its container starts.'));
}
function viewOverview() {
  var list = S.apps.slice().sort(byAttention);
  if (list.length === 0 && S.releases.length === 0) return [h('h1', { text: 'Overview' }), noApps()];
  var c = { ok: 0, attn: 0, restarts: 0 };
  list.forEach(function (a) { var st = status(a); if (st.attn) c.attn++; else if (st.key === 'ok') c.ok++; c.restarts += a.restarts; });
  var attn = list.filter(function (a) { return status(a).attn; });
  var attnBody = attn.length ? h('div', { class: 'list' }, attn.slice(0, 6).map(function (a) {
      return h('a', { href: appHref(a.name) }, h('div', { class: 't' }, pill(status(a)), h('b', { class: 'trunc', title: a.name, text: a.name })), h('span', { class: 'muted wrap-any', text: str(a.note, 240) || 'Open the app to see its recent activity and logs.' }));
    })) : stateBlock('Nothing needs attention', h('p', { text: list.length ? 'Every app is running.' : 'There are no apps.' }), null, true);
  var legend = h('div', { class: 'legend' }, h('span', null, h('i', { class: 'series' }), 'Succeeded'), h('span', null, h('i', { class: 'crit' }), 'Failed'));
  var recent = sortedEvents().slice(0, 6);
  return [h('div', { class: 'grid' },
    stat('Apps', fmtNum(list.length), 'on this server'),
    stat('Running normally', fmtNum(c.ok), 'up and passing their checks', { tone: c.ok ? 'okc' : '' }),
    stat('Need attention', fmtNum(c.attn), c.attn ? 'unhealthy, restarting or stopped' : 'nothing to do', { tone: c.attn ? 'crit' : '' }),
    stat('Automatic restarts', fmtNum(c.restarts), 'since the apps were last stable'),
    list.length ? timelinePanel(list.slice(0, 20), 'c12') : null,
    panel('Needs attention', attnBody, { span: 'c5', flush: true }),
    panel('Deploys per day', barChart(deploysPerDay()), { span: 'c7', side: legend }),
    panel('Latest activity', recent.length ? eventTable(recent, true) : stateBlock('Nothing has happened yet', null, null, true), { span: 'c12', flush: true, side: h('a', { class: 'btn small', href: '#activity' }, 'All activity') }))];
}

// ---------- view: apps ----------
function viewApps() {
  var list = S.apps;
  var out = [h('div', { class: 'head' }, h('div', { class: 'grow' }, h('h1', { text: 'Apps' })))];
  if (list.length === 0) { out.push(noApps()); return out; }
  var attn = list.filter(function (a) { return status(a).attn; }).length;
  var $rows = h('div', { class: 'rows', id: 'app-rows' });
  var $search = h('input', { class: 'input', id: 'app-search', type: 'search', placeholder: 'Filter by name, domain or image', maxlength: '100', autocomplete: 'off', 'aria-label': 'Filter apps', value: S.q, oninput: function (e) { S.q = e.target.value; fill(); } });
  function segBtn(key, label) { return h('button', { type: 'button', 'aria-pressed': S.only === key ? 'true' : 'false', onclick: function () { S.only = key; render(); } }, label); }
  out.push(h('div', { class: 'toolbar' }, $search, h('div', { class: 'seg', role: 'group', 'aria-label': 'Show' }, segBtn('all', 'All'), segBtn('attn', 'Needs attention' + (attn ? ' (' + attn + ')' : '')))));
  function fill() {
    clear($rows);
    var q = str(S.q, 100).toLowerCase();
    var shown = list.filter(function (a) { if (S.only === 'attn' && !status(a).attn) return false; return q === '' || [a.name, a.domain, a.image].some(function (f) { return str(f).toLowerCase().indexOf(q) !== -1; }); }).sort(byAttention);
    if (shown.length === 0) {
      $rows.appendChild(stateBlock(S.only === 'attn' && q === '' ? 'Nothing needs attention' : 'No app matches this filter', h('p', { text: S.only === 'attn' && q === '' ? 'Every app is running.' : 'Check the spelling, or clear the filter to see all ' + list.length + ' apps.' }),
        h('button', { class: 'btn', onclick: function () { S.q = ''; S.only = 'all'; render(); } }, 'Show all apps')));
      return;
    }
    $rows.appendChild(h('div', { class: 'row-h' }, h('div', { text: 'App' }), h('div', { text: 'Status' }), h('div', { text: 'Release' }), h('div', { text: 'Address' }), h('div', { class: 'right', text: 'Restarts' }), h('div', { text: 'Deployed' })));
    shown.forEach(function (a) {
      var st = status(a);
      $rows.appendChild(h('a', { class: 'row', href: appHref(a.name), 'aria-label': a.name + ', ' + st.label },
        h('div', { class: 'c app' }, h('span', { class: 'name trunc', title: a.name, text: a.name }), h('span', { class: 'sub2 trunc', title: str(a.domain), text: orDash(a.domain) })),
        h('div', { class: 'c st' }, h('div', null, pill(st))),
        h('div', { class: 'c wide' }, h('span', { class: 'k', text: 'Release' }), h('span', { class: 'mono trunc', title: str(a.release), text: orDash(a.release) }), h('span', { class: 'sub2 trunc', title: str(a.image), text: str(a.image) })),
        h('div', { class: 'c wide' }, h('span', { class: 'k', text: 'Address' }), h('span', { class: 'mono trunc', text: orDash(a.address) })),
        h('div', { class: 'c wide' }, h('span', { class: 'k', text: 'Restarts' }), h('span', { class: 'num', text: fmtNum(a.restarts) })),
        h('div', { class: 'c wide' }, h('span', { class: 'k', text: 'Deployed' }), when(a.deployed))));
    });
  }
  fill();
  out.push(h('div', { class: 'panel' }, $rows));
  return out;
}

// ---------- view: one app, in tabs ----------
var TABS = [['overview', 'Overview'], ['releases', 'Releases'], ['secrets', 'Secrets'], ['activity', 'Activity'], ['logs', 'Logs']];
function setTab(tab) {
  if (tab !== 'logs') stopFollowing();
  S.tab = tab; render();
  var r = route();
  if (tab === 'logs' && r.view === 'app' && findApp(r.name) && S.logs.app !== r.name) loadLogs(r.name);
  ensureExtras();
}
function viewApp(name) {
  var a = findApp(name);
  if (!a) return [fullState('There is no app called “' + str(name, 80) + '”', h('p', { text: 'It may have been stopped and removed, or the link is mistyped.' }), h('a', { class: 'btn', href: '#apps' }, 'Back to all apps'))];
  var st = status(a);
  if (!TABS.some(function (t) { return t[0] === S.tab; })) S.tab = 'overview';
  var out = [h('div', { class: 'head' },
    h('div', { class: 'grow' }, h('h1', { text: a.name }), h('div', { class: 'hstack' }, pill(st), h('span', { class: 'mono wrap-any muted', text: orDash(a.domain) }))),
    h('button', { class: 'btn', id: 'act-rollback', disabled: !canAct() || a.previous.length === 0, title: a.previous.length ? null : 'No previous release is kept for this app', onclick: function () { dlgRollback(a); } }, 'Roll back'),
    h('button', { class: 'btn danger', id: 'act-stop', disabled: !canAct(), onclick: function () { dlgStop(a); } }, 'Stop and remove'))];
  if (str(a.note)) out.push(h('div', { class: 'banner ' + (st.key === 'crit' || st.key === 'warn' ? st.key : 'info') }, h('div', { class: 'grow wrap-any', text: str(a.note, 500) })));
  out.push(h('div', { class: 'tabs', role: 'tablist', 'aria-label': 'App sections' }, TABS.map(function (t) {
    return h('button', { role: 'tab', id: 'tab-' + t[0], 'aria-selected': S.tab === t[0] ? 'true' : 'false', onclick: function () { setTab(t[0]); } }, t[1]);
  })));

  if (S.tab === 'overview') {
    var prev = a.previous.length ? a.previous[a.previous.length - 1] : null;
    out.push(h('div', { class: 'grid' },
      stat('Release', orDash(a.release, 40), 'in service', { small: true }),
      stat('Deployed', ago(a.deployed), abs(a.deployed), { small: true }),
      stat('Address', orDash(a.address, 40), 'where the proxy sends requests', { small: true }),
      stat('Automatic restarts', fmtNum(a.restarts), 'since it was last stable', { tone: a.restarts > 0 ? 'crit' : '' }),
      timelinePanel([a], 'c12'),
      panel('Health check response time', latencyChart(a), { span: 'c7', side: h('span', { class: 'hint', text: 'average per interval, ' + RANGES[S.range] }) }),
      panel('Deployment', h('dl', { class: 'kv' },
        h('dt', { text: 'Image' }), h('dd', { class: 'mono', text: orDash(a.image) }),
        h('dt', { text: 'Container' }), h('dd', { class: 'mono', text: orDash(a.container) }),
        h('dt', { text: 'Docker says' }), h('dd', { text: orDash(a.uptime, 80) }),
        h('dt', { text: 'Domain' }), h('dd', { class: 'mono', text: orDash(a.domain) }),
        h('dt', { text: 'Health check' }), h('dd', { class: 'mono', text: str(a.healthcheck) ? 'GET ' + str(a.healthcheck) : 'none' }),
        h('dt', { text: 'Kept for rollback' }), h('dd', { class: 'mono', text: prev ? orDash(prev.container) : 'nothing' })), { span: 'c5' })));
  } else if (S.tab === 'releases') {
    var rels = S.releases.filter(function (r) { return str(r.app) === a.name; });
    var body = rels.length ? h('div', { class: 'tbl-wrap' }, h('table', null,
      h('thead', null, h('tr', null, h('th', { text: 'Result' }), h('th', { text: 'Release' }), h('th', { text: 'Image' }), h('th', { text: 'Started' }), h('th', { text: 'Took' }), h('th', { text: 'Error' }))),
      h('tbody', null, rels.map(function (r) {
        var t0 = time(r.started_at), t1 = time(r.finished_at), result = str(r.status) === 'succeeded' ? true : str(r.status) === 'failed' ? false : null;
        return h('tr', null,
          h('td', { class: 'nowrap' }, h('div', { class: 'hstack' }, okPill(result), str(r.release) !== '' && str(r.release) === str(a.release) ? pill({ key: 'info', label: 'In service' }) : null)),
          h('td', { class: 'mono nowrap', text: orDash(r.release, 60) }), h('td', { class: 'detail mono', text: orDash(r.image, 300) }),
          h('td', { class: 'nowrap' }, when(r.started_at)), h('td', { class: 'nowrap mono', text: t0 !== null && t1 !== null && t1 >= t0 ? took(t1 - t0) : DASH }),
          h('td', { class: 'detail', text: orDash(r.error, 600) }));
      })))) : stateBlock('No deploys are recorded for this app', null, null, true);
    out.push(h('div', { class: 'grid' }, panel('Deploys', body, { flush: true })));
    out.push(h('div', { class: 'hint', text: a.previous.length ? 'The previous release is kept stopped; \u201cRoll back\u201d puts it back in service.' : 'No previous release is kept, so there is nothing to roll back to.' }));
  } else if (S.tab === 'secrets') {
    out.push(h('div', { class: 'grid' }, panel('Secrets', appSecretTable(a), { flush: true,
      side: S.info && S.info.secrets ? h('button', { class: 'btn small', id: 'act-secret', disabled: !canAct(), onclick: function () { dlgSecret(a, ''); } }, 'Add secret') : null })));
    out.push(h('div', { class: 'hint' }, 'Names only: values are stored encrypted and no interface can read them back. A new or replaced value takes effect on the next deploy. From a script: ', h('code', { text: 'liman secret set ' + a.name + ' NAME' })));
  } else if (S.tab === 'activity') {
    var ev = sortedEvents().filter(function (e) { return str(e.app) === a.name; });
    out.push(h('div', { class: 'grid' }, panel('Activity', ev.length ? eventTable(ev, false) : stateBlock('Nothing recorded for this app in the latest ' + S.events.length + ' steps', null, null, true), { flush: true })));
  } else {
    var lg = S.logs.app === a.name ? S.logs : { loading: true, text: '', error: '', cut: false, following: false, ended: false };
    var logBody = lg.loading ? stateBlock('Reading the logs…', null, null, true)
      : lg.text === '' && lg.error ? stateBlock('The logs could not be read', h('p', { text: str(lg.error, 300) }), null, true)
      : lg.text === '' && lg.following ? stateBlock('Waiting for the container to write something', null, null, true)
      : lg.text === '' ? stateBlock('The container has not written anything', null, null, true)
      : h('pre', { class: 'logs', id: 'logs', tabindex: '0', 'aria-label': 'Log output', 'aria-live': lg.following ? 'off' : null, text: lg.text });
    var what = lg.following ? 'following: new lines appear as they are written' : lg.cut ? 'the last ' + LOG_LINES + ' lines at most' : 'output of the current container';
    out.push(h('div', { class: 'grid' }, panel('Logs', logBody, { flush: true, side: h('div', { class: 'hstack' },
      h('span', { class: 'hint', text: what }),
      lg.following
        ? h('button', { class: 'btn small', id: 'logs-follow', onclick: function () { stopFollowing(); render(); } }, 'Stop following')
        : [h('button', { class: 'btn small', disabled: lg.loading, onclick: function () { loadLogs(a.name); } }, 'Reload'),
           h('button', { class: 'btn small', id: 'logs-follow', disabled: lg.loading, onclick: function () { loadLogs(a.name, true); } }, 'Follow')]) })));
    if (lg.text !== '' && lg.error) out.push(h('div', { class: 'hint', text: str(lg.error, 300) }));
    if (lg.ended) out.push(h('div', { class: 'hint', text: 'The stream ended: the container stopped, or was replaced by a new release. Follow again to watch the current one.' }));
    out.push(h('div', { class: 'hint' }, 'In a terminal: ', h('code', { text: 'liman logs -f ' + a.name })));
  }
  return out;
}

// ---------- tables shared by views ----------
var ACTIONS = { run: 'Step', undo: 'Undo', restart: 'Restart' };
function eventTable(list, withApp) {
  return h('div', { class: 'tbl-wrap' }, h('table', null,
    h('thead', null, h('tr', null, h('th', { text: 'When' }), withApp ? h('th', { text: 'App' }) : null, h('th', { text: 'Kind' }), h('th', { text: 'Step' }), h('th', { text: 'Result' }), h('th', { text: 'Took' }), h('th', { text: 'Error' }))),
    h('tbody', null, list.map(function (e) {
      var app = str(e.app), action = str(e.action).toLowerCase();
      return h('tr', null, h('td', { class: 'nowrap' }, when(e.time)),
        withApp ? h('td', { class: 'name' }, findApp(app) ? h('a', { href: appHref(app), text: app }) : orDash(app, 80)) : null,
        h('td', { class: 'nowrap', text: ACTIONS[action] || orDash(e.action, 40) }), h('td', { class: 'detail', text: orDash(e.step, 300) }),
        h('td', { class: 'nowrap' }, okPill(typeof e.ok === 'boolean' ? e.ok : null)), h('td', { class: 'nowrap mono', text: took(e.duration_ms) }), h('td', { class: 'detail', text: orDash(e.error, 600) }));
    }))));
}
// appSecretTable is the Secrets tab of one app.
function appSecretTable(a) {
  if (!(S.info && S.info.secrets)) return stateBlock('This daemon has no secret storage', null, null, true);
  var have = S.secrets.get(a.name);
  if (!have) return stateBlock('Reading the secret names…', null, null, true);
  if (have.list === null) return stateBlock('The secret names could not be read', h('p', { text: str(have.error, 300) }), null, true);
  var rows = have.list.filter(function (x) { return str(x.name); }).map(function (x) { return { app: a.name, x: x }; });
  if (rows.length === 0) return stateBlock('This app has no secrets', h('p', null, 'Store one with ', h('code', { text: 'liman secret set' }), ', then list its name under ', h('code', { text: 'secrets:' }), ' in app.yaml.'), null, true);
  return secretRows(rows, false);
}
// secretRows draws secrets as { app, x } pairs, x being what the daemon
// listed. With the app column, an app that is not running is said so:
// its secrets are kept, and wait for its next deploy.
function secretRows(rows, withApp) {
  return h('div', { class: 'tbl-wrap' }, h('table', null, h('thead', null, h('tr', null, withApp ? h('th', { text: 'App' }) : null, h('th', { text: 'Name' }), h('th', { text: 'Last changed' }), h('th', null, h('span', { class: 'sr', text: 'Actions' })))),
    h('tbody', null, rows.map(function (r) {
      var app = r.app, x = r.x, name = str(x.name), target = { name: app };
      return h('tr', null, withApp ? h('td', { class: 'name' }, findApp(app) ? h('a', { href: appHref(app), text: app }) : h('span', { class: 'hstack' }, h('span', { class: 'trunc', text: str(app, 80) }), pill({ key: 'none', label: 'Not running' }))) : null,
        h('td', { class: 'mono detail', text: str(name, 300) }), h('td', { class: 'nowrap' }, when(x.updated_at)),
        h('td', { class: 'nowrap right' }, h('button', { class: 'btn small', disabled: !canAct(), onclick: function () { dlgSecret(target, name); } }, 'Replace'), ' ',
          h('button', { class: 'btn small danger', disabled: !canAct(), onclick: function () { dlgRemoveSecret(target, name); } }, 'Remove')));
    }))));
}

// ---------- actions: everything that changes something is confirmed first ----------
// Actions are offered only to a token that may perform them. This is a
// courtesy to the user, not the protection: the daemon refuses a read-only
// token whatever this page shows.
function readOnly() { return !(S.info && S.info.scope === 'admin'); }
// tokenApps are the apps the panel's token is limited to, or null when it
// may use every app. A limited token sees only those apps, and none of the
// pages about the daemon as a whole: the daemon refuses them anyway, and a
// page full of refusals helps nobody.
function tokenApps() {
  var a = S.info && Array.isArray(S.info.token_apps) ? S.info.token_apps.map(function (x) { return str(x, 63); }).filter(Boolean) : [];
  return a.length ? a : null;
}
function limitedPage(title) {
  return [h('h1', { text: title }), fullState('Not for this token', h('p', null, 'This token is limited to ', h('code', { text: tokenApps().join(', ') }), '. This page is about the daemon as a whole; a token for every app sees it.'))];
}
function canAct() { return S.mode === 'ready' && !readOnly(); }
function openDialog(title, body, actions) {
  clear($dlg);
  $dlg.appendChild(h('form', { class: 'dlg', novalidate: true, onsubmit: function (e) { e.preventDefault(); } }, h('h3', { id: 'dlg-title', text: title }), body, h('div', { class: 'acts' }, actions)));
  if (typeof $dlg.showModal === 'function') { if (!$dlg.open) $dlg.showModal(); } else $dlg.setAttribute('open', '');
}
function closeDialog() { if (!$dlg) return; if ($dlg.open && typeof $dlg.close === 'function') $dlg.close(); else $dlg.removeAttribute('open'); clear($dlg); }
function cancelBtn(label) { return h('button', { type: 'button', class: 'btn', onclick: closeDialog }, label || 'Cancel'); }
// failed turns an error into what the user reads. A rejected token ends
// the session; everything else is shown where the action was started.
function failed(e) {
  if (e && e.status === 401) { signOut(rejected(e)); return null; }
  if (e && e.status === 409) return str(e.message, 300) || 'Another operation on this app is in progress. Try again when it has finished.';
  return str(e && e.message, 400) || 'The request failed.';
}
// working swaps a dialog's buttons for a note while the request is out, so
// nothing can be sent twice.
function working(text) { var acts = $dlg.querySelector('.acts'); if (acts) { clear(acts); acts.appendChild(h('span', { class: 'muted', text: text })); } }
function done(kind, text) { S.note = { kind: kind, text: text }; closeDialog(); render(); refresh(); }

function dlgRollback(a) {
  var prev = a.previous[a.previous.length - 1] || {};
  var $out = h('pre', { class: 'logs dlg-out', hidden: true, tabindex: '0', 'aria-label': 'Progress' }), $err = h('div', { class: 'err', role: 'alert' });
  openDialog('Roll back ' + str(a.name, 80) + '?',
    [h('p', { text: 'The previous release is started again and, once it passes its health check, requests are sent to it. The current release is then removed; to get it back you deploy it again.' }),
     h('div', { class: 'swap' }, h('span', { class: 'muted', text: 'From' }), h('span', { class: 'mono wrap-any', text: orDash(a.container) }), h('span', { class: 'muted', text: 'To' }), h('span', { class: 'mono wrap-any', text: orDash(prev.container) + '  ' + str(prev.image, 200) })),
     $out, $err],
    [cancelBtn(), h('button', { type: 'submit', class: 'btn primary', id: 'dlg-go', onclick: function () {
      working('Rolling back\u2026 keep this page open.'); $out.hidden = false;
      stream('POST', appPath(a.name, '/rollback'), function (text) {
        // Progress output is shown as it comes, as text.
        $out.textContent += text.replace(/[\u0000-\u0008\u000b-\u001f\u007f]/g, ''); $out.scrollTop = $out.scrollHeight;
      }).then(function (res) {
        if (res.ok) { done('ok', 'Rolled back ' + str(a.name, 60) + ' to release ' + orDash(res.release, 60) + '.'); return; }
        // A failed rollback undoes itself; say so, and leave the output up.
        $err.textContent = 'The rollback failed and was undone: ' + str(res.error, 400);
        working(''); var acts = $dlg.querySelector('.acts'); clear(acts); acts.appendChild(cancelBtn('Close')); refresh();
      }, function (e) {
        var msg = failed(e); if (msg === null) return;
        $err.textContent = msg; var acts = $dlg.querySelector('.acts'); clear(acts); acts.appendChild(cancelBtn('Close')); refresh();
      });
    } }, 'Roll back')]);
}
function dlgStop(a) {
  var $err = h('div', { class: 'err', id: 'stop-err', role: 'alert' });
  var $in = h('input', { class: 'input mono', id: 'stop-confirm', autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false', maxlength: '100', 'aria-describedby': 'stop-err', oninput: function () { $err.textContent = ''; $in.removeAttribute('aria-invalid'); } });
  openDialog('Stop and remove ' + str(a.name, 80) + '?',
    [h('p', { text: 'Its containers are removed, the previous release included, and ' + orDash(a.domain, 120) + ' stops answering. Its secrets and its history are kept. To bring it back you deploy it again.' }),
     h('div', { class: 'field' }, h('label', { for: 'stop-confirm' }, 'Type the app name to confirm'), $in, $err)],
    [cancelBtn(), h('button', { type: 'submit', class: 'btn danger', id: 'dlg-go', onclick: function () {
      if ($in.value.trim() !== a.name) { $err.textContent = 'That does not match. The name is ' + a.name + '.'; $in.setAttribute('aria-invalid', 'true'); $in.focus(); return; }
      working('Stopping\u2026');
      send('DELETE', appPath(a.name, ''), null, 120000).then(function (res) {
        var n = res && Array.isArray(res.removed) ? res.removed.length : 0;
        window.location.hash = 'apps';
        done('ok', 'Stopped ' + str(a.name, 60) + ' and removed ' + n + (n === 1 ? ' container.' : ' containers.'));
      }, function (e) { var msg = failed(e); if (msg === null) return; closeDialog(); S.note = { kind: 'crit', text: str(a.name, 60) + ' was not stopped. ' + msg }; render(); refresh(); });
    } }, 'Stop and remove')]);
  $in.focus();
}
var SECRET_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/, SECRET_MAX_BYTES = 64 * 1024;
// The daemon's rule for app names (app.ValidName); the daemon checks again.
var APP_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
// dlgSecret stores a secret of app a, or, with a null, of an app named in
// the dialog: one that need not be deployed yet.
function dlgSecret(a, fixedName) {
  var $appErr = h('div', { class: 'err', id: 'sec-app-err', role: 'alert' });
  var $app = a ? null : h('input', { class: 'input mono', id: 'sec-app', maxlength: '63', autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false', placeholder: 'orders-api', 'aria-describedby': 'sec-app-err', oninput: function () { $appErr.textContent = ''; $app.removeAttribute('aria-invalid'); } });
  var $nameErr = h('div', { class: 'err', id: 'sec-name-err', role: 'alert' }), $valErr = h('div', { class: 'err', id: 'sec-val-err', role: 'alert' }), $size = h('span', { class: 'hint', id: 'sec-size', text: '0 bytes' });
  var $name = h('input', { class: 'input mono', id: 'sec-name', value: fixedName || null, readonly: fixedName ? true : null, maxlength: '128', autocomplete: 'off', autocapitalize: 'characters', spellcheck: 'false', placeholder: 'DATABASE_URL', 'aria-describedby': 'sec-name-err', oninput: function () { $nameErr.textContent = ''; $name.removeAttribute('aria-invalid'); } });
  // type=password: not shown on screen, and with this autocomplete value
  // the browser does not offer to remember it.
  var $val = h('input', { class: 'input mono', id: 'sec-value', type: 'password', autocomplete: 'new-password', spellcheck: 'false', 'aria-describedby': 'sec-val-err sec-size' });
  var enc = new TextEncoder();
  $val.addEventListener('input', function () { $valErr.textContent = ''; $val.removeAttribute('aria-invalid'); $size.textContent = fmtNum(enc.encode($val.value).length) + ' bytes'; });
  function wrong($el, $e, msg) { $e.textContent = msg; $el.setAttribute('aria-invalid', 'true'); $el.focus(); return null; }
  // The same rules the daemon applies; checking here only saves a round
  // trip and gives the message next to the field.
  function check() {
    var app = a ? a.name : $app.value.trim(), n = $name.value.trim(), bytes = enc.encode($val.value);
    if (!a && app === '') return wrong($app, $appErr, 'Name the app the secret is for.');
    if (!a && !APP_NAME_RE.test(app)) return wrong($app, $appErr, 'App names use lowercase letters, digits and dashes, up to 63, and start and end with a letter or digit.');
    if (n === '') return wrong($name, $nameErr, 'Give the secret a name.');
    if (!SECRET_NAME_RE.test(n)) return wrong($name, $nameErr, 'Use letters, digits and underscores, and do not start with a digit.');
    if (bytes.length === 0) return wrong($val, $valErr, 'The value is empty.');
    if ($val.value.indexOf('\u0000') !== -1) return wrong($val, $valErr, 'The value contains a NUL character, which cannot be stored.');
    if (bytes.length > SECRET_MAX_BYTES) return wrong($val, $valErr, 'The value is ' + fmtNum(bytes.length) + ' bytes; the limit is ' + fmtNum(SECRET_MAX_BYTES) + '.');
    return { app: app, name: n, bytes: bytes };
  }
  openDialog(a ? (fixedName ? 'Replace secret of ' : 'Add secret to ') + str(a.name, 80) : 'Add a secret',
    [a ? null : h('div', { class: 'field' }, h('label', { for: 'sec-app' }, 'App'), $app, $appErr, h('span', { class: 'hint', text: 'It need not be deployed yet; its first deploy will find the secret.' })),
     h('div', { class: 'field' }, h('label', { for: 'sec-name' }, 'Name'), $name, $nameErr),
     h('div', { class: 'field' }, h('label', { for: 'sec-value' }, 'Value'), $val, h('div', { class: 'between' }, $valErr, $size)),
     h('p', { class: 'hint', text: 'The value is sent once, stored encrypted and cannot be shown again. Running containers keep what they have; it takes effect on the next deploy.' })],
    [cancelBtn(), h('button', { type: 'submit', class: 'btn primary', id: 'dlg-go', onclick: function () {
      var r = check(); if (!r) return;
      working('Storing\u2026');
      // The value leaves the page here and is dropped from the field at once.
      var sending = send('PUT', appPath(r.app, '/secrets/' + encodeURIComponent(r.name)), r.bytes);
      $val.value = '';
      sending.then(function () {
        S.secrets.delete(r.app); S.allSecrets = null;
        done('ok', 'Stored secret ' + str(r.name, 60) + ' of ' + str(r.app, 60) + ' (' + fmtNum(r.bytes.length) + ' bytes). It takes effect on the next deploy.');
        ensureExtras();
      }, function (e) { var msg = failed(e); if (msg === null) return; closeDialog(); S.note = { kind: 'crit', text: 'The secret was not stored. ' + msg }; render(); });
    } }, 'Store secret')]);
  (fixedName ? $val : $app || $name).focus();
}
function dlgRemoveSecret(a, name) {
  openDialog('Remove secret ' + str(name, 80) + '?',
    [h('p', { text: 'It is removed from ' + str(a.name, 80) + ' for good; the value cannot be recovered from here. Running containers keep it until the next deploy, and a deploy whose app.yaml still lists it will be refused.' })],
    [cancelBtn(), h('button', { type: 'submit', class: 'btn danger', id: 'dlg-go', onclick: function () {
      working('Removing\u2026');
      send('DELETE', appPath(a.name, '/secrets/' + encodeURIComponent(name))).then(function () {
        S.secrets.delete(a.name); S.allSecrets = null;
        done('ok', 'Removed secret ' + str(name, 60) + ' of ' + str(a.name, 60) + '.');
        ensureExtras();
      }, function (e) { var msg = failed(e); if (msg === null) return; closeDialog(); S.note = { kind: 'crit', text: 'The secret was not removed. ' + msg }; render(); });
    } }, 'Remove')]);
}

// ---------- views: activity, secrets, daemon ----------
function viewActivity() {
  var all = sortedEvents(), names = [];
  all.forEach(function (e) { var n = str(e.app); if (n && names.indexOf(n) === -1) names.push(n); });
  names.sort();
  if (S.actApp && names.indexOf(S.actApp) === -1) S.actApp = '';
  var shown = all.filter(function (e) { return (!S.actApp || str(e.app) === S.actApp) && (S.actResult !== 'problem' || e.ok !== true || str(e.action).toLowerCase() !== 'run'); });
  var $sel = h('select', { id: 'act-app', 'aria-label': 'App', onchange: function (e) { S.actApp = e.target.value; render(); } }, h('option', { value: '', text: 'All apps' }), names.map(function (n) { return h('option', { value: n, selected: n === S.actApp, text: str(n, 48) }); }));
  function segBtn(key, label) { return h('button', { type: 'button', 'aria-pressed': S.actResult === key ? 'true' : 'false', onclick: function () { S.actResult = key; render(); } }, label); }
  return [h('h1', { text: 'Activity' }), h('div', { class: 'toolbar' }, h('span', { class: 'pick' }, $sel, icon('chevron')), h('div', { class: 'seg', role: 'group', 'aria-label': 'Show' }, segBtn('', 'All'), segBtn('problem', 'Failures, undos and restarts'))),
    h('div', { class: 'panel' }, shown.length ? eventTable(shown, true) : stateBlock(all.length ? 'No activity matches this filter' : 'Nothing has happened yet', h('p', { text: all.length ? 'Try another app, or show everything.' : 'Steps appear here as soon as the first deploy runs.' }),
      all.length ? h('button', { class: 'btn', onclick: function () { S.actApp = ''; S.actResult = ''; render(); } }, 'Clear filters') : null)),
    h('div', { class: 'hint' }, 'The latest ' + fmtNum(all.length) + ' steps. For older ones: ', h('code', { text: 'liman history -n 1000' }))];
}
function viewSecrets() {
  var vault = !!(S.info && S.info.secrets);
  var head = h('div', { class: 'head' }, h('div', { class: 'grow' }, h('h1', { text: 'Secrets' })),
    vault ? h('button', { class: 'btn primary', id: 'add-secret', disabled: !canAct(), title: readOnly() ? 'This token is read-only' : null, onclick: function () { dlgSecret(null, ''); } }, 'Add secret') : null);
  if (!vault) return [head, fullState('This daemon has no secret storage', h('p', { text: 'Apps that name secrets cannot be deployed until it has a master key.' }))];
  var have = S.allSecrets, body;
  if (!have) body = stateBlock('Reading the secret names…', null, null, true);
  else if (have.list === null) body = stateBlock('The secret names could not be read', h('p', { text: str(have.error, 300) }), h('button', { class: 'btn', onclick: function () { S.allSecrets = null; render(); ensureExtras(); } }, 'Try again'), true);
  else {
    var rows = have.list.filter(function (x) { return str(x.app) && str(x.name); }).map(function (x) { return { app: str(x.app), x: x }; });
    body = rows.length ? secretRows(rows, true) : stateBlock('No secrets are stored', h('p', null, 'Add one here or with ', h('code', { text: 'liman secret set' }), ', then list its name under ', h('code', { text: 'secrets:' }), ' in app.yaml.'), null, true);
  }
  return [head, h('div', { class: 'panel' }, body),
    h('div', { class: 'hint', text: 'Names only, for every app: one that is not deployed yet, or was stopped, keeps its secrets for its next deploy. Values are stored encrypted and no interface can read them back.' })];
}
// viewTokens lists the API tokens. The daemon keeps only their hashes, so
// there is nothing here that opens anything. Revoking is offered; creating
// is not, on purpose: a stolen admin token must not be able to make itself
// spares that outlive its revocation.
function viewTokens() {
  if (tokenApps()) return limitedPage('Tokens');
  var have = S.tokens, out = [h('h1', { text: 'Tokens' })];
  if (!have) { out.push(h('div', { class: 'panel' }, stateBlock('Reading the tokens…', null, null, true))); return out; }
  if (have.list === null) { out.push(fullState('The tokens could not be listed', h('p', { text: str(have.error, 300) }), h('button', { class: 'btn', onclick: function () { S.tokens = null; render(); ensureExtras(); } }, 'Try again'))); return out; }
  var list = have.list.filter(function (t) { return str(t.name); });
  var now = S.info ? time(S.info.now) : null;
  out.push(panel('Tokens', h('div', { class: 'tbl-wrap' }, h('table', null,
    h('thead', null, h('tr', null, h('th', { text: 'Name' }), h('th', { text: 'Can' }), h('th', { text: 'Apps' }), h('th', { text: 'Created' }), h('th', { text: 'Expires' }), h('th', { text: 'Last used' }), h('th', null, h('span', { class: 'sr', text: 'Actions' })))),
    h('tbody', null, list.map(function (t) {
      var end = time(t.expires_at), expires;
      if (t.expired === true) expires = h('span', { title: abs(t.expires_at) }, pill({ key: 'crit', label: 'Expired' }));
      else if (end === null) expires = h('span', { class: 'muted', text: 'Never' });
      else expires = h('span', { title: abs(t.expires_at), text: now === null ? abs(t.expires_at) : 'in ' + roughly(Math.max(end - now, 0)) });
      return h('tr', null,
        h('td', { class: 'name' }, h('span', { class: 'hstack' }, h('span', { class: 'mono trunc', text: str(t.name, 64) }), t.current === true ? pill({ key: 'info', label: 'This panel' }) : null)),
        h('td', { class: 'nowrap' }, t.scope === 'admin' ? pill({ key: 'warn', label: 'Change things' }) : pill({ key: 'none', label: 'Look only' })),
        h('td', { class: 'detail' }, Array.isArray(t.apps) && t.apps.length ? h('span', { class: 'mono', text: t.apps.map(function (x) { return str(x, 63); }).join(', ') }) : h('span', { class: 'muted', text: 'All apps' })),
        h('td', { class: 'nowrap' }, when(t.created_at)),
        h('td', { class: 'nowrap' }, expires),
        h('td', { class: 'nowrap' }, time(t.last_used_at) === null ? h('span', { class: 'muted', title: 'Not used since the daemon started keeping track', text: DASH }) : when(t.last_used_at)),
        h('td', { class: 'nowrap right' }, h('button', { class: 'btn small danger', disabled: !canAct(), title: readOnly() ? 'This token is read-only' : null, onclick: function () { dlgRevokeToken(t); } }, 'Revoke')));
    })))), { flush: true }));
  out.push(h('div', { class: 'hint' }, '“Last used” is written down at most once a minute. Tokens are created on the server, never here: ',
    h('code', { text: 'limand token create <name>' }), ', with ', h('code', { text: '--read-only' }), ' for one that can only look, ', h('code', { text: '--app <app>' }), ' for one that sees and changes only that app, and ', h('code', { text: '--expires 30d' }), ' for one that stops working by itself.'));
  return out;
}
function dlgRevokeToken(t) {
  var name = str(t.name, 64), own = t.current === true, $err = h('div', { class: 'err', role: 'alert' });
  openDialog('Revoke token ' + name + '?',
    [h('p', { text: 'Every client that uses it is refused from its next request on. A revoked token cannot be brought back; a new one is created on the server with limand token create.' }),
     own ? h('div', { class: 'banner warn', role: 'note' }, h('div', { class: 'grow', text: 'This is the token this panel is signed in with. The panel signs out as soon as it is revoked.' })) : null,
     time(t.last_used_at) !== null ? h('p', { class: 'hint', text: 'Last used ' + ago(t.last_used_at) + '.' }) : null, $err],
    [cancelBtn(), h('button', { type: 'submit', class: 'btn danger', id: 'dlg-go', onclick: function () {
      working('Revoking…');
      send('DELETE', '/v1/tokens/' + encodeURIComponent(t.name)).then(function () {
        if (own) { signOut('You revoked the token this panel was signed in with (' + name + ').'); return; }
        S.tokens = null;
        done('ok', 'Revoked token ' + name + '. Clients that used it are refused from now on.');
        ensureExtras();
      }, function (e) { var msg = failed(e); if (msg === null) return; closeDialog(); S.note = { kind: 'crit', text: 'The token was not revoked. ' + msg }; render(); });
    } }, own ? 'Revoke and sign out' : 'Revoke')]);
}
function viewBackups() {
  if (tokenApps()) return limitedPage('Backups');
  var out = [h('div', { class: 'head' }, h('div', { class: 'grow' }, h('h1', { text: 'Backups' })),
    S.info && S.info.backups ? h('button', { class: 'btn primary', id: 'backup-now', disabled: S.backupBusy || !canAct(), title: readOnly() ? 'This token is read-only' : null, onclick: backUpNow }, S.backupBusy ? 'Backing up\u2026' : 'Back up now') : null)];
  if (!(S.info && S.info.backups)) { out.push(fullState('This daemon does not offer backups', h('p', null, 'On the server, ', h('code', { text: 'limand backup create <dir>' }), ' still works.'))); return out; }
  if (S.backupNote) out.push(h('div', { class: 'banner ' + (S.backupNote.kind === 'ok' ? 'info' : S.backupNote.kind), role: 'status' }, h('div', { class: 'grow wrap-any', text: S.backupNote.text })));

  var have = S.backups;
  if (!have) { out.push(h('div', { class: 'panel' }, stateBlock('Reading the list of backups\u2026', null, null, true))); return out; }
  if (!have.list) { out.push(fullState('The backups could not be listed', h('p', { text: str(have.error, 300) }), h('button', { class: 'btn', onclick: function () { S.backups = null; render(); ensureExtras(); } }, 'Try again'))); return out; }

  var list = cleanList(have.list.backups), keep = num(have.list.keep) || 0, total = 0;
  list.forEach(function (b) { total += num(b.size_bytes) || 0; });
  var newest = list[0], age = newest ? time(newest.created_at) : null, stale = age !== null && Date.now() - age > 2 * DAY;
  out.push(h('div', { class: 'grid' },
    stat('Latest backup', newest ? ago(newest.created_at) : 'none', newest ? abs(newest.created_at) : 'nothing has been backed up yet', { small: true, tone: !newest || stale ? 'crit' : '' }),
    stat('Kept', fmtNum(list.length), keep ? 'the newest ' + keep + ' are kept' : 'none are removed automatically'),
    stat('Size on disk', size(total), 'all backups together', { small: true }),
    stat('Where', orDash(have.list.dir, 60), 'on the server\u2019s own disk', { small: true }),
    panel('Backups', list.length ? h('div', { class: 'tbl-wrap' }, h('table', null,
      h('thead', null, h('tr', null, h('th', { text: 'File' }), h('th', { text: 'Made' }), h('th', { text: 'Size' }), h('th', { text: 'Contents' }), h('th', { text: 'Check' }))),
      h('tbody', null, list.map(function (b) {
        var name = str(b.name), c = S.checks.get(name), cell;
        if (c && c.busy) cell = h('span', { class: 'muted', text: 'Checking\u2026' });
        else if (c && c.report) cell = h('div', { class: 'hstack' }, pill(c.report.ok ? { key: 'ok', label: 'Good' } : { key: 'crit', label: 'Not usable' }), h('span', { class: 'muted wrap-any', text: reportText(c.report) }));
        else if (c && c.error) cell = h('div', { class: 'hstack' }, pill({ key: 'crit', label: 'Damaged' }), h('span', { class: 'muted wrap-any', text: c.error }));
        else cell = h('button', { class: 'btn small', disabled: !canAct(), onclick: function () { checkBackup(name); } }, 'Check');
        // Unencrypted is not an error: backups from before encryption,
        // and the one taken before an upgrade, are plain on purpose.
        var enc = b.encrypted === true ? pill({ key: 'ok', label: 'Encrypted' }) : pill({ key: 'none', label: 'Not encrypted' });
        return h('tr', null, h('td', { class: 'mono nowrap', text: orDash(name, 60) }), h('td', { class: 'nowrap' }, when(b.created_at)), h('td', { class: 'nowrap mono', text: size(b.size_bytes) }), h('td', { class: 'nowrap' }, enc), h('td', { class: 'detail' }, cell));
      })))) : stateBlock('No backups yet', h('p', null, 'Make the first one with the button above. On a server installed with ', h('code', { text: 'install.sh' }), ' one is also made every night.'), null, true), { flush: true }),
    panel('What a backup is, and is not', h('dl', { class: 'kv' },
      h('dt', { text: 'In it' }), h('dd', null, 'The daemon\u2019s database: tokens, secrets (still encrypted), deploy history. Made while everything keeps running.'),
      h('dt', { text: 'Not in it' }), h('dd', null, 'The master key. Keep a copy of ', h('code', { text: 'master.key' }), ' somewhere else; without it an encrypted backup cannot be opened at all, and the secrets in an older, unencrypted one are lost. \u201cCheck\u201d tells you whether the server\u2019s key opens a backup.'),
      h('dt', { text: 'Restoring' }), h('dd', null, 'Not from here: it replaces the database the daemon is using, so the daemon has to be stopped. On the server: ', h('code', { text: 'limand backup restore <file>' }), '.'),
      h('dt', { text: 'Off the server' }), h('dd', null, 'These files are on the same disk as the data they protect. Copy them elsewhere regularly, or they are lost together with the server. Encrypted ones can go anywhere: they open only with the master key, so keep a copy of that key too, apart from the backups.')), { span: 'c12' })));
  return out;
}
// certificatesPanel lists what is in nginx.cert_dir. Nothing here can
// change a certificate: that is the daemon's job, or the command line's.
function certificatesPanel() {
  if (!(S.info && S.info.certificates)) return null;
  var have = S.certs, data = have && have.data, list = data && Array.isArray(data.certificates) ? data.certificates : [];
  var side = h('span', { class: 'hint', text: data ? (data.acme ? 'obtained and renewed by liman' : 'acme is off: renewing is up to you') : '' });
  var body;
  if (!have) body = stateBlock('Reading the certificates…', null, null, true);
  else if (!data) body = stateBlock('The certificates could not be read', h('p', { text: str(have.error, 300) }), null, true);
  else if (!list.length) body = stateBlock('No certificates yet', h('p', null, data.acme ? 'An app with a domain gets one after its next deploy.' : 'Put fullchain.pem and privkey.pem into cert_dir/<domain>/, or set up acme in config.yaml.'), null, true);
  else body = h('div', { class: 'tbl-wrap' }, h('table', null,
    h('thead', null, h('tr', null, h('th', { text: 'Domain' }), h('th', { text: 'Status' }), h('th', { text: 'Expires' }), h('th', { text: 'Issuer' }))),
    h('tbody', null, list.map(function (c) {
      var st = !c.in_use ? { key: 'none', label: c.status === 'expired' ? 'Expired, not in use' : 'Not in use' }
        : c.status === 'expired' ? { key: 'crit', label: 'Expired' } : c.status === 'renewal due' ? { key: 'warn', label: 'Renewal due' } : { key: 'ok', label: 'Valid' };
      var end = time(c.not_after), left = end === null ? '' : end > Date.now() ? 'in ' + roughly(end - Date.now()) : 'expired';
      return h('tr', null, h('td', { class: 'mono wrap-word', text: str(c.domain, 120) }), h('td', null, pill(st)),
        h('td', { class: 'nowrap' }, h('span', { text: abs(c.not_after) }), h('span', { class: 'muted', text: left ? ' · ' + left : '' })),
        h('td', { class: 'muted wrap-any', text: orDash(c.issuer, 80) }));
    }))));
  var extra = data && Array.isArray(data.errors) && data.errors.length ? h('div', { class: 'hint wrap-any', text: str(data.errors.join('; '), 300) }) : null;
  var unused = list.some(function (c) { return c && !c.in_use; }) ? h('div', { class: 'hint', text: 'Not in use: no app has this domain any more. liman keeps the files for a later deploy but does not renew them.' }) : null;
  return panel('Certificates', [body, unused, extra], { span: 'c12', side: side, flush: !!(data && list.length) });
}
// keysPanel lists the master keys by ID. An ID says nothing about the key;
// the panel never sees a key, only which one sealed what.
function keysPanel() {
  if (!(S.info && S.info.secrets)) return null;
  var have = S.keys, data = have && have.data, list = data && Array.isArray(data.keys) ? data.keys : [];
  var roles = { primary: { key: 'ok', label: 'Primary' }, old: { key: 'none', label: 'Old, opens only' }, 'unfinished rotation': { key: 'warn', label: 'Unfinished rotation' }, missing: { key: 'crit', label: 'Missing' } };
  var body;
  if (!have) body = stateBlock('Reading the keys…', null, null, true);
  else if (!data) body = stateBlock('The keys could not be read', h('p', { text: str(have.error, 300) }), null, true);
  else body = h('div', { class: 'tbl-wrap' }, h('table', null,
    h('thead', null, h('tr', null, h('th', { text: 'Key' }), h('th', { text: 'Role' }), h('th', { class: 'right', text: 'Secrets' }), h('th', { class: 'right', text: 'Backups' }))),
    h('tbody', null, list.map(function (k) {
      return h('tr', null, h('td', { class: 'mono', text: str(k.id, 32) }), h('td', null, pill(roles[k.role] || { key: 'none', label: str(k.role, 40) })),
        h('td', { class: 'right', text: fmtNum(k.secrets) }), h('td', { class: 'right', text: fmtNum(k.backups) }));
    }))));
  var hint = h('div', { class: 'hint' }, 'Secrets: sealed with the key, in the database. Backups: signed with it, in the backup directory. Rotating and retiring keys is done on the server: ', h('code', { text: 'limand key rotate' }), ', ', h('code', { text: 'limand key retire <id>' }), '.');
  return panel('Master keys', [body, hint], { span: 'c12', flush: !!data });
}

function viewDaemon() {
  var d = S.info || {}, m = d.monitor && typeof d.monitor === 'object' ? d.monitor : null;
  function part(label, key, word, text) { return [h('dt', { text: label }), h('dd', { class: 'hstack' }, pill({ key: key, label: word }), h('span', { class: 'muted wrap-any', text: text }))]; }
  return [h('h1', { text: 'Daemon' }), h('div', { class: 'grid' },
    panel('Parts', h('dl', { class: 'kv' },
      S.engineError ? part('Docker', 'crit', 'Problem', str(S.engineError, 240)) : part('Docker', 'ok', 'OK', 'answers the daemon'),
      d.proxy ? part('Reverse proxy', 'ok', 'Managed', 'the daemon writes and reloads the Nginx config') : part('Reverse proxy', 'none', 'Off', 'apps are only reachable on their own port'),
      d.secrets ? part('Secret storage', 'ok', 'On', 'values are encrypted with the master key') : part('Secret storage', 'none', 'Off', 'apps that name secrets cannot be deployed'),
      m ? part('Health monitor', 'ok', 'On', 'checks every ' + span(m.interval_seconds) + ', restarts after ' + fmtNum(m.failure_threshold) + ' failures in a row, keeps ' + span(m.retention_seconds) + ' of history')
        : part('Health monitor', 'none', 'Off', 'apps are not checked or restarted')), { span: 'c6' }),
    panel('Process', h('dl', { class: 'kv' },
      h('dt', { text: 'Version' }), h('dd', { class: 'mono', text: orDash(d.version, 80) }),
      h('dt', { text: 'Commit' }), h('dd', { class: 'mono', text: orDash(d.commit, 80) }),
      h('dt', { text: 'Built' }), h('dd', { class: 'mono', text: orDash(d.built, 80) }),
      h('dt', { text: 'Running since' }), h('dd', null, when(d.started_at), h('span', { class: 'muted', text: time(d.started_at) === null ? '' : ' · ' + abs(d.started_at) })),
      h('dt', { text: 'Panel address' }), h('dd', { class: 'mono', text: str(window.location.host, 120) })), { span: 'c6' }),
    tokenApps() ? null : certificatesPanel(), tokenApps() ? null : keysPanel(),
    panel('Access', h('dl', { class: 'kv' },
      h('dt', { text: 'This panel' }), h('dd', null, 'Signed in as ', h('code', { text: orDash(d.token, 64) }), readOnly()
        ? ', a read-only token: this panel can look at everything and change nothing. The token is kept in this browser tab only.'
        : tokenApps() ? ', a token that can change its apps: this panel offers rollback, stop and secrets for them, and asks before anything that removes something. The token is kept in this browser tab only.'
        : ', a token that can change things: this panel offers rollback, stop, secrets, revoking tokens and backups, and asks before anything that removes something. The token is kept in this browser tab only.'),
      h('dt', { text: 'Apps' }), h('dd', null, tokenApps() ? h('span', null, 'Only ', h('code', { text: tokenApps().join(', ') }), '. Other apps, and the pages about the daemon as a whole, are not shown.') : 'Every app.'),
      h('dt', { text: 'Expires' }), h('dd', null, tokenLeft() === null ? 'Never: this token works until it is revoked.'
        : h('span', null, abs(d.token_expires_at), h('span', { class: 'muted', text: ' · in ' + roughly(Math.max(tokenLeft(), 0)) }))),
      h('dt', { text: 'Signing out' }), h('dd', null, 'By hand, by closing the tab, or by itself after ' + Math.round(IDLE_LIMIT / 60000) + ' minutes without a click or a key press.'),
      tokenApps() ? null : h('dt', { text: 'Tokens' }), tokenApps() ? null : h('dd', null, 'Listed and revoked on the ', h('a', { href: '#tokens', text: 'Tokens' }), ' page; created on the server only. For a panel that should only look: ', h('code', { text: 'limand token create panel --read-only' }), '; for one that stops working by itself, add ', h('code', { text: '--expires 30d' }), '. Revoking a token signs its panel out at the next refresh.')),
      { span: 'c12', side: h('button', { class: 'btn small', onclick: function () { signOut(''); } }, 'Sign out') }))];
}

// ---------- render, with a last line of defence ----------
function render() {
  try {
    hideTip(); afterMount = [];
    var gated = S.mode === 'signin' || (S.mode === 'loading' && !S.info);
    $gate.hidden = !gated; $app.hidden = gated;
    document.body.className = gated ? 'gated' : '';
    if (gated) { renderGate(); return; }

    // A background refresh redraws the page. Whatever the user is in the
    // middle of (typing in a filter, reading the end of a log) is put back.
    var active = document.activeElement, keepId = active && $main.contains(active) ? active.id : '', selStart = null, selEnd = null;
    if (keepId) { try { selStart = active.selectionStart; selEnd = active.selectionEnd; } catch (e) { /* not a text field */ } }
    var oldLogs = $('logs'), logScroll = oldLogs ? oldLogs.scrollTop : null, pageScroll = $main.scrollTop;

    var r = route(); renderSide(r); renderCrumbs(r); clear($main);
    var page = h('div', { class: 'page' }); $main.appendChild(page);
    if (!S.info) { add(page, skeleton()); return; }
    add(page, banners());
    add(page, r.view === 'app' ? viewApp(r.name) : r.view === 'apps' ? viewApps() : r.view === 'activity' ? viewActivity() : r.view === 'secrets' ? viewSecrets() : r.view === 'tokens' ? viewTokens() : r.view === 'daemon' ? viewDaemon() : r.view === 'backups' ? viewBackups() : viewOverview());
    afterMount.forEach(function (fn) { try { fn(); } catch (e) { /* one broken chart must not take the page down */ } });

    $main.scrollTop = pageScroll;
    var newLogs = $('logs'); if (newLogs && logScroll !== null) newLogs.scrollTop = logScroll;
    var again = keepId ? $(keepId) : null;
    if (again) { try { again.focus({ preventScroll: true }); if (selStart !== null && again.setSelectionRange) again.setSelectionRange(selStart, selEnd); } catch (e) { /* focus is a convenience */ } }
  } catch (err) { crash(err); }
}
function crash(err) {
  try {
    $gate.hidden = true; $app.hidden = false; document.body.className = '';
    clear($main);
    $main.appendChild(h('div', { class: 'page' }, fullState('This page could not be drawn', h('p', { text: 'The panel received something it could not display. Your apps are not affected. Details: ' + str(err && err.message, 200) }),
      h('button', { class: 'btn', onclick: function () { window.location.reload(); } }, 'Reload the panel'))));
  } catch (e) { /* nothing left to do */ }
}
function setDrawer(open) { S.drawer = open; $app.className = 'app' + (S.collapsed ? ' collapsed' : '') + (S.drawer ? ' drawer' : ''); }

// ---------- wiring ----------
$('menu').appendChild(icon('menu'));
$('menu').addEventListener('click', function () { setDrawer(true); });
$('scrim').addEventListener('click', function () { setDrawer(false); });
$('range').addEventListener('click', function (e) {
  var btn = e.target instanceof Element ? e.target.closest('button[data-range]') : null;
  if (!btn || !RANGES[btn.getAttribute('data-range')]) return;
  S.range = btn.getAttribute('data-range');
  Array.prototype.forEach.call($('range').querySelectorAll('button'), function (b) { b.setAttribute('aria-pressed', b === btn ? 'true' : 'false'); });
  render(); ensureExtras();
});
$('every').addEventListener('change', function (e) { S.every = num(e.target.value) || 0; });
$('refresh').addEventListener('click', function () { refresh(); });
window.addEventListener('error', function (e) { crash(e.error || e); });
window.addEventListener('hashchange', function () {
  // A dialog belongs to the page it was opened on, unless it is busy.
  if ($dlg.open && $dlg.querySelector('#dlg-go')) closeDialog();
  stopFollowing();
  S.drawer = false; S.tab = 'overview'; render(); $main.scrollTop = 0; ensureExtras();
});
document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && S.drawer) setDrawer(false); });
var lastW = window.innerWidth, resizeTimer = null;
window.addEventListener('resize', function () { if (window.innerWidth === lastW) return; lastW = window.innerWidth; clearTimeout(resizeTimer); resizeTimer = setTimeout(render, 150); });

// A signed-in panel left alone signs itself out: the token of an admin can
// stop every app, and a forgotten tab on a shared screen should not hold it
// forever. Any click or key press counts as activity.
var IDLE_LIMIT = 30 * 60000, lastActive = Date.now();
['pointerdown', 'keydown'].forEach(function (ev) { document.addEventListener(ev, function () { lastActive = Date.now(); }, true); });

// One timer drives both the "updated 12s ago" text and the automatic
// refresh. A tab nobody is looking at does not ask the daemon for anything.
var tick = 0;
setInterval(function () {
  var el = $('updated'); if (el && S.updated) el.textContent = ' · updated ' + ago(S.updated);
  tick++;
  if (S.token && S.mode !== 'signin' && Date.now() - lastActive > IDLE_LIMIT && !$dlg.open) { signOut('Signed out after ' + Math.round(IDLE_LIMIT / 60000) + ' minutes without activity.'); return; }
  // Not while a dialog is open: the page behind it should hold still.
  if (S.every > 0 && tick % S.every === 0 && (S.mode === 'ready' || S.mode === 'offline') && !document.hidden && !$dlg.open) refresh();
}, 1000);
// Coming back to the tab after a while: show fresh data at once.
document.addEventListener('visibilitychange', function () {
  if (!document.hidden && S.every > 0 && (S.mode === 'ready' || S.mode === 'offline') && Date.now() - S.updated > S.every * 1000) refresh();
});

// ---------- start ----------
var saved = loadToken(); lastActive = Date.now();
if (saved) { S.token = saved; S.mode = 'loading'; S.gateBusy = true; render(); refresh().then(function () { S.gateBusy = false; render(); }); }
else render();
})();
