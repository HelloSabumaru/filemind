import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import { access, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
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
  const result = await api(page,"/api/transfers","POST",{title:name,files:[{name,size:payload.length,sha256:digest(payload)}]});
  assert.equal(result.status,200);
  const draft = result.body;
  await page.evaluate(async ({fileID,prefix,size}) => {
    const csrf = document.querySelector('meta[name="csrf-token"]').content;
    const created = await fetch("/uploads/",{method:"POST",headers:{"X-CSRF-Token":csrf,"Tus-Resumable":"1.0.0","Upload-Length":String(size),"Upload-Metadata":`file_id ${btoa(fileID)}`}});
    if (created.status !== 201) throw new Error(`Create failed: ${created.status}`);
    if (prefix.length) {
      const patched = await fetch(`/uploads/${fileID}`,{method:"PATCH",headers:{"X-CSRF-Token":csrf,"Tus-Resumable":"1.0.0","Upload-Offset":"0","Content-Type":"application/offset+octet-stream"},body:prefix});
      if (patched.status !== 204) throw new Error(`Patch failed: ${patched.status}`);
    }
  },{fileID:draft.files[0].id,prefix:payload.subarray(0,prefixLength).toString(),size:payload.length});
  return draft;
}

async function select(page, name, payload) {
  await page.locator("#file-picker").setInputFiles({name,mimeType:"application/octet-stream",buffer:payload});
}

async function publishedResult(page) {
  await page.locator("#share-result").waitFor({state:"visible"});
  return page.locator("#share-url").inputValue();
}

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
  assert.equal(transfer.files[0].sha256,digest(payload));
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
  await admin.locator("#user-form [name=password]").fill("browser-user-password-for-validation");
  await admin.locator("#user-form").getByRole("button",{name:"Save",exact:true}).click();
  await admin.locator("#user-dialog").waitFor({state:"hidden"});
  const user = await login(t,false,"browser-account","browser-user-password-for-validation");
  await user.goto(`${ownerURL}/account`);
  await user.locator("[name=currentPassword]").fill("browser-user-password-for-validation");
  await user.locator("[name=newPassword]").fill("browser-new-password-for-validation");
  await user.getByRole("button",{name:"Change password",exact:true}).click();
  await user.waitForURL("**/login");
  await user.locator("[name=username]").fill("browser-account");
  await user.locator("[name=password]").fill("browser-new-password-for-validation");
  await user.getByRole("button",{name:"Sign in",exact:true}).click(); await user.waitForURL("**/upload");
  admin.on("dialog",dialog => dialog.accept());
  const card = admin.locator(".transfer-card").filter({has:admin.getByRole("heading",{name:"browser-account",exact:true})});
  await card.getByRole("button",{name:"Delete",exact:true}).click();
  await card.waitFor({state:"hidden"});
  assert.equal((await api(user,"/api/transfers")).status,401);
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
