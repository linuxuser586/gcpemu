import { block, gcloud, keyValues, omit, resource, type Recipe } from '@/lib/copyAs'

import { lastSegment, type Repository } from './api'

// Copy as (FR-UI-015) for repositories: gcloud artifacts repositories,
// the artifactregistry/v1 create request and the
// google_artifact_registry_repository resource. Images are pushed, not
// created, so they have none.

/** CleanupPolicy is one of a repository's cleanup policies, keyed by its ID in the API. */
interface CleanupPolicy {
  id?: string
  action?: string
  condition?: {
    tagState?: string
    tagPrefixes?: string[]
    versionNamePrefixes?: string[]
    packageNamePrefixes?: string[]
    olderThan?: string
    newerThan?: string
  }
  mostRecentVersions?: { packageNamePrefixes?: string[]; keepCount?: number }
}

type Repo = Repository & {
  cleanupPolicies?: Record<string, CleanupPolicy>
  kmsKeyName?: string
}

const lower = (s?: string) => s?.toLowerCase().replaceAll('_', '-')

export function repositoryRecipe(r: Repo): Recipe {
  const [, project = '', , location = ''] = r.name.split('/')
  const id = lastSegment(r.name)
  const mode = r.mode && r.mode !== 'STANDARD_REPOSITORY' ? r.mode : undefined
  const remote = r.remoteRepositoryConfig
  const docker = remote?.dockerRepository
  const cleanup = Object.entries(r.cleanupPolicies ?? {}).map(([key, p]) =>
    block({
      id: p.id ?? key,
      action: p.action,
      condition: p.condition
        ? block({
            tag_state: p.condition.tagState,
            tag_prefixes: p.condition.tagPrefixes,
            version_name_prefixes: p.condition.versionNamePrefixes,
            package_name_prefixes: p.condition.packageNamePrefixes,
            older_than: p.condition.olderThan,
            newer_than: p.condition.newerThan,
          })
        : undefined,
      most_recent_versions: p.mostRecentVersions
        ? block({
            package_name_prefixes: p.mostRecentVersions.packageNamePrefixes,
            keep_count: p.mostRecentVersions.keepCount,
          })
        : undefined,
    }),
  )
  return {
    // Cleanup policies and upstreams take a file (set-cleanup-policies,
    // --upstream-policy-file), so the command leaves them out.
    gcloud: gcloud(['artifacts', 'repositories', 'create', id], {
      project,
      location,
      'repository-format': r.format?.toLowerCase(),
      mode: lower(mode),
      description: r.description,
      labels: keyValues(r.labels),
      'kms-key': r.kmsKeyName,
      'immutable-tags': r.dockerConfig?.immutableTags,
      'remote-repo-config-desc': remote?.description,
      'remote-docker-repo':
        docker?.publicRepository === 'DOCKER_HUB' ? 'DOCKER-HUB' : docker?.customRepository?.uri,
    }),
    rest: {
      method: 'POST',
      path: `/artifactregistry/v1/projects/${project}/locations/${location}/repositories?${new URLSearchParams({ repositoryId: id })}`,
      body: omit(r, [
        'name',
        'createTime',
        'updateTime',
        'sizeBytes',
        'registryUri',
        'satisfiesPzs',
        'satisfiesPzi',
      ]),
    },
    tofu: resource('google_artifact_registry_repository', id, {
      project,
      location,
      repository_id: id,
      format: r.format,
      mode,
      description: r.description,
      labels: r.labels,
      kms_key_name: r.kmsKeyName,
      cleanup_policy_dry_run: r.cleanupPolicyDryRun || undefined,
      docker_config: r.dockerConfig?.immutableTags
        ? block({ immutable_tags: r.dockerConfig.immutableTags })
        : undefined,
      cleanup_policies: cleanup,
      remote_repository_config: remote
        ? block({
            description: remote.description,
            docker_repository: docker
              ? block({
                  public_repository: docker.publicRepository,
                  custom_repository: docker.customRepository?.uri
                    ? block({ uri: docker.customRepository.uri })
                    : undefined,
                })
              : undefined,
            common_repository: remote.commonRepository?.uri
              ? block({ uri: remote.commonRepository.uri })
              : undefined,
          })
        : undefined,
      virtual_repository_config: r.virtualRepositoryConfig?.upstreamPolicies?.length
        ? block({
            upstream_policies: r.virtualRepositoryConfig.upstreamPolicies.map((u) =>
              block({ id: u.id, repository: u.repository, priority: u.priority }),
            ),
          })
        : undefined,
    }),
  }
}
