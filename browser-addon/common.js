// Shared by the popup and the background script.
const api = typeof browser !== "undefined" ? browser : chrome;

const DEFAULT_SERVER = "http://localhost:8080";
const INTERNAL_PREFIXES = ["about:", "chrome://", "chrome-extension://", "moz-extension://", "edge://", "brave://", "opera://", "view-source:"];

function isInternalPage(url) {
  return !url || INTERNAL_PREFIXES.some((p) => url.startsWith(p));
}

// loadSettings migrates the old "serverPassword" setting: an API token
// (recommended) or the password both go into the Bearer header.
async function loadSettings() {
  const d = await api.storage.local.get(["serverAddress", "serverPassword", "serverToken", "preset", "library"]);
  return {
    server: (d.serverAddress || DEFAULT_SERVER).replace(/\/+$/, ""),
    token: d.serverToken || d.serverPassword || "",
    preset: d.preset || "",
    library: d.library || "",
  };
}

function headers(s) {
  const h = { "Content-Type": "application/json" };
  if (s.token) h["Authorization"] = "Bearer " + s.token;
  return h;
}

// fetchOptions returns presets and libraries, or null for a v1 server.
async function fetchOptions(s) {
  try {
    const res = await fetch(s.server + "/api/v1/options", { headers: headers(s) });
    if (!res.ok) return null;
    return await res.json();
  } catch {
    return null;
  }
}

// submitUrl sends a URL. It uses /api/v1/jobs and falls back to the v1
// endpoint for servers that predate the overhaul.
// Result: {ok, kind: "created"|"duplicate"|"auth"|"error", message, jobId}
async function submitUrl(s, url) {
  const options = {};
  if (s.preset) options.preset = s.preset;
  if (s.library) options.library = s.library;
  let res;
  try {
    res = await fetch(s.server + "/api/v1/jobs", { method: "POST", headers: headers(s), body: JSON.stringify({ url, options }) });
    if (res.status === 404 || res.status === 405) {
      res = await fetch(s.server + "/api/download", { method: "POST", headers: headers(s), body: JSON.stringify({ url }) });
    }
  } catch {
    return { ok: false, kind: "error", message: "Could not reach server. Check the address in settings." };
  }
  let body = {};
  try { body = await res.json(); } catch { /* empty */ }
  if (res.status === 201) return { ok: true, kind: "created", message: "Download queued successfully!", jobId: body.id };
  if (res.status === 409) return { ok: false, kind: "duplicate", message: "Already queued (duplicate).", jobId: body.jobId };
  if (res.status === 401) return { ok: false, kind: "auth", message: "Not authorized. Check the API token in settings." };
  return { ok: false, kind: "error", message: body.error || "Server returned " + res.status };
}

function jobLink(s, id) {
  return id ? `${s.server}/#/jobs/${id}` : `${s.server}/`;
}
