// The Services an Instance can run, by Service ID (CONTEXT.md). The order
// is the sidebar's.
export interface ServiceInfo {
  id: string
  name: string
}

export const SERVICES: readonly ServiceInfo[] = [
  { id: 'iam', name: 'IAM & Resource Manager' },
  { id: 'compute', name: 'VPC network' },
  { id: 'dns', name: 'Cloud DNS' },
  { id: 'certs', name: 'Certificate Manager' },
  { id: 'ar', name: 'Artifact Registry' },
  { id: 'pubsub', name: 'Pub/Sub' },
  { id: 'gcs', name: 'Cloud Storage' },
  { id: 'sql', name: 'Cloud SQL' },
  { id: 'gke', name: 'Google Kubernetes Engine' },
  { id: 'lb', name: 'Cloud Load Balancing' },
  { id: 'cdn', name: 'Cloud CDN' },
  { id: 'nat', name: 'Cloud NAT' },
]

const byId = new Map(SERVICES.map((s) => [s.id, s]))

export function serviceInfo(id: string): ServiceInfo | undefined {
  return byId.get(id)
}

export function serviceName(id: string): string {
  return byId.get(id)?.name ?? id
}

/** PROJECT_ID matches a valid GCP project ID (internal/project.ValidID). */
export const PROJECT_ID = /^[a-z][a-z0-9-]{4,28}[a-z0-9]$/
