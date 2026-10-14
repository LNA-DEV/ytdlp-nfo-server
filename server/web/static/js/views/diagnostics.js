import { get, post } from '../api.js';
import { h, clear, bytes, time, toast } from '../util.js';

export class DiagnosticsView {
  render() {
    this.root = h('div', {}, h('h1', { style: 'font-size:1.3rem' }, 'Diagnostics'));
    this.info = h('div', {}, h('div', { class: 'muted' }, 'Loading…'));
    this.root.append(this.info, this.testBox());
    this.load();
    return this.root;
  }

  async load() {
    let s;
    try { s = await get('/system'); } catch (ex) { clear(this.info).append(h('div', { class: 'bad' }, ex.message)); return; }
    const y = s.ytdlp || {};
    const a = y.active;
    const q = s.queue || {};
    const yes = (b) => b ? h('span', { class: 'ok' }, 'yes') : h('span', { class: 'bad' }, 'no');
    clear(this.info).append(
      h('div', { class: 'section' }, h('h2', {}, 'Versions'),
        h('dl', { class: 'kv' },
          h('dt', {}, 'Server'), h('dd', {}, `${s.server.version} (commit ${s.server.commit}${s.server.date ? ', ' + s.server.date : ''})`),
          h('dt', {}, 'yt-dlp'), h('dd', {}, a ? `${a.version || a.tag} (${a.id})` : h('span', { class: 'bad' }, 'none active')),
          a ? [h('dt', {}, 'yt-dlp path'), h('dd', { class: 'mono' }, a.path)] : null,
          ...Object.entries(s.tools || {}).flatMap(([k, v]) => [h('dt', {}, k), h('dd', {}, v === 'unavailable' ? h('span', { class: 'bad' }, 'not found') : v)]))),
      h('div', { class: 'section' }, h('h2', {}, 'Impersonation'),
        h('dl', { class: 'kv' },
          h('dt', {}, 'Available'), h('dd', {}, yes(y.impersonationAvailable)),
          h('dt', {}, 'Targets'), h('dd', {}, (y.targets || []).join(', ') || '—')),
        !y.impersonationAvailable ? h('p', { class: 'muted' }, 'The active yt-dlp build has no curl_cffi targets. The official standalone builds include them.') : null),
      h('div', { class: 'section' }, h('h2', {}, 'Queue'),
        h('dl', { class: 'kv' },
          h('dt', {}, 'Running'), h('dd', {}, `${(q.running || []).length} of ${q.max}`),
          h('dt', {}, 'Queued'), h('dd', {}, String(q.queued || 0)),
          h('dt', {}, 'Waiting for retry'), h('dd', {}, String(q.waiting || 0)),
          h('dt', {}, 'Paused'), h('dd', {}, q.paused ? `yes: ${q.pauseReason}` : 'no'),
          h('dt', {}, 'Jobs'), h('dd', {}, Object.entries(q.counts || {}).map(([k, v]) => `${k} ${v}`).join(' · ')),
          ...Object.entries(q.cooldowns || {}).flatMap(([k, v]) => [h('dt', {}, 'Cooldown'), h('dd', {}, `${k} until ${time(v)}`)]))),
      h('div', { class: 'section' }, h('h2', {}, 'Storage'),
        h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Directory'), h('th', {}, 'Path'), h('th', {}, 'Free')),
          (s.disk || []).map((d) => h('tr', {}, h('td', {}, d.name), h('td', { class: 'mono' }, d.path), h('td', {}, bytes(d.free) || '?')))),
        !s.persisted ? h('p', { class: 'bad' }, 'DATA_DIR is not set: jobs and history are kept in memory only.') : null),
      (s.warnings || []).length ? h('div', { class: 'section' }, h('h2', {}, 'Warnings'), h('ul', {}, s.warnings.map((w) => h('li', {}, `${time(w.at)}: ${w.message}`)))) : null);
  }

  testBox() {
    const url = h('input', { type: 'url', placeholder: 'https://…', style: 'flex:1;min-width:0' });
    const imp = h('select', {}, h('option', { value: '' }, 'no impersonation'), h('option', { value: 'any' }, 'impersonate any'));
    const out = h('pre', { class: 'log hidden' });
    const btn = h('button', { class: 'btn primary', onclick: async () => {
      if (!url.value) return;
      btn.disabled = true;
      out.classList.remove('hidden');
      out.textContent = 'Running yt-dlp…';
      try {
        const r = await post('/system/test-url', { url: url.value.trim(), impersonate: imp.value });
        const lines = [`$ ${r.command.join(' ')}`, ...r.output, '', `exit code ${r.exitCode} after ${r.duration.toFixed(1)} s`];
        if (r.class) lines.push(`classified as ${r.class}: ${r.reason}`, `hint: ${r.hint}`);
        if (r.result) {
          lines.push('', `${r.result.playlist ? 'Playlist' : 'Video'}: ${r.result.title || ''}${r.result.uploader ? ' — ' + r.result.uploader : ''}`);
          if (r.result.playlist) lines.push(`${r.result.count} entries`, ...r.result.entries.map((e) => `  ${e.key || '?'}  ${e.title || e.url}`));
        }
        out.textContent = lines.join('\n');
      } catch (ex) { out.textContent = ex.message; toast(ex.message, 'err'); }
      btn.disabled = false;
    } }, 'Test URL');
    return h('div', { class: 'section' }, h('h2', {}, 'Test URL'),
      h('p', { class: 'muted' }, 'Runs a probe with the active yt-dlp and shows the raw output. Nothing is downloaded.'),
      h('div', { class: 'btn-row' }, url, imp, btn), out);
  }
}
