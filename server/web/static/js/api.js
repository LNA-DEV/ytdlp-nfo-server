// API client. Authentication uses an HttpOnly session cookie, so the
// password never appears in URLs (F13).

export class ApiError extends Error {
  constructor(status, message, body) { super(message); this.status = status; this.body = body; }
}

const listeners = new Set();
export function onUnauthorized(fn) { listeners.add(fn); }

export async function api(method, path, body, opts = {}) {
  const init = { method, credentials: 'same-origin', headers: { 'X-Requested-With': 'ytdlp-nfo' } };
  if (body instanceof FormData) init.body = body;
  else if (typeof body === 'string') { init.body = body; init.headers['Content-Type'] = 'text/plain'; }
  else if (body !== undefined) { init.body = JSON.stringify(body); init.headers['Content-Type'] = 'application/json'; }
  const resp = await fetch('/api/v1' + path, init);
  if (resp.status === 401 && !opts.noAuthRedirect) listeners.forEach((fn) => fn());
  const type = resp.headers.get('Content-Type') || '';
  const data = type.includes('application/json') ? await resp.json().catch(() => null) : await resp.text();
  if (!resp.ok) throw new ApiError(resp.status, (data && data.error) || resp.statusText, data);
  return data;
}

export const get = (p) => api('GET', p);
export const post = (p, b) => api('POST', p, b ?? {});
export const put = (p, b) => api('PUT', p, b);
export const del = (p) => api('DELETE', p);
export const patch = (p, b) => api('PATCH', p, b);
