import { get, post, put, del, api } from '../api.js';
import { h, clear, time, toast, modal, confirmDialog, bytes } from '../util.js';

const SECTIONS = [
  ['general', 'Defaults'],
  ['ytdlp', 'yt-dlp'],
  ['sites', 'Sites'],
  ['cookies', 'Cookies'],
  ['naming', 'Naming'],
  ['libraries', 'Libraries'],
  ['subscriptions', 'Subscriptions'],
  ['reddit', 'Reddit'],
  ['notifications', 'Notifications'],
  ['extras', 'Extras'],
  ['security', 'Security'],
];

function field(label, input, help, locked) {
  return h('label', { class: 'field' },
    h('span', {}, label, locked ? h('span', { class: 'locked' }, ' (set by environment)') : null),
    input, help ? h('span', { class: 'help' }, help) : null);
}

function bindText(obj, key, attrs = {}) {
  return h('input', { type: attrs.type || 'text', value: obj[key] ?? '', ...attrs, oninput: (e) => { obj[key] = attrs.type === 'number' ? Number(e.target.value) : e.target.value; } });
}

function bindCheck(obj, key, label, disabled) {
  return h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: !!obj[key], disabled, onchange: (e) => { obj[key] = e.target.checked; } }), label);
}

function bindSelect(obj, key, choices, disabled) {
  return h('select', { disabled, onchange: (e) => { obj[key] = e.target.value; } },
    choices.map(([v, l]) => h('option', { value: v, selected: obj[key] === v }, l)));
}

export class SettingsView {
  constructor(section) { this.section = section || 'general'; }

  render() {
    this.root = h('div', {});
    this.nav = h('div', { class: 'subnav' }, SECTIONS.map(([id, label]) => h('a', { href: '#/settings/' + id, class: id === this.section ? 'active' : '' }, label)));
    this.body = h('div', {}, h('div', { class: 'muted' }, 'Loading…'));
    this.root.append(h('h1', { style: 'font-size:1.3rem' }, 'Settings'), this.nav, this.body);
    this.load();
    return this.root;
  }

  async load() {
    try {
      this.resp = await get('/settings');
      this.s = this.resp.settings;
      this.show();
    } catch (ex) { clear(this.body).append(h('div', { class: 'bad' }, ex.message)); }
  }

  locked(name) { return (this.s.locked || []).includes(name); }

  // usedBy returns a note listing the subscriptions whose own options use
  // a library, preset or cookie profile (shown before it is removed).
  usedBy(kind, name) {
    const subs = ((this.resp.usedBySubscriptions || {})[kind] || {})[name];
    return subs && subs.length ? h('div', { class: 'warn-text small' }, `Used by subscriptions: ${subs.join(', ')}`) : null;
  }

  async confirmRemove(kind, name, what) {
    const subs = ((this.resp.usedBySubscriptions || {})[kind] || {})[name];
    if (!subs || !subs.length) return true;
    return confirmDialog(`${what} is used by these subscriptions: ${subs.join(', ')}. Their checks fail until it is restored or their options are changed. Remove it anyway?`, 'Remove', true);
  }

  async save(msg = 'Saved') {
    try {
      this.resp = await put('/settings', this.s);
      this.s = this.resp.settings;
      toast(msg, 'ok');
      this.show();
    } catch (ex) { toast(ex.message, 'err'); }
  }

  saveBtn() { return h('div', { class: 'btn-row', style: 'margin-top:1rem' }, h('button', { class: 'btn primary', onclick: () => this.save() }, 'Save')); }

  show() {
    const b = clear(this.body);
    const fn = {
      general: this.general, ytdlp: this.ytdlp, sites: this.sites, cookies: this.cookies, naming: this.naming,
      libraries: this.libraries, subscriptions: this.subscriptions, reddit: this.reddit, notifications: this.notifications, extras: this.extras, security: this.security,
    }[this.section] || this.general;
    b.append(fn.call(this));
  }

  // --- defaults ---
  general() {
    const s = this.s;
    const d = s.defaults;
    const presets = Object.keys(s.presets).map((p) => [p, p]);
    const libs = [['default', 'default'], ...(s.libraries || []).map((l) => [l.name, l.name])];
    return h('div', {},
      h('div', { class: 'section' }, h('h2', {}, 'Queue and retries'),
        h('div', { class: 'grid' },
          field('Concurrent downloads', bindText(s, 'maxConcurrent', { type: 'number', min: 1, max: 50, disabled: this.locked('maxConcurrent') }), 'Jobs running in parallel', this.locked('maxConcurrent')),
          field('Retries for transient errors', bindText(s, 'maxRetries', { type: 'number', min: 0, max: 20, disabled: this.locked('maxRetries') }), 'Backoff 30 s × 2ⁿ, at most 15 min; an item gives up after 2 h', this.locked('maxRetries')),
          field('Rate-limit cooldown (minutes)', bindText(s, 'rateLimitCooldownMinutes', { type: 'number', min: 1 }), 'Pause per site after HTTP 429'),
          field('Minimum free space (MB)', bindText(s, 'minFreeSpaceMB', { type: 'number', min: 0 }), 'The queue pauses below this'),
          field('Auto-prune completed jobs after (days)', bindText(s, 'autoPruneDays', { type: 'number', min: 0 }), '0 keeps history forever. Files are never touched.')),
        h('h3', {}, 'Automatic escalation'),
        h('div', { class: 'btn-row' },
          bindCheck(s, 'impersonationEscalation', 'Retry once with impersonation when a site blocks access'),
          bindCheck(s.ytdlp, 'nightlyRetryOnBroken', 'Retry once with the latest nightly when site support is broken'))),
      h('div', { class: 'section' }, h('h2', {}, 'Defaults for new downloads'),
        h('div', { class: 'grid' },
          field('Quality preset', bindSelect(d, 'preset', presets)),
          field('Container', bindSelect(d, 'container', [['mkv', 'MKV'], ['mp4', 'MP4']])),
          field('Impersonate', bindText(d, 'impersonate', { placeholder: 'off' }), 'Empty = off, "any", or a target like chrome'),
          field('Library', bindSelect(d, 'library', libs))),
        h('div', { class: 'btn-row', style: 'margin-top:.8rem' }, bindCheck(d, 'allAudio', 'All audio tracks'), bindCheck(d, 'subtitles', 'Subtitles'))),
      this.saveBtn());
  }

  // --- yt-dlp ---
  ytdlp() {
    const wrap = h('div', {});
    const s = this.s;
    const policies = [['off', 'Off'], ['notify', 'Notify'], ['auto', 'Auto']];
    const channelSel = bindSelect(s.ytdlp, 'channel', this.resp.channels.map((c) => [c, c]), this.locked('ytdlp.channel'));
    wrap.append(h('div', { class: 'section' }, h('h2', {}, 'Channel and updates'),
      h('div', { class: 'grid' },
        field('Channel', channelSel, 'nightly is recommended by yt-dlp; master may regress', this.locked('ytdlp.channel')),
        ...this.resp.channels.map((c) => field(`Auto-update (${c})`, bindSelect(s.ytdlp.autoUpdate, c, policies), c === s.ytdlp.channel ? 'Auto installs, smoke-tests and activates' : 'Only the active channel is auto-activated')),
        field('Smoke-test URL (optional)', bindText(s.ytdlp, 'smokeTestURL', { type: 'url', placeholder: 'https://…' }), 'Simulated download after each install')),
      h('div', { class: 'btn-row', style: 'margin-top:1rem' },
        h('button', { class: 'btn primary', onclick: () => this.save() }, 'Save'),
        h('button', { class: 'btn', onclick: async (e) => { e.target.disabled = true; try { await post('/ytdlp/check'); toast('Update check done', 'ok'); this.show(); } catch (ex) { toast(ex.message, 'err'); } e.target.disabled = false; } }, 'Check for updates now'))));
    const installed = h('div', { class: 'section' }, h('h2', {}, 'Installed versions'), h('div', { class: 'muted' }, 'Loading…'));
    const available = h('div', { class: 'section' }, h('h2', {}, 'Available versions'));
    wrap.append(installed, available);
    this.renderInstalled(installed);
    this.renderAvailable(available, s.ytdlp.channel);
    return wrap;
  }

  async renderInstalled(box) {
    let data;
    try { data = await get('/ytdlp/versions'); } catch (ex) { box.append(h('div', { class: 'bad' }, ex.message)); return; }
    const sec = clear(box);
    sec.append(h('h2', {}, 'Installed versions'));
    if (!data.managed) sec.append(h('p', { class: 'muted' }, 'YTDLP_PATH is set: the server uses that binary and does not manage versions.'));
    const rows = data.versions.map((v) => {
      const active = v.id === data.active;
      const act = (label, fn, cls = '') => h('button', { class: 'btn small ' + cls, onclick: async (e) => { e.target.disabled = true; try { await fn(); this.show(); } catch (ex) { toast(ex.message, 'err'); e.target.disabled = false; } } }, label);
      return h('tr', {},
        h('td', {}, h('div', {}, v.tag, active ? h('strong', { class: 'ok' }, ' (active)') : null), h('div', { class: 'muted' }, v.channel + (v.bundled ? ' · bundled' : '') + (v.pinned ? ' · pinned' : ''))),
        h('td', {}, v.smokeOk ? h('span', { class: 'ok' }, 'ok') : h('span', { class: 'bad', title: v.smokeOutput }, 'failed'),
          h('div', { class: 'muted', title: (v.impersonateTargets || []).join(', ') }, `${(v.impersonateTargets || []).length} impersonation targets`)),
        h('td', { class: 'actions' },
          !active && v.smokeOk ? act('Activate', () => post(`/ytdlp/versions/${v.id}/activate`)) : null,
          act(v.pinned ? 'Unpin' : 'Pin', () => post(`/ytdlp/versions/${v.id}/pin`, { pinned: !v.pinned })),
          act('Test', () => post(`/ytdlp/versions/${v.id}/test`)),
          !active && !v.bundled && v.channel !== 'system' ? act('Delete', () => del(`/ytdlp/versions/${v.id}`), 'danger') : null));
    });
    sec.append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Version'), h('th', {}, 'Smoke test'), h('th', {})), rows));
    if (data.updates && Object.keys(data.updates).length) {
      sec.append(h('p', {}, 'Updates available: ', Object.entries(data.updates).map(([c, t]) => `${c} ${t}`).join(', ')));
    }
  }

  async renderAvailable(box, channel) {
    const sel = h('select', { onchange: (e) => this.renderAvailable(box, e.target.value) }, this.resp.channels.map((c) => h('option', { value: c, selected: c === channel }, c)));
    const list = h('div', { class: 'muted' }, 'Loading…');
    clear(box).append(h('h2', {}, 'Available versions'), h('div', { class: 'btn-row' }, 'Channel ', sel), list);
    let rels;
    try { rels = await get(`/ytdlp/channels/${channel}/releases`); } catch (ex) { clear(list).append(h('span', { class: 'bad' }, ex.message)); return; }
    const rows = rels.slice(0, 15).map((r) => h('tr', {},
      h('td', {}, r.tag, r.active ? h('strong', { class: 'ok' }, ' (active)') : null),
      h('td', { class: 'muted' }, r.publishedAt ? time(r.publishedAt) : ''),
      h('td', { class: 'actions' }, r.installed
        ? (r.active ? null : h('button', { class: 'btn small', onclick: async () => { try { await post(`/ytdlp/versions/${channel}/${r.tag}/activate`); toast('Activated', 'ok'); this.show(); } catch (ex) { toast(ex.message, 'err'); } } }, 'Activate'))
        : h('button', { class: 'btn small primary', onclick: async (e) => {
          e.target.disabled = true; e.target.textContent = 'Installing…';
          try { const v = await post('/ytdlp/versions', { channel, tag: r.tag }); toast(v.smokeOk ? `Installed ${r.tag}` : `Installed ${r.tag}, but the smoke test failed`, v.smokeOk ? 'ok' : 'err'); this.show(); } catch (ex) { toast(ex.message, 'err'); e.target.disabled = false; e.target.textContent = 'Install'; }
        } }, 'Install'))));
    clear(list).className = '';
    list.append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Tag'), h('th', {}, 'Published'), h('th', {})), rows));
  }

  // --- sites ---
  sites() {
    const box = h('div', { class: 'section' }, h('h2', {}, 'Site rules'), h('p', { class: 'muted' }, 'Matched on the URL host in order. Explicit job options win over site rules, which win over the defaults.'));
    const table = h('div', {});
    box.append(table, h('div', { class: 'btn-row', style: 'margin-top:1rem' }, h('button', { class: 'btn primary', onclick: () => this.editRule({}) }, 'Add rule')));
    get('/sites').then((rules) => {
      if (!rules.length) { table.append(h('div', { class: 'muted' }, 'No site rules.')); return; }
      table.append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Pattern'), h('th', {}, 'Settings'), h('th', {})),
        rules.map((r) => h('tr', {}, h('td', { class: 'mono' }, r.pattern),
          h('td', {}, [r.impersonate && `impersonate ${r.impersonate}`, r.cookieProfile && `cookies ${r.cookieProfile}`, r.preset && `preset ${r.preset}`,
            r.maxConcurrent && `max ${r.maxConcurrent} jobs`, r.limitRate && `limit ${r.limitRate}/s`, r.sleepRequests && `sleep ${r.sleepRequests}s/request`,
            r.sleepInterval && `sleep ${r.sleepInterval}s/download`, r.library && `library ${r.library}`].filter(Boolean).join(', ')),
          h('td', { class: 'actions' }, h('button', { class: 'btn small', onclick: () => this.editRule(r) }, 'Edit'),
            h('button', { class: 'btn small danger', onclick: async () => { await del('/sites/' + r.id); this.show(); } }, 'Delete'))))));
    });
    return box;
  }

  async editRule(r) {
    const rule = { pattern: '', impersonate: '', cookieProfile: '', preset: '', maxConcurrent: 0, limitRate: '', sleepRequests: 0, sleepInterval: 0, library: '', ...r };
    const opts = await get('/options');
    const ok = await modal((close) => h('div', {}, h('h2', {}, r.id ? 'Edit site rule' : 'New site rule'),
      h('div', { class: 'grid' },
        field('Host pattern', bindText(rule, 'pattern', { placeholder: '*.example.com' })),
        field('Impersonate', bindSelect(rule, 'impersonate', [['', 'Default'], ['any', 'Any client'], ...opts.impersonateTargets.map((t) => [t, t])])),
        field('Cookie profile', bindSelect(rule, 'cookieProfile', [['', 'None'], ...opts.cookieProfiles.map((c) => [c, c])])),
        field('Preset', bindSelect(rule, 'preset', [['', 'Default'], ...opts.presets.map((p) => [p, p])])),
        field('Max concurrent jobs', bindText(rule, 'maxConcurrent', { type: 'number', min: 0 }), '0 = no site limit'),
        field('Rate limit', bindText(rule, 'limitRate', { placeholder: 'e.g. 5M' })),
        field('Sleep between requests (s)', bindText(rule, 'sleepRequests', { type: 'number', min: 0, step: '0.1' })),
        field('Sleep before downloads (s)', bindText(rule, 'sleepInterval', { type: 'number', min: 0, step: '0.1' })),
        opts.libraries.length > 1 ? field('Library', bindSelect(rule, 'library', [['', 'Default'], ...opts.libraries.map((l) => [l, l])])) : null),
      h('div', { class: 'btn-row' }, h('button', { class: 'btn', onclick: () => close(false) }, 'Cancel'), h('button', { class: 'btn primary', onclick: () => close(true) }, 'Save'))));
    if (!ok) return;
    try {
      if (r.id) await put('/sites/' + r.id, rule);
      else await post('/sites', rule);
      this.show();
    } catch (ex) { toast(ex.message, 'err'); }
  }

  // --- cookies ---
  cookies() {
    const box = h('div', { class: 'section' }, h('h2', {}, 'Cookie profiles'),
      h('p', { class: 'muted' }, 'Upload a Netscape cookies.txt (e.g. exported with a browser extension). Files are stored with 0600 permissions and never shown again. Each job uses its own copy.'));
    const table = h('div', {});
    const name = h('input', { type: 'text', placeholder: 'Profile name' });
    const file = h('input', { type: 'file', accept: '.txt,text/plain' });
    box.append(table, h('h3', {}, 'Upload'), h('div', { class: 'btn-row' }, name, file,
      h('button', { class: 'btn primary', onclick: async () => {
        if (!name.value || !file.files[0]) { toast('Name and file are required', 'err'); return; }
        const fd = new FormData();
        fd.append('name', name.value);
        fd.append('file', file.files[0]);
        try { await api('POST', '/cookies', fd); toast('Cookie profile saved', 'ok'); this.show(); } catch (ex) { toast(ex.message, 'err'); }
      } }, 'Upload')));
    get('/cookies').then((list) => {
      if (!list.length) { table.append(h('div', { class: 'muted' }, 'No cookie profiles.')); return; }
      table.append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Size'), h('th', {}, 'Uploaded'), h('th', {})),
        list.map((c) => h('tr', {}, h('td', {}, c.name), h('td', {}, bytes(c.size)), h('td', {}, time(c.createdAt)),
          h('td', { class: 'actions' }, this.usedBy('cookieProfile', c.name), h('button', { class: 'btn small danger', onclick: async () => {
            if (!(await this.confirmRemove('cookieProfile', c.name, `Cookie profile ${c.name}`))) return;
            if (await confirmDialog(`Delete cookie profile ${c.name}?`, 'Delete', true)) { await del('/cookies/' + encodeURIComponent(c.name)); this.show(); }
          } }, 'Delete'))))));
    });
    return box;
  }

  // --- naming ---
  naming() {
    const n = this.s.naming;
    const tpl = bindText(n, 'template', { placeholder: '{playlist_title?}/{title} [{id}]/{title} [{id}].{ext}', style: 'width:100%' });
    const sel = bindSelect(n, 'preset', [['default', 'Title [id] (collision-free)'], ['legacy', 'Legacy (Title)'], ['custom', 'Custom template']]);
    const custom = h('div', { class: n.preset === 'custom' ? '' : 'hidden' }, field('Template', tpl,
      'Fields: {title} {id} {uploader} {channel} {upload_date} {upload_year} {playlist_title} {extractor} {ext}. Append ? for optional fields; the last part must end with .{ext}.'));
    sel.addEventListener('change', () => custom.classList.toggle('hidden', n.preset !== 'custom'));
    return h('div', { class: 'section' }, h('h2', {}, 'Library layout'),
      h('p', { class: 'muted' }, 'Applies to new downloads only. Existing library folders are never renamed. The NFO title is what Jellyfin displays.'),
      h('dl', { class: 'kv' },
        h('dt', {}, 'Title [id]'), h('dd', { class: 'mono' }, '{playlist_title?}/{title} [{id}]/{title} [{id}].{ext}'),
        h('dt', {}, 'Legacy'), h('dd', { class: 'mono' }, '{playlist_title?}/{title}/{title}.{ext} — a different video with the same title gets " (2)"')),
      h('div', { class: 'grid', style: 'margin-top:1rem' }, field('Preset', sel)), custom, this.saveBtn());
  }

  // --- libraries ---
  libraries() {
    const s = this.s;
    s.libraries = s.libraries || [];
    const list = h('div', {});
    const draw = () => {
      clear(list).append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Path'), h('th', {})),
        h('tr', {}, h('td', {}, 'default'), h('td', { class: 'mono' }, this.resp.defaultLibraryPath), h('td', { class: 'muted' }, 'OUTPUT_DIR')),
        s.libraries.map((l, i) => h('tr', {}, h('td', {}, bindText(l, 'name'), this.usedBy('library', l.name)), h('td', {}, bindText(l, 'path', { style: 'width:100%' })),
          h('td', { class: 'actions' }, h('button', { class: 'btn small danger', onclick: async () => { if (await this.confirmRemove('library', l.name, `Library ${l.name}`)) { s.libraries.splice(i, 1); draw(); } } }, 'Remove'))))));
    };
    draw();
    return h('div', { class: 'section' }, h('h2', {}, 'Libraries'),
      h('p', { class: 'muted' }, 'Named output targets, selectable per job and per site rule. Paths must be absolute and writable (mount them into the container). Libraries from the LIBRARIES environment variable are always present.'),
      list, h('div', { class: 'btn-row', style: 'margin-top:.8rem' }, h('button', { class: 'btn', onclick: () => { s.libraries.push({ name: '', path: '' }); draw(); } }, 'Add library')),
      this.saveBtn());
  }

  // --- subscriptions (subscriptions concept §10) ---
  subscriptions() {
    const s = this.s.subscriptions;
    const num = (key, min, max) => bindText(s, key, { type: 'number', min, max });
    return h('div', {},
      h('div', { class: 'section' }, h('h2', {}, 'Checks'),
        h('div', { class: 'btn-row', style: 'margin-bottom:.8rem' }, bindCheck(s, 'paused', 'Pause all checks (subscriptions keep their own setting)')),
        h('div', { class: 'grid' },
          field('Default check interval (minutes)', num('intervalMinutes', 15, 10080), '15 minutes to 7 days, ±10 % jitter. Exact times: POST /api/v1/subscriptions/check from a cron job.'),
          field('Parallel checks', num('checkConcurrency', 1, 10), 'Never more than one per site'),
          field('Pause between checks of a site (s)', num('siteDelaySeconds', 0, 300), '±20 % jitter'),
          field('Window (newest videos per check)', num('window', 10, 500), 'Channel tabs are listed newest first; the window doubles when it holds no known video'),
          field('Full listing every (days)', num('fullRescanDays', 1, 90), 'Finds removed videos and gaps'))),
      h('div', { class: 'section' }, h('h2', {}, 'Automatic downloads'),
        h('div', { class: 'grid' },
          field('Safety cap per check', num('autoQueueCap', 1, 1000), 'More new videos than this are not downloaded automatically but listed for review'),
          field('Background slots', num('backgroundSlots', 1, 50), 'Downloads of new videos and backlogs together; they give way to downloads started by hand'),
          field('Retry "not available yet" for (days)', num('upcomingDays', 1, 30), 'Premieres without a live status are tried again by every check'),
          field('Forget removed videos after (days)', num('pruneGoneDays', 0, 3650), '0 = never. Only videos that were never downloaded'))),
      this.saveBtn());
  }

  // --- Reddit (Reddit concept §9) ---
  reddit() {
    const r = this.s.reddit;
    const chains = [['native,arcticshift', 'Native, then Arctic Shift'], ['native', 'Native only'],
      ['arcticshift,native', 'Arctic Shift, then native'], ['arcticshift', 'Arctic Shift only']];
    const chain = { value: (r.providers || []).join(',') };
    const sel = bindSelect(chain, 'value', chains);
    sel.addEventListener('change', () => { r.providers = chain.value.split(','); });
    const num = (key, min, max) => bindText(r, key, { type: 'number', min, max });
    const libs = [['', 'None'], ['default', 'default'], ...(this.s.libraries || []).map((l) => [l.name, l.name])];
    return h('div', {},
      h('div', { class: 'section' }, h('h2', {}, 'Providers'),
        h('p', { class: 'muted' }, 'Native reads the pages Reddit shows a logged-out browser. Arctic Shift is a third-party archive without guarantees; it sees the subreddits and posts the server asks for. A provider that is blocked or rate-limited hands over to the next one.'),
        h('div', { class: 'grid' },
          field('Order', sel, 'Subscriptions can override it'),
          field('Proxy', bindText(r, 'proxy', { placeholder: 'socks5://host:1080' }), 'Reddit, Arctic Shift and media downloads go through it (http://, https:// or socks5://)'),
          field('Pause between native pages (s)', num('pageDelaySeconds', 1, 60), '±20 % jitter'))),
      h('div', { class: 'section' }, h('h2', {}, 'Listings'),
        h('div', { class: 'grid' },
          field('First listing (posts)', num('firstListing', 25, 1000), 'Depth of the first check and of a full rescan; native lists at most 1000'),
          field('Posts of a pasted subreddit URL', num('jobListLimit', 1, 1000)),
          field('Library for new Reddit subscriptions', bindSelect(r, 'defaultLibrary', libs), 'Use a Jellyfin library of the type "Home videos and photos": movie libraries don\'t show images')),
        h('div', { class: 'btn-row', style: 'margin-top:.8rem' }, bindCheck(r, 'gapFill', 'Fill gaps from Arctic Shift when more posts appeared than native can list')),
        h('div', { class: 'grid' }, field('Posts per gap fill', num('backfillMax', 100, 50000)))),
      this.saveBtn());
  }

  // --- notifications ---
  notifications() {
    const n = this.s.notifications;
    const j = this.s.jellyfin;
    j.pathMap = j.pathMap || [];
    const maps = h('div', {});
    const drawMaps = () => {
      clear(maps).append(...j.pathMap.map((m, i) => h('div', { class: 'btn-row', style: 'margin-bottom:.4rem' },
        bindText(m, 'from', { placeholder: '/media (server path)' }), '→', bindText(m, 'to', { placeholder: '/data/media (Jellyfin path)' }),
        h('button', { class: 'btn small danger', onclick: () => { j.pathMap.splice(i, 1); drawMaps(); } }, 'Remove'))));
    };
    drawMaps();
    return h('div', {},
      h('div', { class: 'section' }, h('h2', {}, 'Notifications'),
        h('div', { class: 'grid' },
          field('Webhook URL (JSON POST)', bindText(n, 'webhookURL', { type: 'url' })),
          field('ntfy topic URL', bindText(n, 'ntfyURL', { type: 'url', placeholder: 'https://ntfy.sh/my-topic' })),
          field('Gotify server URL', bindText(n, 'gotifyURL', { type: 'url' })),
          field('Gotify app token', bindText(n, 'gotifyToken', { type: 'password' })),
          field('Discord webhook URL', bindText(n, 'discordWebhookURL', { type: 'url' }))),
        h('div', { class: 'btn-row', style: 'margin-top:.8rem' }, bindCheck(n, 'onCompleted', 'On completed'), bindCheck(n, 'onFailed', 'On failed / partial')),
        h('p', { class: 'muted' }, 'Playlists send one summary message.')),
      h('div', { class: 'section' }, h('h2', {}, 'Jellyfin'),
        h('p', { class: 'muted' }, 'After an item is finalized the server calls /Library/Media/Updated so it appears within seconds.'),
        h('div', { class: 'grid' },
          field('Jellyfin URL', bindText(j, 'url', { type: 'url', disabled: this.locked('jellyfin.url') }), null, this.locked('jellyfin.url')),
          field('API key', bindText(j, 'apiKey', { type: 'password', disabled: this.locked('jellyfin.apiKey') }), null, this.locked('jellyfin.apiKey'))),
        h('h3', {}, 'Path mapping'), maps,
        h('button', { class: 'btn small', onclick: () => { j.pathMap.push({ from: '', to: '' }); drawMaps(); } }, 'Add mapping')),
      this.saveBtn());
  }

  // --- extras ---
  extras() {
    const e = this.s.extras;
    const presets = h('div', {});
    const drawPresets = () => {
      clear(presets).append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Preset'), h('th', {}, 'yt-dlp sort (-S)'), h('th', {})),
        Object.keys(this.s.presets).sort().map((k) => h('tr', {}, h('td', {}, k, this.usedBy('preset', k)), h('td', {}, bindText(this.s.presets, k, { placeholder: '(no override)', style: 'width:100%' })),
          h('td', { class: 'actions' }, ['best', 'compatible', 'max1080', 'max720'].includes(k) ? null : h('button', { class: 'btn small danger', onclick: async () => { if (await this.confirmRemove('preset', k, `Preset ${k}`)) { delete this.s.presets[k]; drawPresets(); } } }, 'Remove'))))));
    };
    drawPresets();
    const newName = h('input', { type: 'text', placeholder: 'new preset name' });
    const rules = h('div', {});
    const drawRules = () => {
      clear(rules).append(...this.s.ageRules.map((r, i) => h('div', { class: 'btn-row', style: 'margin-bottom:.4rem' },
        bindText(r, 'match', { placeholder: 'extractor contains…' }), '→', bindText(r, 'rating', { placeholder: 'rating' }),
        h('button', { class: 'btn small danger', onclick: () => { this.s.ageRules.splice(i, 1); drawRules(); } }, 'Remove'))));
    };
    drawRules();
    return h('div', {},
      h('div', { class: 'section' }, h('h2', {}, 'Output extras'),
        h('p', { class: 'muted' }, 'All off by default so the output stays unchanged.'),
        h('div', { style: 'display:flex;flex-direction:column;gap:.5rem' },
          bindCheck(e, 'embedChapters', 'Embed chapters'),
          bindCheck(e, 'sponsorBlockMark', 'Mark SponsorBlock segments as chapters'),
          bindCheck(e, 'convertSubsSRT', 'Convert subtitles to SRT'),
          bindCheck(e, 'embedSubs', 'Embed subtitles into the media file (external files are kept)'))),
      h('div', { class: 'section' }, h('h2', {}, 'Quality presets'), presets,
        h('div', { class: 'btn-row', style: 'margin-top:.6rem' }, newName, h('button', { class: 'btn small', onclick: () => { if (newName.value) { this.s.presets[newName.value] = ''; newName.value = ''; drawPresets(); } } }, 'Add preset'))),
      h('div', { class: 'section' }, h('h2', {}, 'NFO rating rules'), h('p', { class: 'muted' }, 'If the extractor name contains the text, <mpaa> gets the rating; otherwise the age limit is used.'), rules,
        h('button', { class: 'btn small', onclick: () => { this.s.ageRules.push({ match: '', rating: '' }); drawRules(); } }, 'Add rule')),
      this.saveBtn());
  }

  // --- security ---
  security() {
    const wrap = h('div', {});
    const cur = h('input', { type: 'password', placeholder: 'Current password', autocomplete: 'current-password' });
    const nw = h('input', { type: 'password', placeholder: 'New password (min. 8 characters, empty removes it)', autocomplete: 'new-password', style: 'min-width:280px' });
    const pwBox = h('div', { class: 'section' }, h('h2', {}, 'Password'));
    if (this.resp.passwordFromEnv) pwBox.append(h('p', { class: 'muted' }, 'The password is set by the PASSWORD environment variable.'));
    else {
      pwBox.append(h('p', { class: 'muted' }, this.resp.passwordSet ? 'A password protects this server.' : 'No password is set: everyone who can reach the server can use it.'),
        h('div', { class: 'btn-row' }, this.resp.passwordSet ? cur : null, nw,
          h('button', { class: 'btn primary', onclick: async () => {
            try { await put('/settings/password', { current: cur.value, new: nw.value }); toast('Password changed. Please log in again.', 'ok'); setTimeout(() => location.reload(), 1200); } catch (ex) { toast(ex.message, 'err'); }
          } }, 'Change')));
    }
    const tokens = h('div', {});
    const tname = h('input', { type: 'text', placeholder: 'Token name, e.g. Browser add-on' });
    const tokBox = h('div', { class: 'section' }, h('h2', {}, 'API tokens'),
      h('p', { class: 'muted' }, 'For the browser add-on and scripts: send "Authorization: Bearer <token>". Tokens can be revoked at any time. The old "Bearer <password>" still works but is deprecated.'),
      tokens, h('div', { class: 'btn-row', style: 'margin-top:.8rem' }, tname, h('button', { class: 'btn primary', onclick: async () => {
        try {
          const r = await post('/tokens', { name: tname.value });
          tname.value = '';
          await modal((close) => h('div', {}, h('h2', {}, 'New API token'), h('p', {}, 'Copy it now, it is not shown again:'),
            h('input', { type: 'text', value: r.token, readonly: true, style: 'width:100%', onfocus: (e) => e.target.select() }),
            h('div', { class: 'btn-row' }, h('button', { class: 'btn primary', onclick: () => close() }, 'Done'))));
          this.show();
        } catch (ex) { toast(ex.message, 'err'); }
      } }, 'Create token')));
    get('/tokens').then((list) => {
      if (!list.length) { tokens.append(h('div', { class: 'muted' }, 'No API tokens.')); return; }
      tokens.append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Token'), h('th', {}, 'Created'), h('th', {}, 'Last used'), h('th', {})),
        list.map((t) => h('tr', {}, h('td', {}, t.name), h('td', { class: 'mono' }, t.prefix + '…'), h('td', {}, time(t.createdAt)), h('td', {}, t.lastUsedAt ? time(t.lastUsedAt) : 'never'),
          h('td', { class: 'actions' }, h('button', { class: 'btn small danger', onclick: async () => { if (await confirmDialog(`Revoke token ${t.name}?`, 'Revoke', true)) { await del('/tokens/' + t.id); this.show(); } } }, 'Revoke'))))));
    });
    wrap.append(pwBox, tokBox);
    return wrap;
  }
}
