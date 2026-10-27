// Context menu "Send link to ytdlp-nfo-server". In Chrome this runs as a
// service worker; Firefox loads common.js first via background.scripts.
if (typeof submitUrl === "undefined" && typeof importScripts === "function") {
  importScripts("common.js");
}

const MENU_LINK = "ytdlp-send-link";
const MENU_PAGE = "ytdlp-send-page";
const MENU_MEDIA = "ytdlp-send-media";
const MENU_SUB_PAGE = "ytdlp-subscribe-page";
const MENU_SUB_LINK = "ytdlp-subscribe-link";

function createMenus() {
  api.contextMenus.removeAll(() => {
    api.contextMenus.create({ id: MENU_LINK, title: "Send link to ytdlp-nfo-server", contexts: ["link"] });
    api.contextMenus.create({ id: MENU_MEDIA, title: "Send video to ytdlp-nfo-server", contexts: ["video", "audio"] });
    api.contextMenus.create({ id: MENU_PAGE, title: "Send page to ytdlp-nfo-server", contexts: ["page"] });
    api.contextMenus.create({ id: MENU_SUB_PAGE, title: "Subscribe on ytdlp-nfo-server", contexts: ["page"] });
    api.contextMenus.create({ id: MENU_SUB_LINK, title: "Subscribe to link on ytdlp-nfo-server", contexts: ["link"] });
  });
}

// Firefox for Android has no context menus.
if (api.contextMenus) {
  api.runtime.onInstalled.addListener(createMenus);
  if (api.runtime.onStartup) api.runtime.onStartup.addListener(createMenus);
  api.contextMenus.onClicked.addListener(onMenuClick);
}

function flash(text, color, tabId) {
  const action = api.action || api.browserAction;
  action.setBadgeBackgroundColor({ color, tabId });
  action.setBadgeText({ text, tabId });
  setTimeout(() => action.setBadgeText({ text: "", tabId }), 4000);
}

async function onMenuClick(info, tab) {
  let url = "";
  if (info.menuItemId === MENU_LINK || info.menuItemId === MENU_SUB_LINK) url = info.linkUrl;
  else if (info.menuItemId === MENU_MEDIA) url = info.pageUrl || info.srcUrl;
  else url = info.pageUrl || (tab && tab.url);
  const tabId = tab && tab.id >= 0 ? tab.id : undefined;
  if (isInternalPage(url) || !/^https?:/i.test(url)) {
    flash("!", "#f87171", tabId);
    return;
  }
  const s = await loadSettings();
  const subscribe = info.menuItemId === MENU_SUB_PAGE || info.menuItemId === MENU_SUB_LINK;
  const r = subscribe ? await subscribeUrl(s, url) : await submitUrl(s, url);
  if (r.kind === "created") flash("✓", "#16a34a", tabId);
  else if (r.kind === "duplicate") flash("=", "#ca8a04", tabId);
  else flash("✗", "#dc2626", tabId);
}
