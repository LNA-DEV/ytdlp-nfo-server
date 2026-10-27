import { api, get, post, onUnauthorized } from './api.js';
import * as events from './events.js';
import { h, clear, clock } from './util.js';
import { JobsView } from './views/jobs.js';
import { SettingsView } from './views/settings.js';
import { DiagnosticsView } from './views/diagnostics.js';
import { ArchiveView } from './views/archive.js';
import { SubscriptionsView } from './views/subscriptions.js';

const state = { system: null, view: null, authRequired: false };
export const app = state;

// --- auth ---

function showLogin() {
  events.disconnect();
  document.getElementById('login').classList.remove('hidden');
  document.getElementById('login-password').focus();
}

onUnauthorized(showLogin);

document.getElementById('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const err = document.getElementById('login-error');
  const pw = document.getElementById('login-password');
  err.textContent = '';
  try {
    await api('POST', '/login', { password: pw.value }, { noAuthRedirect: true });
    pw.value = '';
    document.getElementById('login').classList.add('hidden');
    document.getElementById('logout-btn').classList.remove('hidden');
    start();
  } catch (ex) {
    err.textContent = ex.status === 429 ? 'Too many attempts, try again later.' : ex.status === 401 ? 'Wrong password.' : ex.message;
  }
});

document.getElementById('logout-btn').addEventListener('click', async () => {
  await post('/logout');
  location.reload();
});

// --- header and banners ---

function renderSystem(sys) {
  state.system = sys;
  const badge = document.getElementById('ytdlp-badge');
  const y = sys.ytdlp || {};
  badge.textContent = y.active ? `yt-dlp ${y.active.version || y.active.tag} (${y.active.channel})` : 'yt-dlp: none active';
  const upd = document.getElementById('update-badge');
  const newer = y.updates && y.active && y.updates[y.active.channel];
  upd.classList.toggle('hidden', !newer);
  if (newer) upd.title = `yt-dlp ${newer} is available (${y.active.channel})`;
  document.getElementById('imp-badge').classList.toggle('hidden', !y.active || y.impersonationAvailable);

  const banners = clear(document.getElementById('banners'));
  const q = sys.queue || {};
  if (q.paused) {
    banners.append(h('div', { class: 'banner warn' },
      h('span', { class: 'grow' }, `Queue paused: ${q.pauseReason || ''}${q.pauseUntil ? ` (retrying at ${clock(q.pauseUntil)})` : ''}`),
      h('button', { class: 'btn small', onclick: () => post('/system/resume') }, 'Resume')));
  }
  for (const [site, until] of Object.entries(q.cooldowns || {})) {
    banners.append(h('div', { class: 'banner warn' }, h('span', { class: 'grow' }, `Rate-limited by ${site}, resuming at ${clock(until)}`)));
  }
  for (const w of sys.warnings || []) {
    if (w.key === 'disk' && q.paused) continue;
    banners.append(h('div', { class: 'banner warn' }, h('span', { class: 'grow' }, w.message),
      h('button', { class: 'btn small', onclick: () => api('DELETE', '/system/warnings/' + encodeURIComponent(w.key)) }, 'Dismiss')));
  }
  if (!y.active) {
    banners.append(h('div', { class: 'banner err' }, h('span', { class: 'grow' }, 'No yt-dlp version is active. Install one in Settings → yt-dlp.')));
  }
}

document.getElementById('update-badge').addEventListener('click', () => { location.hash = '#/settings/ytdlp'; });
document.getElementById('imp-badge').addEventListener('click', () => { location.hash = '#/diagnostics'; });

events.on('system', renderSystem);
events.on('connection', (ok) => {
  document.body.classList.toggle('offline', !ok);
});

// --- router ---

const routes = [
  { re: /^#\/settings(?:\/(\w+))?$/, nav: 'settings', make: (m) => new SettingsView(m[1]) },
  { re: /^#\/diagnostics$/, nav: 'diagnostics', make: () => new DiagnosticsView() },
  { re: /^#\/archive$/, nav: 'archive', make: () => new ArchiveView() },
  { re: /^#\/subscriptions\/(\d+)$/, nav: 'subscriptions', make: (m) => new SubscriptionsView({ id: m[1] }) },
  { re: /^#\/subscriptions$/, nav: 'subscriptions', make: () => new SubscriptionsView({}) },
  { re: /^#\/jobs\/(\d+)$/, nav: 'jobs', make: (m) => new JobsView({ focus: m[1] }) },
  { re: /^#\/(active|queued|completed|failed)?$/, nav: 'jobs', make: (m) => new JobsView({ tab: m[1] }) },
];

function route() {
  const hash = location.hash || '#/';
  let match = null, r = null;
  for (const x of routes) {
    match = hash.match(x.re);
    if (match) { r = x; break; }
  }
  if (!r) { r = routes[routes.length - 1]; match = ['#/']; }
  if (state.view && state.view.destroy) state.view.destroy();
  for (const a of document.querySelectorAll('[data-nav]')) a.classList.toggle('active', a.dataset.nav === r.nav);
  const root = clear(document.getElementById('view'));
  state.view = r.make(match);
  root.append(state.view.render());
}

window.addEventListener('hashchange', () => {
  // Tab switches inside the jobs view do not rebuild the view.
  if (state.view && state.view.handleHash && state.view.handleHash(location.hash)) return;
  route();
});

// --- start ---

async function start() {
  try {
    const sys = await get('/system');
    renderSystem(sys);
  } catch (e) {
    if (e.status === 401) return;
  }
  events.connect();
  route();
}

async function boot() {
  try {
    const st = await api('GET', '/auth', undefined, { noAuthRedirect: true });
    state.authRequired = st.authRequired;
    document.getElementById('logout-btn').classList.toggle('hidden', !st.authRequired || st.method !== 'session');
    if (!st.authenticated) { showLogin(); return; }
  } catch { /* server unreachable: try anyway */ }
  start();
}

boot();
