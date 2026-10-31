import { get, post, del, patch } from '../api.js';
import * as events from '../events.js';
import { h, clear, bytes, speed, duration, time, clock, toast, modal, confirmDialog, debounce } from '../util.js';

const TABS = [
  ['active', 'Active'],
  ['queued', 'Queued'],
  ['completed', 'Completed'],
  ['failed', 'Failed'],
];
const PAGE = 50;

const CLASS_TEXT = {
  permanent: 'Not retried automatically: this error does not go away by waiting.',
};

let optionsCache = null;
async function loadOptions(force = false) {
  if (!optionsCache || force) optionsCache = await get('/options');
  return optionsCache;
}

function select(name, choices, value) {
  return h('select', { name }, choices.map(([v, label]) => h('option', { value: v, selected: v === value }, label)));
}

// optionFields builds the advanced option inputs (used by the submit form
// and by "Retry with options…"). Empty values mean "default".
function optionFields(opts, cur = {}) {
  const imp = [['', 'Default'], ['off', 'Off'], ['any', 'Any client'], ...opts.impersonateTargets.map((t) => [t, t])];
  const cookies = [['', 'Default'], ['none', 'None'], ...opts.cookieProfiles.map((c) => [c, c])];
  const vers = [['', 'Active version'], ['nightly:latest', 'Latest nightly'], ['stable:latest', 'Latest stable'], ...opts.ytdlpVersions.map((v) => [v, v])];
  const libs = opts.libraries.map((l) => [l, l]);
  return h('div', { class: 'more' },
    h('label', {}, 'Impersonate', select('impersonate', imp, cur.impersonate ?? '')),
    h('label', {}, 'Cookie profile', select('cookieProfile', cookies, cur.cookieProfile ?? '')),
    h('label', {}, 'yt-dlp version', select('ytdlpVersion', vers, cur.ytdlpVersion ?? '')),
    libs.length > 1 ? h('label', {}, 'Library', select('library', [['', 'Default'], ...libs], cur.library ?? '')) : null,
    h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'force', checked: !!cur.force }), 'Force re-download (ignore archive)'));
}

function readOptions(root) {
  const o = {};
  const val = (n) => { const el = root.querySelector(`[name=${n}]`); return el ? el.value : ''; };
  const chk = (n) => { const el = root.querySelector(`[name=${n}]`); return el ? el.checked : undefined; };
  if (val('preset')) o.preset = val('preset');
  if (val('container')) o.container = val('container');
  if (chk('allAudio') !== undefined) o.allAudio = chk('allAudio');
  if (chk('subtitles') !== undefined) o.subtitles = chk('subtitles');
  const imp = val('impersonate');
  if (imp === 'off') o.impersonate = '';
  else if (imp) o.impersonate = imp;
  const ck = val('cookieProfile');
  if (ck === 'none') o.cookieProfile = '';
  else if (ck) o.cookieProfile = ck;
  if (val('ytdlpVersion')) o.ytdlpVersion = val('ytdlpVersion');
  if (val('library')) o.library = val('library');
  if (chk('force')) o.force = true;
  return o;
}

function stateLabel(s) { return s.replace('_', ' '); }

// warningTag shows an item warning; "Looks like <path>" (a similar image,
// Reddit concept §7.5) links to the archive entry of that folder.
function warningTag(w) {
  const m = /^(?:Image \d+ looks|Looks) like (.+)$/.exec(w);
  const tag = h('span', { class: 'warn-tag', style: 'margin-right:.3rem' }, w);
  if (!m) return tag;
  const folder = m[1].includes('/') ? m[1].slice(0, m[1].lastIndexOf('/')) : m[1];
  return h('a', { href: '#/archive?q=' + encodeURIComponent(folder), title: 'Show in the archive' }, tag);
}

export class JobsView {
  constructor({ tab, focus }) {
    this.tab = tab || 'active';
    this.focus = focus;
    this.cards = new Map(); // id -> {el, job, expanded, pane, items, logSource}
    this.cursor = '';
    this.query = '';
    this.unsub = [];
  }

  destroy() {
    this.unsub.forEach((u) => u());
    for (const c of this.cards.values()) this.closeLog(c);
  }

  handleHash(hash) {
    const m = hash.match(/^#\/(active|queued|completed|failed)?$/);
    if (!m) return false;
    this.switchTab(m[1] || 'active');
    return true;
  }

  render() {
    this.root = h('div', {});
    this.root.append(this.renderSubmit());
    this.tabsEl = h('div', { class: 'tabs' });
    this.counts = {};
    for (const [id, label] of TABS) {
      const b = h('button', { class: 'tab', dataset: { tab: id }, onclick: () => { location.hash = '#/' + id; } }, label, h('span', { class: 'count' }, ''));
      this.tabsEl.append(b);
    }
    const filter = h('input', { type: 'text', class: 'filter', placeholder: 'Filter…', oninput: debounce((e) => { this.query = e.target.value.trim(); this.reload(); }, 300) });
    this.tabsEl.append(filter);
    this.root.append(this.tabsEl);
    this.actionsEl = h('div', { class: 'list-actions' });
    this.root.append(this.actionsEl);
    this.listEl = h('div', {});
    this.moreBtn = h('button', { class: 'btn load-more hidden', onclick: () => this.loadPage() }, 'Load more');
    this.root.append(this.listEl, this.moreBtn);

    this.unsub.push(events.on('job', (j) => this.onJob(j)));
    this.unsub.push(events.on('job_removed', (j) => this.removeCard(j.id)));
    this.unsub.push(events.on('item', (it) => this.onItem(it)));
    this.unsub.push(events.on('progress', (p) => this.onProgress(p)));
    this.unsub.push(events.on('counts', (c) => this.renderCounts(c)));
    this.unsub.push(events.on('resync', () => this.reload()));
    this.unsub.push(events.on('connection', (ok) => { if (ok) this.reload(); }));

    if (this.focus) {
      get('/jobs/' + this.focus).then((j) => { this.tab = j.tab; this.switchTab(j.tab, j.id); }).catch(() => this.switchTab(this.tab));
    } else {
      this.switchTab(this.tab);
    }
    return this.root;
  }

  // --- submit form ---

  renderSubmit() {
    const form = h('form', { class: 'submit' });
    const url = h('input', { type: 'url', name: 'url', placeholder: 'Paste video or playlist URL…', autocomplete: 'off', required: true });
    const btn = h('button', { class: 'btn primary', type: 'submit' }, 'Download');
    const bulk = h('button', { class: 'btn', type: 'button', onclick: () => this.bulkDialog(form) }, 'Bulk');
    const optsRow = h('div', { class: 'options-row' });
    const morePanel = h('div', { class: 'hidden' });
    const moreBtn = h('button', { class: 'link-btn', type: 'button', onclick: () => { morePanel.classList.toggle('hidden'); moreBtn.textContent = morePanel.classList.contains('hidden') ? 'More ▸' : 'More ▾'; } }, 'More ▸');
    form.append(h('div', { class: 'submit-row' }, url, btn, bulk), optsRow, morePanel);
    loadOptions().then((o) => {
      const d = o.defaults || {};
      optsRow.append(
        h('label', {}, 'Preset ', select('preset', [['', `Default (${d.preset})`], ...o.presets.map((p) => [p, p])], '')),
        h('label', {}, 'Container ', select('container', o.containers.map((c) => [c, c.toUpperCase()]), d.container)),
        h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'allAudio', checked: d.allAudio }), 'All audio'),
        h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'subtitles', checked: d.subtitles }), 'Subtitles'),
        moreBtn);
      morePanel.append(optionFields(o));
    }).catch(() => {});
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      btn.disabled = true;
      try {
        const job = await post('/jobs', { url: url.value.trim(), options: readOptions(form) });
        url.value = '';
        toast(`Queued: ${job.title || job.url}`, 'ok');
      } catch (ex) {
        toast(ex.message, 'err');
      } finally {
        btn.disabled = false;
        url.focus();
      }
    });
    return form;
  }

  bulkDialog(form) {
    modal((close) => {
      const ta = h('textarea', { rows: 12, placeholder: 'One URL per line (max 500)' });
      const count = h('div', { class: 'muted' }, '0 URLs');
      const result = h('div', {});
      const go = h('button', { class: 'btn primary' }, 'Import');
      ta.addEventListener('input', () => {
        const n = ta.value.split('\n').filter((l) => l.trim()).length;
        count.textContent = `${n} URL${n === 1 ? '' : 's'}`;
        count.className = n > 500 ? 'bad' : 'muted';
      });
      go.addEventListener('click', async () => {
        const urls = ta.value.split('\n').map((l) => l.trim()).filter(Boolean);
        if (!urls.length) return;
        if (urls.length > 500) { toast('Maximum 500 URLs', 'err'); return; }
        go.disabled = true;
        try {
          const r = await post('/jobs/bulk', { urls, options: readOptions(form) });
          clear(result).append(`${r.created} created, ${r.duplicates} duplicates, ${r.errors} errors`);
          const errs = r.results.filter((x) => x.status === 'error');
          if (errs.length) result.append(h('ul', {}, errs.slice(0, 20).map((x) => h('li', { class: 'bad' }, `${x.url}: ${x.error}`))));
          go.classList.add('hidden');
        } catch (ex) {
          toast(ex.message, 'err');
          go.disabled = false;
        }
      });
      return h('div', {}, h('h2', {}, 'Bulk import'), h('p', { class: 'muted' }, 'Uses the options selected above.'), ta, count, result,
        h('div', { class: 'btn-row' }, h('button', { class: 'btn', onclick: () => close() }, 'Close'), go));
    });
  }

  // --- list ---

  switchTab(tab, focusId) {
    this.tab = tab;
    for (const b of this.tabsEl.querySelectorAll('.tab')) b.classList.toggle('active', b.dataset.tab === tab);
    this.renderActions();
    this.reload(focusId);
  }

  renderActions() {
    const a = clear(this.actionsEl);
    if (this.tab === 'completed') {
      a.append(h('button', { class: 'btn small', onclick: () => this.clearTab('completed') }, 'Clear completed'));
    }
    if (this.tab === 'failed') {
      a.append(
        h('button', { class: 'btn small', onclick: () => this.retryAll() }, 'Retry all'),
        h('button', { class: 'btn small', onclick: () => this.clearTab('failed') }, 'Clear failed'));
    }
  }

  async clearTab(tab) {
    if (!(await confirmDialog(`Remove all ${tab} jobs from the list? Files in the library are not touched.`, 'Remove', true))) return;
    try {
      const r = await post('/jobs/clear?state=' + tab);
      toast(`Removed ${r.removed} jobs`);
      this.reload();
    } catch (ex) { toast(ex.message, 'err'); }
  }

  async retryAll() {
    let n = 0;
    for (const c of [...this.cards.values()]) {
      try { await post(`/jobs/${c.job.id}/retry`, {}); n++; } catch { /* skip */ }
    }
    toast(`Retrying ${n} jobs`);
  }

  async reload(focusId) {
    this.cursor = '';
    for (const c of this.cards.values()) this.closeLog(c);
    this.cards.clear();
    clear(this.listEl);
    await this.loadPage();
    if (focusId && this.cards.has(focusId)) {
      const c = this.cards.get(focusId);
      this.toggle(c, true);
      c.el.scrollIntoView({ block: 'center' });
    }
  }

  async loadPage() {
    const q = new URLSearchParams({ state: this.tab, limit: PAGE });
    if (this.cursor) q.set('cursor', this.cursor);
    if (this.query) q.set('q', this.query);
    let data;
    try { data = await get('/jobs?' + q); } catch (ex) { if (ex.status !== 401) toast(ex.message, 'err'); return; }
    for (const j of data.jobs) this.upsert(j, true);
    this.cursor = data.nextCursor;
    this.moreBtn.classList.toggle('hidden', !this.cursor);
    this.renderCounts(data.counts);
    this.renderEmpty();
  }

  renderEmpty() {
    const empty = this.listEl.querySelector('.empty');
    if (this.cards.size === 0 && !empty) {
      const text = { active: 'Nothing is downloading.', queued: 'The queue is empty.', completed: 'No completed downloads yet.', failed: 'No failed downloads.' }[this.tab];
      this.listEl.append(h('div', { class: 'empty' }, text));
    } else if (this.cards.size > 0 && empty) empty.remove();
  }

  renderCounts(c) {
    if (!c) return;
    for (const b of this.tabsEl.querySelectorAll('.tab')) {
      const n = c[b.dataset.tab] || 0;
      b.querySelector('.count').textContent = n || '';
    }
  }

  matches(j) {
    if (!this.query) return true;
    const q = this.query.toLowerCase();
    return (j.title || '').toLowerCase().includes(q) || j.url.toLowerCase().includes(q) || (j.uploader || '').toLowerCase().includes(q) || j.id === this.query;
  }

  onJob(j) {
    const c = this.cards.get(j.id);
    if (j.tab !== this.tab || !this.matches(j)) {
      if (c) this.removeCard(j.id);
      return;
    }
    this.upsert(j, false);
  }

  upsert(j, append) {
    let c = this.cards.get(j.id);
    if (!c) {
      c = { job: j, expanded: false, pane: 'items', items: null };
      c.el = h('div', { class: 'job' });
      this.cards.set(j.id, c);
      if (append) this.listEl.append(c.el);
      else this.place(c, true);
    } else {
      const moved = c.job.queuePos !== j.queuePos;
      c.job = j;
      if (moved) this.place(c, false);
    }
    this.renderCard(c);
    this.renderEmpty();
  }

  // place keeps queued/active jobs in queue order and puts new finished
  // jobs at the top.
  place(c, isNew) {
    const ordered = this.tab === 'queued' || this.tab === 'active';
    if (!ordered) {
      if (isNew) this.listEl.prepend(c.el);
      return;
    }
    const after = [...this.cards.values()].filter((x) => x !== c && x.el.isConnected && x.job.queuePos > c.job.queuePos)
      .sort((a, b) => a.job.queuePos - b.job.queuePos)[0];
    if (after) this.listEl.insertBefore(c.el, after.el);
    else this.listEl.insertBefore(c.el, this.listEl.querySelector('.empty'));
  }

  removeCard(id) {
    const c = this.cards.get(id);
    if (!c) return;
    this.closeLog(c);
    c.el.remove();
    this.cards.delete(id);
    this.renderEmpty();
  }

  // --- card ---

  renderCard(c) {
    const j = c.job;
    const el = clear(c.el);
    el.className = 'job ' + j.state;
    const title = j.title || j.url;
    const head = h('div', { class: 'job-head', onclick: () => this.toggle(c) },
      j.thumbnail ? h('img', { class: 'job-thumb', src: j.thumbnail, loading: 'lazy', referrerpolicy: 'no-referrer', alt: '' }) : h('div', { class: 'job-thumb' }),
      h('div', { class: 'job-main' },
        h('div', { class: 'job-title', title }, title),
        h('div', { class: 'job-sub' }, [j.uploader, j.title ? j.url : null].filter(Boolean).join(' · ')),
        this.metaLine(j),
        j.subscriptionId ? h('a', { class: 'sub-link', href: '#/subscriptions/' + j.subscriptionId, onclick: (e) => e.stopPropagation() }, 'Subscription') : null),
      h('div', { class: 'job-side' },
        h('span', { class: 'badge b-' + j.state }, stateLabel(j.state)),
        j.band && j.band !== 'interactive' && j.state === 'queued' ? h('span', { class: 'band-tag', title: 'Background download of a subscription' }, j.band) : null,
        h('span', { class: 'muted', style: 'font-size:.75rem' }, time(j.finishedAt || j.createdAt))));
    el.append(head);
    const bar = h('div', { class: 'progress' }, h('div', {}));
    c.bar = bar.firstChild;
    this.setProgress(c, j.progress);
    if (j.state === 'completed') c.bar.style.width = '100%';
    el.append(bar);

    if (j.state === 'retrying' && j.retryAt) {
      el.append(h('div', { class: 'job-retry' }, `Retrying at ${clock(j.retryAt)}${j.errorClass ? ` (${j.errorClass})` : ''}: ${j.error || ''}`));
    } else if (j.state === 'queued' && j.error) {
      el.append(h('div', { class: 'job-retry' }, j.error));
    }
    if ((j.state === 'failed' || j.state === 'partial') && (j.error || j.hint)) {
      el.append(h('div', { class: 'job-error' },
        j.errorClass ? h('span', { class: 'cls' }, j.errorClass.replace('_', ' ')) : null,
        j.error,
        h('span', { class: 'hint' }, j.hint || CLASS_TEXT[j.errorClass] || '')));
    }
    el.append(this.actions(c));
    if (c.expanded) el.append(this.body(c));
  }

  metaLine(j) {
    const parts = [];
    const n = j.counts || {};
    if (j.progress && (j.state === 'running' || j.state === 'probing')) {
      const p = j.progress;
      if (p.itemCount > 1) parts.push(`${p.itemIndex + 1}/${p.itemCount}`);
      if (p.stage && p.stage !== 'downloading') parts.push(p.stage);
      else {
        parts.push(`${p.percent.toFixed(0)}%`);
        if (p.parts > 1) parts.push(`part ${p.part}/${p.parts}`);
        if (p.speed) parts.push(speed(p.speed));
        if (p.eta) parts.push(`ETA ${duration(p.eta)}`);
      }
      if (p.itemCount > 1 && p.itemTitle) parts.push(`“${p.itemTitle}”`);
    } else if (j.state === 'probing') {
      parts.push('looking up video…');
    } else if (n.total > 0) {
      if (n.total > 1 || j.isPlaylist) parts.push(`${n.total} items`);
      if (n.done) parts.push(`${n.done} downloaded`);
      if (n.skipped) parts.push(n.skipped === n.total && n.total > 1 ? `all ${n.total} already in library` : n.skipped === 1 && n.total === 1 ? 'Already in library' : `${n.skipped} already in library`);
      if (n.failed) parts.push(`${n.failed} failed`);
    }
    if (j.state === 'completed' && j.hint && !parts.length) parts.push(j.hint);
    if (j.state === 'queued') parts.push(`#${j.queuePos} in queue`);
    if (j.ytdlpVersion && (j.state === 'completed' || j.state === 'failed' || j.state === 'partial')) parts.push('yt-dlp ' + j.ytdlpVersion.split('/').pop());
    const line = h('div', { class: 'job-meta' }, parts.map((p) => h('span', {}, p)));
    if (n.warnings) line.append(h('span', { class: 'warn-tag' }, `${n.warnings} with warnings`));
    return line;
  }

  setProgress(c, p) {
    if (!c.bar) return;
    if (!p) { c.bar.style.width = '0'; return; }
    const per = p.itemCount > 1 ? ((p.itemIndex + (p.percent || 0) / 100) / p.itemCount) * 100 : (p.percent || 0);
    c.bar.style.width = Math.min(100, per).toFixed(1) + '%';
  }

  onProgress(p) {
    const c = this.cards.get(String(p.jobId));
    if (!c) return;
    c.job.progress = p;
    const meta = c.el.querySelector('.job-meta');
    if (meta) meta.replaceWith(this.metaLine(c.job));
    this.setProgress(c, p);
  }

  actions(c) {
    const j = c.job;
    const id = j.id;
    const act = (label, fn, cls = '') => h('button', { class: 'btn small ' + cls, onclick: async (e) => { e.stopPropagation(); try { await fn(); } catch (ex) { toast(ex.message, 'err'); } } }, label);
    const row = h('div', { class: 'job-actions' });
    const terminal = ['completed', 'failed', 'partial', 'cancelled'].includes(j.state);
    if (j.state === 'queued' || j.state === 'retrying') {
      row.append(act('Start now', () => post(`/jobs/${id}/start`)));
      if (j.state === 'queued') row.append(act('Move to top', () => patch('/queue', { order: [id] })));
    }
    if (!terminal) row.append(act('Cancel', () => post(`/jobs/${id}/cancel`)));
    if (j.state === 'failed' || j.state === 'partial' || j.state === 'cancelled') {
      row.append(
        act('Retry', () => post(`/jobs/${id}/retry`, {})),
        act('Retry with options…', () => this.retryDialog(c)),
        act('Retry with latest nightly', () => post(`/jobs/${id}/retry`, { options: { ytdlpVersion: 'nightly:latest' } })));
    }
    if (j.state === 'completed') {
      row.append(act('Re-download (force)', async () => {
        if (await confirmDialog('Download all items of this job again and replace them in the library?', 'Re-download')) {
          await post(`/jobs/${id}/retry`, { failedOnly: false, options: { force: true } });
        }
      }));
    }
    row.append(act(c.expanded && c.pane === 'log' ? 'Hide log' : 'Show log', () => { c.pane = 'log'; this.toggle(c, !(c.expanded && c.pane === 'log')); }));
    row.append(act(terminal ? 'Remove from list' : 'Delete', async () => {
      const msg = terminal ? 'Remove this job from the list? Files in the library are not touched.' : 'Cancel and delete this job?';
      if (await confirmDialog(msg, 'Remove', true)) await del(`/jobs/${id}`);
    }, 'danger'));
    return row;
  }

  async retryDialog(c) {
    const o = await loadOptions(true);
    const cur = c.job.options || {};
    const v = await modal((close) => {
      const fields = h('div', {},
        h('div', { class: 'options-row' },
          h('label', {}, 'Preset ', select('preset', [['', 'Unchanged'], ...o.presets.map((p) => [p, p])], '')),
          h('label', {}, 'Container ', select('container', [['', 'Unchanged'], ...o.containers.map((x) => [x, x.toUpperCase()])], ''))),
        optionFields(o, {}));
      return h('div', {}, h('h2', {}, 'Retry with options'),
        h('p', { class: 'muted' }, `Current: impersonate=${cur.impersonate || 'off'}, cookies=${cur.cookieProfile || 'none'}, yt-dlp=${cur.ytdlpVersion || 'active'}`),
        fields,
        h('div', { class: 'btn-row' }, h('button', { class: 'btn', onclick: () => close() }, 'Cancel'),
          h('button', { class: 'btn primary', onclick: () => close(readOptions(fields)) }, 'Retry')));
    });
    if (!v) return;
    await post(`/jobs/${c.job.id}/retry`, { options: v });
  }

  // --- expanded body: items and log ---

  toggle(c, force) {
    c.expanded = force === undefined ? !c.expanded : force;
    if (!c.expanded) this.closeLog(c);
    this.renderCard(c);
  }

  body(c) {
    const wrap = h('div', { class: 'job-body' });
    const tabs = h('div', { class: 'body-tabs' },
      h('button', { class: 'btn small' + (c.pane === 'items' ? ' primary' : ''), onclick: () => { c.pane = 'items'; this.closeLog(c); this.renderCard(c); } }, 'Items'),
      h('button', { class: 'btn small' + (c.pane === 'log' ? ' primary' : ''), onclick: () => { c.pane = 'log'; this.renderCard(c); } }, 'Log'),
      c.pane === 'log' ? h('a', { class: 'btn small', href: `/api/v1/jobs/${c.job.id}/log?download=1` }, 'Download log') : null);
    wrap.append(tabs);
    if (c.pane === 'log') {
      const pre = h('pre', { class: 'log' }, 'Loading…');
      wrap.append(pre);
      this.openLog(c, pre);
    } else {
      const list = h('ul', { class: 'items' }, h('li', { class: 'muted' }, 'Loading…'));
      wrap.append(list);
      c.itemsEl = list;
      get('/jobs/' + c.job.id).then((full) => {
        c.items = new Map(full.items.map((it) => [it.id, it]));
        this.renderItems(c);
      }).catch(() => {});
    }
    return wrap;
  }

  renderItems(c) {
    if (!c.itemsEl || !c.items) return;
    const list = clear(c.itemsEl);
    if (!c.items.size) { list.append(h('li', { class: 'muted' }, c.job.state === 'failed' ? 'The URL could not be resolved (see log).' : 'No items yet.')); return; }
    const items = [...c.items.values()].sort((a, b) => a.index - b.index);
    for (const it of items) {
      const info = [];
      if (it.state === 'done') {
        if (it.path) info.push(it.path);
        if (it.size) info.push(bytes(it.size));
        if (it.duration) info.push(duration(it.duration));
      }
      if (it.state === 'pending' && it.retryAt) info.push(`retry at ${clock(it.retryAt)}`);
      const label = it.state === 'skipped' ? (it.error || 'Already in library') : stateLabel(it.state);
      list.append(h('li', {},
        h('span', { class: 'badge b-' + it.state, title: it.state }, label.length > 22 ? stateLabel(it.state) : label),
        h('span', { class: 't', title: it.title || it.url },
          it.title || it.url,
          info.length ? h('div', { class: 'p' }, info.join(' · ')) : null,
          (it.state === 'failed' || it.state === 'pending') && it.error ? h('div', { class: 'e' }, `${it.errorClass ? it.errorClass + ': ' : ''}${it.error}`, it.hint && it.state === 'failed' ? ` — ${it.hint}` : '') : null,
          (it.warnings || []).map(warningTag)),
        it.state === 'done' && (it.warnings || []).length ? h('button', { class: 'btn small', onclick: async (e) => {
          e.target.disabled = true;
          try { await post(`/jobs/${c.job.id}/items/${it.id}/extras`); toast('Extras updated', 'ok'); } catch (ex) { toast(ex.message, 'err'); }
          e.target.disabled = false;
        } }, 'Re-run extras') : null));
    }
  }

  onItem(it) {
    const c = this.cards.get(it.jobId);
    if (!c || !c.items || !c.expanded || c.pane !== 'items') return;
    c.items.set(it.id, it);
    this.renderItems(c);
  }

  openLog(c, pre) {
    this.closeLog(c);
    const es = new EventSource(`/api/v1/jobs/${c.job.id}/log/stream`, { withCredentials: true });
    c.logSource = es;
    let pending = [];
    let scheduled = false;
    const flush = () => {
      scheduled = false;
      if (!pending.length) return;
      const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
      pre.append(pending.join('\n') + '\n');
      pending = [];
      if (atBottom) pre.scrollTop = pre.scrollHeight;
    };
    es.addEventListener('snapshot', (e) => {
      pre.textContent = JSON.parse(e.data) || '(empty)\n';
      pre.scrollTop = pre.scrollHeight;
    });
    es.addEventListener('line', (e) => {
      pending.push(JSON.parse(e.data));
      if (!scheduled) { scheduled = true; requestAnimationFrame(flush); }
    });
    es.addEventListener('end', () => this.closeLog(c));
    es.onerror = () => { if (es.readyState === EventSource.CLOSED) c.logSource = null; };
  }

  closeLog(c) {
    if (c.logSource) { c.logSource.close(); c.logSource = null; }
  }
}

export { loadOptions, select };
