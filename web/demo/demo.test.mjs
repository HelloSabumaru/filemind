import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { access, readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { after, before, test } from 'node:test';
import { chromium } from 'playwright';

let browser, server, base;
const root = new URL('../../.local/demo/', import.meta.url);
const types = { html: 'text/html', js: 'text/javascript', css: 'text/css', svg: 'image/svg+xml' };
before(async () => {
  server = createServer(async (request, response) => {
    const path = new URL(request.url, 'http://localhost').pathname;
    const name = path === '/filemind/' ? 'index.html' : path.slice('/filemind/'.length);
    if (!path.startsWith('/filemind/') || !/^[\w.-]+$/.test(name)) { response.writeHead(404).end(); return; }
    try {
      const data = await readFile(new URL(name, root));
      response.writeHead(200, { 'Content-Type': types[name.split('.').pop()] || 'text/plain' }).end(data);
    } catch { response.writeHead(404).end(); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  base = `http://127.0.0.1:${server.address().port}/filemind/`;
  let executablePath = process.env.FILEMIND_TEST_CHROME;
  if (!executablePath) { try { await access('/usr/bin/google-chrome'); executablePath = '/usr/bin/google-chrome'; } catch {} }
  browser = await chromium.launch({ executablePath, headless: true, args: ['--no-sandbox'] });
});
after(async () => {
  await browser?.close();
  if (server) await new Promise(resolve => server.close(resolve));
});
async function visit(t, hash = '', viewport) {
  const context = await browser.newContext({ acceptDownloads: true, viewport });
  t.after(() => context.close());
  const page = await context.newPage(), errors = [], requests = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => requests.push({ method: request.method(), url: request.url() }));
  t.after(() => {
    assert.deepEqual(errors, []);
    assert.ok(requests.every(request => request.method === 'GET' && request.url.startsWith(base)), 'Demo must only request its own static assets');
  });
  await page.goto(`${base}${hash}`);
  await page.locator('#view h1').waitFor();
  return page;
}
async function navigate(page, name) { await page.getByRole('navigation', { name: 'Main', exact: true }).getByRole('link', { name, exact: true }).click(); }
async function confirm(page, name) { await page.getByRole('dialog').getByRole('button', { name, exact: true }).click(); await page.getByRole('dialog').waitFor({ state: 'hidden' }); }

test('sample upload pauses, resumes, survives navigation, and publishes a preview', async t => {
  const page = await visit(t);
  await page.getByRole('button', { name: 'Use sample files' }).click();
  await page.getByRole('textbox', { name: 'Name', exact: true }).fill('Demo handoff');
  await page.getByRole('button', { name: 'Upload', exact: true }).click();
  await page.getByRole('button', { name: 'Pause', exact: true }).click();
  const progress = await page.locator('progress').first().getAttribute('value');
  await navigate(page, 'Transfers');
  const transfer = page.locator('article').filter({ has: page.getByRole('heading', { name: 'Demo handoff' }) });
  await transfer.getByRole('link', { name: 'Resume upload' }).click();
  assert.equal(await page.locator('progress').first().getAttribute('value'), progress);
  await page.reload();
  await page.getByRole('button', { name: 'Resume upload' }).click();
  await page.getByRole('heading', { name: 'Share', exact: true }).waitFor();
  const url = await page.getByRole('textbox', { name: 'Share link' }).inputValue();
  assert.ok(url.startsWith(`${base}#share/`));
  await page.getByRole('link', { name: 'Preview shared link' }).click();
  await page.getByRole('heading', { name: 'Demo handoff', level: 1 }).waitFor();
  assert.equal(await page.getByRole('button', { name: 'Download', exact: true }).count(), 2);
});

test('selected file contents never leave the browser and filenames render as text', async t => {
  const page = await visit(t);
  await page.getByLabel('Choose files', { exact: true }).setInputFiles({ name: '<img src=x onerror=alert(1)>.txt', mimeType: 'text/plain', buffer: Buffer.from('PRIVATE FILE CONTENT') });
  assert.equal(await page.locator('#selected-files strong').textContent(), '<img src=x onerror=alert(1)>.txt');
  assert.equal(await page.locator('#selected-files img').count(), 0);
  await page.getByRole('button', { name: 'Upload', exact: true }).click();
  await page.getByRole('heading', { name: 'Share', exact: true }).waitFor();
  await page.getByRole('link', { name: 'Preview shared link' }).click();
  const ready = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Download', exact: true }).click();
  const result = await ready;
  const content = await readFile(await result.path(), 'utf8');
  assert.ok(content.includes('generated sample content'));
  assert.ok(!content.includes('PRIVATE FILE CONTENT'));
});

test('password previews, valid ZIP downloads and download limits work', async t => {
  const page = await visit(t, '#share/project-handoff');
  await page.getByLabel('Password', { exact: true }).fill('wrong');
  await page.getByRole('button', { name: 'Unlock' }).click();
  await page.getByRole('alert').waitFor();
  await page.getByLabel('Password', { exact: true }).fill('demo');
  await page.getByRole('button', { name: 'Unlock' }).click();
  await page.getByRole('button', { name: 'Download ZIP' }).waitFor();
  const ready = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Download ZIP' }).click();
  const archive = await ready;
  assert.equal(archive.suggestedFilename(), 'Project handoff.zip');
  execFileSync('python3', ['-c', 'import sys, zipfile\nwith zipfile.ZipFile(sys.argv[1]) as z:\n assert z.testzip() is None\n assert len(z.namelist()) == 2\n assert all(b"generated sample content" in z.read(n) for n in z.namelist())', await archive.path()]);
  await navigate(page, 'Transfers');
  const transfer = page.locator('article').filter({ has: page.getByRole('heading', { name: 'Project handoff', exact: true }) });
  await transfer.getByRole('button', { name: 'Edit', exact: true }).click();
  await page.getByRole('dialog').getByLabel('Downloads per file', { exact: true }).fill('1');
  await confirm(page, 'Save');
  await transfer.getByRole('link', { name: 'Preview link' }).click();
  await page.getByLabel('Password', { exact: true }).fill('demo');
  await page.getByRole('button', { name: 'Unlock' }).click();
  assert.equal(await page.getByRole('button', { name: 'Download', exact: true }).count(), 0);
  assert.equal(await page.getByText('Unavailable', { exact: true }).count(), 2);
});

test('transfer editing, search, revoke and delete persist until reset', async t => {
  const page = await visit(t, '#transfers');
  await page.getByRole('searchbox').fill('logo.svg');
  assert.equal(await page.locator('article').count(), 1);
  await page.getByRole('button', { name: 'Edit', exact: true }).click();
  await page.getByRole('dialog').getByRole('textbox', { name: 'Name', exact: true }).fill('New brand kit');
  await confirm(page, 'Save');
  const transfer = page.locator('article').filter({ has: page.getByRole('heading', { name: 'New brand kit' }) });
  await transfer.getByRole('button', { name: 'Revoke' }).click();
  await confirm(page, 'Revoke link');
  await page.goto(`${base}#share/brand-kit`);
  await page.getByRole('heading', { name: 'Link unavailable' }).waitFor();
  await navigate(page, 'Transfers');
  await transfer.getByRole('button', { name: 'Delete', exact: true }).click();
  await confirm(page, 'Delete transfer');
  await page.reload();
  assert.equal(await page.getByRole('heading', { name: 'New brand kit' }).count(), 0);
  await page.getByRole('button', { name: 'Reset demo' }).click();
  await confirm(page, 'Reset demo');
  await navigate(page, 'Transfers');
  await page.getByRole('heading', { name: 'Brand kit', exact: true }).waitFor();
});

test('admin user actions and settings are interactive', async t => {
  const page = await visit(t, '#users');
  await page.getByRole('button', { name: 'Add user' }).click();
  await page.getByRole('dialog').getByLabel('Username', { exact: true }).fill('sam');
  await page.getByRole('dialog').getByLabel('Demo password', { exact: true }).fill('sample');
  await confirm(page, 'Save');
  const account = page.locator('article').filter({ has: page.getByRole('heading', { name: 'sam', exact: true }) });
  await account.getByRole('button', { name: 'Disable', exact: true }).click();
  assert.equal(await account.getByText('Disabled', { exact: true }).count(), 1);
  await account.getByRole('button', { name: 'Enable', exact: true }).click();
  await account.getByRole('button', { name: 'Delete', exact: true }).click();
  await confirm(page, 'Delete user');
  assert.equal(await page.getByRole('heading', { name: 'sam', exact: true }).count(), 0);
  await page.getByRole('navigation', { name: 'Admin', exact: true }).getByRole('link', { name: 'Server settings' }).click();
  await page.getByLabel('Default expiry (hours)', { exact: true }).fill('48');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await navigate(page, 'Upload');
  await page.getByRole('button', { name: 'Use sample files' }).click();
  assert.equal(await page.getByLabel('Expiry duration', { exact: true }).inputValue(), '48');
  await navigate(page, 'Settings');
  await page.getByLabel('Date format').selectOption('yyyy-mm-dd');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await navigate(page, 'Transfers');
  assert.match(await page.locator('article').first().textContent(), /Expires \d{4}-\d{2}-\d{2}/);
});

test('mobile layout, theme switching and direct preview links work', async t => {
  const page = await visit(t, '#share/brand-kit', { width: 375, height: 812 });
  await page.getByRole('heading', { name: 'Brand kit', exact: true }).waitFor();
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  await page.locator('#theme-toggle').click();
  const theme = await page.evaluate(() => document.documentElement.dataset.theme);
  await page.reload();
  assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), theme);
  await page.goto(`${base}#share/missing`);
  await page.getByRole('heading', { name: 'Preview unavailable' }).waitFor();
});
