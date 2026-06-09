const state = {
  items: [],
  busy: false,
};

const els = {
  connectionStatus: document.querySelector("#connectionStatus"),
  dropzone: document.querySelector("#dropzone"),
  fileInput: document.querySelector("#fileInput"),
  textForm: document.querySelector("#textForm"),
  textName: document.querySelector("#textName"),
  textInput: document.querySelector("#textInput"),
  items: document.querySelector("#items"),
  itemCount: document.querySelector("#itemCount"),
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
  meta.append(metaPart(formatBytes(item.size)), metaPart(timeLeft(item.expiresAt)));

  main.append(title, meta);

  const actions = document.createElement("div");
  actions.className = "item-actions";

  if (item.kind === "text") {
    const copy = document.createElement("button");
    copy.className = "secondary";
    copy.type = "button";
    copy.textContent = "Copy";
    copy.addEventListener("click", () => copyText(item.id));
    actions.append(copy);
  }

  const download = document.createElement("a");
  download.className = "button-link secondary";
  download.href = `/api/items/${encodeURIComponent(item.id)}/download`;
  download.textContent = "Download";
  actions.append(download);

  const del = document.createElement("button");
  del.className = "danger";
  del.type = "button";
  del.textContent = "Delete";
  del.addEventListener("click", () => deleteItem(item.id));
  actions.append(del);

  row.append(main, actions);
  return row;
}

function metaPart(text) {
  const span = document.createElement("span");
  span.textContent = text;
  return span;
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

async function copyText(id) {
  try {
    const res = await fetch(`/api/items/${encodeURIComponent(id)}/raw`);
    if (!res.ok) throw new Error(await res.text());
    const text = await res.text();
    await navigator.clipboard.writeText(text);
    toast("Copied");
  } catch (err) {
    toast(cleanError(err));
  }
}

async function deleteItem(id) {
  try {
    const res = await fetch(`/api/items/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) throw new Error(await res.text());
    state.items = state.items.filter(item => item.id !== id);
    renderItems();
    toast("Deleted");
  } catch (err) {
    toast(cleanError(err));
  }
}

function cleanError(err) {
  const message = err && err.message ? err.message.trim() : "Something went wrong";
  return message.length > 140 ? `${message.slice(0, 137)}...` : message;
}

function connectEvents() {
  const events = new EventSource("/api/events");
  events.addEventListener("connected", () => setStatus("Live", "live"));
  events.addEventListener("items_changed", () => loadItems().catch(() => setStatus("Offline", "offline")));
  events.onerror = () => setStatus("Reconnecting", "offline");
}

els.fileInput.addEventListener("change", event => uploadFiles(event.target.files));
els.textForm.addEventListener("submit", shareText);

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

setInterval(renderItems, 1000);
loadItems().then(() => setStatus("Live", "live")).catch(() => setStatus("Offline", "offline"));
connectEvents();
