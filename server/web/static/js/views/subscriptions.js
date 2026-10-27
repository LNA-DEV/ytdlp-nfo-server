import { get, post, del, patch } from '../api.js';
import * as events from '../events.js';
import { h, clear, duration, time, clock, toast, modal, confirmDialog, debounce } from '../util.js';
import { loadOptions, select } from './jobs.js';

// Subscriptions (subscriptions concept §7): the list of subscriptions, a
// detail page per subscription and the add/edit dialogs.

const TAB_LABEL = { videos: 'Videos', shorts: 'Shorts', streams: 'Live' };
const INTERVALS = [[15, '15 min'], [30, '30 min'], [60, '1 h'], [180, '3 h'], [360, '6 h'], [720, '12 h'], [1440, '24 h'], [4320, '3 days'], [10080, '7 days']];
const DETAIL_TABS = [
  ['new', 'New', 'newTab'],
  ['backlog', 'Backlog', 'backlogTab'],
  ['library', 'In library', 'library'],
  ['ignored', 'Ignored', 'ignored'],
  ['failed', 'Failed', 'failed'],
];
const PAGE = 50;

// --- formatting ---

function span(ms) {
  const m = Math.round(Math.abs(ms) / 60000);
  if (m < 1) return 'less than a minute';
  if (m < 60) return `${m} min`;
  const hrs = Math.round(m / 60);
  if (hrs < 48) return `${hrs} h`;
  return `${Math.round(hrs / 24)} d`;
}
function ago(iso) { return iso ? span(Date.now() - new Date(iso)) + ' ago' : 'never'; }
function until(iso) {
  if (!iso) return '';
  const d = new Date(iso) - Date.now();
  if (d <= 0) return 'now';
  const m = Math.floor(d / 60000);
  return 'in ' + (m >= 60 && m < 48 * 60 ? `${Math.floor(m / 60)} h` : span(d));
}

// img shows a thumbnail and falls back to the empty placeholder if it
// cannot be loaded.
function img(cls, src, lazy = true) {
  if (!src) return h('div', { class: cls });
  return h('img', { class: cls, src, loading: lazy ? 'lazy' : null, referrerpolicy: 'no-referrer', alt: '',
    onerror: (e) => e.target.replaceWith(h('div', { class: cls })) });
}
function every(min) {
  const x = INTERVALS.find(([m]) => m === min);
  return 'every ' + (x ? x[1] : `${min} min`);
}
function date(ymd) { return ymd && ymd.length === 8 ? `${ymd.slice(0, 4)}-${ymd.slice(4, 6)}-${ymd.slice(6)}` : ''; }

// sanitize approximates the server's folder sanitizing for the path preview.
function sanitize(s) {
  let out = '';
  for (const ch of s.trim()) {
    if (ch === '/') out += '⧸';
    else if (ch === '\\') out += '⧹';
    else if ('"*:<>?|'.includes(ch)) out += String.fromCharCode(ch.charCodeAt(0) + 0xfee0);
    else out += ch;
  }
  return out.replace(/^\.+/, '') || '_';
}

function sourcesLabel(sub) {
  const tabs = sub.sources.filter((s) => s.tab).map((s) => TAB_LABEL[s.tab] || s.tab);
  return [sub.kind || 'source', tabs.join(', ')].filter(Boolean).join(' · ');
}

function modeValue(sub) { return sub.enabled ? sub.mode : 'paused'; }

function modeSelect(sub, onChange) {
  const sel = select('mode', [['auto', 'Auto'], ['manual', 'Manual'], ['paused', 'Paused']], modeValue(sub));
  sel.classList.add('mode-select', 'mode-' + modeValue(sub));
  sel.addEventListener('click', (e) => e.stopPropagation());
  sel.addEventListener('change', async () => {
    const v = sel.value;
    const body = v === 'paused' ? { enabled: false } : { mode: v, enabled: true };
    try { onChange(await patch('/subscriptions/' + sub.id, body)); } catch (ex) { toast(ex.message, 'err'); sel.value = modeValue(sub); }
  });
  return sel;
}

function checkResult(sub) {
  if (sub.state === 'checking') return h('span', { class: 'muted' }, 'checking…');
  if (!sub.lastCheckAt) return h('span', { class: 'muted' }, '– never');
  if (sub.error) return h('span', { class: 'bad', title: sub.error }, '⚠ ' + ago(sub.lastCheckAt));
  return h('span', { class: 'ok' }, '✓ ' + ago(sub.lastCheckAt));
}

function errorLine(sub) {
  if (!sub.error) return null;
  const next = sub.enabled && sub.nextCheckAt ? `, next check ${clock(sub.nextCheckAt)}` : '';
  return h('div', { class: 'sub-error' }, `⚠ ${sub.hint || sub.error}${next}`, sub.hint && sub.hint !== sub.error ? h('span', { class: 'muted' }, ` (${sub.error})`) : null);
}

// --- shared actions ---

async function showLog(sub) {
  let text = '';
  try { text = await get(`/subscriptions/${sub.id}/log`); } catch (ex) { toast(ex.message, 'err'); return; }
  await modal((close) => h('div', { class: 'wide' }, h('h2', {}, `Check log: ${sub.name}`),
    h('pre', { class: 'log' }, text || '(empty)'),
    h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', onclick: () => close() }, 'Close'))));
}

async function deleteSub(sub) {
  const cancel = h('input', { type: 'checkbox' });
  const ok = await modal((close) => h('div', {},
    h('h2', {}, `Delete “${sub.name}”?`),
    h('p', {}, 'The subscription and its list of videos are removed. Files in the library, the archive and finished downloads are kept.'),
    h('label', { class: 'check' }, cancel, 'Also cancel queued downloads of this subscription'),
    h('div', { class: 'btn-row' }, h('button', { class: 'btn', onclick: () => close(false) }, 'Cancel'),
      h('button', { class: 'btn danger', onclick: () => close(true) }, 'Delete'))));
  if (!ok) return false;
  await del(`/subscriptions/${sub.id}?cancelQueued=${cancel.checked ? 1 : 0}`);
  toast(`Deleted ${sub.name}`);
  return true;
}

async function checkNow(sub, full = false) {
  try {
    await post(`/subscriptions/${sub.id}/check`, { full });
    toast(full ? `Full rescan of ${sub.name} started` : `Checking ${sub.name}`);
  } catch (ex) { toast(ex.message, 'err'); }
}

function menu(items) {
  const d = h('details', { class: 'menu', onclick: (e) => e.stopPropagation() }, h('summary', { title: 'More' }, '⋮'));
  const list = h('div', { class: 'menu-list' });
  for (const [label, fn, cls] of items.filter(Boolean)) {
    list.append(h('button', { class: 'menu-item ' + (cls || ''), onclick: async () => { d.open = false; try { await fn(); } catch (ex) { toast(ex.message, 'err'); } } }, label));
  }
  d.append(list);
  return d;
}

// --- option and filter fields (add and edit dialog) ---

function triSelect(name, value) {
  return select(name, [['', 'Default'], ['on', 'On'], ['off', 'Off']], value === true ? 'on' : value === false ? 'off' : '');
}

function optionInputs(o, cur = {}) {
  const imp = [['', 'Default'], ['off', 'Off'], ['any', 'Any client'], ...o.impersonateTargets.map((t) => [t, t])];
  const cookies = [['', 'Default'], ['none', 'None'], ...o.cookieProfiles.map((c) => [c, c])];
  const vers = [['', 'Active version'], ['nightly:latest', 'Latest nightly'], ['stable:latest', 'Latest stable'], ...o.ytdlpVersions.map((v) => [v, v])];
  const impCur = cur.impersonate === undefined ? '' : cur.impersonate === '' ? 'off' : cur.impersonate;
  const ckCur = cur.cookieProfile === undefined ? '' : cur.cookieProfile === '' ? 'none' : cur.cookieProfile;
  return h('div', { class: 'more' },
    h('label', {}, 'Preset', select('preset', [['', `Default (${o.defaults.preset})`], ...o.presets.map((p) => [p, p])], cur.preset ?? '')),
    h('label', {}, 'Container', select('container', [['', `Default (${o.defaults.container.toUpperCase()})`], ...o.containers.map((c) => [c, c.toUpperCase()])], cur.container ?? '')),
    h('label', {}, 'All audio tracks', triSelect('allAudio', cur.allAudio)),
    h('label', {}, 'Subtitles', triSelect('subtitles', cur.subtitles)),
    h('label', {}, 'Impersonate', select('impersonate', imp, impCur)),
    h('label', {}, 'Cookie profile', select('cookieProfile', cookies, ckCur)),
    h('label', {}, 'yt-dlp version', select('ytdlpVersion', vers, cur.ytdlpVersion ?? '')));
}

function readOptionInputs(root, library) {
  const o = {};
  const val = (n) => { const el = root.querySelector(`[name=${n}]`); return el ? el.value : ''; };
  if (val('preset')) o.preset = val('preset');
  if (val('container')) o.container = val('container');
  for (const k of ['allAudio', 'subtitles']) if (val(k)) o[k] = val(k) === 'on';
  const imp = val('impersonate');
  if (imp === 'off') o.impersonate = '';
  else if (imp) o.impersonate = imp;
  const ck = val('cookieProfile');
  if (ck === 'none') o.cookieProfile = '';
  else if (ck) o.cookieProfile = ck;
  if (val('ytdlpVersion')) o.ytdlpVersion = val('ytdlpVersion');
  if (library) o.library = library;
  return o;
}

function filterInputs(f = {}) {
  const mins = (s) => (s ? String(Math.round((s / 60) * 10) / 10) : '');
  const ymd = f.publishedAfter ? date(f.publishedAfter) : '';
  return h('div', { class: 'more' },
    h('label', {}, 'Minimum duration (minutes)', h('input', { type: 'number', name: 'minDuration', min: 0, step: 'any', value: mins(f.minDuration) })),
    h('label', {}, 'Maximum duration (minutes)', h('input', { type: 'number', name: 'maxDuration', min: 0, step: 'any', value: mins(f.maxDuration) })),
    h('label', {}, 'Title must match (regex)', h('input', { type: 'text', name: 'titleInclude', maxlength: 200, value: f.titleInclude || '' })),
    h('label', {}, 'Title must not match (regex)', h('input', { type: 'text', name: 'titleExclude', maxlength: 200, value: f.titleExclude || '', placeholder: 'e.g. trailer|teaser' })),
    h('label', {}, 'Published after', h('input', { type: 'date', name: 'publishedAfter', value: ymd })));
}

function readFilterInputs(root) {
  const val = (n) => root.querySelector(`[name=${n}]`).value.trim();
  const f = {};
  if (val('minDuration')) f.minDuration = Math.round(Number(val('minDuration')) * 60);
  if (val('maxDuration')) f.maxDuration = Math.round(Number(val('maxDuration')) * 60);
  if (val('titleInclude')) f.titleInclude = val('titleInclude');
  if (val('titleExclude')) f.titleExclude = val('titleExclude');
  if (val('publishedAfter')) f.publishedAfter = val('publishedAfter').replaceAll('-', '');
  return f;
}

function collapsible(label, content, open = false) {
  const body = h('div', { class: open ? '' : 'hidden' }, content);
  const btn = h('button', { class: 'link-btn', type: 'button' }, (open ? '▾ ' : '▸ ') + label);
  btn.addEventListener('click', () => { body.classList.toggle('hidden'); btn.textContent = (body.classList.contains('hidden') ? '▸ ' : '▾ ') + label; });
  return h('div', { class: 'collapsible' }, btn, body);
}

function intervalSelect(o, cur) {
  const def = o.subscriptionInterval || 360;
  return select('interval', [['0', `Every ${(INTERVALS.find(([m]) => m === def) || [0, `${def} min`])[1]} (default)`], ...INTERVALS.map(([m, l]) => [String(m), 'Every ' + l])], String(cur || 0));
}

// libraryField returns the library select (only with several libraries)
// and keeps the path preview of library + folder up to date (§7.4).
function libraryAndFolder(o, cur, nameInput) {
  const libs = o.libraries;
  const def = o.defaults.library;
  const lib = libs.length > 1 ? select('library', [['', `Default (${def})`], ...libs.map((l) => [l, l])], cur.library || '') : null;
  const folder = h('input', { type: 'text', name: 'folder', value: cur.folder || '', placeholder: nameInput.value || 'Name', maxlength: 200 });
  const preview = h('div', { class: 'path-preview mono' });
  const update = () => {
    folder.placeholder = nameInput.value || 'Name';
    const l = (lib && lib.value) || cur.effectiveLibrary || def;
    const root = (o.libraryPaths[l] || '').replace(/\/$/, '');
    if (!o.namingTemplate.includes('{playlist_title')) {
      preview.textContent = `→ ${root}/ (the naming template has no {playlist_title?}: videos are not grouped in a folder)`;
      return;
    }
    preview.textContent = `→ ${root}/${sanitize(folder.value || nameInput.value || '_')}/`;
  };
  for (const el of [folder, nameInput, lib]) if (el) el.addEventListener(el.tagName === 'SELECT' ? 'change' : 'input', update);
  update();
  const row = h('div', { class: 'form-row' },
    lib ? h('label', { class: 'field' }, 'Library', lib) : null,
    h('label', { class: 'field grow' }, 'Folder', folder));
  return { row, preview, folder, lib };
}

// --- add dialog ---

export async function addDialog(initialURL = '') {
  const o = await loadOptions(true);
  return modal((close) => {
    const url = h('input', { type: 'url', placeholder: 'https://…/@channel or a playlist URL', value: initialURL, required: true });
    const previewBtn = h('button', { class: 'btn', type: 'button' }, 'Preview');
    const card = h('div', { class: 'preview' });
    const form = h('div', { class: 'hidden' });
    const subscribe = h('button', { class: 'btn primary', disabled: true }, 'Subscribe');
    let p = null;
    const tabsBox = h('div', { class: 'choices' });
    const name = h('input', { type: 'text', name: 'name', maxlength: 200 });
    const lf = libraryAndFolder(o, {}, name);
    const mode = h('div', { class: 'choices' },
      h('label', { class: 'check' }, h('input', { type: 'radio', name: 'mode', value: 'manual', checked: true }), 'Manual'),
      h('label', { class: 'check' }, h('input', { type: 'radio', name: 'mode', value: 'auto' }), 'All new videos'));
    const latestN = h('input', { type: 'number', min: 1, value: 10, style: 'width:5rem' });
    const backlog = h('div', { class: 'choices' },
      h('label', { class: 'check' }, h('input', { type: 'radio', name: 'backlog', value: 'none', checked: true }), "Don't download"),
      h('label', { class: 'check' }, h('input', { type: 'radio', name: 'backlog', value: 'latest' }), 'Latest', latestN),
      h('label', { class: 'check' }, h('input', { type: 'radio', name: 'backlog', value: 'all' }), 'All'));
    const interval = intervalSelect(o, 0);
    const filters = filterInputs();
    const options = optionInputs(o);
    form.append(
      h('div', { class: 'field' }, 'Tabs', tabsBox),
      h('label', { class: 'field' }, 'Name', name),
      lf.row, lf.preview,
      h('div', { class: 'field' }, 'Mode', mode),
      h('div', { class: 'field' }, 'Existing videos', backlog),
      h('label', { class: 'field' }, 'Check', interval),
      collapsible('Filters (duration, title, date)', filters),
      collapsible('Download options (preset, container, cookies, …)', options));

    const doPreview = async () => {
      const u = url.value.trim();
      if (!u) return;
      previewBtn.disabled = true;
      subscribe.disabled = true;
      clear(card).append(h('div', { class: 'muted' }, 'Listing… (up to a minute)'));
      try {
        p = await post('/subscriptions/preview', { url: u });
      } catch (ex) {
        clear(card).append(h('div', { class: 'bad' }, ex.message), ex.body && ex.body.hint ? h('div', { class: 'muted' }, ex.body.hint) : null);
        form.classList.add('hidden');
        previewBtn.disabled = false;
        return;
      }
      previewBtn.disabled = false;
      const count = p.count ? `~${p.count.toLocaleString()} videos` : `${p.entries.length}${p.entries.length >= 10 ? '+' : ''} videos`;
      clear(card).append(h('div', { class: 'preview-head' },
        img('avatar', p.thumbnail, false),
        h('div', {},
          h('div', { class: 'job-title' }, p.name),
          h('div', { class: 'muted' }, `${p.kind}, ${count}${p.inLibrary ? ` (${p.inLibrary} of the latest already in library)` : ''}`),
          p.resolvedFrom ? h('div', { class: 'muted' }, `Subscribing to the channel of “${p.resolvedFrom}”.`) : null)),
        p.entries.length ? h('div', { class: 'preview-latest muted' }, 'Latest: ' + p.entries.slice(0, 3).map((e) => e.title).join(' · ')) : null);
      clear(tabsBox);
      for (const s of p.sources) {
        const cb = h('input', { type: 'checkbox', checked: s.selected && !s.subscriptionId, disabled: !!s.subscriptionId, dataset: { url: s.url, canonical: s.canonical } });
        cb.addEventListener('change', () => { subscribe.disabled = !tabsBox.querySelector('input:checked'); });
        tabsBox.append(h('label', { class: 'check' }, cb, s.tab ? TAB_LABEL[s.tab] || s.tab : 'This playlist',
          s.subscriptionId ? h('a', { href: '#/subscriptions/' + s.subscriptionId, onclick: () => close() }, ` (already in “${s.subscriptionName}”)`) : null));
      }
      name.value = p.name;
      name.dispatchEvent(new Event('input'));
      form.classList.remove('hidden');
      subscribe.disabled = !tabsBox.querySelector('input:checked');
    };
    previewBtn.addEventListener('click', doPreview);
    url.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); doPreview(); } });

    subscribe.addEventListener('click', async () => {
      const sources = [...tabsBox.querySelectorAll('input:checked')].map((cb) => ({ url: cb.dataset.url, canonical: cb.dataset.canonical }));
      const bl = form.querySelector('[name=backlog]:checked').value;
      const body = {
        name: name.value.trim(), folder: lf.folder.value.trim(), sources,
        mode: form.querySelector('[name=mode]:checked').value,
        backlog: bl === 'latest' ? { mode: 'latest', n: Number(latestN.value) || 10 } : { mode: bl },
        intervalMinutes: Number(interval.value), filters: readFilterInputs(filters),
        options: readOptionInputs(options, lf.lib ? lf.lib.value : ''),
      };
      subscribe.disabled = true;
      try {
        const sub = await post('/subscriptions', body);
        toast(`Subscribed to ${sub.name}`, 'ok');
        close(sub);
      } catch (ex) {
        toast(ex.message, 'err');
        subscribe.disabled = false;
      }
    });
    if (initialURL) setTimeout(doPreview, 0);
    return h('div', { class: 'wide' }, h('h2', {}, 'Add subscription'),
      h('div', { class: 'submit-row' }, url, previewBtn), card, form,
      h('div', { class: 'btn-row' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), subscribe));
  });
}

// --- edit dialog ---

async function editDialog(sub) {
  const o = await loadOptions(true);
  return modal((close) => {
    const name = h('input', { type: 'text', name: 'name', value: sub.name, maxlength: 200 });
    const lf = libraryAndFolder(o, { library: sub.options.library, folder: sub.folder, effectiveLibrary: sub.library }, name);
    const warn = h('div', { class: 'warn-text' });
    const updateWarn = () => {
      const folderChanged = (lf.folder.value.trim() || name.value.trim()) !== sub.effectiveFolder;
      const libChanged = lf.lib && (lf.lib.value || '') !== (sub.options.library || '');
      clear(warn);
      if (folderChanged || libChanged) warn.append('New videos go to the new place; existing ones stay where they are.');
      if (sub.sharedWith && sub.sharedWith.length) warn.append(h('div', { class: 'muted' }, `This folder is shared with: ${sub.sharedWith.join(', ')}.`));
    };
    for (const el of [name, lf.folder, lf.lib]) if (el) el.addEventListener(el.tagName === 'SELECT' ? 'change' : 'input', updateWarn);
    updateWarn();
    const mode = select('mode', [['manual', 'Manual'], ['auto', 'All new videos']], sub.mode);
    const interval = intervalSelect(o, sub.intervalMinutes);
    const notify = h('input', { type: 'checkbox', checked: sub.notify });
    const listing = select('listing', [['auto', 'Automatic (window for channel tabs)'], ['window', 'Always a window'], ['full', 'Always a full listing']], sub.listing);
    const filters = filterInputs(sub.filters);
    const options = optionInputs(o, sub.options);
    const removed = new Set();
    const srcList = h('div', {});
    const drawSources = () => {
      clear(srcList).append(...sub.sources.map((s) => h('div', { class: 'source-row' + (removed.has(s.id) ? ' removed' : '') },
        h('span', { class: 'mono grow' }, s.url), s.tab ? h('span', { class: 'muted' }, TAB_LABEL[s.tab] || s.tab) : null,
        h('button', { class: 'btn small', type: 'button', onclick: () => { removed.has(s.id) ? removed.delete(s.id) : removed.add(s.id); drawSources(); } }, removed.has(s.id) ? 'Keep' : 'Remove'))));
    };
    drawSources();
    const addURL = h('input', { type: 'url', placeholder: 'Add a source URL (e.g. the Shorts tab)', style: 'flex:1' });
    const save = h('button', { class: 'btn primary' }, 'Save');
    save.addEventListener('click', async () => {
      const body = {
        name: name.value.trim(), folder: lf.folder.value.trim(), mode: mode.value, intervalMinutes: Number(interval.value),
        notify: notify.checked, listing: listing.value, filters: readFilterInputs(filters),
        options: readOptionInputs(options, lf.lib ? lf.lib.value : sub.options.library || ''),
        removeSources: [...removed],
      };
      if (addURL.value.trim()) body.addSources = [{ url: addURL.value.trim() }];
      save.disabled = true;
      try {
        const v = await patch('/subscriptions/' + sub.id, body);
        toast('Saved', 'ok');
        close(v);
      } catch (ex) { toast(ex.message, 'err'); save.disabled = false; }
    });
    return h('div', { class: 'wide' }, h('h2', {}, `Edit ${sub.name}`),
      h('label', { class: 'field' }, 'Name', name),
      lf.row, lf.preview, warn,
      h('div', { class: 'form-row' }, h('label', { class: 'field' }, 'Mode', mode), h('label', { class: 'field' }, 'Check', interval)),
      h('label', { class: 'check' }, notify, 'Notify on new videos'),
      h('div', { class: 'field' }, 'Sources', srcList, h('div', { class: 'submit-row' }, addURL)),
      collapsible('Filters (duration, title, date)', filters, !!Object.keys(sub.filters || {}).length),
      collapsible('Download options', options),
      collapsible('Advanced', h('label', { class: 'field' }, 'Listing', listing)),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'), save));
  });
}

// --- view ---

export class SubscriptionsView {
  constructor({ id }) {
    this.id = id || null;
    this.unsub = [];
    this.subs = new Map();
    this.sort = 'name';
    this.query = '';
    this.tab = 'new';
    this.offset = 0;
    this.selected = new Set();
  }

  destroy() { this.unsub.forEach((u) => u()); }

  render() {
    this.root = h('div', {});
    this.unsub.push(events.on('subscription', (s) => this.onSub(s)));
    this.unsub.push(events.on('subscription_removed', (s) => this.onRemoved(s.id)));
    this.unsub.push(events.on('entries', (e) => this.onEntries(e)));
    this.unsub.push(events.on('resync', () => this.reload()));
    this.unsub.push(events.on('connection', (ok) => { if (ok) this.reload(); }));
    this.reloadEntries = debounce(() => this.loadEntries(true), 400);
    this.reload();
    return this.root;
  }

  reload() { return this.id ? this.loadDetail() : this.loadList(); }

  onSub(s) {
    if (this.id) {
      if (s.id === this.id) { this.sub = s; this.renderDetailHead(); }
      return;
    }
    this.subs.set(s.id, s);
    this.renderRows();
  }

  onRemoved(id) {
    if (this.id === id) { location.hash = '#/subscriptions'; return; }
    this.subs.delete(id);
    this.renderRows();
  }

  onEntries(e) { if (this.id && e.subscription === this.id) this.reloadEntries(); }

  // --- list (§7.1) ---

  async loadList() {
    let data;
    try { data = await get('/subscriptions'); } catch (ex) { if (ex.status !== 401) toast(ex.message, 'err'); return; }
    this.subs = new Map(data.subscriptions.map((s) => [s.id, s]));
    const r = clear(this.root);
    const filter = h('input', { type: 'text', class: 'filter', placeholder: 'Filter…', value: this.query, oninput: debounce((e) => { this.query = e.target.value.trim().toLowerCase(); this.renderRows(); }, 200) });
    const sort = select('sort', [['name', 'Name'], ['newest', 'Newest video'], ['inbox', 'New videos']], this.sort);
    sort.addEventListener('change', () => { this.sort = sort.value; this.renderRows(); });
    this.countEl = h('span', { class: 'count' });
    r.append(h('div', { class: 'tabs' },
      h('button', { class: 'tab active' }, 'Subscriptions', this.countEl),
      h('div', { class: 'tab-actions' },
        h('button', { class: 'btn small primary', onclick: async () => { const s = await addDialog(); if (s) location.hash = '#/subscriptions/' + s.id; } }, '+ Add subscription'),
        h('button', { class: 'btn small', title: 'Check all subscriptions now', onclick: async () => { try { const x = await post('/subscriptions/check', {}); toast(`Checking ${x.due} subscriptions`); } catch (ex) { toast(ex.message, 'err'); } } }, '⟳ All'))),
      h('div', { class: 'list-tools' }, filter, h('label', { class: 'muted' }, 'Sort ', sort)));
    this.listEl = h('div', {});
    r.append(this.listEl);
    this.renderRows();
  }

  renderRows() {
    if (!this.listEl) return;
    let subs = [...this.subs.values()];
    if (this.query) subs = subs.filter((s) => (s.name + ' ' + s.uploader + ' ' + s.sources.map((x) => x.url).join(' ')).toLowerCase().includes(this.query));
    const by = {
      name: (a, b) => a.name.localeCompare(b.name),
      newest: (a, b) => (b.counts.newest || '').localeCompare(a.counts.newest || ''),
      inbox: (a, b) => b.counts.new - a.counts.new || a.name.localeCompare(b.name),
    }[this.sort];
    subs.sort(by);
    this.countEl.textContent = this.subs.size || '';
    const l = clear(this.listEl);
    if (!this.subs.size) {
      l.append(h('div', { class: 'empty' }, 'No subscriptions yet. Add a channel or playlist to watch it for new videos.'));
      return;
    }
    for (const s of subs) l.append(this.row(s));
  }

  row(s) {
    const c = s.counts || {};
    const counts = [`${c.total || 0} listed`, `${c.library || 0} in library`, `${c.new || 0} new`];
    const replace = (v) => { this.subs.set(v.id, v); this.renderRows(); };
    return h('div', { class: 'sub-row' + (s.enabled ? '' : ' paused'), onclick: () => { location.hash = '#/subscriptions/' + s.id; } },
      img('avatar', s.thumbnail),
      h('div', { class: 'sub-main' },
        h('div', { class: 'sub-name' }, h('a', { href: '#/subscriptions/' + s.id }, s.name), c.new ? h('span', { class: 'dot', title: `${c.new} new` }) : null),
        h('div', { class: 'muted small' }, sourcesLabel(s)),
        h('div', { class: 'muted small' }, counts.join(' · ')),
        s.warning ? h('div', { class: 'warn-text small' }, s.warning) : null,
        errorLine(s)),
      h('div', { class: 'sub-side' },
        modeSelect(s, replace),
        h('div', { class: 'small' }, checkResult(s)),
        s.enabled && s.nextCheckAt ? h('div', { class: 'muted small' }, 'next ' + until(s.nextCheckAt)) : null,
        this.subMenu(s, replace)));
  }

  subMenu(s, replace) {
    return menu([
      ['Check now', () => checkNow(s)],
      ['Full rescan', () => checkNow(s, true)],
      ['Edit', async () => { const v = await editDialog(s); if (v) replace(v); }],
      ['Show check log', () => showLog(s)],
      [s.enabled ? 'Pause' : 'Resume', async () => replace(await patch('/subscriptions/' + s.id, { enabled: !s.enabled }))],
      ['Delete', async () => { await deleteSub(s); }, 'danger'],
    ]);
  }

  // --- detail (§7.3) ---

  async loadDetail() {
    try { this.sub = await get('/subscriptions/' + this.id); } catch (ex) {
      clear(this.root).append(h('div', { class: 'empty' }, ex.status === 404 ? 'This subscription does not exist (anymore).' : ex.message), h('a', { href: '#/subscriptions' }, '← Subscriptions'));
      return;
    }
    const r = clear(this.root);
    this.headEl = h('div', { class: 'section sub-detail' });
    this.tabsEl = h('div', { class: 'tabs' });
    this.actionsEl = h('div', { class: 'entry-actions' });
    this.entriesEl = h('div', { class: 'entries' });
    this.moreBtn = h('button', { class: 'btn load-more hidden', onclick: () => this.loadEntries(false) }, 'Load more');
    r.append(h('a', { href: '#/subscriptions', class: 'back' }, '← Subscriptions'), this.headEl, this.tabsEl, this.actionsEl, this.entriesEl, this.moreBtn);
    this.renderDetailHead();
    this.loadEntries(true);
  }

  renderDetailHead() {
    const s = this.sub;
    if (!s || !this.headEl) return;
    const replace = (v) => { this.sub = v; this.renderDetailHead(); };
    const hd = clear(this.headEl);
    const mode = h('div', { class: 'choices' },
      ...[['auto', 'All new videos'], ['manual', 'Manual']].map(([v, l]) => h('label', { class: 'check' },
        h('input', { type: 'radio', name: 'detail-mode', checked: s.mode === v, onchange: async () => { try { replace(await patch('/subscriptions/' + s.id, { mode: v })); } catch (ex) { toast(ex.message, 'err'); } } }), l)),
      h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: s.notify, onchange: async (e) => { try { replace(await patch('/subscriptions/' + s.id, { notify: e.target.checked })); } catch (ex) { toast(ex.message, 'err'); } } }), 'Notify on new videos'),
      s.enabled ? null : h('span', { class: 'badge b-cancelled' }, 'paused'));
    const o = s.options || {};
    const checked = s.lastCheckAt ? `Checked ${ago(s.lastCheckAt)}` : 'Not checked yet';
    const next = s.state === 'checking' ? 'checking now' : s.enabled && s.nextCheckAt ? `next ${until(s.nextCheckAt)}` : 'paused';
    hd.append(
      h('div', { class: 'detail-head' },
        img('avatar big', s.thumbnail, false),
        h('div', { class: 'grow' }, h('h1', {}, s.name), h('div', { class: 'muted small' }, [s.uploader, sourcesLabel(s)].filter(Boolean).join(' · '))),
        h('div', { class: 'btn-row' },
          h('button', { class: 'btn small', disabled: s.state === 'checking' || !s.enabled, onclick: () => checkNow(s) }, 'Check now'),
          h('button', { class: 'btn small', onclick: async () => { const v = await editDialog(s); if (v) replace(v); } }, 'Edit'),
          menu([
            ['Full rescan', () => checkNow(s, true)],
            ['Show check log', () => showLog(s)],
            [s.enabled ? 'Pause' : 'Resume', async () => replace(await patch('/subscriptions/' + s.id, { enabled: !s.enabled }))],
            ['Delete', async () => { if (await deleteSub(s)) location.hash = '#/subscriptions'; }, 'danger'],
          ]))),
      h('div', { class: 'field' }, 'Mode', mode),
      h('div', { class: 'sources' }, s.sources.map((src) => h('div', { class: 'source-row' },
        h('a', { class: 'mono grow', href: src.url, target: '_blank', rel: 'noreferrer' }, src.url),
        src.tab ? h('span', { class: 'muted' }, TAB_LABEL[src.tab] || src.tab) : null,
        src.error ? h('span', { class: 'bad small', title: src.error }, `⚠ ${src.errorClass ? src.errorClass.replace('_', ' ') + ': ' : ''}${src.error}`) : null,
        src.impersonate ? h('span', { class: 'muted small', title: 'This source only lists with impersonation' }, 'impersonating') : null))),
      h('div', { class: 'muted small' }, `${checked} · ${next} · ${every(s.effectiveIntervalMinutes)}`),
      h('div', { class: 'muted small' }, `Library: ${s.library} · Folder: “${s.effectiveFolder}” · Preset: ${o.preset || 'default'} · Container: ${o.container ? o.container.toUpperCase() : 'default'}`,
        s.sharedWith && s.sharedWith.length ? ` · folder shared with ${s.sharedWith.join(', ')}` : ''));
    const err = errorLine(s);
    if (err) hd.append(err);
    if (s.warning) {
      hd.append(h('div', { class: 'banner warn inline' }, h('span', { class: 'grow' }, s.warning),
        h('button', { class: 'btn small', onclick: () => this.downloadStatus('new') }, 'Download all new'),
        h('button', { class: 'btn small', onclick: async () => replace(await patch('/subscriptions/' + s.id, { dismissWarning: true })) }, 'Dismiss')));
    }
    this.renderTabs();
  }

  renderTabs() {
    const c = this.sub.counts || {};
    const t = clear(this.tabsEl);
    for (const [id, label, key] of DETAIL_TABS) {
      t.append(h('button', { class: 'tab' + (this.tab === id ? ' active' : ''), onclick: () => { this.tab = id; this.selected.clear(); this.renderTabs(); this.loadEntries(true); } },
        label, h('span', { class: 'count' }, c[key] ? c[key].toLocaleString() : '')));
    }
    t.append(h('input', { type: 'text', class: 'filter', placeholder: 'Search…', value: this.entryQuery || '', oninput: debounce((e) => { this.entryQuery = e.target.value.trim(); this.loadEntries(true); }, 300) }));
    this.renderEntryActions();
  }

  renderEntryActions() {
    const a = clear(this.actionsEl);
    const n = this.selected.size;
    const c = this.sub.counts || {};
    if (n) {
      a.append(h('span', { class: 'muted' }, `${n} selected:`),
        h('button', { class: 'btn small primary', onclick: () => this.download([...this.selected]) }, 'Download selected'),
        this.tab === 'ignored'
          ? h('button', { class: 'btn small', onclick: () => this.ignore([...this.selected], false) }, 'Unignore selected')
          : h('button', { class: 'btn small', onclick: () => this.ignore([...this.selected], true) }, 'Ignore selected'),
        h('button', { class: 'btn small', onclick: () => { this.selected.clear(); this.renderEntries(); } }, 'Clear selection'));
      return;
    }
    if (this.tab === 'new' && c.new) a.append(h('button', { class: 'btn small primary', onclick: () => this.downloadStatus('new') }, `Download all new (${c.new})`));
    if (this.tab === 'backlog' && c.backlog) a.append(h('button', { class: 'btn small', onclick: () => this.backlogDialog() }, 'Download backlog…'));
  }

  async loadEntries(reset) {
    if (reset) { this.offset = 0; this.entries = []; }
    const q = new URLSearchParams({ status: this.tab, offset: this.offset });
    if (this.entryQuery) q.set('q', this.entryQuery);
    let data;
    try { data = await get(`/subscriptions/${this.id}/entries?` + q); } catch (ex) { if (ex.status !== 401 && ex.status !== 404) toast(ex.message, 'err'); return; }
    this.entries = reset ? data.entries : this.entries.concat(data.entries);
    this.offset = data.nextOffset ? Number(data.nextOffset) : this.offset + data.entries.length;
    this.moreBtn.classList.toggle('hidden', !data.nextOffset);
    if (reset) {
      const ids = new Set(this.entries.map((e) => e.id));
      for (const id of [...this.selected]) if (!ids.has(id)) this.selected.delete(id);
    }
    this.renderEntries();
  }

  renderEntries() {
    const l = clear(this.entriesEl);
    if (!this.entries.length) {
      const text = { new: 'No new videos.', backlog: 'No backlog.', library: 'Nothing downloaded yet.', ignored: 'No ignored videos.', failed: 'No failed downloads.' }[this.tab];
      l.append(h('div', { class: 'empty' }, text));
    }
    for (const e of this.entries) l.append(this.entryRow(e));
    this.renderEntryActions();
  }

  entryRow(e) {
    const act = (label, fn, cls = '') => h('button', { class: 'btn small ' + cls, onclick: async (ev) => { ev.stopPropagation(); ev.target.disabled = true; try { await fn(); } catch (ex) { toast(ex.message, 'err'); } ev.target.disabled = false; } }, label);
    const meta = [date(e.uploadDate), e.duration ? duration(e.duration) : ''].filter(Boolean);
    const actions = h('div', { class: 'entry-buttons' });
    let badge = null;
    switch (e.status) {
      case 'new': case 'backlog':
        actions.append(act('Download', () => this.download([e.id]), 'primary'), act('Ignore', () => this.ignore([e.id], true)));
        break;
      case 'queued':
        badge = h('span', { class: 'badge b-' + (e.itemState === 'pending' ? 'queued' : 'downloading') }, e.itemState === 'pending' ? '⏳ queued' : 'downloading');
        if (e.jobId) actions.append(h('a', { class: 'btn small', href: '#/jobs/' + e.jobId }, 'Show job'),
          act('Cancel', async () => { if (await confirmDialog('Cancel the download job of this video? Other videos of the same job are cancelled too.', 'Cancel job', true)) await post(`/jobs/${e.jobId}/cancel`); }));
        break;
      case 'library':
        badge = h('span', { class: 'badge b-done' }, 'in library');
        if (e.jobId) actions.append(h('a', { class: 'btn small', href: '#/jobs/' + e.jobId }, 'Show in Downloads'));
        actions.append(act('Re-download', async () => { if (await confirmDialog('Download this video again and replace it in the library?', 'Re-download')) await this.download([e.id], true); }));
        break;
      case 'ignored':
        badge = h('span', { class: 'badge b-cancelled', title: e.ignoreReason || 'ignored by you' }, e.ignoreReason || 'ignored');
        actions.append(act('Download', () => this.download([e.id])), act('Unignore', () => this.ignore([e.id], false)));
        break;
      case 'upcoming':
        badge = h('span', { class: 'badge b-pending' }, '⏰ upcoming');
        break;
      case 'failed':
        badge = h('span', { class: 'badge b-failed', title: e.error || '' }, 'failed');
        actions.append(act('Retry', () => this.download([e.id]), 'primary'), act('Ignore', () => this.ignore([e.id], true)));
        break;
    }
    const cb = h('input', { type: 'checkbox', checked: this.selected.has(e.id), onclick: (ev) => ev.stopPropagation(), onchange: (ev) => { ev.target.checked ? this.selected.add(e.id) : this.selected.delete(e.id); this.renderEntryActions(); } });
    return h('div', { class: 'entry' },
      cb,
      img('entry-thumb', e.thumbnail),
      h('div', { class: 'entry-main' },
        h('a', { class: 'entry-title', href: e.url, target: '_blank', rel: 'noreferrer', title: e.title }, e.title || e.url),
        h('div', { class: 'muted small' }, meta.join(' · '), badge ? ' ' : '', badge,
          e.goneAt ? h('span', { class: 'warn-tag', title: 'No longer listed by the source; files in the library are not touched' }, 'removed from source') : null),
        e.status === 'failed' && (e.error || e.hint) ? h('div', { class: 'bad small' }, e.error, e.hint ? h('span', { class: 'muted' }, ` — ${e.hint}`) : null) : null),
      actions);
  }

  async download(ids, force = false) {
    try {
      const r = await post('/subscriptions/entries/download', { ids, force });
      if (r.queued) toast(`Queued ${r.queued} video${r.queued === 1 ? '' : 's'}`, 'ok');
      else toast('Nothing to download (already queued or in the library)');
      this.selected.clear();
      this.loadEntries(true);
    } catch (ex) { toast(ex.message, 'err'); }
  }

  async downloadStatus(status, latest = 0) {
    try {
      const r = await post('/subscriptions/entries/download', { subscription: this.id, status, latest });
      toast(r.queued ? `Queued ${r.queued} videos` : 'Nothing to download', r.queued ? 'ok' : '');
      if (status === 'new' && this.sub.warning) this.sub = await patch('/subscriptions/' + this.id, { dismissWarning: true });
      this.renderDetailHead();
      this.loadEntries(true);
    } catch (ex) { toast(ex.message, 'err'); }
  }

  async ignore(ids, ignored) {
    try {
      await post('/subscriptions/entries/ignore', { ids, ignored });
      this.selected.clear();
      this.loadEntries(true);
    } catch (ex) { toast(ex.message, 'err'); }
  }

  async backlogDialog() {
    const c = this.sub.counts || {};
    const n = h('input', { type: 'number', min: 1, max: c.backlog, value: Math.min(10, c.backlog), style: 'width:6rem' });
    const r = await modal((close) => h('div', {},
      h('h2', {}, 'Download backlog'),
      h('p', {}, `${c.backlog.toLocaleString()} videos in the backlog${c.backlogDuration ? `, ${duration(c.backlogDuration)} in total (where the duration is known)` : ''}.`),
      h('p', { class: 'muted' }, 'Backlog downloads run in the background, after new videos and everything you start by hand.'),
      h('div', { class: 'btn-row' },
        h('button', { class: 'btn', onclick: () => close() }, 'Cancel'),
        h('span', {}, 'Latest ', n), h('button', { class: 'btn', onclick: () => close({ latest: Number(n.value) || 1 }) }, 'Download latest'),
        h('button', { class: 'btn primary', onclick: () => close({ latest: 0 }) }, `Download all ${c.backlog.toLocaleString()}`))));
    if (r) this.downloadStatus('backlog', r.latest);
  }
}
