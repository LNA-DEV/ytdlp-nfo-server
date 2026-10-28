import { get, post, del, api } from '../api.js';
import { h, clear, time, toast, confirmDialog, debounce } from '../util.js';

export class ArchiveView {
  constructor({ q } = {}) { this.initialQuery = q || ''; }

  render() {
    this.offset = 0;
    this.q = this.initialQuery;
    const search = h('input', { type: 'text', placeholder: 'Search key or path…', value: this.q, style: 'flex:1;min-width:0', oninput: debounce((e) => { this.q = e.target.value.trim(); this.offset = 0; this.load(); }, 300) });
    const file = h('input', { type: 'file', accept: '.txt,text/plain', class: 'hidden', onchange: async (e) => {
      const f = e.target.files[0];
      if (!f) return;
      try {
        const r = await api('POST', '/archive/import', await f.text());
        toast(`Imported ${r.imported} new entries`, 'ok');
        this.load();
      } catch (ex) { toast(ex.message, 'err'); }
      e.target.value = '';
    } });
    this.list = h('div', {});
    this.pager = h('div', { class: 'btn-row', style: 'justify-content:center;margin-top:.8rem' });
    this.checkOut = h('div', {});
    this.checkBtn = h('button', { class: 'btn primary', onclick: () => this.runCheck() }, 'Check library');
    return h('div', {}, h('h1', { style: 'font-size:1.3rem' }, 'Archive'),
      h('div', { class: 'section' }, h('h2', {}, 'Library check'),
        h('p', { class: 'muted' }, 'Compares the archive with the library folders (movie.nfo and media file). Entries whose video is missing or incomplete can be removed from the archive so the video is downloaded again — for example videos that v1 recorded but then deleted with a partly failed playlist. Library files are never changed.'),
        h('div', { class: 'btn-row' }, this.checkBtn), this.checkOut),
      h('p', { class: 'muted' }, 'Videos in the library. An entry is written only after the files were moved into the library. Remove an entry to allow downloading the video again.'),
      h('div', { class: 'section' },
        h('div', { class: 'btn-row' }, search,
          h('button', { class: 'btn', onclick: () => file.click() }, 'Import'),
          h('a', { class: 'btn', href: '/api/v1/archive/export' }, 'Export'), file),
        h('div', { style: 'margin-top:.8rem' }, this.list), this.pager),
      this.loadLater());
  }

  loadLater() { setTimeout(() => this.load(), 0); return null; }

  async runCheck() {
    this.checkBtn.disabled = true;
    this.checkBtn.textContent = 'Checking…';
    const out = clear(this.checkOut);
    try {
      this.renderCheck(await post('/archive/check'));
    } catch (ex) {
      out.append(h('p', { class: 'bad' }, ex.message));
    }
    this.checkBtn.disabled = false;
    this.checkBtn.textContent = 'Check again';
  }

  renderCheck(r) {
    const out = clear(this.checkOut);
    out.append(h('p', {},
      `${r.entries} archive entries, ${r.folders} library folders scanned in ${r.duration.toFixed(1)} s: `,
      h('span', { class: 'ok' }, `${r.ok} ok`), ', ',
      h('span', { class: r.missing ? 'bad' : '' }, `${r.missing} missing`), ', ',
      h('span', { class: r.incomplete ? 'bad' : '' }, `${r.incomplete} incomplete`)));
    if (!r.problems.length) return;
    const boxes = [];
    const rows = r.problems.map((p) => {
      const cb = h('input', { type: 'checkbox', checked: true, dataset: { key: p.key } });
      boxes.push(cb);
      return h('tr', {},
        h('td', {}, cb),
        h('td', { class: 'mono' }, p.key),
        h('td', {}, h('span', { class: 'badge b-failed' }, p.status)),
        h('td', {},
          p.path ? h('div', { class: 'mono' }, p.path) : null,
          p.stranded ? h('div', { class: 'muted' }, 'Files found outside the library: ', h('span', { class: 'mono' }, p.stranded)) : null,
          p.url ? h('div', { class: 'muted' }, p.url) : h('div', { class: 'muted' }, 'URL unknown: re-submit the playlist or video to download it again')));
    });
    const all = h('input', { type: 'checkbox', checked: true, onchange: (e) => boxes.forEach((b) => { b.checked = e.target.checked; }) });
    const selected = () => boxes.filter((b) => b.checked).map((b) => b.dataset.key);
    const fix = async (redownload) => {
      const keys = selected();
      if (!keys.length) return;
      const what = redownload ? 'remove them from the archive and download them again (where the URL is known)' : 'remove them from the archive';
      if (!(await confirmDialog(`${keys.length} entries: ${what}? Library files are not touched.`, 'Continue', !redownload))) return;
      try {
        const res = await post('/archive/fix', { keys, redownload });
        let msg = `Removed ${res.removed} entries`;
        if (redownload) msg += `, queued ${res.queued} downloads`;
        if (res.noUrl.length) msg += `; ${res.noUrl.length} without a known URL (re-submit their playlist)`;
        toast(msg, res.errors.length ? 'err' : 'ok');
        for (const e of res.errors) toast(e, 'err');
        this.runCheck();
        this.load();
      } catch (ex) { toast(ex.message, 'err'); }
    };
    out.append(
      h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, all), h('th', {}, 'Key'), h('th', {}, 'Status'), h('th', {}, 'Details')), rows),
      h('div', { class: 'btn-row', style: 'margin-top:.8rem' },
        h('button', { class: 'btn primary', onclick: () => fix(true) }, 'Remove selected and re-download'),
        h('button', { class: 'btn danger', onclick: () => fix(false) }, 'Only remove from archive')));
  }

  async load() {
    const q = new URLSearchParams({ q: this.q, limit: 100, offset: this.offset });
    let r;
    try { r = await get('/archive?' + q); } catch (ex) { clear(this.list).append(h('div', { class: 'bad' }, ex.message)); return; }
    const list = clear(this.list);
    if (!r.entries.length) { list.append(h('div', { class: 'muted' }, 'No entries.')); clear(this.pager); return; }
    list.append(h('table', { class: 'tbl' }, h('tr', {}, h('th', {}, 'Key'), h('th', {}, 'Path'), h('th', {}, 'Added'), h('th', {})),
      r.entries.map((e) => h('tr', {},
        h('td', { class: 'mono' }, e.key),
        h('td', {}, e.path || h('span', { class: 'muted' }, e.source === 'import' ? 'imported' : '')),
        h('td', {}, time(e.finalizedAt)),
        h('td', { class: 'actions' }, h('button', { class: 'btn small danger', onclick: async () => {
          if (!(await confirmDialog(`Remove ${e.key} from the archive? The files stay in the library; the video can then be downloaded again.`, 'Remove', true))) return;
          try { await del('/archive/' + encodeURIComponent(e.key)); this.load(); } catch (ex) { toast(ex.message, 'err'); }
        } }, 'Remove'))))));
    const pager = clear(this.pager);
    pager.append(h('span', { class: 'muted' }, `${this.offset + 1}–${this.offset + r.entries.length} of ${r.total}`));
    if (this.offset > 0) pager.prepend(h('button', { class: 'btn small', onclick: () => { this.offset = Math.max(0, this.offset - 100); this.load(); } }, '‹ Prev'));
    if (this.offset + r.entries.length < r.total) pager.append(h('button', { class: 'btn small', onclick: () => { this.offset += 100; this.load(); } }, 'Next ›'));
  }
}
