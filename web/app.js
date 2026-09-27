"use strict";

const ALLOWED = [".mp3", ".m4a", ".m4b", ".zip"];

const HELP = {
  no_device: "Connect your iPhone to this computer with its charging cable.",
  not_trusted: "Unlock your iPhone. When it asks “Trust This Computer?”, tap Trust and enter your passcode.",
  locked: "Unlock your iPhone with your passcode, then wait a moment.",
  no_bookplayer: "",
  no_service: "",
  error: "",
};

const $ = (id) => document.getElementById(id);
const els = {
  status: $("status"), statusText: $("status-text"), help: $("status-help"),
  drop: $("drop"), pick: $("pick"), file: $("file"),
  pickFolder: $("pick-folder"), folder: $("folder"),
  progress: $("progress"), progressText: $("progress-text"), barFill: $("bar-fill"),
  bar: document.querySelector(".bar"), progressFile: $("progress-file"),
  done: $("done"), again: $("again"),
  error: $("error"), errorText: $("error-text"), retry: $("retry"),
};

let ready = false;
let sending = false;
let pending = [];   // [{file, rel}] for the current batch
let nextIndex = 0;  // first file not yet sent (for "Try again")

// ---- Device status (Server-Sent Events; the open stream is also the heartbeat)

function showStatus(st) {
  ready = st.state === "ready";
  els.status.className = "status " + (ready ? "ok" : st.state === "no_device" || st.state === "not_trusted" || st.state === "locked" ? "wait" : "bad");
  els.statusText.textContent = st.message;
  const help = ready ? (st.device ? `${st.device} is ready.` : "") : (HELP[st.state] || "");
  els.help.textContent = help;
  els.help.hidden = !help;
  els.drop.classList.toggle("disabled", !ready || sending);
}

let lostTimer = null;
function connect() {
  const es = new EventSource("/api/events");
  es.onmessage = (e) => {
    clearTimeout(lostTimer);
    lostTimer = null;
    showStatus(JSON.parse(e.data));
  };
  es.onerror = () => {
    if (lostTimer) return;
    lostTimer = setTimeout(() => {
      showStatus({
        state: "stopped",
        message: "Send Books to iPhone has stopped",
      });
      els.help.textContent = "Close this tab and open Send Books to iPhone again.";
      els.help.hidden = false;
    }, 4000);
  };
}
connect();

// ---- Picking files

function allowed(name) {
  if (name.startsWith(".")) return false;
  const lower = name.toLowerCase();
  return ALLOWED.some((ext) => lower.endsWith(ext));
}

function readAllEntries(dirEntry) {
  const reader = dirEntry.createReader();
  const all = [];
  return new Promise((resolve, reject) => {
    const next = () => reader.readEntries((batch) => {
      if (batch.length === 0) return resolve(all);
      all.push(...batch);
      next();
    }, reject);
    next();
  });
}

async function walk(entry, out) {
  if (entry.isFile) {
    if (!allowed(entry.name)) return;
    const file = await new Promise((res, rej) => entry.file(res, rej));
    out.push({ file, rel: entry.fullPath.replace(/^\/+/, "") });
  } else if (entry.isDirectory) {
    if (entry.name.startsWith(".")) return;
    for (const child of await readAllEntries(entry)) await walk(child, out);
  }
}

async function filesFromDrop(dt) {
  // Entries must be grabbed synchronously, before any await.
  const entries = [];
  for (const item of dt.items || []) {
    const entry = item.webkitGetAsEntry && item.webkitGetAsEntry();
    if (entry) entries.push(entry);
  }
  const out = [];
  if (entries.length) {
    for (const e of entries) await walk(e, out);
  } else {
    for (const f of dt.files) if (allowed(f.name)) out.push({ file: f, rel: f.name });
  }
  return out;
}

// Stop the browser from opening files dropped outside the drop zone.
window.addEventListener("dragover", (e) => e.preventDefault());
window.addEventListener("drop", (e) => e.preventDefault());

els.drop.addEventListener("dragenter", (e) => { e.preventDefault(); if (ready && !sending) els.drop.classList.add("over"); });
els.drop.addEventListener("dragover", (e) => { e.preventDefault(); e.dataTransfer.dropEffect = ready && !sending ? "copy" : "none"; });
els.drop.addEventListener("dragleave", (e) => { if (!els.drop.contains(e.relatedTarget)) els.drop.classList.remove("over"); });
els.drop.addEventListener("drop", async (e) => {
  e.preventDefault();
  els.drop.classList.remove("over");
  if (!ready || sending) return;
  start(await filesFromDrop(e.dataTransfer));
});

els.pick.addEventListener("click", () => { if (ready && !sending) els.file.click(); });
els.drop.addEventListener("keydown", (e) => {
  if ((e.key === "Enter" || e.key === " ") && e.target === els.drop) { e.preventDefault(); els.pick.click(); }
});
els.file.addEventListener("change", () => {
  const files = [...els.file.files].filter((f) => allowed(f.name)).map((f) => ({ file: f, rel: f.name }));
  els.file.value = "";
  start(files);
});

els.pickFolder.addEventListener("click", () => { if (ready && !sending) els.folder.click(); });
els.folder.addEventListener("change", () => {
  // webkitRelativePath is "Folder/sub/file.mp3", so the folder structure is kept.
  // Skip anything inside hidden folders, same as dropping a folder.
  const files = [...els.folder.files]
    .map((f) => ({ file: f, rel: f.webkitRelativePath || f.name }))
    .filter((it) => allowed(it.file.name) && !it.rel.split("/").some((part) => part.startsWith(".")));
  els.folder.value = "";
  start(files);
});

els.again.addEventListener("click", () => { els.done.hidden = true; els.drop.hidden = false; });
els.retry.addEventListener("click", () => { els.error.hidden = true; send(); });

// ---- Sending

function start(files) {
  if (!files.length) {
    showError("Those don't look like audiobooks. Try .mp3, .m4a, .m4b or .zip files, or a folder of them.", false);
    return;
  }
  pending = files;
  nextIndex = 0;
  send();
}

function showError(msg, canRetry) {
  els.errorText.textContent = msg;
  els.retry.hidden = !canRetry;
  els.error.hidden = false;
  if (!canRetry) setTimeout(() => { if (!sending) els.error.hidden = true; }, 6000);
}

function mb(bytes) { return (bytes / 1e6).toFixed(bytes < 1e8 ? 1 : 0) + " MB"; }

function setBar(fraction) {
  const pct = Math.max(0, Math.min(100, fraction * 100));
  els.barFill.style.width = pct + "%";
  els.bar.setAttribute("aria-valuenow", Math.round(pct));
}

function uploadOne(item, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", "/api/upload?path=" + encodeURIComponent(item.rel));
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg = "Something went wrong sending “" + item.rel + "”.";
      try { msg = JSON.parse(xhr.responseText).error || msg; } catch (_) {}
      reject(new Error(msg));
    };
    xhr.onerror = () => reject(new Error("Lost contact with Send Books to iPhone. Is it still running?"));
    xhr.send(item.file);
  });
}

async function send() {
  sending = true;
  els.error.hidden = true;
  els.done.hidden = true;
  els.drop.hidden = true;
  els.progress.hidden = false;
  els.drop.classList.add("disabled");

  const total = pending.reduce((n, it) => n + it.file.size, 0) || 1;
  let sent = pending.slice(0, nextIndex).reduce((n, it) => n + it.file.size, 0);
  let failed = null;

  for (; nextIndex < pending.length; nextIndex++) {
    const item = pending[nextIndex];
    els.progressText.textContent = pending.length === 1
      ? "Sending your book…"
      : `Sending book ${nextIndex + 1} of ${pending.length}…`;
    els.progressFile.textContent = `${item.rel} (${mb(item.file.size)})`;
    try {
      await uploadOne(item, (loaded) => setBar((sent + loaded) / total));
    } catch (err) {
      failed = err;
      break;
    }
    sent += item.file.size;
    setBar(sent / total);
  }

  await fetch("/api/done", { method: "POST" }).catch(() => {});
  sending = false;
  els.progress.hidden = true;
  els.drop.classList.toggle("disabled", !ready);

  if (failed) {
    els.drop.hidden = false;
    showError(failed.message, true);
  } else {
    setBar(0);
    els.done.hidden = false;
  }
}

window.addEventListener("beforeunload", (e) => {
  if (sending) { e.preventDefault(); e.returnValue = ""; }
});
