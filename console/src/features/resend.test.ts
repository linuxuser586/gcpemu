import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { mintToken, revokeToken } from '@/api/tokens'
import { server } from '@/test/setup'

import * as ar from './ar/api'
import * as build from './build/api'
import * as cdn from './cdn/api'
import * as certs from './certs/api'
import * as tls from './certs/tls'
import * as dns from './dns/api'
import * as faults from './faults/api'
import * as gcs from './gcs/api'
import { signedUrl } from './gcs/signedUrl'
import * as gke from './gke/api'
import * as iam from './iam/api'
import * as lb from './lb/api'
import * as links from './links/api'
import * as nat from './nat/api'
import * as pubsub from './pubsub/api'
import * as secrets from './secrets/api'
import * as sql from './sql/api'
import * as state from './state/api'
import * as vpc from './vpc/api'

// Which POSTs the console re-sends when they get no response (ADR 0062):
// creates whose resource name the client chooses and custom methods whose
// second call changes nothing. Every other POST is sent once.

const P = 'p'
const net = { project: P, network: 'n' }
const subnet = { project: P, region: 'r', subnetwork: 's' }
const address = { project: P, region: 'r', address: 'a' }
const cluster = { project: P, location: 'l', cluster: 'c' }
const instance = { project: P, instance: 'i' }
const lbRef = { project: P, region: 'global', coll: 'urlMaps' as const, name: 'm' }
const repo = { project: P, location: 'l', repo: 'r' }
const secret = { project: P, location: 'global', secret: 's' }
const buildRef = { project: P, location: 'global', id: 'b' }
const loc = { project: P, location: 'global' }
const sa = 'a@p.iam.gserviceaccount.com'

/** REPEATABLE are the POSTs safe to send again: exactly ADR 0062's opt-ins. */
const REPEATABLE: Record<string, () => Promise<unknown>> = {
  'Compute networks.insert': () => vpc.insertNetwork(P, {}),
  'Compute subnetworks.insert': () => vpc.insertSubnetwork(P, 'r', {}),
  'Compute firewalls.insert': () => vpc.insertFirewall(P, {}),
  'Compute routes.insert': () => vpc.insertRoute(P, {}),
  'Compute addresses.insert': () => vpc.insertAddress(P, 'r', {}),
  'Compute routers.insert': () => nat.insertRouter(P, 'r', {}),
  'Compute LB insert': () => lb.insertResource(P, 'global', 'urlMaps', {}),
  'Compute networks.addPeering': () => vpc.addPeering(net, { name: 'x', network: 'm' }),
  'Compute subnetworks.setPrivateIpGoogleAccess': () => vpc.setPrivateIpGoogleAccess(subnet, true),
  'Compute addresses.setLabels': () => vpc.setAddressLabels(address, {}, 'f'),
  'Compute LB set*': () => lb.setResource(lbRef, 'setDefaultService', {}),
  'Compute operations.wait': () =>
    lb.waitOperation({
      name: 'o',
      status: 'RUNNING',
      selfLink: '/compute/v1/projects/p/global/operations/o',
    }),
  'Service Networking createConnection': () => vpc.createConnection(net, ['x']),
  'Cloud SQL instances.insert': () => sql.insertInstance(P, {}),
  'Cloud SQL databases.insert': () => sql.insertDatabase(instance, {}),
  'Cloud SQL users.insert': () => sql.insertUser(instance, {}),
  'Cloud Storage buckets.insert': () => gcs.createBucket(P, {}),
  'Cloud DNS managedZones.create': () => dns.createZone(P, {}),
  'GKE clusters.create': () => gke.createCluster(P, 'l', {}),
  'GKE nodePools.create': () => gke.createNodePool(cluster, {}),
  'GKE nodePools.setSize': () => gke.setNodePoolSize(cluster, 'np', 1),
  'IAM serviceAccounts.create': () => iam.createServiceAccount(P, {}),
  'IAM roles.create': () => iam.createRole(P, 'r', {}),
  'IAM keys.upload': () => iam.uploadKey(P, sa, 'pem'),
  'IAM serviceAccounts.enable': () => iam.setAccountDisabled(P, sa, false),
  'IAM serviceAccounts.disable': () => iam.setAccountDisabled(P, sa, true),
  'IAM keys.enable': () => iam.setKeyDisabled(P, sa, 'k', false),
  'IAM keys.disable': () => iam.setKeyDisabled(P, sa, 'k', true),
  'IAM testIamPermissions': () =>
    iam.testPermissions({ project: P, kind: 'project', name: '' }, ['x.y.z'], 't'),
  'IAM testIamPermissions (Compute)': () =>
    iam.testPermissions({ project: P, kind: 'subnetwork', name: 'r/s' }, ['x.y.z'], 't'),
  'IAM Credentials signBlob': () =>
    signedUrl({
      bucket: 'b',
      object: 'o',
      method: 'GET',
      expiresSeconds: 60,
      serviceAccount: sa,
      host: 'h',
      now: new Date(0),
    }),
  'Secret Manager secrets.create': () => secrets.createSecret(P, 'global', 's', {}),
  'Artifact Registry repositories.create': () => ar.createRepository(P, 'l', 'r', {}),
  'Artifact Registry tags.create': () => ar.createTag(repo, 'img', 't', 'sha256:0'),
  'Certificate Manager certificates.create': () =>
    certs.createCertificate({ ...loc, cert: 'c' }, {}),
  'Certificate Manager certificateMaps.create': () => certs.createMap({ project: P, map: 'm' }, {}),
  'Certificate Manager certificateMapEntries.create': () =>
    certs.createEntry({ project: P, map: 'm', entry: 'e' }, {}),
  'Certificate Manager dnsAuthorizations.create': () =>
    certs.createDnsAuthorization({ ...loc, auth: 'a' }, {}),
  'Certificate Manager trustConfigs.create': () =>
    certs.createTrustConfig({ ...loc, trust: 't' }, {}),
  'Certificate Manager issuanceConfigs.create': () =>
    certs.createIssuanceConfig({ ...loc, config: 'c' }, {}),
  'Cloud Build builds.cancel': () => build.cancelBuild(buildRef),
  'Pub/Sub acknowledge': () => pubsub.acknowledge(P, 's', ['a']),
  'Faults add with an id': () => faults.addFault({ id: 'f', service: 'storage' }),
  'Admin snapshot save': () => state.saveSnapshot({ name: 's' }),
  'Admin snapshot restore': () => state.restoreSnapshot('s'),
  'Admin seed': () => state.applySeed('projects: []'),
  'Admin reset': () => state.resetInstance(),
  'Admin CDN purge': () => cdn.purge(),
  'Admin links identify': () => links.identify('h:1'),
  'Admin LB route test': () => lb.route({ urlMap: 'm', scheme: 'http', method: 'GET', host: 'h' }),
  'Admin Build daemon prune': () => build.pruneDaemon(),
}

/** ONCE are POSTs never safe to repeat, or not yet checked. */
const ONCE: Record<string, () => Promise<unknown>> = {
  'Pub/Sub publish': () => pubsub.publish(P, 't', [{ data: '' }]),
  'Pub/Sub pull': () => pubsub.pull(P, 's', 1),
  'Pub/Sub modifyAckDeadline': () => pubsub.nack(P, 's', ['a']),
  'Pub/Sub seek': () => pubsub.seek(P, 's', { time: '2026-01-01T00:00:00Z' }),
  'Cloud Build builds.retry': () => build.retryBuild(buildRef),
  'Cloud Build triggers.run': () => build.runTrigger(P, 'global', 't'),
  'Secret Manager addVersion': () => secrets.addVersion(secret, 'x'),
  'Secret Manager rotateSecret': () => secrets.rotateSecret(secret),
  'Secret Manager versions.destroy': () => secrets.changeVersion(secret, '1', 'destroy'),
  'Cloud SQL executeSql': () => sql.executeSql(instance, { sqlStatement: 'select 1' }),
  'Cloud SQL instances.restart': () => sql.restartInstance(instance),
  'Cloud SQL backupRuns.insert': () => sql.insertBackupRun(instance, ''),
  'Cloud SQL instances.restoreBackup': () => sql.restoreBackup(instance, '1', 'i'),
  'Cloud Storage upload': () => gcs.uploadObject('b', 'o', new Blob(['x'])),
  'Cloud Storage copyTo': () => gcs.restoreGeneration('b', 'o', '1'),
  'Cloud Storage notificationConfigs.insert': () => gcs.createNotification('b', {}),
  'Cloud DNS changes.create': () => dns.applyChange({ project: P, zone: 'z' }, {}),
  'IAM keys.create': () => iam.createKey(P, sa),
  'IAM serviceAccounts.undelete': () => iam.undeleteServiceAccount('1'),
  'IAM roles.undelete': () => iam.undeleteRole(P, 'r'),
  'IAM setIamPolicy': () => iam.setPolicy({ project: P, kind: 'project', name: '' }, {}),
  'Compute switchToCustomMode': () => vpc.switchToCustomMode(net),
  'Compute expandIpCidrRange': () => vpc.expandIpCidrRange(subnet, '10.0.0.0/8'),
  'Compute removePeering': () => vpc.removePeering(net, 'x'),
  'Compute urlMaps.invalidateCache': () => cdn.invalidate(lbRef, { path: '/*' }),
  'Service Networking deleteConnection': () => vpc.deleteConnection(net),
  'GKE setResourceLabels': () => gke.setResourceLabels(cluster, {}, 'f'),
  'Network Security TLS policy create': () =>
    tls.createTls({ ...loc, coll: 'serverTlsPolicies', id: 't' }, {}),
  'Admin CA rotate': () => certs.rotateCa(),
  'Faults add with no id': () => faults.addFault({ service: 'storage' }),
  'Admin links pair': () => links.pair({ address: 'h:1', fingerprint: 'f' }),
  'Admin links approve': () => links.approve('1'),
  'Admin links reject': () => links.reject('1'),
  'Admin links share': () => links.share({ project: P, with: 'x' }),
  'Admin links delegate': () => links.delegate({ project: P, link: 'x' }),
  'OAuth2 token': () => mintToken('user:a@example.com'),
  'OAuth2 revoke': () => revokeToken('t'),
}

/**
 * attempts calls call with the first two requests failing with no
 * response, and counts how often its first request was sent.
 */
async function attempts(call: () => Promise<unknown>): Promise<number> {
  let first = ''
  let sent = 0
  let failed = 0
  server.use(
    http.all('*', ({ request }) => {
      const key = `${request.method} ${request.url}`
      first ||= key
      if (key === first) sent++
      if (failed++ < 2) return HttpResponse.error()
      return HttpResponse.json({ status: 'DONE', done: true, signedBlob: '' })
    }),
  )
  let settled = false
  void call()
    .catch(() => undefined)
    .finally(() => (settled = true))
  // Step the clock until the call settles: some, such as signedUrl, await
  // real async work (crypto.subtle) before their first request.
  for (let i = 0; !settled && i < 50; i++) await vi.advanceTimersByTimeAsync(500)
  expect(settled).toBe(true)
  return sent
}

describe('which POSTs are re-sent', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it.each(Object.entries(REPEATABLE))('re-sends %s', async (_, call) => {
    expect(await attempts(call)).toBe(3)
  })

  it.each(Object.entries(ONCE))('sends %s once', async (_, call) => {
    expect(await attempts(call)).toBe(1)
  })
})
