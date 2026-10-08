import { gcpFetch } from '@/api/fetch'

// V4 signed URLs (FR-GCS-006), built the way gcloud storage sign-url
// --impersonate-service-account builds them: the console writes the
// canonical request and has IAM Credentials signBlob sign it with the
// service account's system-managed key. The console holds no key.

export const MAX_EXPIRY_SECONDS = 7 * 24 * 3600
export const SIGNED_METHODS = ['GET', 'PUT', 'HEAD', 'DELETE'] as const
export type SignedMethod = (typeof SIGNED_METHODS)[number]

const ALGORITHM = 'GOOG4-RSA-SHA256'

/** v4Escape percent-encodes all but unreserved characters, as V4 requires. */
export const v4Escape = (s: string) =>
  encodeURIComponent(s).replace(/[!'()*]/g, (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`)

/** resourcePath is /BUCKET/OBJECT with the object's slashes kept. */
export const resourcePath = (bucket: string, object: string) =>
  `/${v4Escape(bucket)}/${object.split('/').map(v4Escape).join('/')}`

/** goog4Date formats t as X-Goog-Date: yyyymmddThhmmssZ. */
export const goog4Date = (t: Date) => t.toISOString().replace(/[-:]|\.\d{3}/g, '')

export interface SignRequest {
  bucket: string
  object: string
  method: SignedMethod
  expiresSeconds: number
  serviceAccount: string
  /** host is the gateway's host:port, which signed URLs are sent to. */
  host: string
  now: Date
}

/** stringToSign returns the V4 string to sign and the URL's query. */
export async function stringToSign(r: SignRequest) {
  const date = goog4Date(r.now)
  const scope = `${date.slice(0, 8)}/auto/storage/goog4_request`
  const params: [string, string][] = [
    ['X-Goog-Algorithm', ALGORITHM],
    ['X-Goog-Credential', `${r.serviceAccount}/${scope}`],
    ['X-Goog-Date', date],
    ['X-Goog-Expires', String(r.expiresSeconds)],
    ['X-Goog-SignedHeaders', 'host'],
  ]
  const query = params.map(([k, v]) => `${v4Escape(k)}=${v4Escape(v)}`).join('&')
  const canonical = [
    r.method,
    resourcePath(r.bucket, r.object),
    query,
    `host:${r.host}\n`,
    'host',
    'UNSIGNED-PAYLOAD',
  ].join('\n')
  return {
    query,
    toSign: [ALGORITHM, date, scope, await sha256Hex(canonical)].join('\n'),
  }
}

/** signedUrl signs a URL for one object with a service account. */
export async function signedUrl(r: SignRequest, protocol = 'http:') {
  const { query, toSign } = await stringToSign(r)
  const res = await gcpFetch<{ keyId: string; signedBlob: string }>(
    `/iamcredentials/v1/projects/-/serviceAccounts/${encodeURIComponent(r.serviceAccount)}:signBlob`,
    { method: 'POST', body: JSON.stringify({ payload: base64(toSign) }) },
  )
  const signature = hex(Uint8Array.from(atob(res.signedBlob), (c) => c.charCodeAt(0)))
  return `${protocol}//${r.host}${resourcePath(r.bucket, r.object)}?${query}&X-Goog-Signature=${signature}`
}

function base64(s: string) {
  let bin = ''
  for (const b of new TextEncoder().encode(s)) bin += String.fromCharCode(b)
  return btoa(bin)
}

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')

/**
 * sha256Hex hashes s. crypto.subtle exists only in secure contexts, so a
 * console reached over plain HTTP on a LAN address uses sha256 below.
 */
export async function sha256Hex(s: string): Promise<string> {
  const data = new TextEncoder().encode(s)
  const subtle = globalThis.crypto?.subtle
  if (subtle) return hex(new Uint8Array(await subtle.digest('SHA-256', data)))
  return hex(sha256(data))
}

const K = Uint32Array.from([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
])

/** sha256 is FIPS 180-4 SHA-256. */
export function sha256(data: Uint8Array): Uint8Array {
  const len = data.length
  const padded = new Uint8Array(Math.ceil((len + 9) / 64) * 64)
  padded.set(data)
  padded[len] = 0x80
  const view = new DataView(padded.buffer)
  view.setUint32(padded.length - 8, Math.floor(len / 0x20000000))
  view.setUint32(padded.length - 4, (len << 3) >>> 0)
  const h = Uint32Array.from([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ])
  const w = new Uint32Array(64)
  const rotr = (x: number, n: number) => (x >>> n) | (x << (32 - n))
  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = view.getUint32(off + i * 4)
    for (let i = 16; i < 64; i++) {
      const a = w[i - 15]!
      const b = w[i - 2]!
      const s0 = rotr(a, 7) ^ rotr(a, 18) ^ (a >>> 3)
      const s1 = rotr(b, 17) ^ rotr(b, 19) ^ (b >>> 10)
      w[i] = (w[i - 16]! + s0 + w[i - 7]! + s1) >>> 0
    }
    let [a, b, c, d, e, f, g, hh] = h as unknown as number[]
    for (let i = 0; i < 64; i++) {
      const t1 =
        (hh! +
          (rotr(e!, 6) ^ rotr(e!, 11) ^ rotr(e!, 25)) +
          ((e! & f!) ^ (~e! & g!)) +
          K[i]! +
          w[i]!) >>>
        0
      const t2 =
        ((rotr(a!, 2) ^ rotr(a!, 13) ^ rotr(a!, 22)) + ((a! & b!) ^ (a! & c!) ^ (b! & c!))) >>> 0
      hh = g
      g = f
      f = e
      e = (d! + t1) >>> 0
      d = c
      c = b
      b = a
      a = (t1 + t2) >>> 0
    }
    const out = [a, b, c, d, e, f, g, hh]
    for (let i = 0; i < 8; i++) h[i] = (h[i]! + out[i]!) >>> 0
  }
  const digest = new Uint8Array(32)
  const dv = new DataView(digest.buffer)
  for (let i = 0; i < 8; i++) dv.setUint32(i * 4, h[i]!)
  return digest
}
