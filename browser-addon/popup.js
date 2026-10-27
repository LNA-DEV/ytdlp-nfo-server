const settingsBtn = document.getElementById("settings-btn");
const settingsPanel = document.getElementById("settings-panel");
const serverUrlInput = document.getElementById("server-url");
const serverTokenInput = document.getElementById("server-token");
const saveBtn = document.getElementById("save-btn");
const tabUrlEl = document.getElementById("tab-url");
const submitBtn = document.getElementById("submit-btn");
const subscribeBtn = document.getElementById("subscribe-btn");
const statusEl = document.getElementById("status");
const optionsRow = document.getElementById("options-row");
const presetSel = document.getElementById("preset");
const librarySel = document.getElementById("library");
const libraryLabel = document.getElementById("library-label");
const jobLinkEl = document.getElementById("job-link");

let currentUrl = "";
let settings = null;

// --- Init ---

async function init() {
  settings = await loadSettings();
  serverUrlInput.value = settings.server;
  serverTokenInput.value = settings.token;
  loadOptions();
}

async function loadOptions() {
  const opts = await fetchOptions(settings);
  if (!opts) {
    optionsRow.classList.add("hidden");
    return;
  }
  fill(presetSel, opts.presets || [], settings.preset);
  const libs = (opts.libraries || []).filter((l) => l !== "default");
  fill(librarySel, libs, settings.library);
  libraryLabel.classList.toggle("hidden", libs.length === 0);
  optionsRow.classList.remove("hidden");
}

function fill(select, values, selected) {
  while (select.options.length > 1) select.remove(1);
  for (const v of values) {
    const o = document.createElement("option");
    o.value = v;
    o.textContent = v;
    o.selected = v === selected;
    select.append(o);
  }
}

api.tabs.query({ active: true, currentWindow: true }).then((tabs) => {
  if (tabs[0] && tabs[0].url) {
    currentUrl = tabs[0].url;
    tabUrlEl.textContent = currentUrl;
  } else {
    tabUrlEl.textContent = "Unable to read tab URL";
    submitBtn.disabled = subscribeBtn.disabled = true;
    return;
  }
  if (isInternalPage(currentUrl)) {
    submitBtn.disabled = subscribeBtn.disabled = true;
    setStatus("Cannot send internal browser pages.", "error");
  }
});

init();

// --- Settings ---

settingsBtn.addEventListener("click", () => {
  const open = settingsPanel.classList.toggle("open");
  settingsBtn.classList.toggle("active", open);
});

saveBtn.addEventListener("click", async () => {
  let url = serverUrlInput.value.trim().replace(/\/+$/, "");
  if (!url) url = DEFAULT_SERVER;
  serverUrlInput.value = url;
  await api.storage.local.set({ serverAddress: url, serverToken: serverTokenInput.value });
  await api.storage.local.remove("serverPassword");
  settings = await loadSettings();
  setStatus("Settings saved.", "success");
  loadOptions();
});

presetSel.addEventListener("change", () => {
  settings.preset = presetSel.value;
  api.storage.local.set({ preset: presetSel.value });
});

librarySel.addEventListener("change", () => {
  settings.library = librarySel.value;
  api.storage.local.set({ library: librarySel.value });
});

// --- Submit ---

submitBtn.addEventListener("click", async () => {
  if (!currentUrl || isInternalPage(currentUrl)) return;
  submitBtn.disabled = true;
  setStatus("");
  jobLinkEl.classList.add("hidden");
  const r = await submitUrl(settings, currentUrl);
  setStatus(r.message, r.kind === "created" ? "success" : r.kind === "duplicate" ? "duplicate" : "error");
  if (r.jobId || r.kind === "created") {
    jobLinkEl.href = jobLink(settings, r.jobId);
    jobLinkEl.classList.remove("hidden");
  }
  submitBtn.disabled = false;
});

subscribeBtn.addEventListener("click", async () => {
  if (!currentUrl || isInternalPage(currentUrl)) return;
  subscribeBtn.disabled = true;
  setStatus("Looking up the channel…");
  jobLinkEl.classList.add("hidden");
  const r = await subscribeUrl(settings, currentUrl);
  setStatus(r.message, r.kind === "created" ? "success" : r.kind === "duplicate" ? "duplicate" : "error");
  if (r.kind === "created" || r.kind === "duplicate") {
    jobLinkEl.href = subscriptionLink(settings, r.subscriptionId);
    jobLinkEl.classList.remove("hidden");
  }
  subscribeBtn.disabled = false;
});

// --- Helpers ---

function setStatus(msg, type) {
  statusEl.textContent = msg;
  statusEl.className = "status" + (type ? " " + type : "");
}
