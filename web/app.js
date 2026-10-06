import { Upload } from "tus-js-client";
const $ = (selector) => document.querySelector(selector);
const csrf = $('meta[name="csrf-token"]')?.content;
const page = document.body.dataset.page;
const adminSurface = document.body.dataset.adminSurface === "true";
const allTransfers = document.body.dataset.allTransfers === "true";
const personalBase = adminSurface ? "/admin/api/personal" : "/api";
const uploadPath = adminSurface ? "/admin/upload" : "/upload";
const uploadsBase = adminSurface ? "/admin/uploads/" : "/uploads/";
let dateFormat = document.body.dataset.dateFormat || "dd/mm/yyyy";
const transferBase = allTransfers ? "/admin/api/transfers" : `${personalBase}/transfers`;
const bytes = (n) => {
  if (n < 1024) return `${n} B`;
  for (const unit of ["KiB", "MiB", "GiB", "TiB"]) {
    n /= 1024;
    if (n < 1024) return `${n.toFixed(1)} ${unit}`;
  }
  return "Large";
};
let noticeTimer, noticeHideTimer;
function hideNotice() {
  clearTimeout(noticeTimer);
  clearTimeout(noticeHideTimer);
  const node = $("#notice");
  node.hidden = true;
  node.textContent = "";
  node.classList.remove("dismissing");
}
function notice(message, error = false) {
  hideNotice();
  const n = $("#notice");
  n.textContent = message;
  n.classList.toggle("error", error);
  n.hidden = false;
  noticeTimer = setTimeout(() => {
    n.classList.add("dismissing");
    noticeHideTimer = setTimeout(hideNotice, 250);
  }, 4000);
}
function fail(error) {
  notice(error.message || "Something went wrong. Please retry.", true);
}
async function api(path, method = "GET", value) {
  const response = await fetch(path, { method, credentials: "same-origin", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf }, body: value === void 0 ? void 0 : JSON.stringify(value) });
  const content = await response.text();
  let result;
  try {
    result = JSON.parse(content);
  } catch {
    result = { error: content.trim() };
  }
  if (!response.ok) {
    if (response.status === 401 && page !== "login" && page !== "public") throw new Error("Your session expired. Sign in again.");
    throw new Error(result.error || `Request failed (${response.status}).`);
  }
  return result;
}
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== void 0) node.textContent = text;
  return node;
}
function icon(name) {
  const paths = {
    copy: ["M9 9h11v11H9z", "M15 9V4H4v11h5"],
    close: ["M6 6l12 12M6 18L18 6"],
    moon: ["M21 13a9 9 0 1 1-10-10 7 7 0 0 0 10 10Z"],
    sun: ["M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0Z", "M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5"],
  };
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  for (const [key, value] of Object.entries({ viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": "1.6", "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", focusable: "false" })) svg.setAttribute(key, value);
  for (const d of paths[name]) {
    const path = document.createElementNS(svg.namespaceURI, "path");
    path.setAttribute("d", d);
    svg.append(path);
  }
  return svg;
}
document.querySelectorAll("[data-icon]").forEach((button) => button.append(icon(button.dataset.icon)));
const systemTheme = matchMedia("(prefers-color-scheme: dark)");
let selectedTheme;
const themeURL = new URL(location.href);
const incomingTheme = themeURL.searchParams.get("theme");
try {
  const saved = incomingTheme === "light" || incomingTheme === "dark" ? incomingTheme : localStorage.getItem("filemind-theme");
  if (saved === "light" || saved === "dark") selectedTheme = saved;
  if (incomingTheme === "light" || incomingTheme === "dark") localStorage.setItem("filemind-theme", incomingTheme);
} catch {}
if (incomingTheme === "light" || incomingTheme === "dark") {
  selectedTheme = incomingTheme;
  themeURL.searchParams.delete("theme");
  history.replaceState(null, "", themeURL.pathname + themeURL.search + themeURL.hash);
}
function applyTheme() {
  if (selectedTheme) document.documentElement.dataset.theme = selectedTheme;
  const dark = selectedTheme ? selectedTheme === "dark" : systemTheme.matches;
  document.querySelectorAll("[data-app-link]").forEach(link => {
    const destination = new URL(link.href);
    if (destination.origin !== location.origin) {
      destination.searchParams.set("theme", dark ? "dark" : "light");
      link.href = destination.href;
    }
  });
  const button = $("#theme-toggle");
  if (!button) return;
  button.replaceChildren(icon(dark ? "sun" : "moon"));
  button.title = dark ? "Switch to light theme" : "Switch to dark theme";
  button.setAttribute("aria-label", button.title);
}
$("#theme-toggle")?.addEventListener("click", () => {
  selectedTheme = (selectedTheme ? selectedTheme === "dark" : systemTheme.matches) ? "light" : "dark";
  try { localStorage.setItem("filemind-theme", selectedTheme); } catch {}
  applyTheme();
});
systemTheme.addEventListener("change", applyTheme);
applyTheme();
function openQR(transferID) {
  $("#qr-image").src = `${transferBase}/${encodeURIComponent(transferID)}/qr.png`;
  $("#qr-dialog").showModal();
}
$("#close-qr")?.addEventListener("click", () => $("#qr-dialog").close());
function action(label, callback, style = "secondary") {
  const button = el("button", style, label);
  button.type = "button";
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      await callback();
    } catch (e) {
      fail(e);
    } finally {
      button.disabled = false;
    }
  });
  return button;
}
function copyAction(url) {
  const button = action("Copy link", () => copy(url), "icon-button secondary");
  button.replaceChildren(icon("copy"));
  button.setAttribute("aria-label", "Copy link");
  button.title = "Copy link";
  return button;
}
async function copy(value) {
  try {
    await navigator.clipboard.writeText(value);
    notice("Link copied.");
  } catch {
    window.prompt("Copy this link:", value);
  }
}
function time(timestamp) {
  const value = new Date(timestamp * 1e3);
  const day = String(value.getDate()).padStart(2, "0");
  const month = String(value.getMonth() + 1).padStart(2, "0");
  const year = value.getFullYear();
  const date = dateFormat === "yyyy-mm-dd" ? `${year}-${month}-${day}` : dateFormat === "mm/dd/yyyy" ? `${month}/${day}/${year}` : `${day}/${month}/${year}`;
  return `${date}, ${value.toLocaleTimeString()}`;
}
document.querySelectorAll("time[data-timestamp]").forEach((node) => {
  node.textContent = time(Number(node.dataset.timestamp));
});
$("#logout")?.addEventListener("click", async () => {
  try {
    await api("/logout", "POST");
    location.assign("/login");
  } catch (e) {
    fail(e);
  }
});
$("#login-form")?.addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const button = form.querySelector("button");
  button.disabled = true;
  try {
    await api("/login", "POST", { username: form.elements.username.value, password: form.elements.password.value });
    form.elements.password.value = "";
    location.assign(adminSurface ? "/admin/users" : "/upload");
  } catch (e) {
    fail(e);
  } finally {
    button.disabled = false;
  }
});
$("#unlock-form")?.addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const button = form.querySelector("button");
  button.disabled = true;
  try {
    await api(`${location.pathname}/unlock`, "POST", { password: form.elements.password.value });
    form.elements.password.value = "";
    location.reload();
  } catch (e) {
    fail(e);
  } finally {
    button.disabled = false;
  }
});
const fileDigests = new WeakMap();
function conditionalFields(form) {
  const groups = [...form.querySelectorAll("[data-toggle]")];
  function sync() {
    for (const group of groups) {
      const checkbox = form.elements[group.dataset.toggle];
      group.hidden = !checkbox.checked;
      for (const field of group.querySelectorAll("input, select")) field.disabled = checkbox.disabled || !checkbox.checked;
    }
  }
  for (const group of groups) form.elements[group.dataset.toggle].addEventListener("change", sync);
  sync();
  return sync;
}
function digestFile(file) {
  if (fileDigests.has(file)) return fileDigests.get(file);
  const result = new Promise((resolve, reject) => {
    const worker = new Worker("/assets/hash-worker.js");
    worker.onmessage = ({data}) => {
      if (data.digest || data.error) {
        worker.terminate();
        if (data.error) reject(new Error(data.error));
        else resolve(data.digest);
      }
    };
    worker.onerror = () => { worker.terminate(); reject(new Error("Cannot verify this file. Refresh and retry.")); };
    worker.postMessage(file);
  });
  fileDigests.set(file,result);
  result.catch(() => fileDigests.delete(file));
  return result;
}
async function uploadPage() {
  const config = await api(`${personalBase}/config`);
  const form = $("#upload-form");
  form.elements.expiryEnabled.checked = config.expirySeconds > 0;
  form.elements.expiryValue.value = config.expirySeconds ? config.expirySeconds / 86400 : 1;
  form.elements.expiryUnit.value = "86400";
  form.elements.limitEnabled.checked = config.downloadLimit > 0;
  form.elements.downloadLimit.value = config.downloadLimit || 1;
  let chosen = [], draft = null, job = null, paused = false, running = false, cancelled = false;
  const states = new Map();
  const resumeID = new URLSearchParams(location.search).get("resume");
  if (resumeID) {
    draft = await api(`${transferBase}/${encodeURIComponent(resumeID)}`);
    if (draft.status !== "draft") throw new Error("This transfer is no longer a resumable draft.");
    form.elements.title.value = draft.title;
    form.elements.expiryEnabled.checked = draft.expirySeconds > 0;
    form.elements.expiryValue.value = draft.expirySeconds / 3600 || 24;
    form.elements.expiryUnit.value = "3600";
    form.elements.limitEnabled.checked = draft.downloadLimit > 0;
    form.elements.downloadLimit.value = draft.downloadLimit || 1;
    form.hidden = false;
    $("#resume-draft").hidden = false;
    $("#upload-submit").hidden = true;
    for (const f of draft.files) states.set(f.id, { name: f.name, size: f.size, percent: f.uploaded ? 100 : 0, label: f.uploaded ? "Uploaded" : "Reselect this file to resume" });
    notice("Reselect unfinished files to resume.");
  }
  const syncUploadFields = conditionalFields(form);
  const progressViews = new Map();
  const pendingProgress = new Set();
  let progressFrame;
  function updateProgress(state) {
    const view = progressViews.get(state);
    if (!view) return;
    view.status.textContent = `${bytes(state.size)} · ${state.label}`;
    view.progress.value = state.percent;
  }
  function queueProgress(state) {
    pendingProgress.add(state);
    if (progressFrame) return;
    progressFrame = requestAnimationFrame(() => {
      progressFrame = null;
      for (const state of pendingProgress) updateProgress(state);
      pendingProgress.clear();
    });
  }
  function renderFiles() {
    progressViews.clear();
    const list = $("#selected-files");
    list.replaceChildren();
    const entries = draft ? [...states.values()] : chosen.map((f) => ({ name: f.name, size: f.size, percent: 0, label: "Selected" }));
    for (const f of entries) {
      const li = el("li");
      const detail = el("div", "file-description");
      const status = el("span", "small muted", `${bytes(f.size)} · ${f.label}`);
      detail.append(el("strong", "", f.name), status);
      if (draft) {
        const progress = el("progress");
        progress.max = 100;
        progress.value = f.percent;
        progress.setAttribute("aria-label", `Upload progress for ${f.name}`);
        detail.append(progress);
        progressViews.set(f, {status,progress});
      }
      li.append(detail);
      list.append(li);
    }
  }
  function select(files) {
    if (running) return;
    const selected = [...files];
    if (!selected.length) return;
    if (selected.length > 100 || (draft ? selected.some(f => !draft.files.some(saved => saved.name === f.name && saved.size === f.size)) : selected.some((f) => f.size > config.maxFileSize) || selected.reduce((n, f) => n + f.size, 0) > config.maxTransferSize)) {
      notice("The selected files exceed the upload limits.", true);
      return;
    }
    chosen = selected;
    if (!draft) form.elements.title.placeholder = chosen[0].name + (chosen.length > 1 ? ` + ${chosen.length - 1} more` : "");
    form.hidden = false;
    renderFiles();
  }
  $("#file-picker").addEventListener("change", (event) => select(event.target.files));
  const drop = $("#dropzone");
  for (const event of ["dragenter", "dragover"]) drop.addEventListener(event, (e) => {
    e.preventDefault();
    drop.classList.add("drag");
  });
  for (const event of ["dragleave", "drop"]) drop.addEventListener(event, (e) => {
    e.preventDefault();
    drop.classList.remove("drag");
  });
  drop.addEventListener("drop", (event) => select(event.dataTransfer.files));
  function controls(active) {
    running = active;
    $("#file-picker").disabled = active;
    $("#upload-submit").disabled = active;
    $("#pause-upload").hidden = !active || !draft;
    $("#cancel-upload").hidden = !draft;
    $("#resume-draft").disabled = active;
    $("#pause-upload").textContent = paused ? "Resume" : "Pause";
    for (const name of ["title", "password", "expiryEnabled", "expiryValue", "expiryUnit", "limitEnabled", "downloadLimit"]) form.elements[name].disabled = active || !!draft;
    syncUploadFields();
  }
  async function run() {
    if (running) return;
    paused = false;
    cancelled = false;
    controls(true);
    try {
      if (!draft) {
        if (!chosen.length) throw new Error("Choose at least one file.");
        const expiry = form.elements.expiryEnabled.checked ? Math.round(Number(form.elements.expiryValue.value) * Number(form.elements.expiryUnit.value)) : 0;
        const limit = form.elements.limitEnabled.checked ? Number(form.elements.downloadLimit.value) : 0;
        if (!Number.isSafeInteger(expiry) || expiry < 0 || !Number.isSafeInteger(limit) || limit < 0) throw new Error("Check the transfer settings.");
        const files = [];
        for (const file of chosen) {
          notice(`Checking ${file.name} before upload…`);
          files.push({ name: file.name, size: file.size, sha256: await digestFile(file) });
        }
        draft = await api(transferBase, "POST", { title: form.elements.title.value, password: form.elements.password.value, expirySeconds: expiry, downloadLimit: limit, files });
        form.elements.password.value = "";
        history.replaceState(null, "", `/upload?resume=${draft.id}`);
      } else {
        draft = await api(`${transferBase}/${draft.id}`);
      }
      const pending = draft.files.filter((f) => !f.uploaded);
      const fileMap = new Map();
      const remaining = [...chosen];
      for (const serverFile of pending) {
        const index = remaining.findIndex((f) => f.name === serverFile.name && f.size === serverFile.size);
        if (index < 0) throw new Error(`Reselect ${serverFile.name} (${bytes(serverFile.size)}) to resume.`);
        const file = remaining.splice(index, 1)[0];
        notice(`Checking ${file.name} before resuming…`);
        const sha256 = await digestFile(file);
        if (sha256 !== serverFile.sha256) throw new Error(`Selected file does not match the original: ${file.name}. Reselect the original file.`);
        fileMap.set(serverFile.id,file);
      }
      for (const f of draft.files) states.set(f.id, { name: f.name, size: f.size, percent: f.uploaded ? 100 : 0, label: f.uploaded ? "Uploaded" : "Queued" });
      hideNotice();
      controls(true);
      $("#resume-draft").hidden = true;
      renderFiles();
      for (const serverFile of pending) {
        if (cancelled) return;
        await new Promise((resolve, reject) => {
          const state = states.get(serverFile.id);
          const finish = (error) => {
            job = null;
            if (error) reject(error);
            else resolve();
          };
          const up = new Upload(fileMap.get(serverFile.id), {
            endpoint: uploadsBase,
            uploadUrl: serverFile.started ? `${uploadsBase}${serverFile.id}` : void 0,
            chunkSize: 8 * 1024 * 1024,
            retryDelays: [0, 1e3, 3e3, 5e3],
            metadata: { file_id: serverFile.id },
            headers: { "X-CSRF-Token": csrf },
            storeFingerprintForResuming: false,
            onBeforeRequest: (req) => {
              req.getUnderlyingObject().withCredentials = true;
            },
            onProgress: (sent, total) => {
              state.percent = total ? Math.min(99, Math.round(sent / total * 100)) : 0;
              state.label = `${state.percent}%`;
              queueProgress(state);
            },
            onSuccess: () => {
              state.percent = 100;
              state.label = "Uploaded";
              updateProgress(state);
              finish();
            },
            onError: (error) => {
              state.label = "Interrupted — resume from Transfers";
              updateProgress(state);
              finish(new Error(error.originalResponse?.getStatus() === 401 ? "Your session expired. Sign in again and resume from Transfers." : `Upload interrupted: ${serverFile.name}. Resume from Transfers to retry.`));
            }
          });
          job = { up, finish };
          state.label = "Uploading";
          updateProgress(state);
          if (!paused) up.start();
        });
      }
      if (cancelled) return;
      const shared = await api(`${transferBase}/${draft.id}/publish`, "POST", {});
      $("#share-url").value = shared.shareUrl;
      $("#share-result").hidden = false;
      form.hidden = true;
      drop.hidden = true;
      $("#selected-files").hidden = true;
      history.replaceState(null, "", "/upload");
      $("#upload-heading").textContent = "Share";
      hideNotice();
    } catch (e) {
      if (job) {
        const interrupted = job;
        await interrupted.up.abort(false);
        interrupted.finish(e);
      }
      if (!cancelled) notice(e.message, true);
      if (draft) {
        $("#resume-draft").hidden = false;
        $("#upload-submit").hidden = true;
      }
    } finally {
      controls(false);
    }
  }
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    run().catch(fail);
  });
  $("#resume-draft").addEventListener("click", () => run().catch(fail));
  $("#pause-upload").addEventListener("click", async (event) => {
    const button = event.currentTarget;
    button.disabled = true;
    paused = !paused;
    button.textContent = paused ? "Resume" : "Pause";
    try {
      if (paused) await job?.up.abort(false);
      else job?.up.start();
    } catch (e) {
      fail(e);
    } finally {
      button.disabled = false;
    }
  });
  $("#cancel-upload").addEventListener("click", async () => {
    if (!confirm("Cancel this transfer and delete its uploaded files?")) return;
    cancelled = true;
    if (job) {
      const stopped = job;
      await stopped.up.abort(false);
      stopped.finish(new Error("Transfer cancelled."));
    }
    try {
      await api(`${transferBase}/${draft.id}`, "DELETE");
      location.assign(uploadPath);
    } catch (e) {
      fail(e);
    }
  });
  $("#copy-share").addEventListener("click", () => copy($("#share-url").value));
  $("#show-share-qr").addEventListener("click", () => openQR(draft.id));
  $("#new-upload").addEventListener("click", () => location.assign(uploadPath));
  renderFiles();
  controls(false);
}
async function transfersPage() {
  let offset = 0, editing = null, debounce, generation = 0, expiryChanged = false;
  const container = $("#transfers"), dialog = $("#edit-dialog"), form = $("#edit-form");
  const syncEditFields = conditionalFields(form);
  async function load() {
    const request = ++generation;
    const [transfers, config] = await Promise.all([api(`${transferBase}?q=${encodeURIComponent($("#search").value)}&offset=${offset}`), api(allTransfers ? "/admin/api/settings" : `${personalBase}/config`)]);
    if (request !== generation) return;
    $("#storage").textContent = `${bytes(config.storageUsed)} / ${bytes(config.storageQuota)}`;
    container.replaceChildren();
    if (!transfers.length) container.append(el("div", "empty", "No transfers"));
    for (const t of transfers) {
      const card = el("article", "transfer-card"), heading = el("div", "row");
      heading.append(el("h2", "", t.title), el("span", "badge", t.status));
      card.append(heading);
      if (t.files.length > 1 || (t.files.length === 1 && t.files[0].name !== t.title)) {
        const names = t.files.slice(0, 3).map(file => file.name).join(" · ");
        card.append(el("p", "transfer-filenames small muted", names + (t.files.length > 3 ? ` · + ${t.files.length - 3} more` : "")));
      }
      const meta = el("div", "transfer-meta");
      meta.append(el("span", "", `${t.files.length} file${t.files.length === 1 ? "" : "s"} \xB7 ${bytes(t.files.reduce((n, f) => n + f.size, 0))}`));
      if (allTransfers) {
        const owner = el("span", "", "Owner: ");
        const link = el("a", "owner-link", t.ownerUsername);
        link.href = `/admin/users#user-${encodeURIComponent(t.ownerId)}`;
        owner.append(link);
        meta.append(owner);
      }
      if (t.expiresAt) meta.append(el("span", "", `Expires ${time(t.expiresAt)}`));
      if (t.downloadLimit) meta.append(el("span", "", `${t.downloadLimit} downloads per file`));
      if (t.passwordRequired) meta.append(el("span", "", "Password protected"));
      card.append(meta);
      const details = el("details");
      details.append(el("summary", "", "Files"));
      const list = el("ul", "file-list");
      for (const f of t.files) {
        const li = el("li");
        li.append(el("span", "", f.name), el("span", "small muted", f.deleted ? "Deleted" : `${f.downloads} downloads \xB7 ${f.uploaded ? bytes(f.size) : "Unfinished"}${f.inProgress ? " \xB7 downloading" : ""}`));
        if (f.integrityError) li.append(el("span","small danger",f.integrityError));
        if (t.status === "draft" && !allTransfers && (f.started || f.integrityError)) li.append(action("Restart file", async () => {
          if (!confirm(`Discard saved upload data for ${f.name}?`)) return;
          await api(`${transferBase}/${t.id}/files/${f.id}/restart`, "POST", {});
          await load();
        }));
        list.append(li);
      }
      details.append(list);
      card.append(details);
      const actions = el("div", "actions");
      if (t.shareUrl) {
        const qr = action("QR", () => openQR(t.id));
        qr.setAttribute("aria-label", "QR code");
        actions.append(copyAction(t.shareUrl), qr);
      }
      if (t.status === "draft" && !allTransfers) {
        const resume = el("a", "button secondary", "Resume upload");
        resume.href = `${uploadPath}?resume=${t.id}`;
        actions.append(resume);
      }
      if (t.status === "draft" || t.status === "published") actions.append(action("Edit", () => {
        editing = t;
        expiryChanged = false;
        $("#edit-error").hidden = true;
        form.elements.title.value = t.title;
        form.elements.expiryEnabled.checked = (t.status === "draft" ? t.expirySeconds : t.expiresAt) > 0;
        const hours = t.status === "draft" ? t.expirySeconds / 3600 || 24 : t.expiresAt ? (t.expiresAt - Date.now() / 1e3) / 3600 : 24;
        form.elements.expiryHours.value = Math.max(0.01, hours).toFixed(2);
        form.elements.limitEnabled.checked = t.downloadLimit > 0;
        form.elements.downloadLimit.value = t.downloadLimit || 1;
        form.elements.passwordAction.value = "keep";
        form.elements.password.value = "";
        form.elements.password.required = false;
        $("#edit-password-label").hidden = true;
        syncEditFields();
        dialog.showModal();
      }));
      if (t.shareUrl) actions.append(action("Revoke", async () => {
        if (confirm("Disable this link and stop active downloads? Files stay until expiry or deletion.")) {
          await api(`${transferBase}/${t.id}/revoke`, "POST", {});
          await load();
        }
      }, "quiet danger"));
      actions.append(action("Delete", async () => {
        if (confirm("Delete this transfer\u2019s stored files? This cannot be undone.")) {
          await api(`${transferBase}/${t.id}`, "DELETE");
          await load();
        }
      }, "quiet danger"));
      card.append(actions);
      container.append(card);
    }
    $("#previous").disabled = offset === 0;
    $("#next").disabled = transfers.length < 50;
  }
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector('button[type="submit"]');
    button.disabled = true;
    try {
      const input = { revision: editing.revision, title: form.elements.title.value, downloadLimit: form.elements.limitEnabled.checked ? Number(form.elements.downloadLimit.value) : 0 };
      if (expiryChanged) input.expirySeconds = form.elements.expiryEnabled.checked ? Math.round(Number(form.elements.expiryHours.value) * 3600) : 0;
      if (form.elements.passwordAction.value !== "keep") input.password = form.elements.passwordAction.value === "set" ? form.elements.password.value : "";
      await api(`${transferBase}/${editing.id}`, "PATCH", input);
      form.elements.password.value = "";
      dialog.close();
      await load();
      notice("Transfer updated.");
    } catch (e) {
      $("#edit-error").textContent = e.message;
      $("#edit-error").hidden = false;
    } finally {
      button.disabled = false;
    }
  });
  for (const name of ["expiryEnabled","expiryHours"]) form.elements[name].addEventListener("input", () => {expiryChanged=true;});
  form.elements.passwordAction.addEventListener("change", () => {
    $("#edit-password-label").hidden = form.elements.passwordAction.value !== "set";
    form.elements.password.required = form.elements.passwordAction.value === "set";
  });
  $("#close-edit").addEventListener("click", () => dialog.close());
  $("#search").addEventListener("input", () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      offset = 0;
      load().catch(fail);
    }, 250);
  });
  $("#previous").addEventListener("click", () => {
    offset = Math.max(0, offset - 50);
    load().catch(fail);
  });
  $("#next").addEventListener("click", () => {
    offset += 50;
    load().catch(fail);
  });
  await load();
}
async function usersPage() {
  const container = $("#users"), dialog = $("#user-dialog"), form = $("#user-form");
  let editing = null, defaults;
  let highlighted, highlightTimer;
  function highlightUser() {
    clearTimeout(highlightTimer);
    highlighted?.classList.remove("user-highlight");
    if (!location.hash.startsWith("#user-")) return;
    highlighted = document.getElementById(location.hash.slice(1));
    if (!highlighted) { notice("This user is no longer available.", true); return; }
    highlighted.classList.add("user-highlight");
    highlighted.focus({preventScroll: true});
    highlighted.scrollIntoView({block: "center", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "instant" : "smooth"});
    highlightTimer = setTimeout(() => highlighted.classList.remove("user-highlight"), 1000);
  }
  function edit(user = null) {
    editing = user;
    form.reset();
    $("#user-error").hidden = true;
    $("#user-dialog-title").textContent = user ? "Edit user" : "Add user";
    form.elements.username.value = user?.username || "";
    form.elements.username.readOnly = !!user;
    form.elements.password.required = !user;
    form.elements.password.placeholder = user ? "Leave blank to keep current" : "";
    syncCurrentPassword();
    const quota = user ? user.storageQuota : defaults.defaultUserQuota;
    form.elements.quotaEnabled.checked = quota > 0;
    form.elements.quotaGiB.value = (quota || defaults.defaultUserQuota || 1073741824) / 1073741824;
    form.elements.quotaGiB.disabled = !form.elements.quotaEnabled.checked;
    dialog.showModal();
  }
  function syncCurrentPassword() {
    const required = !!editing?.isAdmin && !!form.elements.password.value;
    $("#user-current-password").hidden = !required;
    form.elements.currentPassword.disabled = !required;
    form.elements.currentPassword.required = required;
    if (!required) form.elements.currentPassword.value = "";
  }
  form.elements.password.addEventListener("input", syncCurrentPassword);
  async function load() {
    const [users, settings] = await Promise.all([api("/admin/api/users"), api("/admin/api/settings")]);
    defaults = settings;
    container.replaceChildren();
    for (const user of users) {
      const card = el("article", "transfer-card"), heading = el("div", "row");
      card.id = `user-${user.id}`;
      card.dataset.userId = user.id;
      card.tabIndex = -1;
      heading.append(el("h2", "", user.username));
      if (user.isAdmin || user.disabled) heading.append(el("span", "badge", user.isAdmin ? "Admin" : "Disabled"));
      card.append(heading);
      const usage = el("span", "small muted", user.storageQuota ? `${bytes(user.storageUsed)} / ${bytes(user.storageQuota)}` : bytes(user.storageUsed));
      usage.title = user.storageQuota ? "Stored and reserved space / account quota" : "Stored and reserved space; shared server limit applies";
      card.append(usage);
      const actions = el("div", "actions");
      actions.append(action("Edit", () => edit(user)));
      if (!user.isAdmin) actions.append(action(user.disabled ? "Enable" : "Disable", async () => {
        if (!user.disabled && !confirm(`Disable ${user.username}? Their sessions and active uploads will stop.`)) return;
        await api(`/admin/api/users/${user.id}`, "PATCH", { disabled: !user.disabled });
        await load();
      }, user.disabled ? "secondary" : "quiet danger"));
      if (!user.isAdmin) actions.append(action("Delete", async () => {
        if (!confirm(`Delete ${user.username} and all their transfers? Stored files will be removed and published links will stop.`)) return;
        await api(`/admin/api/users/${user.id}`, "DELETE");
        await load();
      }, "quiet danger"));
      card.append(actions);
      container.append(card);
    }
  }
  form.elements.quotaEnabled.addEventListener("change", () => {
    form.elements.quotaGiB.disabled = !form.elements.quotaEnabled.checked;
  });
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector('button[type="submit"]');
    button.disabled = true;
    try {
      const storageQuota = form.elements.quotaEnabled.checked ? Math.round(Number(form.elements.quotaGiB.value) * 1073741824) : 0;
      if (!Number.isSafeInteger(storageQuota) || storageQuota < 0 || (form.elements.quotaEnabled.checked && storageQuota === 0)) throw new Error("Enter a valid storage quota.");
      const input = { storageQuota };
      const target = editing;
      $("#user-error").hidden = true;
      if (!target) input.username = form.elements.username.value;
      const password = form.elements.password.value;
      if (!target || password) input.password = password;
      if (target?.isAdmin && password) input.currentPassword = form.elements.currentPassword.value;
      await api(target ? `/admin/api/users/${target.id}` : "/admin/api/users", target ? "PATCH" : "POST", input);
      form.elements.password.value = "";
      form.elements.currentPassword.value = "";
      dialog.close();
      if (target?.id === document.body.dataset.userId && password) {
        location.assign("/login");
        return;
      }
      await load();
      notice(target ? "Account updated." : "Account created.");
    } catch (e) {
      if (dialog.open) {
        $("#user-error").textContent = e.message;
        $("#user-error").hidden = false;
      } else fail(e);
    } finally {
      button.disabled = false;
    }
  });
  dialog.addEventListener("close", () => { form.elements.password.value = ""; form.elements.currentPassword.value = ""; });
  $("#close-user").addEventListener("click", () => dialog.close());
  $("#new-user").addEventListener("click", () => { if (defaults) edit(); });
  await load();
  window.addEventListener("hashchange", highlightUser);
  highlightUser();
}
async function settingsPage() {
  const form = $("#settings-form");
  const settings = await api("/admin/api/settings");
  const fields = { defaultUserQuotaGiB: "defaultUserQuota", maxFileSizeGiB: "maxFileSize", maxTransferSizeGiB: "maxTransferSize", storageQuotaGiB: "storageQuota" };
  for (const [name, key] of Object.entries(fields)) form.elements[name].value = settings[key] / 1073741824;
  form.elements.expiryHours.value = settings.expirySeconds / 3600;
  form.elements.downloadLimit.value = settings.downloadLimit;
  $("#settings-storage").textContent = `${bytes(settings.storageUsed)} reserved`;
  const maintenance=settings.maintenance;
  $("#maintenance-status").textContent = `${bytes(maintenance.freeSpace)} disk space free · ${maintenance.purgeBacklog} pending file removals · Last cleanup ${time(maintenance.lastCleanup)}${maintenance.cleanupFailed ? " · Cleanup needs attention" : ""}`;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector('button[type="submit"]');
    button.disabled = true;
    try {
      const input = {};
      for (const [name, key] of Object.entries(fields)) input[key] = Math.round(Number(form.elements[name].value) * 1073741824);
      input.expirySeconds = Math.round(Number(form.elements.expiryHours.value) * 3600);
      input.downloadLimit = Number(form.elements.downloadLimit.value);
      if (Object.values(input).some((value) => !Number.isSafeInteger(value) || value < 0)) throw new Error("Enter valid settings.");
      await api("/admin/api/settings", "PUT", input);
      notice("Settings saved.");
    } catch (e) {
      fail(e);
    } finally {
      button.disabled = false;
    }
  });
}
if (page === "settings") settingsPage().catch(fail);
if (page === "users") usersPage().catch(fail);
if (page === "upload") uploadPage().catch(fail);
if (page === "transfers") transfersPage().catch(fail);

$("#preferences-form")?.addEventListener("submit", async event => {
  event.preventDefault();
  const form = event.currentTarget, button = form.querySelector("button");
  button.disabled = true;
  try {
    const preferences = await api(adminSurface ? "/admin/api/preferences" : "/api/preferences", "PUT", {dateFormat: form.elements.dateFormat.value});
    dateFormat = preferences.dateFormat;
    notice("Settings saved.");
  } catch (error) { fail(error); } finally { button.disabled = false; }
});

$("#password-form")?.addEventListener("submit", async event => {
 event.preventDefault();
 const form=event.currentTarget, button=form.querySelector("button");button.disabled=true;
 try {
  await api(adminSurface ? "/admin/api/password" : "/api/password", "POST", {currentPassword:form.elements.currentPassword.value,newPassword:form.elements.newPassword.value});
  form.reset();location.assign("/login");
 } catch(error) {fail(error);} finally {button.disabled=false;}
});
