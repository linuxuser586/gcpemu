import { expect, it } from 'vitest'

import { parsePem, pemError } from './pem'
import { INTERMEDIATE_PEM, ROOT_PEM } from './testcerts'

it('reads the subject, issuer and expiry of each certificate of a PEM string', () => {
  expect(parsePem(`${ROOT_PEM}\n${INTERMEDIATE_PEM}`)).toEqual({
    certs: [
      {
        subject: 'C=US, O=Example Corp, CN=Example Root CA',
        issuer: 'C=US, O=Example Corp, CN=Example Root CA',
        notAfter: '2036-01-01T00:00:00.000Z',
      },
      {
        subject: 'O=Example Corp, OU=Edge, CN=Example Issuing CA',
        issuer: 'C=US, O=Example Corp, CN=Example Root CA',
        notAfter: '2051-01-01T00:00:00.000Z',
      },
    ],
  })
})

it('rejects what the Service rejects, with its message', () => {
  const field = 'trust_stores.trust_anchors.pem_certificate'
  expect(pemError(field, 'not a certificate')).toBe(
    `Invalid PEM certificate in ${field}: no CERTIFICATE block found.`,
  )
  expect(pemError(field, '-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----')).toBe(
    `Invalid PEM certificate in ${field}: no CERTIFICATE block found.`,
  )
  expect(
    pemError(field, '-----BEGIN CERTIFICATE-----\nMAMCAQA=\n-----END CERTIFICATE-----'),
  ).toMatch(new RegExp(`^Invalid PEM certificate in ${field}: `))
  expect(pemError(field, ROOT_PEM)).toBeUndefined()
})

it('skips a block that is not base64, as the Service does', () => {
  const bad = '-----BEGIN CERTIFICATE-----\n!!not base64!!\n-----END CERTIFICATE-----'
  expect(parsePem(`${bad}\n${ROOT_PEM}`).certs.map((c) => c.subject)).toEqual([
    'C=US, O=Example Corp, CN=Example Root CA',
  ])
  expect(parsePem(bad).error).toBe('no CERTIFICATE block found.')
})
