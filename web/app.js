const state = {
  items: [],
  activeItem: null,
  activePreviewText: "",
  busy: false,
};

const els = {
  connectionStatus: document.querySelector("#connectionStatus"),
  composeView: document.querySelector("#composeView"),
  listHead: document.querySelector("#listHead"),
  dropzone: document.querySelector("#dropzone"),
  fileInput: document.querySelector("#fileInput"),
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

  if (item.previewKind === "text") {
    actions.append(actionButton("Copy", "secondary", () => copyItemText(item.id)));
  }

  actions.append(downloadLink(item, "secondary"));
  actions.append(actionButton("Delete", "danger", () => deleteItem(item.id)));

  row.append(main, actions);
  return row;
}

function renderMeta(container, item) {
  container.replaceChildren(
    metaPart(formatBytes(item.size)),
    metaPart(timeLeft(item.expiresAt)),
    metaPart(item.contentType || "unknown type"),
  );
}

function metaPart(text) {
  const span = document.createElement("span");
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
  link.href = `/api/items/${encodeURIComponent(item.id)}/download`;
  link.download = item.name;
  link.textContent = "Download";
  link.addEventListener("click", event => event.stopPropagation());
  return link;
}

function currentDetailID() {
  const match = window.location.pathname.match(/^\/items\/([^/]+)$/);
  return match ? decodeURIComponent(match[1]) : "";
}

function setMode(mode) {
  const isDetail = mode === "detail";
  els.composeView.hidden = isDetail;
  els.listHead.hidden = isDetail;
  els.items.hidden = isDetail;
  els.detailView.hidden = !isDetail;
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
      renderMissingDetail();
      return;
    }
    if (!res.ok) throw new Error(await res.text());
    const item = await res.json();
    state.activeItem = item;
    renderDetail(item);
    await renderPreview(item);
  } catch (err) {
    renderPreviewMessage(cleanError(err));
  }
}

function renderMissingDetail() {
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
  if (item.previewKind === "text") {
    actions.push(actionButton("Copy", "secondary", () => copyItemText(item.id)));
  }
  actions.push(downloadLink(item, "secondary"));
  actions.push(actionButton("Delete", "danger", () => deleteItem(item.id, { returnHome: true })));
  els.detailActions.replaceChildren(...actions);
}

async function renderPreview(item) {
  if (item.previewKind === "text") {
    const res = await fetch(`/api/items/${encodeURIComponent(item.id)}/raw`);
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
    img.src = `/api/items/${encodeURIComponent(item.id)}/view`;
    img.alt = item.name;
    els.previewArea.replaceChildren(img);
    return;
  }

  if (item.previewKind === "pdf") {
    const frame = document.createElement("iframe");
    frame.title = item.name;
    frame.src = `/api/items/${encodeURIComponent(item.id)}/view`;
    els.previewArea.replaceChildren(frame);
    return;
  }

  if (item.previewKind === "audio") {
    const audio = document.createElement("audio");
    audio.controls = true;
    audio.src = `/api/items/${encodeURIComponent(item.id)}/view`;
    els.previewArea.replaceChildren(audio);
    return;
  }

  if (item.previewKind === "video") {
    const video = document.createElement("video");
    video.controls = true;
    video.src = `/api/items/${encodeURIComponent(item.id)}/view`;
    els.previewArea.replaceChildren(video);
    return;
  }

  renderPreviewMessage("No browser preview is available for this file type. Download it to open it locally.");
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

async function uploadFiles(files) {
  const selected = Array.from(files || []);
  if (selected.length === 0 || state.busy) return;

  state.busy = true;
  toast(`Uploading ${selected.length} ${selected.length === 1 ? "file" : "files"}...`);
  const data = new FormData();
  selected.forEach(file => data.append("files", file, file.name));

  try {
    const res = await fetch("/api/items/files", { method: "POST", body: data });
    if (!res.ok) throw new Error(await res.text());
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
  const text = els.textInput.value;
  const name = els.textName.value;
  if (!text.trim()) {
    toast("Add text first");
    return;
  }

  try {
    const res = await fetch("/api/items/text", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, text }),
    });
    if (!res.ok) throw new Error(await res.text());
    els.textInput.value = "";
    els.textName.value = "";
    await loadItems();
    toast("Text shared");
  } catch (err) {
    toast(cleanError(err));
  }
}

async function copyItemText(id) {
  try {
    let text = "";
    if (state.activeItem && state.activeItem.id === id && state.activePreviewText) {
      text = state.activePreviewText;
    } else {
      const res = await fetch(`/api/items/${encodeURIComponent(id)}/raw`);
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

els.fileInput.addEventListener("change", event => uploadFiles(event.target.files));
els.textForm.addEventListener("submit", shareText);
els.backButton.addEventListener("click", openHome);
window.addEventListener("popstate", renderRoute);

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

els.dropzone.addEventListener("drop", event => uploadFiles(event.dataTransfer.files));

setInterval(() => {
  renderItems();
  if (state.activeItem) renderMeta(els.detailMeta, state.activeItem);
}, 1000);

loadItems()
  .then(renderRoute)
  .then(() => setStatus("Live", "live"))
  .catch(() => setStatus("Offline", "offline"));
connectEvents();
