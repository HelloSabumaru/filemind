import { copyFile, mkdir, rm, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';

const root = new URL('../../', import.meta.url);
const target = new URL('.local/demo/', root);
await rm(target, { recursive: true, force: true });
await mkdir(target, { recursive: true });
await build({ entryPoints: [fileURLToPath(new URL('demo.js', import.meta.url))], bundle: true, minify: true, outfile: fileURLToPath(new URL('demo.js', target)), target: ['es2022'] });
await Promise.all([
  ...['index.html', 'demo.css'].map(name => copyFile(new URL(name, import.meta.url), new URL(name, target))),
  ...['style.css', 'icon.svg'].map(name => copyFile(new URL(`web/${name}`, root), new URL(name, target))),
  copyFile(new URL('LICENSE', root), new URL('LICENSE', target)),
  writeFile(new URL('.nojekyll', target), ''),
]);
console.log('Built demo in .local/demo');
