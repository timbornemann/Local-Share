const DEFAULT_TTL_MINUTES = 5;
const MAX_TTL_MINUTES = 1440;

const state = {
  items: [],
  activeItem: null,
  activePreviewText: "",
  pendingFiles: [],
  composeTab: "files",
  busy: false,
  tokens: new Map(),
};

const els = {
  connectionStatus: document.querySelector("#connectionStatus"),
  composeTabs: document.querySelector("#composeTabs"),
  composeView: document.querySelector("#composeView"),
  listHead: document.querySelector("#listHead"),
  dropzone: document.querySelector("#dropzone"),
  dropTitle: document.querySelector("#dropTitle"),
  dropSubtitle: document.querySelector("#dropSubtitle"),
  fileForm: document.querySelector("#fileForm"),
  fileInput: document.querySelector("#fileInput"),
  pendingFiles: document.querySelector("#pendingFiles"),
  shareFilesButton: document.querySelector("#shareFilesButton"),
  shareTTL: document.querySelector("#shareTTL"),
  sharePassword: document.querySelector("#sharePassword"),
  textForm: document.querySelector("#textForm"),
  textName: document.querySelector("#textName"),
  textInput: document.querySelector("#textInput"),
  items: document.querySelector("#items"),
  itemCount: document.querySelector("#itemCount"),
  detailView: document.querySelector("#detailView"),
  detailActions: document.querySelector("#detailActions"),
  detailBadge: document.querySelector("#detailBadge"),
  detailName: document.querySelector("#detailName"),
  detailMeta: document.querySelector("#detailMeta"),
  previewArea: document.querySelector("#previewArea"),
  backButton: document.querySelector("#backButton"),
  toast: document.querySelector("#toast"),
};

function setStatus(label, mode) {
  els.connectionStatus.textContent = label;
  els.connectionStatus.className = `status ${mode || ""}`.trim();
}

function toast(message) {
  els.toast.textContent = message;
  els.toast.classList.add("show");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => els.toast.classList.remove("show"), 2200);
}

async function loadItems() {
  const res = await fetch("/api/items");
  if (!res.ok) throw new Error("Could not load items");
  state.items = await res.json();
  const activeIDs = new Set(state.items.map(item => item.id));
  Array.from(state.tokens.keys()).forEach(id => {
    if (!activeIDs.has(id)) state.tokens.delete(id);
  });
  renderItems();
}

function renderItems() {
  const count = state.items.length;
  els.itemCount.textContent = `${count} ${count === 1 ? "item" : "items"}`;

  if (count === 0) {
    els.items.innerHTML = `<div class="empty">Nothing shared yet.</div>`;
    return;
  }

  els.items.replaceChildren(...state.items.map(renderItem));
}

function renderItem(item) {
  const row = document.createElement("article");
  row.className = "item";
  row.dataset.id = item.id;
  row.tabIndex = 0;
  row.setAttribute("role", "button");
  row.setAttribute("aria-label", `Open ${item.name}`);
  row.addEventListener("click", () => openDetail(item.id));
  row.addEventListener("keydown", event => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      openDetail(item.id);
    }
  });

  const main = document.createElement("div");
  main.className = "item-main";

  const title = document.createElement("div");
  title.className = "item-title";

  const badge = document.createElement("span");
  badge.className = `badge ${item.kind}`;
  badge.textContent = item.kind;

  const name = document.createElement("strong");
  name.textContent = item.name;
  name.title = item.name;

  title.append(badge, name);

  const meta = document.createElement("div");
  meta.className = "item-meta";
  renderMeta(meta, item);

  main.append(title, meta);

  const actions = document.createElement("div");
  actions.className = "item-actions";
  actions.addEventListener("click", event => event.stopPropagation());
  actions.addEventListener("keydown", event => event.stopPropagation());

  if (hasAccess(item)) {
    if (item.previewKind === "text") {
      actions.append(actionButton("Copy", "secondary", () => copyItemText(item.id)));
    }
    actions.append(downloadLink(item, "secondary"));
  } else {
    actions.append(actionButton("Unlock", "secondary", () => openDetail(item.id)));
  }
  actions.append(actionButton("Delete", "danger", () => deleteItem(item.id)));

  row.append(main, actions);
  return row;
}

function renderMeta(container, item) {
  if (item.protected && !hasAccess(item)) {
    container.replaceChildren(
      metaPart("password protected", "meta-lock"),
      metaPart(timeLeft(item.expiresAt), "meta-expiry"),
    );
    return;
  }

  const parts = [
    metaPart(formatBytes(item.size), "meta-size"),
    metaPart(timeLeft(item.expiresAt), "meta-expiry"),
  ];
  if (item.protected) {
    parts.push(metaPart("unlocked", "meta-lock"));
  }
  parts.push(metaPart(item.contentType || "unknown type", "meta-type"));
  container.replaceChildren(...parts);
}

function metaPart(text, className = "") {
  const span = document.createElement("span");
  span.className = className;
  span.textContent = text;
  return span;
}

function actionButton(label, className, onClick) {
  const button = document.createElement("button");
  button.className = className;
  button.type = "button";
  button.textContent = label;
  button.addEventListener("click", event => {
    event.preventDefault();
    event.stopPropagation();
    onClick();
  });
  return button;
}

function downloadLink(item, className) {
  const link = document.createElement("a");
  link.className = `button-link ${className || ""}`.trim();
  link.href = itemURL(item, "download");
  link.download = item.name;
  link.textContent = "Download";
  link.addEventListener("click", event => event.stopPropagation());
  return link;
}

function itemURL(item, action) {
  const base = `/api/items/${encodeURIComponent(item.id)}/${action}`;
  const token = state.tokens.get(item.id);
  return token ? `${base}?token=${encodeURIComponent(token)}` : base;
}

function hasAccess(item) {
  return !item.protected || state.tokens.has(item.id);
}

function knownItem(id) {
  if (state.activeItem && state.activeItem.id === id) return state.activeItem;
  return state.items.find(item => item.id === id) || { id, protected: false };
}

function currentDetailID() {
  const match = window.location.pathname.match(/^\/items\/([^/]+)$/);
  return match ? decodeURIComponent(match[1]) : "";
}

function setMode(mode) {
  const isDetail = mode === "detail";
  els.composeTabs.hidden = isDetail;
  els.composeView.hidden = isDetail;
  els.listHead.hidden = isDetail;
  els.items.hidden = isDetail;
  els.detailView.hidden = !isDetail;
}

const mobileLayout = window.matchMedia("(max-width: 760px)");

function setComposeTab(tab) {
  state.composeTab = tab;
  els.composeTabs.querySelectorAll("[data-compose-tab]").forEach(button => {
    const active = button.dataset.composeTab === tab;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  });
  syncComposePanels();
}

function syncComposePanels() {
  document.querySelectorAll("[data-compose-panel]").forEach(panel => {
    panel.hidden = mobileLayout.matches && panel.dataset.composePanel !== state.composeTab;
  });
}

function openDetail(id) {
  window.history.pushState({}, "", `/items/${encodeURIComponent(id)}`);
  renderRoute();
}

function openHome() {
  window.history.pushState({}, "", "/");
  renderRoute();
}

async function renderRoute() {
  const id = currentDetailID();
  if (!id) {
    state.activeItem = null;
    state.activePreviewText = "";
    setMode("home");
    return;
  }

  setMode("detail");
  await loadDetail(id);
}

async function loadDetail(id) {
  state.activeItem = null;
  state.activePreviewText = "";
  els.detailActions.replaceChildren();
  els.detailBadge.className = "badge";
  els.detailBadge.textContent = "item";
  els.detailName.textContent = "Loading";
  els.detailMeta.replaceChildren();
  renderPreviewMessage("Loading preview...");

  try {
    const res = await fetch(`/api/items/${encodeURIComponent(id)}`);
    if (res.status === 404) {
      renderMissingDetail(id);
      return;
    }
    if (!res.ok) throw new Error(await res.text());
    const item = await res.json();
    state.activeItem = item;
    renderDetail(item);
    if (item.protected && !hasAccess(item)) {
      renderUnlockPrompt(item);
      return;
    }
    await renderPreview(item);
  } catch (err) {
    renderPreviewMessage(cleanError(err));
  }
}

function renderMissingDetail(id = currentDetailID()) {
  state.tokens.delete(id);
  els.detailName.textContent = "Item unavailable";
  els.detailBadge.textContent = "gone";
  els.detailMeta.replaceChildren(metaPart("It may have expired or been deleted."));
  els.detailActions.replaceChildren();
  renderPreviewMessage("This item is no longer available.");
}

function renderDetail(item) {
  els.detailBadge.className = `badge ${item.kind}`;
  els.detailBadge.textContent = item.kind;
  els.detailName.textContent = item.name;
  renderMeta(els.detailMeta, item);

  const actions = [];
  if (hasAccess(item)) {
    if (item.previewKind === "text") {
      actions.push(actionButton("Copy", "secondary", () => copyItemText(item.id)));
    }
    actions.push(downloadLink(item, "secondary"));
  }
  actions.push(actionButton("Delete", "danger", () => deleteItem(item.id, { returnHome: true })));
  els.detailActions.replaceChildren(...actions);
}

function renderUnlockPrompt(item) {
  const form = document.createElement("form");
  form.className = "unlock-form";

  const title = document.createElement("div");
  title.className = "unlock-form-title";
  title.textContent = "Password required";

  const label = document.createElement("label");
  const labelText = document.createElement("span");
  labelText.textContent = "Password";
  const input = document.createElement("input");
  input.type = "password";
  input.autocomplete = "current-password";
  input.required = true;
  label.append(labelText, input);

  const button = document.createElement("button");
  button.type = "submit";
  button.textContent = "Unlock";

  form.append(title, label, button);
  form.addEventListener("submit", event => {
    event.preventDefault();
    unlockItem(item, input.value);
  });

  const box = document.createElement("div");
  box.className = "preview-message";
  box.append(form);
  els.previewArea.replaceChildren(box);
  setTimeout(() => input.focus(), 0);
}

async function unlockItem(item, password) {
  if (!password.trim()) {
    toast("Enter password");
    return;
  }

  try {
    const res = await fetch(`/api/items/${encodeURIComponent(item.id)}/unlock`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password }),
    });
    if (res.status === 404) {
      renderMissingDetail(item.id);
      await loadItems();
      return;
    }
    if (res.status === 401) throw new Error("Wrong password");
    if (!res.ok) throw new Error(await res.text());
    const payload = await res.json();
    state.tokens.set(item.id, payload.token);
    toast("Unlocked");
    await loadItems();
    await loadDetail(item.id);
  } catch (err) {
    toast(cleanError(err));
  }
}

async function renderPreview(item) {
  if (item.previewKind === "text") {
    const res = await fetch(itemURL(item, "raw"));
    if (res.status === 401) {
      handleAuthRequired(item);
      return;
    }
    if (!res.ok) throw new Error(await res.text());
    const text = await res.text();
    state.activePreviewText = text;
    const pre = document.createElement("pre");
    const code = document.createElement("code");
    code.textContent = text;
    pre.append(code);
    els.previewArea.replaceChildren(pre);
    return;
  }

  if (item.previewKind === "image") {
    const img = document.createElement("img");
    img.src = itemURL(item, "view");
    img.alt = item.name;
    img.addEventListener("error", () => handleAuthRequired(item));
    els.previewArea.replaceChildren(img);
    return;
  }

  if (item.previewKind === "pdf") {
    const frame = document.createElement("iframe");
    frame.title = item.name;
    frame.src = itemURL(item, "view");
    els.previewArea.replaceChildren(frame);
    return;
  }

  if (item.previewKind === "audio") {
    const audio = document.createElement("audio");
    audio.controls = true;
    audio.src = itemURL(item, "view");
    audio.addEventListener("error", () => handleAuthRequired(item));
    els.previewArea.replaceChildren(audio);
    return;
  }

  if (item.previewKind === "video") {
    const video = document.createElement("video");
    video.controls = true;
    video.src = itemURL(item, "view");
    video.addEventListener("error", () => handleAuthRequired(item));
    els.previewArea.replaceChildren(video);
    return;
  }

  renderPreviewMessage("No browser preview is available for this file type. Download it to open it locally.");
}

function handleAuthRequired(item) {
  if (!item.protected) return;
  state.tokens.delete(item.id);
  state.activePreviewText = "";
  renderDetail(item);
  renderUnlockPrompt(item);
  toast("Enter password again");
}

function renderPreviewMessage(message) {
  const box = document.createElement("div");
  box.className = "preview-message";
  box.textContent = message;
  els.previewArea.replaceChildren(box);
}

function formatBytes(bytes) {
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const power = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / 1024 ** power;
  return `${value >= 10 || power === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[power]}`;
}

function timeLeft(expiresAt) {
  const ms = new Date(expiresAt).getTime() - Date.now();
  if (ms <= 0) return "expires now";
  const total = Math.ceil(ms / 1000);
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  if (minutes === 0) return `${seconds}s left`;
  return `${minutes}m ${seconds.toString().padStart(2, "0")}s left`;
}

function ttlSecondsFrom(input) {
  const parsed = Number.parseFloat(input.value);
  let minutes = Number.isFinite(parsed) ? Math.round(parsed) : DEFAULT_TTL_MINUTES;
  minutes = Math.min(MAX_TTL_MINUTES, Math.max(1, minutes));
  input.value = String(minutes);
  return minutes * 60;
}

function stageFiles(files) {
  const selected = Array.from(files || []);
  if (selected.length === 0) return;
  state.pendingFiles.push(...selected);
  els.fileInput.value = "";
  renderPendingFiles();
}

function removePendingFile(index) {
  state.pendingFiles.splice(index, 1);
  renderPendingFiles();
}

function renderPendingFiles() {
  const files = state.pendingFiles;
  const count = files.length;
  els.pendingFiles.hidden = count === 0;
  els.dropTitle.textContent = count === 0 ? "Choose files" : "Add more files";
  els.dropSubtitle.textContent = count === 0
    ? "Tap, click, or drop files, then share"
    : "Review the list, then confirm below";
  els.shareFilesButton.textContent = count === 1
    ? "Share File"
    : count > 1
      ? `Share ${count} Files`
      : "Share Files";

  els.pendingFiles.replaceChildren(...files.map((file, index) => {
    const row = document.createElement("li");
    row.className = "pending-file";

    const name = document.createElement("span");
    name.className = "pending-file-name";
    name.textContent = file.name;
    name.title = file.name;

    const size = document.createElement("span");
    size.className = "pending-file-size";
    size.textContent = formatBytes(file.size);

    const remove = actionButton("Remove", "secondary", () => removePendingFile(index));
    remove.setAttribute("aria-label", `Remove ${file.name}`);

    row.append(name, size, remove);
    return row;
  }));
}

async function shareFiles(event) {
  event.preventDefault();
  if (state.busy) return;

  const selected = state.pendingFiles.slice();
  if (selected.length === 0) {
    toast("Choose files first");
    return;
  }

  state.busy = true;
  toast(`Uploading ${selected.length} ${selected.length === 1 ? "file" : "files"}...`);
  const data = new FormData();
  data.append("ttlSeconds", String(ttlSecondsFrom(els.shareTTL)));
  if (els.sharePassword.value) {
    data.append("password", els.sharePassword.value);
  }
  selected.forEach(file => data.append("files", file, file.name));

  try {
    const res = await fetch("/api/items/files", { method: "POST", body: data });
    if (!res.ok) throw new Error(await res.text());
    state.pendingFiles = [];
    renderPendingFiles();
    els.sharePassword.value = "";
    await loadItems();
    toast("Upload complete");
  } catch (err) {
    toast(cleanError(err));
  } finally {
    state.busy = false;
    els.fileInput.value = "";
  }
}

async function shareText(event) {
  event.preventDefault();
  if (state.busy) return;

  const text = els.textInput.value;
  const name = els.textName.value;
  if (!text.trim()) {
    toast("Add text first");
    return;
  }

  state.busy = true;
  try {
    const res = await fetch("/api/items/text", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name,
        text,
        ttlSeconds: ttlSecondsFrom(els.shareTTL),
        password: els.sharePassword.value,
      }),
    });
    if (!res.ok) throw new Error(await res.text());
    els.textInput.value = "";
    els.textName.value = "";
    els.sharePassword.value = "";
    await loadItems();
    toast("Text shared");
  } catch (err) {
    toast(cleanError(err));
  } finally {
    state.busy = false;
  }
}

async function copyItemText(id) {
  const item = knownItem(id);
  if (item.protected && !hasAccess(item)) {
    openDetail(id);
    toast("Enter password first");
    return;
  }

  try {
    let text = "";
    if (state.activeItem && state.activeItem.id === id && state.activePreviewText) {
      text = state.activePreviewText;
    } else {
      const res = await fetch(itemURL(item, "raw"));
      if (res.status === 401) {
        handleAuthRequired(item);
        return;
      }
      if (!res.ok) throw new Error(await res.text());
      text = await res.text();
    }
    await copyToClipboard(text);
    toast("Copied");
  } catch (err) {
    toast(cleanError(err));
  }
}

async function copyToClipboard(text) {
  if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch (_err) {
      // Fall through to the legacy selection-based copy path.
    }
  }

  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  textarea.style.top = "0";
  document.body.append(textarea);
  textarea.focus();
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) {
    throw new Error("Copy is not available in this browser");
  }
}

async function deleteItem(id, options = {}) {
  try {
    const res = await fetch(`/api/items/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) throw new Error(await res.text());
    state.tokens.delete(id);
    state.items = state.items.filter(item => item.id !== id);
    renderItems();
    toast("Deleted");
    if (options.returnHome || currentDetailID() === id) {
      openHome();
    }
  } catch (err) {
    toast(cleanError(err));
  }
}

function cleanError(err) {
  const message = err && err.message ? err.message.trim().replace(/\s+/g, " ") : "Something went wrong";
  return message.length > 140 ? `${message.slice(0, 137)}...` : message;
}

function connectEvents() {
  const events = new EventSource("/api/events");
  events.addEventListener("connected", () => setStatus("Live", "live"));
  events.addEventListener("items_changed", async () => {
    try {
      await loadItems();
      const id = currentDetailID();
      if (id) await loadDetail(id);
      setStatus("Live", "live");
    } catch (_err) {
      setStatus("Offline", "offline");
    }
  });
  events.onerror = () => setStatus("Reconnecting", "offline");
}

els.fileForm.addEventListener("submit", shareFiles);
els.fileInput.addEventListener("change", event => stageFiles(event.target.files));
els.textForm.addEventListener("submit", shareText);
els.backButton.addEventListener("click", openHome);
window.addEventListener("popstate", renderRoute);
if (typeof mobileLayout.addEventListener === "function") {
  mobileLayout.addEventListener("change", syncComposePanels);
} else {
  mobileLayout.addListener(syncComposePanels);
}
els.composeTabs.querySelectorAll("[data-compose-tab]").forEach(button => {
  button.addEventListener("click", () => setComposeTab(button.dataset.composeTab));
});

els.shareTTL.addEventListener("blur", () => ttlSecondsFrom(els.shareTTL));

["dragenter", "dragover"].forEach(type => {
  els.dropzone.addEventListener(type, event => {
    event.preventDefault();
    els.dropzone.classList.add("dragging");
  });
});

["dragleave", "drop"].forEach(type => {
  els.dropzone.addEventListener(type, event => {
    event.preventDefault();
    els.dropzone.classList.remove("dragging");
  });
});

els.dropzone.addEventListener("drop", event => stageFiles(event.dataTransfer.files));

setInterval(() => {
  renderItems();
  if (state.activeItem) renderMeta(els.detailMeta, state.activeItem);
}, 1000);

setComposeTab(state.composeTab);
loadItems()
  .then(renderRoute)
  .then(() => setStatus("Live", "live"))
  .catch(() => setStatus("Offline", "offline"));
connectEvents();
