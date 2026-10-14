// Small DOM and formatting helpers.

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) { while (el.firstChild) el.firstChild.remove(); return el; }

export function bytes(n) {
  if (!n || n < 0) return '';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

export function speed(n) { return n ? bytes(n) + '/s' : ''; }

export function duration(sec) {
  if (!sec || sec < 0) return '';
  sec = Math.round(sec);
  const hh = Math.floor(sec / 3600), mm = Math.floor((sec % 3600) / 60), ss = sec % 60;
  const p = (x) => String(x).padStart(2, '0');
  return hh ? `${hh}:${p(mm)}:${p(ss)}` : `${mm}:${p(ss)}`;
}

export function time(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  const today = new Date();
  if (d.toDateString() === today.toDateString()) return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  return d.toLocaleString([], { dateStyle: 'short', timeStyle: 'short' });
}

export function clock(iso) {
  return iso ? new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';
}

export function toast(msg, kind = '') {
  const t = h('div', { class: 'toast ' + kind }, msg);
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), kind === 'err' ? 8000 : 4000);
}

// modal shows content and resolves with the value passed to close().
export function modal(build) {
  const overlay = document.getElementById('modal');
  const body = clear(document.getElementById('modal-body'));
  return new Promise((resolve) => {
    const close = (v) => {
      overlay.classList.add('hidden');
      overlay.onclick = null;
      document.removeEventListener('keydown', onKey);
      resolve(v);
    };
    const onKey = (e) => { if (e.key === 'Escape') close(undefined); };
    overlay.onclick = (e) => { if (e.target === overlay) close(undefined); };
    document.addEventListener('keydown', onKey);
    body.append(build(close));
    overlay.classList.remove('hidden');
    const f = body.querySelector('input, textarea, select, button.primary');
    if (f) f.focus();
  });
}

export function confirmDialog(message, okLabel = 'OK', danger = false) {
  return modal((close) => h('div', {},
    h('p', {}, message),
    h('div', { class: 'btn-row' },
      h('button', { class: 'btn', onclick: () => close(false) }, 'Cancel'),
      h('button', { class: 'btn ' + (danger ? 'danger' : 'primary'), onclick: () => close(true) }, okLabel))));
}

export function debounce(fn, ms) {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}
