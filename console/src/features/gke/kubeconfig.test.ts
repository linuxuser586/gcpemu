import { expect, it } from 'vitest'

import { buildKubeconfig, contextName } from './kubeconfig'

it('builds the kubeconfig gcloud get-credentials writes', () => {
  const c = {
    name: 'web',
    location: 'us-central1-a',
    endpoint: '172.18.0.5',
    masterAuth: { clusterCaCertificate: 'LS0tQ0E=' },
  }
  const kc = buildKubeconfig('demo-project', c)
  const name = contextName('demo-project', c)
  expect(name).toBe('gke_demo-project_us-central1-a_web')
  expect(kc).toContain(`current-context: "${name}"`)
  expect(kc).toContain('server: "https://172.18.0.5"')
  expect(kc).toContain('certificate-authority-data: "LS0tQ0E="')
  expect(kc).toContain('command: gke-gcloud-auth-plugin')
})
