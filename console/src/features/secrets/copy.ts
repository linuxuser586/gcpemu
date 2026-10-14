import { block, gcloud, keyValues, omit, resource, type Recipe } from '@/lib/copyAs'

import { lastSegment, type Secret } from './api'

// Copy as (FR-UI-015) for secrets, global and regional: gcloud secrets,
// the secretmanager/v1 create request and the google_secret_manager_*
// resource. Versions are left out: their payloads are the secret.

/** secretParts reads projects/P[/locations/L]/secrets/S. */
const secretParts = (name: string) => {
  const parts = name.split('/')
  return { project: parts[1] ?? '', location: parts[2] === 'locations' ? (parts[3] ?? '') : '' }
}

/**
 * secretRecipe is secret s. The API names it by Project number; project,
 * when given, is the ID the commands use instead.
 */
export function secretRecipe(s: Secret, project = secretParts(s.name).project): Recipe {
  const { location } = secretParts(s.name)
  const id = lastSegment(s.name)
  const auto = s.replication?.automatic
  const replicas = s.replication?.userManaged?.replicas?.map((r) => r.location)
  const kms = auto?.customerManagedEncryption?.kmsKeyName
  // ttl is input only: the API answers with the expireTime it set.
  const expireTime = s.expireTime
  const ttl = expireTime ? undefined : s.ttl
  const topics = s.topics?.map((t) => t.name)
  const rotation =
    s.rotation?.rotationPeriod || s.rotation?.nextRotationTime ? s.rotation : undefined
  const parent = `projects/${project}${location ? `/locations/${location}` : ''}`
  return {
    gcloud: gcloud(['secrets', 'create', id], {
      project,
      location,
      'replication-policy': location ? undefined : replicas ? 'user-managed' : 'automatic',
      locations: replicas,
      'kms-key-name': kms,
      labels: keyValues(s.labels),
      'set-annotations': keyValues(s.annotations),
      'expire-time': expireTime,
      ttl,
      topics,
      'next-rotation-time': rotation?.nextRotationTime,
      'rotation-period': rotation?.rotationPeriod,
      'version-destroy-ttl': s.versionDestroyTtl,
    }),
    rest: {
      method: 'POST',
      path: `/secretmanager/v1/${parent}/secrets?${new URLSearchParams({ secretId: id })}`,
      body: {
        ...omit(s, ['name', 'createTime', 'etag', 'versionAliases', 'secretType', 'rotation']),
        rotation: rotation && omit(rotation, ['managedRotationStatus']),
      },
    },
    tofu: resource(
      location ? 'google_secret_manager_regional_secret' : 'google_secret_manager_secret',
      id,
      {
        project,
        secret_id: id,
        location: location || undefined,
        labels: s.labels,
        annotations: s.annotations,
        replication: location
          ? undefined
          : block(
              replicas
                ? { user_managed: block({ replicas: replicas.map((l) => block({ location: l })) }) }
                : {
                    auto: block({
                      customer_managed_encryption: kms ? block({ kms_key_name: kms }) : undefined,
                    }),
                  },
            ),
        topics: topics?.map((name) => block({ name })),
        expire_time: expireTime,
        ttl,
        rotation: rotation
          ? block({
              next_rotation_time: rotation.nextRotationTime,
              rotation_period: rotation.rotationPeriod,
            })
          : undefined,
        version_destroy_ttl: s.versionDestroyTtl,
      },
    ),
  }
}
