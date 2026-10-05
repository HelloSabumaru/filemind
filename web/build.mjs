import { build } from 'esbuild';
import { mkdir, copyFile } from 'node:fs/promises';
const target = 'internal/filemind/web/dist';
await mkdir(target, { recursive: true });
await build({ entryPoints: ['web/app.js'], bundle: true, minify: true, outfile: `${target}/app.js`, target: ['es2022'], legalComments: 'eof' });
await Promise.all(['style.css', 'icon.svg'].map(name => copyFile(`web/${name}`, `${target}/${name}`)));
