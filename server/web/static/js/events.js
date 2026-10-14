// One global event stream for the whole UI (§8.2). EventSource resends
// Last-Event-ID on reconnect, so no event is lost; "resync" asks the views
// to reload because the server could not replay everything.

const handlers = new Map();
let source = null;
let connected = false;

export function on(type, fn) {
  if (!handlers.has(type)) handlers.set(type, new Set());
  handlers.get(type).add(fn);
  return () => handlers.get(type).delete(fn);
}

function emit(type, data) {
  for (const fn of handlers.get(type) || []) {
    try { fn(data); } catch (e) { console.error(e); }
  }
}

const types = ['job', 'job_removed', 'item', 'progress', 'counts', 'system', 'resync'];

export function connect() {
  if (source) return;
  source = new EventSource('/api/v1/events', { withCredentials: true });
  for (const t of types) {
    source.addEventListener(t, (e) => {
      let data = {};
      try { data = JSON.parse(e.data); } catch { /* ignore */ }
      emit(t, data);
    });
  }
  source.onopen = () => {
    if (!connected) emit('connection', true);
    connected = true;
  };
  source.onerror = () => {
    if (connected) emit('connection', false);
    connected = false;
    if (source.readyState === EventSource.CLOSED) {
      source = null;
      setTimeout(connect, 3000);
    }
  };
}

export function disconnect() {
  if (source) { source.close(); source = null; }
}
