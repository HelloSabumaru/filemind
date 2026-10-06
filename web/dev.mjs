import { mkdir, writeFile } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const root = fileURLToPath(new URL('../', import.meta.url));
const local = join(root, '.local');
const binary = join(local, 'filemind');
const passwordFile = join(local, 'owner-password');
const password = 'password';
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
    GOTOOLCHAIN: 'auto',
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
      FILEMIND_DEMO: 'true',
      FILEMIND_OWNER_LISTEN: '127.0.0.1:9080',
      FILEMIND_PUBLIC_LISTEN: '127.0.0.1:9081',
      FILEMIND_ADMIN_LISTEN: '127.0.0.1:9082',
      FILEMIND_OWNER_URL: 'http://localhost:9080',
      FILEMIND_PUBLIC_URL: 'http://localhost:9081',
      FILEMIND_ADMIN_URL: 'http://localhost:9082',
      FILEMIND_OWNER_USERNAME: 'admin',
      FILEMIND_DATA_DIR: join(local, 'data-v2'),
      FILEMIND_OWNER_PASSWORD_FILE: passwordFile,
    });
    console.log('Uploads: http://localhost:9080/login — initial demo login: user / password.');
    console.log('Admin: http://localhost:9082/login — initial admin login: admin / password.');
    console.log('Administrator sign-in also works on port 9080. Production can restrict it with FILEMIND_REQUIRE_PRIVATE_ADMIN_SIGN_IN=true.');
    console.log('Initial administrator password is in .local/owner-password; later account changes persist.');
    console.log('Test data persists in .local/data-v2. Press Ctrl+C to stop.');
    process.exitCode = await run(binary, [], env);
  }
} catch (error) {
  console.error(`Local launch failed: ${error.message}`);
  process.exitCode = 1;
}
