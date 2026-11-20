import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { build } from 'esbuild'

// #1202, ADR 0090: unzip-stream is stubbed, which holds only while the
// action never runs @actions/artifact's download path.

const root = fileURLToPath(new URL('..', import.meta.url))

// The entry points and target of `npm run build`; minifying and the banner
// don't change which modules are bundled.
const { metafile } = await build({
  absWorkingDir: root,
  entryPoints: ['src/main.js', 'src/post.js'],
  bundle: true,
  platform: 'node',
  target: 'node24',
  format: 'esm',
  outdir: 'dist',
  write: false,
  metafile: true,
  logLevel: 'silent',
})

test('bundles ship none of the real unzip-stream or its dependencies', () => {
  for (const [out, { inputs }] of Object.entries(metafile.outputs)) {
    const shipped = Object.keys(inputs).filter(p => /node_modules\/(unzip-stream|binary|buffers|chainsaw|traverse)\//.test(p))
    assert.deepEqual(shipped, [], `${out} bundles them`)
  }
})

test('only @actions/artifact downloads import unzip-stream', () => {
  const importers = Object.entries(metafile.inputs)
    .filter(([, { imports }]) => imports.some(i => i.original === 'unzip-stream'))
    .map(([p]) => p)
  assert.ok(importers.length > 0, 'nothing imports unzip-stream; drop the stub and this test')
  for (const p of importers) {
    assert.match(p, /node_modules\/@actions\/artifact\/lib\/internal\/download\//, `${p} imports unzip-stream`)
  }
})
