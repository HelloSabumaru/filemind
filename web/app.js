import { Upload } from "tus-js-client";
const $ = (selector) => document.querySelector(selector);
const csrf = $('meta[name="csrf-token"]')?.content;
const page = document.body.dataset.page;
const bytes = (n) => {
  if (n < 1024) return `${n} B`;
  for (const unit of ["KiB", "MiB", "GiB", "TiB"]) {
    n /= 1024;
    if (n < 1024) return `${n.toFixed(1)} ${unit}`;
  }
  return "Large";
};
function notice(message, error = false) {
  const n = $("#notice");
  n.textContent = message;
  n.classList.toggle("error", error);
  n.hidden = false;
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
    if (response.status === 401 && page !== "login" && page !== "public") throw new Error("Your session expired. Sign in again, then resume the transfer.");
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
try {
  const saved = localStorage.getItem("filemind-theme");
  if (saved === "light" || saved === "dark") selectedTheme = saved;
} catch {}
function applyTheme() {
  if (selectedTheme) document.documentElement.dataset.theme = selectedTheme;
  const dark = selectedTheme ? selectedTheme === "dark" : systemTheme.matches;
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
  $("#qr-image").src = `/admin/api/transfers/${encodeURIComponent(transferID)}/qr.png`;
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
  return new Date(timestamp * 1e3).toLocaleString();
}
document.querySelectorAll("time[data-timestamp]").forEach((node) => {
  node.textContent = time(Number(node.dataset.timestamp));
});
$("#logout")?.addEventListener("click", async () => {
  try {
    await api("/admin/logout", "POST");
    location.assign("/admin/login");
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
    await api("/admin/login", "POST", { username: form.elements.username.value, password: form.elements.password.value });
    form.elements.password.value = "";
    location.assign("/admin");
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
document.querySelectorAll(".download-link").forEach((link) => {
  link.addEventListener("click", async (event) => {
    event.preventDefault();
    try {
      const response = await fetch(link.href, { method: "HEAD", credentials: "same-origin" });
      if (!response.ok) throw new Error(response.status === 401 ? "Reopen the transfer link and enter its password." : "This file is no longer available or is already downloading. Refresh and retry.");
      location.assign(link.href);
    } catch (e) {
      fail(e);
    }
  });
});
async function uploadPage() {
  const config = await api("/admin/api/config");
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
    draft = await api(`/admin/api/transfers/${encodeURIComponent(resumeID)}`);
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
  function renderFiles() {
    const list = $("#selected-files");
    list.replaceChildren();
    const entries = draft ? [...states.values()] : chosen.map((f) => ({ name: f.name, size: f.size, percent: 0, label: "Selected" }));
    for (const f of entries) {
      const li = el("li");
      const detail = el("div", "file-description");
      detail.append(el("strong", "", f.name), el("span", "small muted", `${bytes(f.size)} \xB7 ${f.label}`));
      if (draft) {
        const progress = el("progress");
        progress.max = 100;
        progress.value = f.percent;
        progress.setAttribute("aria-label", `Upload progress for ${f.name}`);
        detail.append(progress);
      }
      li.append(detail);
      list.append(li);
    }
  }
  function select(files) {
    if (running) return;
    const selected = [...files];
    if (!selected.length) return;
    if (selected.length > 100 || selected.some((f) => f.size > config.maxFileSize) || selected.reduce((n, f) => n + f.size, 0) > config.maxTransferSize) {
      notice("The selected files exceed the upload limits.", true);
      return;
    }
    chosen = selected;
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
        draft = await api("/admin/api/transfers", "POST", { title: form.elements.title.value, password: form.elements.password.value, expirySeconds: expiry, downloadLimit: limit, files: chosen.map((f) => ({ name: f.name, size: f.size })) });
        form.elements.password.value = "";
        history.replaceState(null, "", `/admin?resume=${draft.id}`);
      } else {
        draft = await api(`/admin/api/transfers/${draft.id}`);
      }
      const pending = draft.files.filter((f) => !f.uploaded);
      const fileMap = new Map();
      const remaining = [...chosen];
      for (const serverFile of pending) {
        const index = remaining.findIndex((f) => f.name === serverFile.name && f.size === serverFile.size);
        if (index < 0) throw new Error(`Reselect ${serverFile.name} (${bytes(serverFile.size)}) to resume.`);
        fileMap.set(serverFile.id, remaining.splice(index, 1)[0]);
      }
      for (const f of draft.files) states.set(f.id, { name: f.name, size: f.size, percent: f.uploaded ? 100 : 0, label: f.uploaded ? "Uploaded" : "Queued" });
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
            endpoint: "/admin/uploads/",
            uploadUrl: serverFile.started ? `/admin/uploads/${serverFile.id}` : void 0,
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
              renderFiles();
            },
            onSuccess: () => {
              state.percent = 100;
              state.label = "Uploaded";
              renderFiles();
              finish();
            },
            onError: (error) => {
              state.label = "Interrupted \u2014 resume from Transfers";
              renderFiles();
              finish(new Error(error.originalResponse?.getStatus() === 401 ? "Your session expired. Sign in again and resume from Transfers." : `Upload interrupted: ${serverFile.name}. Resume from Transfers to retry.`));
            }
          });
          job = { up, finish };
          state.label = "Uploading";
          renderFiles();
          if (!paused) up.start();
        });
      }
      if (cancelled) return;
      const shared = await api(`/admin/api/transfers/${draft.id}/publish`, "POST", {});
      $("#share-url").value = shared.shareUrl;
      $("#share-result").hidden = false;
      form.hidden = true;
      drop.hidden = true;
      $("#selected-files").hidden = true;
      history.replaceState(null, "", "/admin");
      $("#upload-heading").textContent = "Share";
      $("#notice").hidden = true;
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
      await api(`/admin/api/transfers/${draft.id}`, "DELETE");
      location.assign("/admin");
    } catch (e) {
      fail(e);
    }
  });
  $("#copy-share").addEventListener("click", () => copy($("#share-url").value));
  $("#show-share-qr").addEventListener("click", () => openQR(draft.id));
  $("#new-upload").addEventListener("click", () => location.assign("/admin"));
  renderFiles();
  controls(false);
}
async function transfersPage() {
  let offset = 0, editing = null, debounce, generation = 0;
  const container = $("#transfers"), dialog = $("#edit-dialog"), form = $("#edit-form");
  async function load() {
    const request = ++generation;
    const [transfers, config] = await Promise.all([api(`/admin/api/transfers?q=${encodeURIComponent($("#search").value)}&offset=${offset}`), api("/admin/api/config")]);
    if (request !== generation) return;
    $("#storage").textContent = `${bytes(config.storageUsed)} / ${bytes(config.storageQuota)}`;
    container.replaceChildren();
    if (!transfers.length) container.append(el("div", "empty", "No transfers"));
    for (const t of transfers) {
      const card = el("article", "transfer-card"), heading = el("div", "row");
      heading.append(el("h2", "", t.title), el("span", "badge", t.status));
      card.append(heading);
      const meta = el("div", "transfer-meta");
      meta.append(el("span", "", `${t.files.length} file${t.files.length === 1 ? "" : "s"} \xB7 ${bytes(t.files.reduce((n, f) => n + f.size, 0))}`));
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
      if (t.status === "draft") {
        const resume = el("a", "button secondary", "Resume upload");
        resume.href = `/admin?resume=${t.id}`;
        actions.append(resume);
      }
      if (t.status === "draft" || t.status === "published") actions.append(action("Edit", () => {
        editing = t;
        form.elements.title.value = t.title;
        form.elements.expiryEnabled.checked = (t.status === "draft" ? t.expirySeconds : t.expiresAt) > 0;
        form.elements.expiryHours.value = t.status === "draft" ? t.expirySeconds / 3600 || 24 : t.expiresAt ? Math.max(1e-3, (t.expiresAt - Date.now() / 1e3) / 3600) : 24;
        form.elements.limitEnabled.checked = t.downloadLimit > 0;
        form.elements.downloadLimit.value = t.downloadLimit || 1;
        form.elements.passwordAction.value = "keep";
        form.elements.password.value = "";
        form.elements.password.required = false;
        $("#edit-password-label").hidden = true;
        dialog.showModal();
      }));
      if (t.shareUrl) actions.append(action("Revoke", async () => {
        if (confirm("Disable this link and stop active downloads? Files stay until expiry or deletion.")) {
          await api(`/admin/api/transfers/${t.id}/revoke`, "POST", {});
          await load();
        }
      }, "quiet danger"));
      actions.append(action("Delete", async () => {
        if (confirm("Delete this transfer\u2019s stored files? This cannot be undone.")) {
          await api(`/admin/api/transfers/${t.id}`, "DELETE");
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
      const input = { title: form.elements.title.value, downloadLimit: form.elements.limitEnabled.checked ? Number(form.elements.downloadLimit.value) : 0 };
      if (!form.elements.expiryEnabled.checked) input.expirySeconds = 0;
      else input.expirySeconds = Math.round(Number(form.elements.expiryHours.value) * 3600);
      if (form.elements.passwordAction.value !== "keep") input.password = form.elements.passwordAction.value === "set" ? form.elements.password.value : "";
      await api(`/admin/api/transfers/${editing.id}`, "PATCH", input);
      form.elements.password.value = "";
      dialog.close();
      await load();
      notice("Transfer updated.");
    } catch (e) {
      dialog.close();
      fail(e);
    } finally {
      button.disabled = false;
    }
  });
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
if (page === "upload") uploadPage().catch(fail);
if (page === "transfers") transfersPage().catch(fail);
