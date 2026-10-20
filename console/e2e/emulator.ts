// Runs the Ephemeral Instance the Playwright tests share. Playwright's
// webServer starts this script and reads the gateway URL from the line it
// prints once the Instance is ready.
import { startInstance } from './instance.ts'

// Compute Operations take 2 s, long enough to watch them
// run (FR-UI-012); compute needs no container runtime either. GKE runs
// on Linux only; its clusters need a container runtime, so the GKE view
// tests that create them skip without one (gke.spec.ts), as do the Cloud
// SQL view tests that create instances (sql.spec.ts). Cloud NAT is
// ready only with a container runtime, so it runs on Linux too. Each
// browser runs a cluster of up to 2 nodes and a private one of 1 node
// behind Cloud NAT (nat.spec.ts), beyond the default limit of 5.
const services = [
  'ar',
  'gcs',
  'pubsub',
  'secrets',
  'compute',
  'dns',
  'lb',
  'cdn',
  'certs',
  'sql',
  ...(process.platform === 'linux' ? ['gke', 'nat'] : []),
]

let instance
try {
  instance = await startInstance(services, ['--lro-latency', 'compute=2s'], {
    env: { GCPEMU_GKE_MAX_NODES: process.env.GCPEMU_GKE_MAX_NODES ?? '12' },
    onExit: (code) => {
      console.error(`gcpemu exited (${code}) before the tests finished`)
      process.exit(1)
    },
  })
} catch (e) {
  console.error((e as Error).message)
  process.exit(1)
}
console.log(`gcpemu ready at ${instance.url}`)

const stop = () => void instance.stop().then(() => process.exit(0))
process.on('SIGTERM', stop)
process.on('SIGINT', stop)
