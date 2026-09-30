import { build } from 'esbuild';
import { mkdir, copyFile, cp, writeFile, rm } from 'node:fs/promises';

const outputDirectory = 'backend/dist';

await mkdir(outputDirectory, { recursive: true });
await rm(`${outputDirectory}/chunks`, { recursive: true, force: true });

const result = await build({
  entryPoints: ['web/app.js'],
  bundle: true,
  splitting: true,
  format: 'esm',
  minify: true,
  target: ['es2022'],
  outdir: outputDirectory,
  chunkNames: 'chunks/[name]-[hash]',
  metafile: true,
  legalComments: 'linked'
});

await copyFile('web/index.html', `${outputDirectory}/index.html`);
await cp('web/assets', `${outputDirectory}/assets`, { recursive: true });
const bytes = Object.fromEntries(
  Object.entries(result.metafile.outputs).map(([name, info]) => [name, info.bytes])
);
await writeFile(`${outputDirectory}/build-meta.json`, JSON.stringify({ bytes }, null, 2));
console.log('Frontend built and ready for go:embed.');
