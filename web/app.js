const DEFAULT_TTL_MINUTES = 5;
const MAX_TTL_MINUTES = 1440;

// Incremented whenever the detail view changes, so that slow responses for a
// previous item or route are discarded instead of overwriting the current view.
let detailRequest = 0;

const state = {
  items: [],
  activeItem: null,
  activePreviewText: "",
  pendingFiles: [],
  composeTab: "files",
  expiryMinutes: DEFAULT_TTL_MINUTES,
  customExpiry: false,
  busy: false,
  tokens: new Map(),
  // Server-side limits in bytes; 0 means unlimited or not loaded yet.
  limits: { maxFileSize: 0, maxTextSize: 0 },
};

const els = {
  connectionStatus: document.querySelector("#connectionStatus"),
  homeView: document.querySelector("#homeView"),
  shareForm: document.querySelector("#shareForm"),
  composeTabs: document.querySelector("#composeTabs"),
  dropzone: document.querySelector("#dropzone"),
  dropTitle: document.querySelector("#dropTitle"),
  dropSubtitle: document.querySelector("#dropSubtitle"),
  fileInput: document.querySelector("#fileInput"),
  pendingFiles: document.querySelector("#pendingFiles"),
  textName: document.querySelector("#textName"),
  textInput: document.querySelector("#textInput"),
  expiryChips: document.querySelector("#expiryChips"),
  customExpiry: document.querySelector("#customExpiry"),
  shareTTL: document.querySelector("#shareTTL"),
  passwordToggle: document.querySelector("#passwordToggle"),
  passwordField: document.querySelector("#passwordField"),
  sharePassword: document.querySelector("#sharePassword"),
  passwordReveal: document.querySelector("#passwordReveal"),
  shareButton: document.querySelector("#shareButton"),
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
  els.homeView.hidden = isDetail;
  els.detailView.hidden = !isDetail;
}

function setComposeTab(tab) {
  state.composeTab = tab;
  els.composeTabs.querySelectorAll("[data-compose-tab]").forEach(button => {
    const active = button.dataset.composeTab === tab;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  });
  document.querySelectorAll("[data-compose-panel]").forEach(panel => {
    panel.hidden = panel.dataset.composePanel !== tab;
  });
  updateShareButton();
}

function setExpiry(choice) {
  state.customExpiry = choice === "custom";
  if (!state.customExpiry) state.expiryMinutes = Number(choice);
  els.expiryChips.querySelectorAll(".chip").forEach(chip => {
    chip.setAttribute("aria-pressed", String(chip.dataset.minutes === String(choice)));
  });
  els.customExpiry.hidden = !state.customExpiry;
  if (state.customExpiry) {
    els.shareTTL.focus();
    els.shareTTL.select();
  }
}

function selectedTTLSeconds() {
  return state.customExpiry ? ttlSecondsFrom(els.shareTTL) : state.expiryMinutes * 60;
}

function setPasswordEnabled(enabled) {
  els.passwordToggle.checked = enabled;
  els.passwordField.hidden = !enabled;
  if (enabled) {
    els.sharePassword.focus();
  } else {
    els.sharePassword.value = "";
    setPasswordRevealed(false);
  }
}

function setPasswordRevealed(revealed) {
  els.sharePassword.type = revealed ? "text" : "password";
  els.passwordReveal.textContent = revealed ? "Hide" : "Show";
  els.passwordReveal.setAttribute("aria-pressed", String(revealed));
}

// Returns the password to apply ("" for none), or null if the input is invalid.
function readPassword() {
  if (!els.passwordToggle.checked) return "";
  const value = els.sharePassword.value;
  if (!value.trim()) {
    toast("Enter a password or turn protection off");
    els.sharePassword.focus();
    return null;
  }
  return value;
}

function updateShareButton() {
  const files = state.pendingFiles.length;
  let label;
  let ready;
  if (state.composeTab === "files") {
    label = files === 0 ? "Share files" : files === 1 ? "Share 1 file" : `Share ${files} files`;
    ready = files > 0;
  } else {
    label = "Share text";
    ready = els.textInput.value.trim() !== "";
  }
  if (state.busy) {
    label = "Sharing…";
    ready = false;
  }
  els.shareButton.textContent = label;
  els.shareButton.disabled = !ready;
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
    detailRequest++;
    state.activeItem = null;
    state.activePreviewText = "";
    setMode("home");
    return;
  }

  setMode("detail");
  await loadDetail(id);
}

async function loadDetail(id) {
  const request = ++detailRequest;
  const isCurrent = () => request === detailRequest;
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
    if (!isCurrent()) return;
    if (res.status === 404) {
      renderMissingDetail(id);
      return;
    }
    if (!res.ok) throw new Error(await res.text());
    const item = await res.json();
    if (!isCurrent()) return;
    state.activeItem = item;
    renderDetail(item);
    if (item.protected && !hasAccess(item)) {
      renderUnlockPrompt(item);
      return;
    }
    await renderPreview(item, isCurrent);
  } catch (err) {
    if (isCurrent()) renderPreviewMessage(cleanError(err));
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

async function renderPreview(item, isCurrent = () => true) {
  if (item.previewKind === "text") {
    const res = await fetch(itemURL(item, "raw"));
    if (!isCurrent()) return;
    if (res.status === 401) {
      handleAuthRequired(item);
      return;
    }
    if (!res.ok) throw new Error(await res.text());
    const text = await res.text();
    if (!isCurrent()) return;
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

async function loadLimits() {
  try {
    const res = await fetch("/api/config");
    if (!res.ok) return;
    const config = await res.json();
    state.limits.maxFileSize = config.maxFileSize || 0;
    state.limits.maxTextSize = config.maxTextSize || 0;
  } catch (_err) {
    // The server enforces the limits anyway; this only enables earlier feedback.
  }
}

function stageFiles(files) {
  let selected = Array.from(files || []);
  const maxFileSize = state.limits.maxFileSize;
  if (maxFileSize > 0) {
    const tooLarge = selected.filter(file => file.size > maxFileSize);
    if (tooLarge.length > 0) {
      selected = selected.filter(file => file.size <= maxFileSize);
      toast(`${tooLarge.length === 1 ? tooLarge[0].name : `${tooLarge.length} files`} exceed${tooLarge.length === 1 ? "s" : ""} the ${formatBytes(maxFileSize)} limit`);
    }
  }
  if (selected.length === 0) {
    els.fileInput.value = "";
    return;
  }
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
    ? "Drop files here, paste, or click to browse"
    : "Drop, paste or click to add to the list";
  updateShareButton();

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

async function submitShare(event) {
  event.preventDefault();
  if (state.busy) return;

  const password = readPassword();
  if (password === null) return;

  if (state.composeTab === "files") {
    await shareFiles(password);
  } else {
    await shareText(password);
  }
}

function setBusy(busy) {
  state.busy = busy;
  updateShareButton();
}

function resetShareOptions() {
  setPasswordEnabled(false);
}

async function shareFiles(password) {
  const selected = state.pendingFiles.slice();
  if (selected.length === 0) return;

  setBusy(true);
  toast(`Uploading ${selected.length} ${selected.length === 1 ? "file" : "files"}...`);
  // Options must come before the files: they only apply to files that follow.
  const data = new FormData();
  data.append("ttlSeconds", String(selectedTTLSeconds()));
  if (password) data.append("password", password);
  selected.forEach(file => data.append("files", file, file.name));

  try {
    const res = await fetch("/api/items/files", { method: "POST", body: data });
    if (!res.ok) throw new Error(await res.text());
    state.pendingFiles = [];
    renderPendingFiles();
    resetShareOptions();
    await loadItems();
    toast(selected.length === 1 ? "File shared" : "Files shared");
  } catch (err) {
    toast(cleanError(err));
  } finally {
    els.fileInput.value = "";
    setBusy(false);
  }
}

async function shareText(password) {
  const text = els.textInput.value;
  if (!text.trim()) return;
  if (state.limits.maxTextSize > 0 && new Blob([text]).size > state.limits.maxTextSize) {
    toast(`Text exceeds the ${formatBytes(state.limits.maxTextSize)} limit`);
    return;
  }

  setBusy(true);
  try {
    const res = await fetch("/api/items/text", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: els.textName.value,
        text,
        ttlSeconds: selectedTTLSeconds(),
        password,
      }),
    });
    if (!res.ok) throw new Error(await res.text());
    els.textInput.value = "";
    els.textName.value = "";
    resetShareOptions();
    await loadItems();
    toast("Text shared");
  } catch (err) {
    toast(cleanError(err));
  } finally {
    setBusy(false);
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

// Reloads the list and keeps the open detail view consistent with it. Items
// never change once created, so the detail view is only touched when its item
// disappeared (or was never loaded); reloading it on every event would restart
// playing media and throw away a half-typed password.
async function refreshFromServer() {
  await loadItems();
  const id = currentDetailID();
  if (!id) return;
  if (!state.items.some(item => item.id === id)) {
    detailRequest++;
    state.activeItem = null;
    state.activePreviewText = "";
    renderMissingDetail(id);
    return;
  }
  if (!state.activeItem || state.activeItem.id !== id) {
    await loadDetail(id);
  }
}

function connectEvents() {
  const events = new EventSource("/api/events");
  let connectedBefore = false;
  events.addEventListener("connected", async () => {
    setStatus("Live", "live");
    // Changes made while the stream was down were never announced.
    if (connectedBefore) {
      try {
        await refreshFromServer();
      } catch (_err) {
        setStatus("Offline", "offline");
      }
    }
    connectedBefore = true;
  });
  events.addEventListener("items_changed", async () => {
    try {
      await refreshFromServer();
      setStatus("Live", "live");
    } catch (_err) {
      setStatus("Offline", "offline");
    }
  });
  events.onerror = () => setStatus("Reconnecting", "offline");
}

els.shareForm.addEventListener("submit", submitShare);
els.fileInput.addEventListener("change", event => stageFiles(event.target.files));
els.backButton.addEventListener("click", openHome);
window.addEventListener("popstate", renderRoute);

els.composeTabs.querySelectorAll("[data-compose-tab]").forEach(button => {
  button.addEventListener("click", () => setComposeTab(button.dataset.composeTab));
});
els.expiryChips.querySelectorAll(".chip").forEach(chip => {
  chip.addEventListener("click", () => setExpiry(chip.dataset.minutes));
});
els.shareTTL.addEventListener("blur", () => ttlSecondsFrom(els.shareTTL));
els.passwordToggle.addEventListener("change", () => setPasswordEnabled(els.passwordToggle.checked));
els.passwordReveal.addEventListener("click", () => setPasswordRevealed(els.sharePassword.type === "password"));
els.textInput.addEventListener("input", updateShareButton);
els.textInput.addEventListener("keydown", event => {
  if ((event.ctrlKey || event.metaKey) && event.key === "Enter") {
    event.preventDefault();
    els.shareForm.requestSubmit();
  }
});

// Files can be dropped anywhere on the page, not only on the drop zone; a
// miss would otherwise make the browser navigate away to the dropped file.
function dragHasFiles(event) {
  return Array.from((event.dataTransfer && event.dataTransfer.types) || []).includes("Files");
}

window.addEventListener("dragover", event => {
  if (!dragHasFiles(event)) return;
  event.preventDefault();
  if (!els.homeView.hidden) els.shareForm.classList.add("dragging");
});

window.addEventListener("dragleave", event => {
  if (!event.relatedTarget) els.shareForm.classList.remove("dragging");
});

window.addEventListener("drop", event => {
  if (!dragHasFiles(event)) return;
  event.preventDefault();
  els.shareForm.classList.remove("dragging");
  if (els.homeView.hidden) return;
  setComposeTab("files");
  stageFiles(event.dataTransfer.files);
});

// Pasting a screenshot or copied file stages it just like dropping it.
document.addEventListener("paste", event => {
  const files = event.clipboardData && event.clipboardData.files;
  if (els.homeView.hidden || !files || files.length === 0) return;
  event.preventDefault();
  setComposeTab("files");
  stageFiles(files);
});

// Only the countdowns change from second to second. Update them in place so the
// list keeps keyboard focus and in-progress clicks instead of being rebuilt.
function tickMeta() {
  const byID = new Map(state.items.map(item => [item.id, item]));
  els.items.querySelectorAll(".item").forEach(row => {
    const item = byID.get(row.dataset.id);
    const meta = row.querySelector(".item-meta");
    if (item && meta) renderMeta(meta, item);
  });
  if (state.activeItem) renderMeta(els.detailMeta, state.activeItem);
}

setInterval(tickMeta, 1000);

setComposeTab(state.composeTab);
renderPendingFiles();
loadLimits();
loadItems()
  .then(renderRoute)
  .then(() => setStatus("Live", "live"))
  .catch(() => setStatus("Offline", "offline"));
connectEvents();
