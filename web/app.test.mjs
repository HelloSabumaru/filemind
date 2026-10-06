import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { access, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { chromium } from "playwright";

let directory, child, browser, ownerURL, publicURL, adminURL, binary, serverEnv;
let diagnostics = "";
const adminPassword = "browser-admin-password-for-validation";
const digest = value => createHash("sha256").update(value).digest("hex");

async function freePort() {
  const server = createServer();
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}

before(async () => {
  directory = await mkdtemp(join(tmpdir(), "filemind-browser-"));
  binary = join(directory, "filemind");
  const secret = join(directory, "password");
  execFileSync("go", ["build", "-o", binary, "./cmd/filemind"], {stdio:"pipe"});
  await writeFile(secret, `${adminPassword}\n`, {mode:0o600});
  const ports = [];
  while (ports.length < 3) { const port = await freePort(); if (!ports.includes(port)) ports.push(port); }
  [ownerURL, publicURL, adminURL] = ports.map(port => `http://localhost:${port}`);
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("FILEMIND_")));
  Object.assign(env, {
    FILEMIND_INSECURE_DEVELOPMENT:"true", FILEMIND_DEMO:"true", FILEMIND_OWNER_USERNAME:"admin",
    FILEMIND_OWNER_PASSWORD_FILE:secret, FILEMIND_DATA_DIR:join(directory,"data"),
    FILEMIND_OWNER_LISTEN:`127.0.0.1:${ports[0]}`, FILEMIND_PUBLIC_LISTEN:`127.0.0.1:${ports[1]}`, FILEMIND_ADMIN_LISTEN:`127.0.0.1:${ports[2]}`,
    FILEMIND_OWNER_URL:ownerURL, FILEMIND_PUBLIC_URL:publicURL, FILEMIND_ADMIN_URL:adminURL,
  });
  serverEnv = env;
  await startServer();
  let executablePath = process.env.FILEMIND_TEST_CHROME;
  if (!executablePath) { try { await access("/usr/bin/google-chrome"); executablePath = "/usr/bin/google-chrome"; } catch {} }
  browser = await chromium.launch({executablePath, headless:true, args:["--no-sandbox"]});
}, {timeout:120000});

async function startServer() {
  child = spawn(binary, [], {env:serverEnv, stdio:["ignore","pipe","pipe"]});
  const collect = data => { diagnostics = (diagnostics + data.toString()).slice(-12000); };
  child.stdout.on("data", collect); child.stderr.on("data", collect);
  let ready = false;
  for (let attempt = 0; attempt < 100; attempt++) {
    try { ready = (await fetch(`${ownerURL}/healthz`)).ok; } catch {}
    if (ready) break;
    if (child.exitCode !== null) throw new Error(`Server exited: ${diagnostics}`);
    await delay(100);
  }
  assert.ok(ready, `Server did not become ready: ${diagnostics}`);
}

async function stopServer(signal) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const current = child;
  const stopped = new Promise(resolve => current.once("exit",resolve));
  current.kill(signal);
  const timeout = setTimeout(() => current.kill("SIGKILL"),12000);
  try { await stopped; } finally { clearTimeout(timeout); }
}

after(async () => {
  await browser?.close();
  await stopServer("SIGTERM");
  if (directory) await rm(directory,{recursive:true,force:true});
});

async function login(t, admin = false, username = admin ? "admin" : "user", password = admin ? adminPassword : "password") {
  const context = await browser.newContext({acceptDownloads:true});
  t.after(() => context.close());
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", error => pageErrors.push(error.message));
  t.after(() => assert.deepEqual(pageErrors, []));
  await page.goto(`${admin ? adminURL : ownerURL}/login`);
  await page.locator("[name=username]").fill(username);
  await page.locator("[name=password]").fill(password);
  await page.getByRole("button",{name:"Sign in",exact:true}).click();
  await page.waitForURL(admin ? "**/admin/users" : "**/upload");
  if (!admin) await readyUpload(page);
  return page;
}

async function readyUpload(page) {
  await page.waitForFunction(() => document.querySelector("#file-picker")?.disabled === false);
}

async function api(page, path, method = "GET", body) {
  return page.evaluate(async ({path,method,body}) => {
    const response = await fetch(path,{method, headers:{"Content-Type":"application/json","X-CSRF-Token":document.querySelector('meta[name="csrf-token"]').content},body:body === undefined ? undefined : JSON.stringify(body)});
    return {status:response.status,body:await response.json()};
  },{path,method,body});
}

async function partialDraft(page, name, payload, prefixLength) {
  const privateAdmin = new URL(page.url()).origin === adminURL;
  const base = privateAdmin ? "/admin/api/personal" : "/api";
  const uploads = privateAdmin ? "/admin/uploads/" : "/uploads/";
  const result = await api(page,`${base}/transfers`,"POST",{title:name,files:[{name,size:payload.length,sha256:digest(payload)}]});
  assert.equal(result.status,200);
  const draft = result.body;
  await page.evaluate(async ({fileID,prefix,size,uploads}) => {
    const csrf = document.querySelector('meta[name="csrf-token"]').content;
    const created = await fetch(uploads,{method:"POST",headers:{"X-CSRF-Token":csrf,"Tus-Resumable":"1.0.0","Upload-Length":String(size),"Upload-Metadata":`file_id ${btoa(fileID)}`}});
    if (created.status !== 201) throw new Error(`Create failed: ${created.status}`);
    if (prefix.length) {
      const patched = await fetch(`${uploads}${fileID}`,{method:"PATCH",headers:{"X-CSRF-Token":csrf,"Tus-Resumable":"1.0.0","Upload-Offset":"0","Content-Type":"application/offset+octet-stream"},body:prefix});
      if (patched.status !== 204) throw new Error(`Patch failed: ${patched.status}`);
    }
  },{fileID:draft.files[0].id,prefix:payload.subarray(0,prefixLength).toString(),size:payload.length,uploads});
  return draft;
}

async function select(page, name, payload) {
  await page.locator("#file-picker").setInputFiles({name,mimeType:"application/octet-stream",buffer:payload});
}

async function publishedResult(page) {
  await page.locator("#share-result").waitFor({state:"visible"});
  return page.locator("#share-url").inputValue();
}

test("Admin keeps the main navigation and theme while switching sections", async t => {
  const context = await browser.newContext();
  t.after(() => context.close());
  const page = await context.newPage();
  await page.goto(`${adminURL}/login`);
  const main = page.getByRole("navigation",{name:"Main",exact:true});
  const expected = ["Upload","Transfers","Settings","Admin"];
  async function checkMain() { assert.deepEqual(await main.getByRole("link").allTextContents(),expected); }
  await checkMain();
  await page.emulateMedia({colorScheme:"light"});
  await page.getByRole("button",{name:"Switch to dark theme"}).click();
  await page.getByRole("heading",{name:"Sign in to Admin",exact:true}).waitFor();
  await checkMain();
  assert.equal(await main.getByRole("link",{name:"Admin",exact:true}).getAttribute("aria-current"),"page");
  assert.equal(await page.locator("html").getAttribute("data-theme"),"dark");
  await page.locator("[name=username]").fill("admin");
  await page.locator("[name=password]").fill(adminPassword);
  await page.getByRole("button",{name:"Sign in",exact:true}).click();
  await page.waitForURL(`${adminURL}/admin/users`);
  await checkMain();
  const admin = page.getByRole("navigation",{name:"Admin",exact:true});
  assert.deepEqual(await admin.getByRole("link").allTextContents(),["Users","All transfers","Server settings"]);
  await admin.getByRole("link",{name:"All transfers",exact:true}).click();
  await page.getByRole("heading",{name:"All transfers",exact:true}).waitFor();
  await checkMain();
  assert.equal(await main.getByRole("link",{name:"Admin",exact:true}).getAttribute("aria-current"),"page");
  assert.equal(await main.getByRole("link",{name:"Transfers",exact:true}).getAttribute("aria-current"),null);
  assert.equal(await admin.getByRole("link",{name:"All transfers",exact:true}).getAttribute("aria-current"),"page");
  await admin.getByRole("link",{name:"Server settings",exact:true}).click();
  await page.getByRole("heading",{name:"Server settings",exact:true}).waitFor();
  await checkMain();
  await main.getByRole("link",{name:"Settings",exact:true}).click();
  await page.getByRole("heading",{name:"Change password",exact:true}).waitFor();
  await checkMain();
  assert.equal(await main.getByRole("link",{name:"Settings",exact:true}).getAttribute("aria-current"),"page");
  await main.getByRole("link",{name:"Upload",exact:true}).click();
  await page.waitForURL(`${adminURL}/admin/upload`); await readyUpload(page);
  await checkMain();
  assert.equal(await page.locator("html").getAttribute("data-theme"),"dark");
  await main.getByRole("link",{name:"Transfers",exact:true}).click();
  await page.getByRole("heading",{name:"Transfers",exact:true}).waitFor();
  assert.equal(page.url(),`${adminURL}/admin/my-transfers`);
  await main.getByRole("link",{name:"Settings",exact:true}).click();
  await page.getByRole("heading",{name:"Change password",exact:true}).waitFor();
  assert.equal(page.url(),`${adminURL}/admin/preferences`);
  await main.getByRole("link",{name:"Admin",exact:true}).click();
  await page.getByRole("button",{name:"Add user",exact:true}).waitFor();
  await page.setViewportSize({width:390,height:844});
  await checkMain();
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),"mobile navigation overflows the viewport");
  if (process.env.FILEMIND_TEST_SCREENSHOTS) await page.screenshot({path:join(process.env.FILEMIND_TEST_SCREENSHOTS,"admin-navigation.png"),fullPage:true});
  await page.getByRole("button",{name:"Switch to light theme"}).click();
  await page.locator(".brand").click();
  await page.waitForURL(`${adminURL}/admin/upload`); await readyUpload(page);
  assert.equal(await page.locator("html").getAttribute("data-theme"),"light");
});

test("upload hashing spans multiple chunks and the browser receives exact contents", {timeout:60000}, async t => {
  const page = await login(t);
  const payload = Buffer.alloc(9 * 1024 * 1024 + 17, "u");
  payload[0] = 0; payload[payload.length - 1] = 255;
  await select(page,"browser-integrity.bin",payload);
  await page.getByRole("button",{name:"Upload",exact:true}).click();
  const share = await publishedResult(page);
  const recipient = await browser.newPage({acceptDownloads:true}); t.after(() => recipient.close());
  await recipient.goto(share);
  const downloaded = recipient.waitForEvent("download");
  await recipient.locator(".download-link").first().click();
  const download = await downloaded;
  assert.equal(await download.failure(),null);
  assert.equal(digest(await readFile(await download.path())),digest(payload));
  const transfers = await api(page,"/api/transfers");
  const transfer = transfers.body.find(item => item.files.some(file => file.name === "browser-integrity.bin"));
  assert.equal(transfer.title,"browser-integrity.bin");
  assert.equal(transfer.files[0].sha256,digest(payload));
});

test("filename titles identify batches and custom titles retain a visible filename preview", async t => {
  const page = await login(t);
  const names = ["invoice.pdf", "notes.txt", "photo.png", "data.csv"];
  await page.locator("#file-picker").setInputFiles(names.map(name => ({name,mimeType:"application/octet-stream",buffer:Buffer.from(name)})));
  assert.equal(await page.locator("#upload-form [name=title]").getAttribute("placeholder"),"invoice.pdf + 3 more");
  await page.getByRole("button",{name:"Upload",exact:true}).click();
  const share = await publishedResult(page);
  const batch = (await api(page,"/api/transfers")).body.find(item => item.shareUrl === share);
  assert.equal(batch.title,"invoice.pdf + 3 more");
  assert.deepEqual(batch.files.map(file => file.name),names);
  const recipient = await browser.newPage(); t.after(() => recipient.close());
  await recipient.goto(share);
  await recipient.getByRole("heading",{name:"invoice.pdf + 3 more",exact:true}).waitFor();
  assert.deepEqual(await recipient.locator(".file-description strong").allTextContents(),names);
  await page.goto(`${ownerURL}/upload`); await readyUpload(page);
  await select(page,"meeting.txt",Buffer.from("meeting notes"));
  await page.locator("#upload-form [name=title]").fill("Team meeting");
  await select(page,"minutes.txt",Buffer.from("meeting minutes"));
  assert.equal(await page.locator("#upload-form [name=title]").inputValue(),"Team meeting");
  assert.equal(await page.locator("#upload-form [name=title]").getAttribute("placeholder"),"minutes.txt");
  await page.getByRole("button",{name:"Upload",exact:true}).click();
  await publishedResult(page);
  await page.goto(`${ownerURL}/transfers`);
  const card = page.locator(".transfer-card").filter({has:page.getByRole("heading",{name:"invoice.pdf + 3 more",exact:true})});
  await card.waitFor();
  const preview = card.locator(".transfer-filenames");
  assert.equal(await preview.isVisible(),true);
  assert.equal(await preview.textContent(),"invoice.pdf · notes.txt · photo.png · + 1 more");
  await card.getByText("Files",{exact:true}).click();
  await card.locator(".file-list").getByText("data.csv",{exact:true}).waitFor();
  const custom = page.locator(".transfer-card").filter({has:page.getByRole("heading",{name:"Team meeting",exact:true})});
  assert.equal(await custom.locator(".transfer-filenames").textContent(),"minutes.txt");
  await page.setViewportSize({width:390,height:844});
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),"filename preview overflows on mobile");
  const admin = await login(t,true);
  await admin.goto(`${adminURL}/admin/transfers`);
  const adminCard = admin.locator(".transfer-card").filter({has:admin.getByRole("heading",{name:"invoice.pdf + 3 more",exact:true})});
  await adminCard.waitFor();
  assert.equal(await adminCard.locator(".transfer-filenames").textContent(),"invoice.pdf · notes.txt · photo.png · + 1 more");
});

test("resume rejects a different same-name same-size file before changing saved bytes", async t => {
  const page = await login(t), original = Buffer.from("abcdefgh");
  const draft = await partialDraft(page,"resume-identity.txt",original,4);
  await page.goto(`${ownerURL}/upload?resume=${draft.id}`); await readyUpload(page);
  await select(page,"resume-identity.txt",Buffer.from("12345678"));
  await page.getByRole("button",{name:"Resume",exact:true}).click();
  await page.waitForFunction(() => document.querySelector("#notice")?.textContent.includes("does not match the original"));
  const offset = await page.evaluate(async id => (await fetch(`/uploads/${id}`,{method:"HEAD",headers:{"Tus-Resumable":"1.0.0"}})).headers.get("Upload-Offset"),draft.files[0].id);
  assert.equal(offset,"4");
  await select(page,"resume-identity.txt",original);
  await page.getByRole("button",{name:"Resume",exact:true}).click();
  await publishedResult(page);
});

test("old drafts resume after an administrator lowers upload limits", async t => {
  const page = await login(t), admin = await login(t,true);
  const payload = Buffer.alloc(2048,"r");
  const draft = await partialDraft(page,"reserved-size.txt",payload,8);
  await admin.goto(`${adminURL}/admin/settings`);
  const current = (await api(admin,"/admin/api/settings")).body;
  const settings = Object.fromEntries(Object.entries(current).filter(([key]) => !["storageUsed","maintenance"].includes(key)));
  const smaller = {...settings,maxFileSize:1024,maxTransferSize:1024};
  assert.equal((await api(admin,"/admin/api/settings","PUT",smaller)).status,200);
  try {
    await page.goto(`${ownerURL}/upload?resume=${draft.id}`); await readyUpload(page);
    await select(page,"reserved-size.txt",payload);
    await page.getByRole("button",{name:"Resume",exact:true}).click();
    await publishedResult(page);
  } finally { assert.equal((await api(admin,"/admin/api/settings","PUT",settings)).status,200); }
});

test("unrelated edits preserve expiry and a rejected edit stays open", async t => {
  const page = await login(t);
  await select(page,"edit-expiry.txt",Buffer.from("edit"));
  await page.getByRole("button",{name:"Upload",exact:true}).click(); await publishedResult(page);
  const transfer = (await api(page,"/api/transfers")).body.find(item => item.files.some(file => file.name === "edit-expiry.txt"));
  await page.goto(`${ownerURL}/transfers`);
  const card = page.locator(".transfer-card").filter({hasText:"edit-expiry.txt"});
  await card.getByRole("button",{name:"Edit",exact:true}).click();
  assert.match(await page.locator("#edit-form [name=expiryHours]").inputValue(),/^\d+\.\d{2}$/);
  await page.locator("#edit-form [name=title]").fill("Only title changed");
  await delay(1100);
  await page.locator("#edit-form").getByRole("button",{name:"Save",exact:true}).click();
  await page.locator("#edit-dialog").waitFor({state:"hidden"});
  const changed = (await api(page,`/api/transfers/${transfer.id}`)).body;
  assert.equal(changed.expiresAt,transfer.expiresAt);
  await card.getByRole("button",{name:"Edit",exact:true}).click();
  assert.equal((await api(page,`/api/transfers/${transfer.id}`,"PATCH",{revision:changed.revision,title:"Changed elsewhere"})).status,200);
  await page.locator("#edit-form [name=title]").fill("Stale edit");
  await page.locator("#edit-form").getByRole("button",{name:"Save",exact:true}).click();
  await page.locator("#edit-error").waitFor({state:"visible"});
  assert.equal(await page.locator("#edit-dialog").evaluate(dialog => dialog.open),true);
  assert.equal(await page.locator("#edit-form [name=title]").inputValue(),"Stale edit");
});

test("protected links unlock through the form and reuse the existing download session", async t => {
  const page = await login(t);
  await select(page,"protected-browser.txt",Buffer.from("protected download"));
  await page.locator("#upload-form [name=password]").fill("secret");
  await page.getByRole("button",{name:"Upload",exact:true}).click();
  const share = await publishedResult(page);
  const recipient = await browser.newPage({acceptDownloads:true}); t.after(() => recipient.close());
  await recipient.goto(share);
  await recipient.getByRole("heading",{name:"Password-protected transfer",exact:true}).waitFor();
  assert.equal(await recipient.locator(".download-link").count(),0);
  await recipient.locator("#unlock-form [name=password]").fill("wrong");
  await recipient.getByRole("button",{name:"Unlock",exact:true}).click();
  await recipient.getByText("Incorrect password.",{exact:true}).waitFor();
  await recipient.locator("#unlock-form [name=password]").fill("secret");
  await recipient.getByRole("button",{name:"Unlock",exact:true}).click();
  await recipient.locator(".file-description strong",{hasText:"protected-browser.txt"}).waitFor();
  const before = (await recipient.context().cookies(share)).find(cookie => cookie.name.startsWith("filemind-share-"));
  assert.ok(before,"unlock did not create a download session");
  assert.equal((await api(recipient,`${new URL(share).pathname}/unlock`,"POST",{password:"secret"})).status,200);
  const after = (await recipient.context().cookies(share)).find(cookie => cookie.name === before.name);
  assert.equal(after.value,before.value);
  assert.equal(after.expires,before.expires);
  const downloaded = recipient.waitForEvent("download");
  await recipient.locator(".download-link").first().click();
  const download = await downloaded;
  assert.equal(await download.failure(),null);
  assert.equal(await readFile(await download.path(),"utf8"),"protected download");
});

test("actual download errors offer a return link instead of a JSON page", async t => {
  const page = await login(t);
  await select(page,"revoked-browser.txt",Buffer.from("download"));
  await page.getByRole("button",{name:"Upload",exact:true}).click();
  const share = await publishedResult(page);
  const transfer = (await api(page,"/api/transfers")).body.find(item => item.shareUrl === share);
  const recipient = await browser.newPage(); t.after(() => recipient.close()); await recipient.goto(share);
  assert.equal((await api(page,`/api/transfers/${transfer.id}/revoke`,"POST",{})).status,200);
  await recipient.locator(".download-link").first().click();
  await recipient.getByRole("heading",{name:"Download unavailable"}).waitFor();
  assert.equal(await recipient.getByRole("link",{name:"Return to transfer"}).getAttribute("href"),new URL(share).pathname);
});

test("admin account management and self-service password changes work through forms", async t => {
  const admin = await login(t,true);
  await admin.getByRole("button",{name:"Add user",exact:true}).click();
  await admin.locator("#user-form [name=username]").fill("browser-account");
  await admin.locator("#user-form [name=password]").fill("x");
  await admin.locator("#user-form").getByRole("button",{name:"Save",exact:true}).click();
  await admin.locator("#user-dialog").waitFor({state:"hidden"});
  const user = await login(t,false,"browser-account","x");
  await user.goto(`${ownerURL}/settings`);
  assert.equal(await user.getByText("Use 16–256 bytes. Changing your password signs out all sessions and stops active uploads.",{exact:true}).count(),0);
  assert.ok(await user.locator('input[type="password"]').evaluateAll(inputs => inputs.every(input => !input.hasAttribute("minlength") && !input.hasAttribute("maxlength"))));
  await user.locator("[name=currentPassword]").fill("x");
  await user.locator("[name=newPassword]").fill("p".repeat(1024));
  await user.getByRole("button",{name:"Change password",exact:true}).click();
  await user.waitForURL("**/login");
  await user.locator("[name=username]").fill("browser-account");
  await user.locator("[name=password]").fill("p".repeat(1024));
  await user.getByRole("button",{name:"Sign in",exact:true}).click(); await user.waitForURL("**/upload");
  admin.on("dialog",dialog => dialog.accept());
  const card = admin.locator(".transfer-card").filter({has:admin.getByRole("heading",{name:"browser-account",exact:true})});
  await card.getByRole("button",{name:"Delete",exact:true}).click();
  await card.waitFor({state:"hidden"});
  assert.equal((await api(user,"/api/transfers")).status,401);
});


test("expiry and download values appear only when enabled in upload and edit forms", async t => {
  const page = await login(t);
  await select(page,"conditional-values.txt",Buffer.from("conditional settings"));
  const upload = page.locator("#upload-form");
  for (const [checkbox, fields] of [["expiryEnabled", ["expiryValue", "expiryUnit"]], ["limitEnabled", ["downloadLimit"]]]) {
    await upload.locator(`[name=${checkbox}]`).uncheck();
    for (const field of fields) {
      assert.equal(await upload.locator(`[name=${field}]`).isVisible(),false);
      assert.equal(await upload.locator(`[name=${field}]`).isDisabled(),true);
    }
    await upload.locator(`[name=${checkbox}]`).check();
    for (const field of fields) assert.equal(await upload.locator(`[name=${field}]`).isVisible(),true);
    await upload.locator(`[name=${fields[0]}]`).fill("-1");
    await upload.locator(`[name=${checkbox}]`).uncheck();
  }
  await upload.getByRole("button",{name:"Upload",exact:true}).click();
  const share = await publishedResult(page);
  const transfer = (await api(page,"/api/transfers")).body.find(item => item.shareUrl === share);
  assert.equal(transfer.expiresAt,0);
  assert.equal(transfer.downloadLimit,0);
  await page.goto(`${ownerURL}/transfers`);
  await page.locator(".transfer-card").filter({hasText:"conditional-values.txt"}).getByRole("button",{name:"Edit",exact:true}).click();
  const edit = page.locator("#edit-form");
  for (const [checkbox, field] of [["expiryEnabled", "expiryHours"], ["limitEnabled", "downloadLimit"]]) {
    assert.equal(await edit.locator(`[name=${field}]`).isVisible(),false);
    assert.equal(await edit.locator(`[name=${field}]`).isDisabled(),true);
    await edit.locator(`[name=${checkbox}]`).check();
    assert.equal(await edit.locator(`[name=${field}]`).isVisible(),true);
    await edit.locator(`[name=${field}]`).fill("-1");
    await edit.locator(`[name=${checkbox}]`).uncheck();
  }
  await edit.getByRole("button",{name:"Save",exact:true}).click();
  await page.locator("#edit-dialog").waitFor({state:"hidden"});
  const changed = (await api(page,`/api/transfers/${transfer.id}`)).body;
  assert.equal(changed.expiresAt,0);
  assert.equal(changed.downloadLimit,0);
});

test("date format defaults to dd/mm/yyyy and persists across personal and admin Settings", async t => {
  const page = await login(t,true);
  await page.goto(`${adminURL}/admin/upload`); await readyUpload(page);
  await select(page,"date-format.txt",Buffer.from("date preference"));
  await page.getByRole("button",{name:"Upload",exact:true}).click();
  const share = await publishedResult(page);
  const transfer = (await api(page,"/admin/api/personal/transfers")).body.find(item => item.shareUrl === share);
  const dates = await page.evaluate(timestamp => {
    const date = new Date(timestamp * 1000);
    const day = String(date.getDate()).padStart(2,"0"), month = String(date.getMonth()+1).padStart(2,"0"), year = date.getFullYear();
    return {"dd/mm/yyyy":`${day}/${month}/${year}`, "mm/dd/yyyy":`${month}/${day}/${year}`, "yyyy-mm-dd":`${year}-${month}-${day}`};
  },transfer.expiresAt);
  await page.goto(`${adminURL}/admin/my-transfers`);
  const card = page.locator(".transfer-card").filter({hasText:"date-format.txt"});
  await card.getByText(`Expires ${dates["dd/mm/yyyy"]}`,{exact:false}).waitFor();
  await page.goto(`${adminURL}/admin/preferences`);
  assert.equal(await page.locator("[name=dateFormat]").inputValue(),"dd/mm/yyyy");
  try {
    for (const format of ["mm/dd/yyyy", "yyyy-mm-dd"]) {
      await page.locator("[name=dateFormat]").selectOption(format);
      await page.locator("#preferences-form").getByRole("button",{name:"Save",exact:true}).click();
      await page.getByText("Settings saved.",{exact:true}).waitFor();
      await page.reload();
      assert.equal(await page.locator("[name=dateFormat]").inputValue(),format);
      await page.goto(`${adminURL}/admin/my-transfers`);
      await card.getByText(`Expires ${dates[format]}`,{exact:false}).waitFor();
      await page.goto(`${adminURL}/admin/preferences`);
    }
    const admin = await login(t,true);
    await admin.getByRole("navigation",{name:"Main",exact:true}).getByRole("link",{name:"Settings",exact:true}).click();
    assert.equal(await admin.locator("[name=dateFormat]").inputValue(),"yyyy-mm-dd");
    await admin.goto(`${adminURL}/admin/transfers`);
    await admin.locator(".transfer-card").filter({hasText:"date-format.txt"}).getByText(`Expires ${dates["yyyy-mm-dd"]}`,{exact:false}).waitFor();
    const recipient = await browser.newPage(); t.after(() => recipient.close());
    await recipient.goto(share);
    await recipient.locator("time[data-timestamp]").filter({hasText:dates["dd/mm/yyyy"]}).waitFor();
  } finally { assert.equal((await api(page,"/admin/api/preferences","PUT",{dateFormat:"dd/mm/yyyy"})).status,200); }
});

test("copy notifications float without moving content and fade after a fresh timeout", async t => {
  const page = await login(t);
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"], {origin:ownerURL});
  await select(page,"toast-layout.txt",Buffer.from("toast"));
  await page.getByRole("button",{name:"Upload",exact:true}).click();
  await publishedResult(page);
  await page.setViewportSize({width:390,height:844});
  await page.clock.install();
  const content = page.locator("#share-result");
  const before = await content.boundingBox();
  const copy = page.getByRole("button",{name:"Copy link",exact:true});
  const toast = page.locator("#notice");
  await copy.click();
  await page.getByText("Link copied.",{exact:true}).waitFor();
  assert.deepEqual(await content.boundingBox(),before);
  const box = await toast.boundingBox();
  assert.ok(box.x >= 0 && box.x + box.width <= 390 && box.y + box.height <= 844);
  await page.clock.runFor(3500);
  const repeated = page.evaluate(() => new Promise(resolve => {
    const observer = new MutationObserver(() => { observer.disconnect(); resolve(); });
    observer.observe(document.querySelector("#notice"), {childList:true});
  }));
  await copy.click();
  await repeated;
  await page.clock.runFor(1000);
  assert.equal(await toast.isVisible(),true,"previous notification timer dismissed the new one");
  await page.clock.runFor(3050);
  assert.equal(await toast.evaluate(node => node.classList.contains("dismissing")),true);
  await page.clock.runFor(300);
  assert.equal(await toast.isVisible(),false);
  assert.deepEqual(await content.boundingBox(),before);
});

test("admin transfer owners link by UUID to a briefly highlighted user", async t => {
  const owner = await login(t);
  await select(owner,"owner-link.txt",Buffer.from("user deep link"));
  await owner.getByRole("button",{name:"Upload",exact:true}).click();
  await publishedResult(owner);
  const admin = await login(t,true);
  const users = (await api(admin,"/admin/api/users")).body;
  const user = users.find(item => item.username === "user");
  assert.match(user.id,/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  await admin.goto(`${adminURL}/admin/transfers`);
  const card = admin.locator(".transfer-card").filter({hasText:"owner-link.txt"});
  const link = card.getByRole("link",{name:"user",exact:true});
  assert.equal(await link.getAttribute("href"),`/admin/users#user-${user.id}`);
  // Hold the target request so the clock is installed before highlighting starts.
  await admin.route("**/admin/api/users",async route => {
    await admin.clock.install();
    await route.continue();
  });
  await link.click();
  const target = admin.locator(`[data-user-id="${user.id}"]`);
  await admin.locator(`[data-user-id="${user.id}"].user-highlight`).waitFor();
  assert.equal(admin.url(),`${adminURL}/admin/users#user-${user.id}`);
  assert.equal(await target.getByRole("heading",{name:"user",exact:true}).count(),1);
  assert.equal(await target.evaluate(node => document.activeElement === node),true);
  assert.equal(await admin.locator(".user-highlight").count(),1);
  await admin.clock.runFor(1300);
  assert.equal(await target.evaluate(node => node.classList.contains("user-highlight")),false);
});

test("short draft expiry displays two decimals without changing expiry on an unrelated edit", async t => {
  const page = await login(t);
  const draft = await partialDraft(page,"short-expiry.txt",Buffer.from("short"),0);
  assert.equal((await api(page,`/api/transfers/${draft.id}`,"PATCH",{revision:draft.revision,expirySeconds:10})).status,200);
  await page.goto(`${ownerURL}/transfers`);
  await page.locator(".transfer-card").filter({hasText:"short-expiry.txt"}).getByRole("button",{name:"Edit",exact:true}).click();
  assert.equal(await page.locator("#edit-form [name=expiryHours]").inputValue(),"0.01");
  await page.locator("#edit-form [name=title]").fill("Short draft renamed");
  await page.locator("#edit-form").getByRole("button",{name:"Save",exact:true}).click();
  await page.locator("#edit-dialog").waitFor({state:"hidden"});
  assert.equal((await api(page,`/api/transfers/${draft.id}`)).body.expirySeconds,10);
});

test("an abrupt process crash preserves links and unfinished uploads", {timeout:60000}, async t => {
  const page = await login(t), payload = Buffer.from("recover after an abrupt crash");
  const draft = await partialDraft(page,"crash-resume.txt",payload,8);
  const existing = (await api(page,"/api/transfers")).body.find(item => item.status === "published" && item.files.some(file => !file.deleted));
  assert.ok(existing);
  // Kill the live process without closing SQLite or synchronizing at shutdown.
  await stopServer("SIGKILL");
  await startServer();
  const recipient = await browser.newPage(); t.after(() => recipient.close());
  assert.equal((await recipient.goto(existing.shareUrl)).status(),200);
  await page.goto(`${ownerURL}/upload?resume=${draft.id}`); await readyUpload(page);
  await select(page,"crash-resume.txt",payload);
  await page.getByRole("button",{name:"Resume",exact:true}).click();
  const share = await publishedResult(page);
  const result = await api(page,`/api/transfers/${draft.id}`);
  assert.equal(result.body.files[0].sha256,digest(payload));
  const response = await fetch(`${share}/files/${draft.files[0].id}`);
  assert.equal(response.status,200);
  assert.equal(digest(Buffer.from(await response.arrayBuffer())),digest(payload));
});

test("private admin uploads resume while administrator credentials fail on the public upload listener", async t => {
  const page = await login(t,true);
  const publicSignIn = await page.context().newPage();
  await publicSignIn.goto(`${ownerURL}/login`);
  await publicSignIn.locator("[name=username]").fill("admin");
  await publicSignIn.locator("[name=password]").fill(adminPassword);
  await publicSignIn.getByRole("button",{name:"Sign in",exact:true}).click();
  await publicSignIn.getByText("Incorrect username or password.",{exact:true}).waitFor();
  assert.equal((await api(publicSignIn,"/api/transfers")).status,401);
  const main = page.getByRole("navigation",{name:"Main",exact:true});
  await main.getByRole("link",{name:"Upload",exact:true}).click(); await readyUpload(page);
  const payload = Buffer.alloc(128*1024+13,"a");
  const draft = await partialDraft(page,"private-admin-resume.txt",payload,17);
  await main.getByRole("link",{name:"Transfers",exact:true}).click();
  const card = page.locator(".transfer-card").filter({hasText:"private-admin-resume.txt"});
  await card.getByRole("link",{name:"Resume upload",exact:true}).click(); await readyUpload(page);
  assert.equal(new URL(page.url()).pathname,"/admin/upload");
  await select(page,"private-admin-resume.txt",payload);
  await page.getByRole("button",{name:"Resume",exact:true}).click();
  const share = await publishedResult(page);
  const stored = (await api(page,`/admin/api/personal/transfers/${draft.id}`)).body;
  const response = await fetch(`${share}/files/${stored.files[0].id}`);
  assert.equal(response.status,200);
  assert.deepEqual(Buffer.from(await response.arrayBuffer()),payload);
  await page.locator("#new-upload").click(); await readyUpload(page);
  assert.equal(page.url(),`${adminURL}/admin/upload`);
  const cancelled = await partialDraft(page,"private-admin-cancel.txt",Buffer.from("cancel"),1);
  await page.goto(`${adminURL}/admin/upload?resume=${cancelled.id}`); await readyUpload(page);
  page.once("dialog",dialog => dialog.accept());
  await page.locator("#cancel-upload").click();
  await page.waitForURL(`${adminURL}/admin/upload`); await readyUpload(page);
  assert.equal((await api(page,`/admin/api/personal/transfers/${cancelled.id}`)).body.status,"deleted");
  await main.getByRole("link",{name:"Admin",exact:true}).click();
  await page.getByRole("navigation",{name:"Admin",exact:true}).getByRole("link",{name:"All transfers",exact:true}).click();
  await page.locator(".transfer-card").filter({hasText:"private-admin-resume.txt"}).waitFor();
  assert.equal(new URL(page.url()).origin,adminURL);
});

test("administrator password changes require the current password through Users and Settings", async t => {
  const admin = await login(t,true);
  const second = await login(t,true);
  const users = (await api(admin,"/admin/api/users")).body;
  const account = users.find(user => user.isAdmin);
  const replacement = "replacement-browser-admin-password";
  await admin.locator(`#user-${account.id}`).getByRole("button",{name:"Edit",exact:true}).click();
  const form = admin.locator("#user-form");
  assert.equal(await form.locator("[name=currentPassword]").isVisible(),false);
  await form.locator("[name=password]").fill(replacement);
  assert.equal(await form.locator("[name=currentPassword]").isVisible(),true);
  assert.equal(await form.locator("[name=currentPassword]").getAttribute("required"),"");
  assert.equal((await api(admin,`/admin/api/users/${account.id}`,"PATCH",{password:replacement})).status,403);
  await form.locator("[name=currentPassword]").fill("wrong");
  await form.getByRole("button",{name:"Save",exact:true}).click();
  await admin.locator("#user-error").getByText("Current password is incorrect.",{exact:true}).waitFor();
  await form.locator("[name=currentPassword]").fill(adminPassword);
  await form.getByRole("button",{name:"Save",exact:true}).click();
  await admin.waitForURL("**/login");
  assert.equal((await api(second,"/admin/api/users")).status,401);
  const fresh = await login(t,true,"admin",replacement);
  await fresh.getByRole("navigation",{name:"Main",exact:true}).getByRole("link",{name:"Settings",exact:true}).click();
  const settings = fresh.locator("#password-form");
  await settings.locator("[name=currentPassword]").fill(replacement);
  await settings.locator("[name=newPassword]").fill(adminPassword);
  await settings.getByRole("button",{name:"Change password",exact:true}).click();
  await fresh.waitForURL("**/login");
  await login(t,true);
});
