import { time } from '../gcs/format'
import { expired as hasExpired, parsePem } from './pem'

/**
 * PemSummary is what each certificate of a PEM string is: its subject,
 * the issuer when another CA signed it, and when it expires (or that it
 * has, by the browser's clock).
 */
export function PemSummary({ pem }: { pem: string }) {
  const { certs, error } = parsePem(pem)
  if (error) {
    return <p className="text-sm text-destructive">Cannot read the certificate: {error}</p>
  }
  return (
    <ul className="flex flex-col gap-1 text-sm" data-testid="pem-summary">
      {certs.map((c, i) => {
        const expired = hasExpired(c)
        return (
          <li key={i}>
            <span className="font-mono break-all">{c.subject}</span>
            {c.issuer !== c.subject && (
              <span className="block text-xs text-muted-foreground">
                Issued by <span className="font-mono">{c.issuer}</span>
              </span>
            )}
            <span
              className={
                expired ? 'block text-xs text-destructive' : 'block text-xs text-muted-foreground'
              }
            >
              {expired ? 'Expired' : 'Expires'} {time(c.notAfter)}
            </span>
          </li>
        )
      })}
    </ul>
  )
}
