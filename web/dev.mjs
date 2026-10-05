import { mkdir, writeFile } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const root = fileURLToPath(new URL('../', import.meta.url));
const local = join(root, '.local');
const binary = join(local, 'filemind');
const passwordFile = join(local, 'owner-password');
const password = 'local-test-password-1234';
let child;
let stopping = false;

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    stopping = true;
    process.exitCode = signal === 'SIGINT' ? 130 : 143;
    child?.kill(signal);
  });
}

function run(command, args, env) {
  if (stopping) return Promise.resolve(process.exitCode);
  return new Promise((resolve, reject) => {
    child = spawn(command, args, { cwd: root, env, stdio: 'inherit' });
    child.once('error', reject);
    child.once('close', (code, signal) => {
      child = undefined;
      resolve(code ?? (signal === 'SIGINT' ? 130 : 143));
    });
  });
}

try {
  await mkdir(local, { recursive: true, mode: 0o700 });
  const buildCode = await run('go', ['build', '-o', binary, './cmd/filemind'], {
    ...process.env,
    GOTOOLCHAIN: 'local',
  });
  if (buildCode !== 0 || stopping) {
    process.exitCode = stopping ? process.exitCode : buildCode;
  } else {
    await writeFile(passwordFile, `${password}\n`, { mode: 0o600 });
    const env = { ...process.env };
    for (const key of Object.keys(env)) {
      if (key.startsWith('FILEMIND_')) delete env[key];
    }
    Object.assign(env, {
      FILEMIND_INSECURE_DEVELOPMENT: 'true',
      FILEMIND_OWNER_LISTEN: '127.0.0.1:9080',
      FILEMIND_PUBLIC_LISTEN: '127.0.0.1:9081',
      FILEMIND_OWNER_URL: 'http://localhost:9080',
      FILEMIND_PUBLIC_URL: 'http://localhost:9081',
      FILEMIND_OWNER_USERNAME: 'admin',
      FILEMIND_DATA_DIR: join(local, 'data'),
      FILEMIND_OWNER_PASSWORD_FILE: passwordFile,
    });
    console.log('Local development: http://localhost:9080/admin');
    console.log('Login: admin. Password is in .local/owner-password.');
    console.log('Test data persists in .local/data. Press Ctrl+C to stop.');
    process.exitCode = await run(binary, [], env);
  }
} catch (error) {
  console.error(`Local launch failed: ${error.message}`);
  process.exitCode = 1;
}
