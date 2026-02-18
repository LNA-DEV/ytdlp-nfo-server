const api = typeof browser !== "undefined" ? browser : chrome;

const DEFAULT_SERVER = "http://localhost:8080";
const INTERNAL_PREFIXES = ["about:", "chrome://", "chrome-extension://", "moz-extension://", "edge://", "brave://", "opera://"];

const settingsBtn = document.getElementById("settings-btn");
const settingsPanel = document.getElementById("settings-panel");
const serverUrlInput = document.getElementById("server-url");
const saveBtn = document.getElementById("save-btn");
const tabUrlEl = document.getElementById("tab-url");
const submitBtn = document.getElementById("submit-btn");
const statusEl = document.getElementById("status");

let currentUrl = "";
let serverAddress = DEFAULT_SERVER;

// --- Init ---

api.storage.local.get("serverAddress").then((data) => {
  serverAddress = data.serverAddress || DEFAULT_SERVER;
  serverUrlInput.value = serverAddress;
});

api.tabs.query({ active: true, currentWindow: true }).then((tabs) => {
  if (tabs[0] && tabs[0].url) {
    currentUrl = tabs[0].url;
    tabUrlEl.textContent = currentUrl;
  } else {
    tabUrlEl.textContent = "Unable to read tab URL";
    submitBtn.disabled = true;
    return;
  }

  if (isInternalPage(currentUrl)) {
    submitBtn.disabled = true;
    setStatus("Cannot send internal browser pages.", "error");
  }
});

// --- Settings ---

settingsBtn.addEventListener("click", () => {
  const open = settingsPanel.classList.toggle("open");
  settingsBtn.classList.toggle("active", open);
});

saveBtn.addEventListener("click", () => {
  let url = serverUrlInput.value.trim().replace(/\/+$/, "");
  if (!url) url = DEFAULT_SERVER;
  serverAddress = url;
  serverUrlInput.value = url;
  api.storage.local.set({ serverAddress: url });
  setStatus("Server address saved.", "success");
});

// --- Submit ---

submitBtn.addEventListener("click", async () => {
  if (!currentUrl || isInternalPage(currentUrl)) return;

  submitBtn.disabled = true;
  setStatus("");

  try {
    const res = await fetch(serverAddress + "/api/download", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url: currentUrl }),
    });

    if (res.status === 201) {
      setStatus("Download queued successfully!", "success");
    } else if (res.status === 409) {
      setStatus("Already queued (duplicate).", "duplicate");
    } else {
      let msg = "Server returned " + res.status;
      try {
        const body = await res.json();
        if (body.error) msg = body.error;
      } catch {}
      setStatus(msg, "error");
    }
  } catch {
    setStatus("Could not reach server. Check address in settings.", "error");
  } finally {
    submitBtn.disabled = false;
  }
});

// --- Helpers ---

function isInternalPage(url) {
  return INTERNAL_PREFIXES.some((p) => url.startsWith(p));
}

function setStatus(msg, type) {
  statusEl.textContent = msg;
  statusEl.className = "status" + (type ? " " + type : "");
}
