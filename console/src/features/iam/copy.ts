import {
  block,
  gcloud,
  omit,
  resource,
  tofuName,
  type FlagValue,
  type HclBody,
  type Recipe,
  type RestCall,
} from '@/lib/copyAs'

import {
  kindOf,
  lastSegment,
  resourcePath,
  type Binding,
  type Expr,
  type Policy,
  type ResourceRef,
  type Role,
  type ServiceAccount,
} from './api'

// Copy as (FR-UI-015) for service accounts, custom roles and the IAM
// policy on a resource: gcloud iam and add-iam-policy-binding, the iam/v1
// create and setIamPolicy requests, and the google_*_iam_* resources.

const projectOf = (name: string) => name.split('/')[1] ?? ''

export function serviceAccountRecipe(sa: ServiceAccount): Recipe {
  const project = sa.projectId || projectOf(sa.name)
  const id = sa.email.slice(0, sa.email.indexOf('@'))
  const base = `/iam/v1/projects/${project}/serviceAccounts`
  const create: RestCall = {
    method: 'POST',
    path: base,
    body: {
      accountId: id,
      serviceAccount: { displayName: sa.displayName, description: sa.description },
    },
  }
  return {
    gcloud: [
      gcloud(['iam', 'service-accounts', 'create', id], {
        project,
        'display-name': sa.displayName,
        description: sa.description,
      }),
      ...(sa.disabled
        ? [gcloud(['iam', 'service-accounts', 'disable', sa.email], { project })]
        : []),
    ],
    rest: sa.disabled
      ? [create, { method: 'POST', path: `${base}/${sa.email}:disable`, body: {} }]
      : create,
    tofu: resource('google_service_account', id, {
      project,
      account_id: id,
      display_name: sa.displayName,
      description: sa.description,
      disabled: sa.disabled || undefined,
    }),
  }
}

/** customRoleRecipe is a Project's custom role (projects/P/roles/ID). */
export function customRoleRecipe(r: Role): Recipe {
  const project = projectOf(r.name)
  const id = lastSegment(r.name)
  return {
    gcloud: gcloud(['iam', 'roles', 'create', id], {
      project,
      title: r.title,
      description: r.description,
      permissions: r.includedPermissions,
      stage: r.stage,
    }),
    rest: {
      method: 'POST',
      path: `/iam/v1/projects/${project}/roles`,
      body: { roleId: id, role: omit(r, ['name', 'etag', 'deleted']) },
    },
    tofu: resource('google_project_iam_custom_role', id, {
      project,
      role_id: id,
      title: r.title ?? '',
      description: r.description,
      permissions: r.includedPermissions,
      stage: r.stage,
    }),
  }
}

// ---- the IAM policy on a resource ----

/** Target is how gcloud and OpenTofu name a resource whose policy they bind. */
interface Target {
  /** gcloud is the add-iam-policy-binding command's words and flags, less the binding's. */
  gcloud?: { args: string[]; flags: Record<string, FlagValue> }
  /** tofu is the google_*_iam_binding resource type and the attributes naming the resource. */
  tofu?: { type: string; attrs: HclBody }
}

function target(r: ResourceRef): Target {
  const project = r.project
  const [a = '', b = ''] = r.name.split('/')
  switch (r.kind) {
    case 'project':
      return {
        gcloud: { args: ['projects', 'add-iam-policy-binding', project], flags: {} },
        tofu: { type: 'google_project_iam_binding', attrs: { project } },
      }
    case 'serviceAccount':
      return {
        gcloud: {
          args: ['iam', 'service-accounts', 'add-iam-policy-binding', a],
          flags: { project },
        },
        tofu: {
          type: 'google_service_account_iam_binding',
          attrs: { service_account_id: `projects/${project}/serviceAccounts/${a}` },
        },
      }
    case 'bucket':
      return {
        gcloud: { args: ['storage', 'buckets', 'add-iam-policy-binding', `gs://${a}`], flags: {} },
        tofu: { type: 'google_storage_bucket_iam_binding', attrs: { bucket: a } },
      }
    case 'topic':
      return {
        gcloud: { args: ['pubsub', 'topics', 'add-iam-policy-binding', a], flags: { project } },
        tofu: { type: 'google_pubsub_topic_iam_binding', attrs: { project, topic: a } },
      }
    case 'subscription':
      return {
        gcloud: {
          args: ['pubsub', 'subscriptions', 'add-iam-policy-binding', a],
          flags: { project },
        },
        tofu: {
          type: 'google_pubsub_subscription_iam_binding',
          attrs: { project, subscription: a },
        },
      }
    case 'repository':
      return {
        gcloud: {
          args: ['artifacts', 'repositories', 'add-iam-policy-binding', b],
          flags: { project, location: a },
        },
        tofu: {
          type: 'google_artifact_registry_repository_iam_binding',
          attrs: { project, location: a, repository: b },
        },
      }
    case 'secret': {
      const [location, secret] = b ? [a, b] : ['', a]
      return {
        gcloud: {
          args: ['secrets', 'add-iam-policy-binding', secret],
          flags: { project, location },
        },
        tofu: location
          ? {
              type: 'google_secret_manager_regional_secret_iam_binding',
              attrs: { project, location, secret_id: secret },
            }
          : {
              type: 'google_secret_manager_secret_iam_binding',
              attrs: { project, secret_id: secret },
            },
      }
    }
    case 'subnetwork':
      return {
        gcloud: {
          args: ['compute', 'networks', 'subnets', 'add-iam-policy-binding', b],
          flags: { project, region: a },
        },
        tofu: {
          type: 'google_compute_subnetwork_iam_binding',
          attrs: { project, region: a, subnetwork: b },
        },
      }
    default:
      // gcloud and the provider have no per-binding commands for TLS policies.
      return {}
  }
}

/**
 * conditionFlag writes a binding's condition as gcloud's --condition
 * KEY=VALUE list, switching delimiter (^D^) when a value holds a comma.
 */
export function conditionFlag(c: Expr): string {
  const parts = [`expression=${c.expression}`, `title=${c.title ?? ''}`]
  if (c.description) parts.push(`description=${c.description}`)
  const delim = [',', ';', '|', '#'].find((d) => parts.every((p) => !p.includes(d))) ?? ','
  return delim === ',' ? parts.join(',') : `^${delim}^${parts.join(delim)}`
}

/** setIamPolicyCall is the resource's setIamPolicy request, as api.ts sends it. */
function setIamPolicyCall(r: ResourceRef, policy: Policy): RestCall {
  const base = resourcePath(r)
  const body = omit(policy, ['etag', 'kind', 'resourceId'])
  switch (kindOf(r.kind).style) {
    case 'post':
    case 'get':
      return { method: 'POST', path: `${base}:setIamPolicy`, body: { policy: body } }
    case 'compute':
      return { method: 'POST', path: `${base}/setIamPolicy`, body: { policy: body } }
    case 'gcs':
      return { method: 'PUT', path: `${base}/iam`, body }
  }
}

/**
 * policyRecipe is the IAM policy on a resource: one add-iam-policy-binding
 * per member, the setIamPolicy request, and a google_*_iam_binding per
 * binding (authoritative for its role).
 */
export function policyRecipe(r: ResourceRef, policy: Policy): Recipe {
  const t = target(r)
  const bindings = (policy.bindings ?? []).filter((b) => b.members?.length)
  // With a conditional binding in the policy, gcloud asks for a condition on every binding.
  const conditional = bindings.some((b) => b.condition)
  const condition = (b: Binding) =>
    b.condition ? conditionFlag(b.condition) : conditional ? 'None' : undefined
  const names = new Map<string, number>()
  const tofuNameOf = (b: Binding) => {
    const n = tofuName(lastSegment(b.role))
    const seen = names.get(n) ?? 0
    names.set(n, seen + 1)
    return seen ? `${n}_${seen + 1}` : n
  }
  const g = t.gcloud
  const tf = t.tofu
  return {
    gcloud: g
      ? bindings.flatMap((b) =>
          (b.members ?? []).map((member) =>
            gcloud(g.args, { ...g.flags, member, role: b.role, condition: condition(b) }),
          ),
        )
      : undefined,
    rest: setIamPolicyCall(r, policy),
    tofu: tf
      ? bindings
          .map((b) =>
            resource(tf.type, tofuNameOf(b), {
              ...tf.attrs,
              role: b.role,
              members: b.members,
              condition: b.condition
                ? block({
                    title: b.condition.title ?? '',
                    description: b.condition.description,
                    expression: b.condition.expression,
                  })
                : undefined,
            }),
          )
          .join('\n\n') || undefined
      : undefined,
  }
}
