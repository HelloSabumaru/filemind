import { download, sampleArchive, sampleContent } from './downloads.js';

const $ = selector => document.querySelector(selector);
const h = value => String(value ?? '').replace(/[&<>"']/g, character => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[character]);
const GiB = 1024 ** 3, storageKey = 'filemind-demo-v1';
const ownerID = 'alex';
function bytes(value) {
  if (value < 1024) return `${value} B`;
  for (const unit of ['KiB', 'MiB', 'GiB', 'TiB']) {
    value /= 1024;
    if (value < 1024) return `${value.toFixed(1)} ${unit}`;
  }
  return 'Large';
}
function seed() {
  const now = Date.now();
  const file = (name, size, downloads = 0) => ({ id: crypto.randomUUID(), name, size, downloads });
  return {
    version: 1, dateFormat: 'dd/mm/yyyy',
    settings: { defaultUserQuota: 5, expiryHours: 24, downloadLimit: 0, maxFileSize: 2, maxTransferSize: 2, storageQuota: 20 },
    users: [{ id: ownerID, username: 'alex', isAdmin: true, quota: 5, disabled: false }, { id: 'maria', username: 'maria', quota: 5, disabled: false }, { id: 'jon', username: 'jon', quota: 2, disabled: true }],
    transfers: [
      { id: 'brand-kit', ownerID, title: 'Brand kit', status: 'published', expiresAt: now + 3 * 86400000, expiryHours: 72, downloadLimit: 10, protected: false, files: [file('brand-guidelines.pdf', 2400000, 2), file('logo.svg', 18000, 1), file('colors.txt', 1200)] },
      { id: 'project-handoff', ownerID, title: 'Project handoff', status: 'published', expiresAt: now + 86400000, expiryHours: 24, downloadLimit: 0, protected: true, files: [file('project-notes.md', 4800), file('wireframes.png', 3200000)] },
      { id: 'weekend-photos', ownerID, title: 'Weekend photos', status: 'draft', progress: 42, expiresAt: 0, expiryHours: 24, downloadLimit: 0, protected: false, files: [file('mountains.jpg', 4800000), file('lake.jpg', 3600000)] },
      { id: 'old-proposal', ownerID, title: 'Old proposal', status: 'revoked', expiresAt: 0, expiryHours: 0, downloadLimit: 0, protected: false, files: [file('proposal.pdf', 870000)] },
      { id: 'meeting-notes', ownerID: 'maria', title: 'Meeting notes', status: 'published', expiresAt: now + 2 * 86400000, expiryHours: 48, downloadLimit: 0, protected: false, files: [file('meeting-notes.md', 6200)] },
    ],
  };
}
let state;
try { state = JSON.parse(sessionStorage.getItem(storageKey)); } catch {}
if (state?.version !== 1 || !Array.isArray(state.transfers) || !Array.isArray(state.users) || !state.settings) state = seed();
function save() { try { sessionStorage.setItem(storageKey, JSON.stringify(state)); } catch {} }
save();
let chosen = [], uploadJob = null, noticeTimer;
const unlocked = new Set();
function notice(message, error = false) {
  clearTimeout(noticeTimer);
  $('#notice').textContent = message;
  $('#notice').classList.toggle('error', error);
  $('#notice').hidden = false;
  noticeTimer = setTimeout(() => { $('#notice').hidden = true; }, 4500);
}
function on(selector, event, callback) {
  $(selector)?.addEventListener(event, async e => {
    try { await callback(e); } catch (error) { notice(error.message, true); }
  });
}
function route() { return location.hash.slice(1).split('/'); }
function go(path) { if (location.hash === `#${path}`) render(); else location.hash = path; }
function find(id) { return state.transfers.find(transfer => transfer.id === id); }
function size(transfer) { return transfer.files.reduce((sum, file) => sum + file.size, 0); }
function used(id) { return state.transfers.filter(transfer => !id || transfer.ownerID === id).reduce((sum, transfer) => sum + size(transfer), 0); }
function status(transfer) { return transfer.status === 'published' && transfer.expiresAt && transfer.expiresAt <= Date.now() ? 'expired' : transfer.status; }
function date(value) {
  const d = new Date(value), day = String(d.getDate()).padStart(2, '0'), month = String(d.getMonth() + 1).padStart(2, '0');
  return state.dateFormat === 'yyyy-mm-dd' ? `${d.getFullYear()}-${month}-${day}` : state.dateFormat === 'mm/dd/yyyy' ? `${month}/${day}/${d.getFullYear()}` : `${day}/${month}/${d.getFullYear()}`;
}
function shareURL(transfer) { const url = new URL(location.href); url.hash = `share/${transfer.id}`; return url.href; }
function button(action, label, id = '', style = 'secondary') { return `<button type="button" class="${style}" data-action="${action}" data-id="${h(id)}">${h(label)}</button>`; }
function filesHTML(files, progress) {
  return files.map(file => `<li><div class="file-description"><strong>${h(file.name)}</strong><span class="small muted">${bytes(file.size)}${progress === undefined ? '' : ` · ${Math.round(progress)}%`}</span>${progress === undefined ? '' : `<progress max="100" value="${progress}" aria-label="Upload progress for ${h(file.name)}"></progress>`}</div></li>`).join('');
}
function openForm(title, fields, submit, label = 'Save') {
  const dialog = $('#modal');
  dialog.innerHTML = `<form id="dialog-form"><h2 id="modal-title">${h(title)}</h2><p id="dialog-error" class="small danger" role="alert" hidden></p>${fields}<div class="actions"><button type="submit">${h(label)}</button><button type="button" class="secondary" id="close-modal">Cancel</button></div></form>`;
  on('#close-modal', 'click', () => dialog.close());
  on('#dialog-form', 'submit', async event => {
    event.preventDefault();
    try { await submit(new FormData(event.currentTarget)); save(); dialog.close(); render(); }
    catch (error) { $('#dialog-error').textContent = error.message; $('#dialog-error').hidden = false; }
  });
  dialog.showModal();
}
function confirmAction(title, message, callback) { openForm(title, `<p>${h(message)}</p>`, callback, title); }

const systemTheme = matchMedia('(prefers-color-scheme: dark)');
let theme;
try { theme = localStorage.getItem('filemind-theme'); } catch {}
function applyTheme() {
  if (theme === 'light' || theme === 'dark') document.documentElement.dataset.theme = theme;
  const dark = theme ? theme === 'dark' : systemTheme.matches;
  const path = dark ? '<circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5"/>' : '<path d="M21 13a9 9 0 1 1-10-10 7 7 0 0 0 10 10Z"/>';
  $('#theme-toggle').innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true">${path}</svg>`;
  $('#theme-toggle').setAttribute('aria-label', `Switch to ${dark ? 'light' : 'dark'} theme`);
}
on('#theme-toggle', 'click', () => {
  theme = (theme ? theme === 'dark' : systemTheme.matches) ? 'light' : 'dark';
  try { localStorage.setItem('filemind-theme', theme); } catch {}
  applyTheme();
});
systemTheme.addEventListener('change', applyTheme);
applyTheme();
on('#reset-demo', 'click', () => confirmAction('Reset demo', 'Restore the sample transfers, users and settings?', () => {
  clearInterval(uploadJob?.timer);
  uploadJob = null;
  chosen = [];
  unlocked.clear();
  state = seed();
  go('upload');
  notice('Demo reset.');
}));

function uploadPage(id) {
  const draft = id && find(id);
  if (id && (!draft || draft.status !== 'draft')) { missingPage(); return; }
  const running = uploadJob?.id === id;
  $('#view').innerHTML = `<section class="upload-page"><h1>Upload</h1><div class="panel upload-panel">${draft ? `
    <h2>${h(draft.title)}</h2><ul id="selected-files" class="file-list">${filesHTML(draft.files, draft.progress)}</ul>
    <div class="actions">${button(running ? 'pause' : 'resume', running ? 'Pause' : 'Resume upload', id, '')}${button('cancel', 'Cancel', id, 'quiet danger')}</div>
    <p class="demo-help">Simulated upload progress. You can pause, resume, or leave this page.</p>` : `
    <div id="dropzone" class="dropzone"><p>Drop files here</p><label class="button file-picker">Choose files<input id="file-picker" type="file" multiple aria-label="Choose files"></label><div class="demo-sample">${button('sample-files', 'Use sample files', '', 'quiet')}</div></div>
    <ul id="selected-files" class="file-list" aria-label="Selected files">${filesHTML(chosen)}</ul>
    <form id="upload-form" class="settings" ${chosen.length ? '' : 'hidden'}>
      <div class="form-grid"><label>Name<input name="title" maxlength="200" placeholder="Use filenames"></label><label>Demo password<input name="password" type="password" autocomplete="off" placeholder="Optional"></label></div>
      <div class="form-grid"><div><label class="check"><input name="expiryEnabled" type="checkbox" ${state.settings.expiryHours ? 'checked' : ''}>Expire after</label><div class="inline-fields"><input name="expiryHours" type="number" min="0.01" max="8760" step="any" value="${state.settings.expiryHours || 24}" aria-label="Expiry duration"><span class="unit">hours</span></div></div>
      <div><label class="check"><input name="limitEnabled" type="checkbox" ${state.settings.downloadLimit ? 'checked' : ''}>Downloads per file</label><input name="downloadLimit" type="number" min="1" max="1000000" value="${state.settings.downloadLimit || 1}" aria-label="Downloads per file"></div></div>
      <p class="demo-help">Protected demo links use the password <strong>demo</strong>. Any password entered here enables that preview.</p>
      <div class="actions"><button type="submit">Upload</button></div>
    </form>`}</div></section>`;
  if (draft) return;
  function select(files) {
    const selected = [...files].map(file => ({ name: file.name, size: file.size }));
    if (!selected.length) return;
    if (selected.length > 100) throw new Error('Choose up to 100 files.');
    if (selected.some(file => file.size > state.settings.maxFileSize * GiB) || selected.reduce((sum, file) => sum + file.size, 0) > state.settings.maxTransferSize * GiB) throw new Error('The selected files exceed the upload limits.');
    chosen = selected;
    $('#selected-files').innerHTML = filesHTML(chosen);
    $('#upload-form').hidden = false;
  }
  on('#file-picker', 'change', event => select(event.target.files));
  const drop = $('#dropzone');
  for (const event of ['dragenter', 'dragover']) on('#dropzone', event, e => { e.preventDefault(); drop.classList.add('drag'); });
  for (const event of ['dragleave', 'drop']) on('#dropzone', event, e => { e.preventDefault(); drop.classList.remove('drag'); });
  on('#dropzone', 'drop', event => select(event.dataTransfer.files));
  for (const name of ['expiry', 'limit']) {
    const form = $('#upload-form');
    const checkbox = form.elements[`${name}Enabled`], field = form.elements[name === 'expiry' ? 'expiryHours' : 'downloadLimit'];
    const sync = () => { field.disabled = !checkbox.checked; };
    checkbox.addEventListener('change', sync);
    sync();
  }
  on('#upload-form', 'submit', event => {
    event.preventDefault();
    if (uploadJob) throw new Error('Pause the current upload before starting another.');
    if (!chosen.length) throw new Error('Choose at least one file.');
    const total = chosen.reduce((sum, file) => sum + file.size, 0), account = state.users.find(user => user.id === ownerID);
    if (used() + total > state.settings.storageQuota * GiB || (account.quota && used(ownerID) + total > account.quota * GiB)) throw new Error('This transfer exceeds the storage quota.');
    const form = event.currentTarget;
    const transfer = { id: crypto.randomUUID(), ownerID, title: form.elements.title.value.trim() || chosen[0].name, status: 'draft', progress: 0, protected: !!form.elements.password.value, expiryHours: form.elements.expiryEnabled.checked ? Number(form.elements.expiryHours.value) : 0, expiresAt: 0, downloadLimit: form.elements.limitEnabled.checked ? Number(form.elements.downloadLimit.value) : 0, files: chosen.map(file => ({ ...file, id: crypto.randomUUID(), downloads: 0 })) };
    state.transfers.unshift(transfer);
    chosen = [];
    save();
    startUpload(transfer.id);
    go(`upload/${transfer.id}`);
  });
}
function startUpload(id) {
  if (uploadJob) throw new Error('Pause the current upload before resuming another.');
  const transfer = find(id);
  if (!transfer || transfer.status !== 'draft') return;
  uploadJob = { id, timer: setInterval(() => {
    transfer.progress = Math.min(100, (transfer.progress || 0) + 4);
    if (route()[0] === 'upload' && route()[1] === id) $('#selected-files').innerHTML = filesHTML(transfer.files, transfer.progress);
    if (transfer.progress === 100) {
      clearInterval(uploadJob.timer);
      uploadJob = null;
      transfer.status = 'published';
      transfer.expiresAt = transfer.expiryHours ? Date.now() + transfer.expiryHours * 3600000 : 0;
      if (route()[0] === 'upload' && route()[1] === id) go(`complete/${id}`);
      else render();
      notice('Transfer ready.');
    }
    save();
  }, 120) };
}
function completePage(id) {
  const transfer = find(id);
  if (!transfer || status(transfer) !== 'published') { missingPage(); return; }
  $('#view').innerHTML = `<section class="upload-page"><h1>Share</h1><div class="panel demo-share"><h2>${h(transfer.title)}</h2><p class="small muted">${transfer.files.length} files · ${bytes(size(transfer))}</p><div class="link-row"><input readonly value="${h(shareURL(transfer))}" aria-label="Share link">${button('copy', 'Copy link', id)}</div><div class="actions"><a class="button" href="#share/${h(id)}">Preview shared link</a><a class="button secondary" href="#upload">New upload</a></div><p class="demo-help">This preview is available in this browser tab. Downloads contain sample content.</p></div></section>`;
}
function adminNav(current) {
  return `<nav class="section-nav" aria-label="Admin"><span class="section-label">Admin</span>${[['users', 'Users'], ['all-transfers', 'All transfers'], ['server-settings', 'Server settings']].map(([page, label]) => `<a href="#${page}" ${page === current ? 'aria-current="page"' : ''}>${label}</a>`).join('')}</nav>`;
}
function transfersPage(all = false) {
  $('#view').innerHTML = `${all ? adminNav('all-transfers') : ''}<section class="page-heading"><h1>${all ? 'All transfers' : 'Transfers'}</h1></section><div class="toolbar"><input id="search" class="search" type="search" aria-label="Search transfers" placeholder="Search"><span class="small muted">${bytes(used(all ? undefined : ownerID))} / ${bytes((all ? state.settings.storageQuota : state.users.find(user => user.id === ownerID).quota || state.settings.storageQuota) * GiB)}</span></div><div id="transfers" aria-live="polite"></div>`;
  const list = () => {
    const query = $('#search').value.toLowerCase();
    const transfers = state.transfers.filter(transfer => (all || transfer.ownerID === ownerID) && `${transfer.title} ${transfer.files.map(file => file.name).join(' ')}`.toLowerCase().includes(query));
    $('#transfers').innerHTML = transfers.map(transfer => `<article class="transfer-card"><div class="row"><h2>${h(transfer.title)}</h2><span class="badge">${status(transfer)}</span></div><p class="transfer-filenames small muted">${transfer.files.map(file => h(file.name)).join(' · ')}</p><div class="transfer-meta"><span>${transfer.files.length} files · ${bytes(size(transfer))}</span>${all ? `<span>Owner: ${h(state.users.find(user => user.id === transfer.ownerID)?.username)}</span>` : ''}${transfer.expiresAt ? `<span>Expires ${date(transfer.expiresAt)}</span>` : ''}${transfer.downloadLimit ? `<span>${transfer.downloadLimit} downloads per file</span>` : ''}${transfer.protected ? '<span>Password protected</span>' : ''}</div><details><summary>Files</summary><ul class="file-list">${transfer.files.map(file => `<li><span>${h(file.name)}</span><span class="small muted">${file.downloads} downloads · ${bytes(file.size)}</span></li>`).join('')}</ul></details><div class="actions">${status(transfer) === 'published' ? `<a class="button secondary" href="#share/${h(transfer.id)}">Preview link</a>${button('copy', 'Copy link', transfer.id)}` : ''}${transfer.status === 'draft' ? `<a class="button secondary" href="#upload/${h(transfer.id)}">Resume upload</a>` : ''}${['published', 'draft'].includes(status(transfer)) ? button('edit-transfer', 'Edit', transfer.id) : ''}${status(transfer) === 'published' ? button('revoke', 'Revoke', transfer.id, 'quiet danger') : ''}${button('delete-transfer', 'Delete', transfer.id, 'quiet danger')}</div></article>`).join('') || '<div class="empty">No transfers</div>';
  };
  on('#search', 'input', list);
  list();
}
function editTransfer(id) {
  const transfer = find(id);
  openForm('Edit transfer', `<label>Name<input name="title" required maxlength="200" value="${h(transfer.title)}"></label><div class="form-grid"><label>Expire after (hours)<input name="expiryHours" type="number" min="0" max="8760" step="any" value="${transfer.expiryHours}"></label><label>Downloads per file<input name="downloadLimit" type="number" min="0" max="1000000" step="1" value="${transfer.downloadLimit}"></label></div><label class="check"><input name="protected" type="checkbox" ${transfer.protected ? 'checked' : ''}>Password protected</label><p class="demo-help">0 disables expiry or the download limit. Protected links use <strong>demo</strong>.</p>`, data => {
    const title = data.get('title').trim();
    if (!title) throw new Error('Enter a transfer name.');
    Object.assign(transfer, { title, expiryHours: Number(data.get('expiryHours')), downloadLimit: Number(data.get('downloadLimit')), protected: data.has('protected') });
    if (transfer.status === 'published') transfer.expiresAt = transfer.expiryHours ? Date.now() + transfer.expiryHours * 3600000 : 0;
    unlocked.delete(id);
    notice('Transfer updated.');
  });
}
function sharePage(id) {
  const transfer = find(id);
  if (!transfer) { missingPage(); return; }
  const available = status(transfer) === 'published';
  const locked = transfer.protected && !unlocked.has(id);
  $('#view').innerHTML = `<section class="recipient"><section class="page-heading"><h1>${h(transfer.title)}</h1>${transfer.expiresAt ? `<p class="small muted">Expires ${date(transfer.expiresAt)}</p>` : ''}</section><section class="panel">${!available ? `<h2>Link unavailable</h2><p>This transfer is ${status(transfer)}.</p><a class="button secondary" href="#transfers">Return to transfers</a>` : locked ? `<form id="unlock-form"><label>Password<input name="password" type="password" autocomplete="off" required></label><p class="demo-help">Use <strong>demo</strong> to unlock this sample transfer.</p><p id="unlock-error" class="small danger" role="alert" hidden></p><button type="submit">Unlock</button></form>` : `<ul class="file-list download-files">${transfer.files.map(file => `<li><div class="file-description"><strong>${h(file.name)}</strong><span class="small muted">${bytes(file.size)} · ${file.downloads}${transfer.downloadLimit ? ` / ${transfer.downloadLimit}` : ''} downloads</span></div>${canDownload(transfer, file) ? button('download', 'Download', `${id}/${file.id}`) : '<span class="badge">Unavailable</span>'}</li>`).join('')}</ul>${transfer.files.some(file => canDownload(transfer, file)) ? button('zip', 'Download ZIP', id, '') : ''}<p class="demo-help">Downloads contain generated sample text, not original files.</p>`}</section></section>`;
  on('#unlock-form', 'submit', event => {
    event.preventDefault();
    if (event.currentTarget.elements.password.value !== 'demo') { $('#unlock-error').textContent = 'Use the demo password: demo'; $('#unlock-error').hidden = false; return; }
    unlocked.add(id);
    render();
  });
}
function canDownload(transfer, file) { return status(transfer) === 'published' && (!transfer.protected || unlocked.has(transfer.id)) && (!transfer.downloadLimit || file.downloads < transfer.downloadLimit); }
function usersPage() {
  $('#view').innerHTML = `${adminNav('users')}<section class="page-heading row"><h1>Users</h1>${button('add-user', 'Add user', '', '')}</section><div id="users">${state.users.map(user => `<article class="transfer-card"><div class="row"><h2>${h(user.username)}</h2>${user.isAdmin || user.disabled ? `<span class="badge">${user.isAdmin ? 'Admin' : 'Disabled'}</span>` : ''}</div><span class="small muted">${bytes(used(user.id))}${user.quota ? ` / ${bytes(user.quota * GiB)}` : ''}</span><div class="actions">${button('edit-user', 'Edit', user.id)}${user.isAdmin ? '' : `${button('toggle-user', user.disabled ? 'Enable' : 'Disable', user.id)}${button('delete-user', 'Delete', user.id, 'quiet danger')}`}</div></article>`).join('')}</div>`;
}
function editUser(id) {
  const user = state.users.find(account => account.id === id);
  openForm(user ? 'Edit user' : 'Add user', `<label>Username<input name="username" required maxlength="100" pattern="[A-Za-z0-9_.@\-]+" ${user ? 'readonly' : ''} value="${h(user?.username || '')}" autocomplete="off"></label>${user ? '' : '<label>Demo password<input name="password" type="password" autocomplete="off" required></label>'}<label>Storage quota (GiB)<input name="quota" type="number" min="0" max="1048576" step="any" value="${user?.quota ?? state.settings.defaultUserQuota}"></label><p class="demo-help">0 uses the shared server limit. Changes apply only to this demo.</p>`, data => {
    const username = data.get('username').trim(), quota = Number(data.get('quota'));
    if (!user && state.users.some(account => account.username.toLowerCase() === username.toLowerCase())) throw new Error('This username already exists.');
    if (user) user.quota = quota;
    else state.users.push({ id: crypto.randomUUID(), username, quota, disabled: false, isAdmin: false });
    notice(user ? 'Account updated.' : 'Account created.');
  });
}
function settingsPage(server = false) {
  if (server) {
    const fields = [['defaultUserQuota', 'Default user quota (GiB)'], ['expiryHours', 'Default expiry (hours)'], ['downloadLimit', 'Default downloads per file'], ['maxFileSize', 'Maximum file size (GiB)'], ['maxTransferSize', 'Maximum transfer size (GiB)'], ['storageQuota', 'Total storage limit (GiB)']];
    $('#view').innerHTML = `${adminNav('server-settings')}<section class="settings-page"><h1>Server settings</h1><form id="settings-form" class="panel"><div class="form-grid">${fields.map(([key, label]) => `<label>${label}<input name="${key}" type="number" min="${['maxFileSize', 'maxTransferSize', 'storageQuota'].includes(key) ? '0.000001' : '0'}" max="1048576" step="${key === 'downloadLimit' ? '1' : 'any'}" required value="${state.settings[key]}"></label>`).join('')}</div><p class="small muted">Defaults apply to new users and transfers. 0 disables the default user quota, expiry or download limit.</p><div class="row"><button type="submit">Save</button><span class="small muted">${bytes(used())} reserved</span></div></form></section>`;
    on('#settings-form', 'submit', event => {
      event.preventDefault();
      for (const [key] of fields) state.settings[key] = Number(event.currentTarget.elements[key].value);
      save(); notice('Server settings saved.');
    });
  } else {
    $('#view').innerHTML = `<section class="settings-page personal-settings"><h1>Settings</h1><form id="preferences-form" class="panel"><label>Date format<select name="dateFormat">${['dd/mm/yyyy', 'mm/dd/yyyy', 'yyyy-mm-dd'].map(value => `<option ${state.dateFormat === value ? 'selected' : ''}>${value}</option>`).join('')}</select></label><div class="actions"><button type="submit">Save</button></div></form></section>`;
    on('#preferences-form', 'submit', event => { event.preventDefault(); state.dateFormat = event.currentTarget.elements.dateFormat.value; save(); notice('Settings saved.'); });
  }
}
function missingPage() { $('#view').innerHTML = '<section class="recipient panel"><h1>Preview unavailable</h1><p>This transfer is no longer in this demo session.</p><a class="button secondary" href="#transfers">Browse sample transfers</a></section>'; }
const actions = {
  'sample-files': () => { chosen = [{ name: 'project-notes.md', size: 4800 }, { name: 'wireframes.png', size: 3200000 }]; render(); },
  resume: id => { startUpload(id); render(); },
  pause: () => { clearInterval(uploadJob?.timer); uploadJob = null; save(); render(); },
  cancel: id => confirmAction('Cancel upload', 'Discard this demo transfer?', () => {
    if (uploadJob?.id === id) { clearInterval(uploadJob.timer); uploadJob = null; }
    state.transfers = state.transfers.filter(transfer => transfer.id !== id);
    go('upload'); notice('Transfer cancelled.');
  }),
  copy: async id => {
    const url = shareURL(find(id));
    try { await navigator.clipboard.writeText(url); notice('Link copied.'); }
    catch { openForm('Copy link', `<input readonly aria-label="Share link" value="${h(url)}">`, () => {}, 'Done'); $('#modal input').select(); }
  },
  'edit-transfer': editTransfer,
  revoke: id => confirmAction('Revoke link', 'Disable this demo link? Files remain in Transfers.', () => { find(id).status = 'revoked'; notice('Link revoked.'); }),
  'delete-transfer': id => confirmAction('Delete transfer', 'Remove this transfer from the demo?', () => {
    if (uploadJob?.id === id) { clearInterval(uploadJob.timer); uploadJob = null; }
    state.transfers = state.transfers.filter(transfer => transfer.id !== id); notice('Transfer deleted.');
  }),
  download: path => {
    const [id, fileID] = path.split('/'), transfer = find(id), file = transfer?.files.find(entry => entry.id === fileID);
    if (!file || !canDownload(transfer, file)) throw new Error('Download unavailable.');
    download(new Blob([sampleContent(file, transfer.title)], { type: 'text/plain' }), `${file.name}.demo.txt`);
    file.downloads++; save(); render();
  },
  zip: id => {
    const transfer = find(id), files = transfer.files.filter(file => canDownload(transfer, file));
    if (!files.length) throw new Error('Download unavailable.');
    download(sampleArchive(files, transfer.title), `${transfer.title}.zip`);
    files.forEach(file => file.downloads++); save(); render();
  },
  'add-user': () => editUser(),
  'edit-user': editUser,
  'toggle-user': id => { const user = state.users.find(account => account.id === id); if (user && !user.isAdmin) { user.disabled = !user.disabled; save(); render(); } },
  'delete-user': id => confirmAction('Delete user', 'Remove this demo user and all their transfers?', () => {
    const user = state.users.find(account => account.id === id);
    if (user?.isAdmin) throw new Error('The administrator account cannot be deleted.');
    state.users = state.users.filter(account => account.id !== id);
    state.transfers = state.transfers.filter(transfer => transfer.ownerID !== id); notice('User deleted.');
  }),
};
on('#view', 'click', event => {
  const target = event.target.closest('button[data-action]');
  if (target) return actions[target.dataset.action]?.(target.dataset.id);
});
function render() {
  const [page = 'upload', id] = route(), current = page || 'upload';
  const admin = ['users', 'all-transfers', 'server-settings'].includes(current);
  document.querySelectorAll('[data-page]').forEach(link => {
    if (link.dataset.page === (admin ? 'admin' : current === 'complete' ? 'upload' : current)) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  });
  const pages = { upload: () => uploadPage(id), transfers: () => transfersPage(), 'all-transfers': () => transfersPage(true), complete: () => completePage(id), share: () => sharePage(id), users: usersPage, settings: () => settingsPage(), 'server-settings': () => settingsPage(true) };
  (pages[current] || missingPage)();
  document.title = `${$('#view h1')?.textContent || 'Demo'} · FileMind demo`;
}
window.addEventListener('hashchange', () => {
  render();
  const heading = $('#view h1');
  if (heading) { heading.tabIndex = -1; heading.focus({ preventScroll: true }); }
});
render();
