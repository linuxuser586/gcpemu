// What a trust config's PEM certificates are (FR-LB-009): each one's
// subject, issuer and expiry, read in the browser so that the form shows
// them before anything is sent. It reads only as much DER as that takes,
// and rejects what the Service rejects, with its messages.

/** PemCert is one certificate of a PEM string. */
export interface PemCert {
  /** subject is the certificate's subject, as attribute=value pairs in DER order. */
  subject: string
  issuer: string
  /** notAfter is the end of its validity, as an ISO 8601 time. */
  notAfter: string
}

/** ParsedPem is a PEM string's certificates, or why it has none. */
export interface ParsedPem {
  certs: PemCert[]
  error?: string
}

// Attribute type OIDs, by their DER content bytes in hex.
const ATTRS: Record<string, string> = {
  '550403': 'CN',
  '550406': 'C',
  '550407': 'L',
  '550408': 'ST',
  '55040a': 'O',
  '55040b': 'OU',
  '550405': 'serialNumber',
  '2a864886f70d010901': 'emailAddress',
  '0992268993f22c640119': 'DC',
}

interface Tlv {
  tag: number
  /** start and end bound the value's bytes. */
  start: number
  end: number
}

class DerError extends Error {}

/** read is the TLV at offset i of b. */
function read(b: Uint8Array, i: number): Tlv {
  if (i + 2 > b.length) throw new DerError('truncated DER')
  const tag = b[i]!
  let len = b[i + 1]!
  let start = i + 2
  if (len & 0x80) {
    const n = len & 0x7f
    if (n === 0 || n > 4 || start + n > b.length) throw new DerError('malformed DER length')
    len = 0
    for (let k = 0; k < n; k++) len = len * 256 + b[start + k]!
    start += n
  }
  const end = start + len
  if (end > b.length) throw new DerError('truncated DER')
  return { tag, start, end }
}

/** children are the TLVs inside constructed value t. */
function children(b: Uint8Array, t: Tlv): Tlv[] {
  const out: Tlv[] = []
  for (let i = t.start; i < t.end;) {
    const c = read(b, i)
    out.push(c)
    i = c.end
  }
  return out
}

const hex = (b: Uint8Array, t: Tlv) =>
  Array.from(b.subarray(t.start, t.end), (x) => x.toString(16).padStart(2, '0')).join('')

const text = (b: Uint8Array, t: Tlv) => new TextDecoder().decode(b.subarray(t.start, t.end))

/** name formats an X.501 Name as attribute=value pairs. */
function name(b: Uint8Array, t: Tlv): string {
  return children(b, t)
    .flatMap((rdn) => children(b, rdn))
    .map((atv) => {
      const [type, value] = children(b, atv)
      if (!type || !value) throw new DerError('malformed name')
      const h = hex(b, type)
      return `${ATTRS[h] ?? h}=${text(b, value)}`
    })
    .join(', ')
}

/** time reads a UTCTime (0x17) or GeneralizedTime (0x18) as ISO 8601. */
function time(b: Uint8Array, t: Tlv): string {
  const s = text(b, t)
  const m =
    t.tag === 0x17
      ? /^(\d{2})(\d{2})(\d{2})(\d{2})(\d{2})(\d{2})Z$/.exec(s)
      : /^(\d{4})(\d{2})(\d{2})(\d{2})(\d{2})(\d{2})Z$/.exec(s)
  if (!m) throw new DerError(`malformed time ${JSON.stringify(s)}`)
  let year = Number(m[1])
  // RFC 5280: a UTCTime year of 50 or more is 19xx.
  if (t.tag === 0x17) year += year >= 50 ? 1900 : 2000
  const [mo, d, h, mi, sec] = m.slice(2).map(Number) as [number, number, number, number, number]
  return new Date(Date.UTC(year, mo - 1, d, h, mi, sec)).toISOString()
}

/** certificate reads one DER certificate. */
function certificate(b: Uint8Array): PemCert {
  const [tbs] = children(b, read(b, 0))
  if (!tbs) throw new DerError('malformed certificate')
  const f = children(b, tbs)
  // An explicit [0] version comes first in v2 and v3 certificates.
  const at = f[0]?.tag === 0xa0 ? 1 : 0
  const [issuer, validity, subject] = [f[at + 2], f[at + 3], f[at + 4]]
  if (!issuer || !validity || !subject) throw new DerError('malformed certificate')
  const [, notAfter] = children(b, validity)
  if (!notAfter) throw new DerError('malformed validity')
  return { subject: name(b, subject), issuer: name(b, issuer), notAfter: time(b, notAfter) }
}

const BLOCK = /-----BEGIN ([A-Z0-9 ]+)-----([\s\S]*?)-----END \1-----/g

/** parsePem reads every CERTIFICATE block of a PEM string, as the Service does. */
export function parsePem(pem: string): ParsedPem {
  const certs: PemCert[] = []
  for (const [, type, body] of pem.matchAll(BLOCK)) {
    if (type !== 'CERTIFICATE') continue
    let der: Uint8Array
    try {
      der = Uint8Array.from(atob(body!.replace(/\s+/g, '')), (c) => c.charCodeAt(0))
    } catch {
      // Go's pem.Decode skips a block that is not base64, as if it were not there.
      continue
    }
    try {
      certs.push(certificate(der))
    } catch (e) {
      return { certs: [], error: e instanceof DerError ? e.message : String(e) }
    }
  }
  if (certs.length === 0) return { certs, error: 'no CERTIFICATE block found.' }
  return { certs }
}

/** expired reports whether c's validity has ended, by the browser's clock. */
export const expired = (c: PemCert) => Date.parse(c.notAfter) < Date.now()

/** pemError is the Service's message for a PEM string it rejects in field, if it does. */
export function pemError(field: string, pem: string): string | undefined {
  const { error } = parsePem(pem)
  return error && `Invalid PEM certificate in ${field}: ${error}`
}
