import assert from 'node:assert/strict'
import { test } from 'node:test'

// #1202: package.json overrides unzip-stream with stubs/unzip-stream, so
// the action ships no unlicensed buffers. Imported as @actions/artifact's
// download path imports it.
import unzip from 'unzip-stream'

for (const name of ['Parse', 'Extract']) {
  test(`unzip-stream stub: ${name} throws`, () => {
    assert.throws(() => unzip[name](), {
      message: 'setup-gcpemu does not download artifacts; unzip-stream is stubbed, see #1202',
    })
  })
}
