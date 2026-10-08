import { createHash } from 'node:crypto'

import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'

import { server } from '@/test/setup'

import { goog4Date, resourcePath, sha256, signedUrl, stringToSign, v4Escape } from './signedUrl'

const hex = (b: Uint8Array) => Buffer.from(b).toString('hex')
const nodeSha = (s: string | Uint8Array) => createHash('sha256').update(s).digest('hex')

it('hashes like SHA-256 at every padding boundary', () => {
  expect(hex(sha256(new Uint8Array()))).toBe(nodeSha(''))
  for (const n of [1, 55, 56, 63, 64, 65, 119, 120, 1000]) {
    const data = Uint8Array.from({ length: n }, (_, i) => (i * 31 + 7) & 0xff)
    expect(hex(sha256(data)), `length ${n}`).toBe(nodeSha(data))
  }
})

it('escapes paths and queries as V4 does', () => {
  expect(v4Escape("a b!'()*~/")).toBe('a%20b%21%27%28%29%2A~%2F')
  expect(resourcePath('bkt', 'dir/file name.txt')).toBe('/bkt/dir/file%20name.txt')
  expect(goog4Date(new Date('2026-10-08T12:34:56.789Z'))).toBe('20261008T123456Z')
})

const req = {
  bucket: 'bkt',
  object: 'dir/a b.txt',
  method: 'GET' as const,
  expiresSeconds: 600,
  serviceAccount: 'signer@p.iam.gserviceaccount.com',
  host: 'localhost:4510',
  now: new Date('2026-10-08T12:00:00Z'),
}

it('writes the canonical request gcloud writes', async () => {
  const { query, toSign } = await stringToSign(req)
  const scope = '20261008/auto/storage/goog4_request'
  expect(query).toBe(
    'X-Goog-Algorithm=GOOG4-RSA-SHA256' +
      `&X-Goog-Credential=signer%40p.iam.gserviceaccount.com%2F${scope.replaceAll('/', '%2F')}` +
      '&X-Goog-Date=20261008T120000Z&X-Goog-Expires=600&X-Goog-SignedHeaders=host',
  )
  const canonical = [
    'GET',
    '/bkt/dir/a%20b.txt',
    query,
    'host:localhost:4510\n',
    'host',
    'UNSIGNED-PAYLOAD',
  ].join('\n')
  expect(toSign).toBe(`GOOG4-RSA-SHA256\n20261008T120000Z\n${scope}\n${nodeSha(canonical)}`)
})

it('signs with signBlob and appends the hex signature', async () => {
  let payload = ''
  server.use(
    http.post('*/iamcredentials/v1/projects/-/serviceAccounts/:sa', async ({ request, params }) => {
      expect(params.sa).toBe('signer@p.iam.gserviceaccount.com:signBlob')
      payload = ((await request.json()) as { payload: string }).payload
      return HttpResponse.json({ keyId: 'k1', signedBlob: btoa('\x01\xab\xff') })
    }),
  )
  const url = await signedUrl(req)
  const { toSign, query } = await stringToSign(req)
  expect(atob(payload)).toBe(toSign)
  expect(url).toBe(`http://localhost:4510/bkt/dir/a%20b.txt?${query}&X-Goog-Signature=01abff`)
})
