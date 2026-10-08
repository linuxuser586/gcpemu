import { z } from 'zod'

// The console's CSP (default-src 'self') forbids eval, which Zod would
// otherwise probe for to compile its parsers. Imported first by main.tsx,
// before any schema is built.
z.config({ jitless: true })
