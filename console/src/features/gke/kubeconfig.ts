import type { Cluster } from './api'

// The kubeconfig `gcloud container clusters get-credentials` writes
// (FR-GKE-004): the cluster's endpoint and CA, and gke-gcloud-auth-plugin
// for emulator IAM tokens. It is built from the cluster resource alone,
// so downloading it needs no endpoint beyond the container API.

/** contextName is gcloud's context name for a cluster. */
export const contextName = (project: string, c: Cluster) => `gke_${project}_${c.location}_${c.name}`

// JSON strings are valid YAML scalars, so quoting with JSON.stringify
// keeps any value safe.
const q = (s: string) => JSON.stringify(s)

export function buildKubeconfig(project: string, c: Cluster): string {
  const name = contextName(project, c)
  return `apiVersion: v1
kind: Config
clusters:
- name: ${q(name)}
  cluster:
    server: ${q(`https://${c.endpoint ?? ''}`)}
    certificate-authority-data: ${q(c.masterAuth?.clusterCaCertificate ?? '')}
users:
- name: ${q(name)}
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      installHint: Install gke-gcloud-auth-plugin for use with kubectl by following https://cloud.google.com/kubernetes-engine/docs/how-to/cluster-access-for-kubectl#install_plugin
      provideClusterInfo: true
      interactiveMode: IfAvailable
contexts:
- name: ${q(name)}
  context:
    cluster: ${q(name)}
    user: ${q(name)}
current-context: ${q(name)}
`
}

/** downloadText saves text as a file through the browser. */
export function downloadText(filename: string, text: string, type = 'application/yaml') {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.append(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 0)
}
