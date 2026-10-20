import type { QueryClient } from '@tanstack/react-query'
import { createBrowserRouter, type RouteObject } from 'react-router'

import {
  containersQuery,
  endpointsQuery,
  envQuery,
  infoQuery,
  operationsQuery,
  readinessQuery,
  requestsQuery,
  resourceCountsQuery,
} from '@/api/queries'
import { AppShell } from '@/components/shell/AppShell'
import { RouteError } from '@/components/shell/RouteError'
import {
  dockerImagesQuery,
  packagesQuery,
  repositoriesQuery,
  repositoryQuery,
} from '@/features/ar/api'
import { ArLayout, hostModeQuery } from '@/features/ar/ArLayout'
import { DigestPage, ImagePage } from '@/features/ar/ImagePage'
import { Repositories } from '@/features/ar/Repositories'
import { CreateRepository, EditRepository } from '@/features/ar/RepositoryForm'
import { Images, RepositoryOverview, RepositoryPage } from '@/features/ar/RepositoryPage'
import { backendsQuery, statsQuery as cdnStatsQuery } from '@/features/cdn/api'
import { CdnLayout } from '@/features/cdn/CdnLayout'
import { AddOrigin, EditOrigin } from '@/features/cdn/OriginForm'
import { OriginPage } from '@/features/cdn/OriginPage'
import { Origins } from '@/features/cdn/Origins'
import { Dashboard } from '@/features/dashboard/Dashboard'
import { changesQuery, rrsetsQuery, zoneQuery, zonesQuery } from '@/features/dns/api'
import { Changes } from '@/features/dns/Changes'
import { DnsLayout } from '@/features/dns/DnsLayout'
import { Query as DnsQuery } from '@/features/dns/Query'
import { CreateRecordSet, EditRecordSet } from '@/features/dns/RecordSetForm'
import { RecordSets } from '@/features/dns/RecordSets'
import { CreateZone, EditZone } from '@/features/dns/ZoneForm'
import { ZoneOverview, ZonePage } from '@/features/dns/ZonePage'
import { Zones } from '@/features/dns/Zones'
import { faultsQuery } from '@/features/faults/api'
import { CreateFault } from '@/features/faults/FaultForm'
import { Faults } from '@/features/faults/Faults'
import { bucketQuery, bucketsQuery, objectQuery } from '@/features/gcs/api'
import { CreateBucket, EditBucket } from '@/features/gcs/BucketForm'
import { BucketOverview, BucketPage } from '@/features/gcs/BucketPage'
import { Buckets } from '@/features/gcs/Buckets'
import { GcsLayout } from '@/features/gcs/GcsLayout'
import { CreateNotification, NotificationPage, Notifications } from '@/features/gcs/Notifications'
import { ObjectBrowser } from '@/features/gcs/ObjectBrowser'
import { EditObject, ObjectPage } from '@/features/gcs/ObjectPage'
import { clusterQuery, clustersQuery } from '@/features/gke/api'
import { CreateCluster, EditCluster } from '@/features/gke/ClusterForm'
import { ClusterOverview, ClusterPage } from '@/features/gke/ClusterPage'
import { Clusters } from '@/features/gke/Clusters'
import { GkeLayout } from '@/features/gke/GkeLayout'
import { Namespaces, Nodes, Pods, Services, Workloads } from '@/features/gke/Kube'
import { Negs } from '@/features/gke/Negs'
import { CreateNodePool, EditNodePool } from '@/features/gke/NodePoolForm'
import { NodePoolPage, NodePools } from '@/features/gke/NodePools'
import { PodLogs } from '@/features/gke/PodLogs'
import { kindInfo, kindQuery, listenersQuery, resourceQuery, type Coll } from '@/features/lb/api'
import { LbLayout } from '@/features/lb/LbLayout'
import { CreateResource, EditResource } from '@/features/lb/ResourceForm'
import { ResourcePage } from '@/features/lb/ResourcePage'
import { Resources } from '@/features/lb/Resources'
import { Traffic } from '@/features/lb/Traffic'
import {
  customRoleQuery,
  customRolesQuery,
  denialsQuery,
  keysQuery,
  permissionsQuery,
  policyQuery,
  predefinedRoleQuery,
  predefinedRolesQuery,
  serviceAccountQuery,
  serviceAccountsQuery,
} from '@/features/iam/api'
import { Audit } from '@/features/iam/Audit'
import { IamLayout } from '@/features/iam/IamLayout'
import { Policies } from '@/features/iam/Policies'
import { CreateRole, EditRole } from '@/features/iam/RoleForm'
import { CustomRolePage, PredefinedRolePage } from '@/features/iam/RolePage'
import { Roles } from '@/features/iam/Roles'
import { CreateServiceAccount, EditServiceAccount } from '@/features/iam/ServiceAccountForm'
import {
  AccountDetails,
  AccountPermissions,
  Keys,
  ServiceAccountPage,
} from '@/features/iam/ServiceAccountPage'
import { ServiceAccounts } from '@/features/iam/ServiceAccounts'
import { Simulator } from '@/features/iam/Simulator'
import {
  mappingsQuery,
  routerQuery,
  routersQuery,
  sinkQuery,
  statusQuery,
} from '@/features/nat/api'
import { AddNat, EditNat } from '@/features/nat/NatForm'
import { NatLayout } from '@/features/nat/NatLayout'
import { NatPage } from '@/features/nat/NatPage'
import { CreateRouter, EditRouter } from '@/features/nat/RouterForm'
import { RouterPage } from '@/features/nat/RouterPage'
import { Routers } from '@/features/nat/Routers'
import { TranslationLogPage } from '@/features/nat/Translations'
import { Operations } from '@/features/operations/Operations'
import {
  statsQuery,
  subscriptionQuery,
  subscriptionsQuery,
  topicQuery,
  topicSubscriptionsQuery,
  topicsQuery,
} from '@/features/pubsub/api'
import { Messages } from '@/features/pubsub/Messages'
import { PubSubLayout } from '@/features/pubsub/PubSubLayout'
import { CreateSubscription, EditSubscription } from '@/features/pubsub/SubscriptionForm'
import { SubscriptionOverview, SubscriptionPage } from '@/features/pubsub/SubscriptionPage'
import { Subscriptions } from '@/features/pubsub/Subscriptions'
import { CreateTopic, EditTopic } from '@/features/pubsub/TopicForm'
import { TopicOverview, TopicPage, TopicSubscriptions } from '@/features/pubsub/TopicPage'
import { Topics } from '@/features/pubsub/Topics'
import { RequestLog } from '@/features/requests/RequestLog'
import { locationsQuery, secretQuery, secretsQuery, versionsQuery } from '@/features/secrets/api'
import { CreateSecret, EditSecret } from '@/features/secrets/SecretForm'
import { SecretOverview, SecretPage } from '@/features/secrets/SecretPage'
import { Secrets } from '@/features/secrets/Secrets'
import { SecretsLayout } from '@/features/secrets/SecretsLayout'
import { Versions } from '@/features/secrets/Versions'
import { ServicePage } from '@/features/services/ServicePage'
import { databasesQuery, instanceQuery, instancesQuery } from '@/features/sql/api'
import { Backups } from '@/features/sql/Backups'
import { CreateDatabase, Databases, EditDatabase } from '@/features/sql/Databases'
import { Flags } from '@/features/sql/Flags'
import { CreateInstance, EditInstance } from '@/features/sql/InstanceForm'
import { InstanceOverview, InstancePage } from '@/features/sql/InstancePage'
import { Instances } from '@/features/sql/Instances'
import { Query } from '@/features/sql/Query'
import { SqlLayout } from '@/features/sql/SqlLayout'
import { CreateUser, EditUser, Users } from '@/features/sql/Users'
import { snapshotsQuery } from '@/features/state/api'
import { SaveSnapshot } from '@/features/state/SaveSnapshot'
import { State } from '@/features/state/State'
import {
  hostOffersQuery,
  networkQuery,
  networksQuery,
  subnetQuery,
  subnetsQuery,
} from '@/features/vpc/api'
import { CreateNetwork, EditNetwork } from '@/features/vpc/NetworkForm'
import { NetworkPage } from '@/features/vpc/NetworkPage'
import { Networks } from '@/features/vpc/Networks'
import { Offers } from '@/features/vpc/Offers'
import { CreateSubnetwork, EditSubnetwork } from '@/features/vpc/SubnetworkForm'
import { SubnetworkPage } from '@/features/vpc/SubnetworkPage'
import { Subnetworks } from '@/features/vpc/Subnetworks'
import { VpcLayout } from '@/features/vpc/VpcLayout'

export const BASENAME = '/console'

const projectOf = (request: Request) => new URL(request.url).searchParams.get('project') ?? ''

// Loaders only warm the query cache with ensureQueryData; components read
// it with useQuery. Only /_emu/v1/info is required to render: a failure of
// any other query is shown inline by the card that uses it.
export function routes(queryClient: QueryClient): RouteObject[] {
  return [
    {
      path: '/',
      element: <AppShell />,
      errorElement: <RouteError />,
      hydrateFallbackElement: <p className="p-6 text-sm text-muted-foreground">Loading…</p>,
      loader: async () => {
        await queryClient.ensureQueryData(infoQuery())
        return null
      },
      children: [
        {
          index: true,
          element: <Dashboard />,
          loader: async () => {
            await Promise.allSettled([
              queryClient.ensureQueryData(readinessQuery()),
              queryClient.ensureQueryData(endpointsQuery()),
              queryClient.ensureQueryData(containersQuery()),
              queryClient.ensureQueryData(resourceCountsQuery()),
              queryClient.ensureQueryData(envQuery()),
            ])
            return null
          },
        },
        {
          path: 'operations',
          element: <Operations />,
          loader: async ({ request }) => {
            await Promise.allSettled([
              queryClient.ensureQueryData(operationsQuery(projectOf(request))),
            ])
            return null
          },
        },
        {
          path: 'faults',
          element: <Faults />,
          loader: async () => {
            await Promise.allSettled([queryClient.ensureQueryData(faultsQuery())])
            return null
          },
        },
        { path: 'faults/create', element: <CreateFault /> },
        {
          path: 'state',
          element: <State />,
          loader: async () => {
            await Promise.allSettled([queryClient.ensureQueryData(snapshotsQuery())])
            return null
          },
        },
        { path: 'state/snapshots/save', element: <SaveSnapshot /> },
        {
          path: 'requests',
          element: <RequestLog />,
          loader: async () => {
            await Promise.allSettled([queryClient.ensureQueryData(requestsQuery())])
            return null
          },
        },
        {
          path: 'iam',
          element: <IamLayout />,
          children: iamRoutes(queryClient),
        },
        {
          path: 'ar',
          element: <ArLayout />,
          children: arRoutes(queryClient),
        },
        {
          path: 'gke',
          element: <GkeLayout />,
          children: gkeRoutes(queryClient),
        },
        {
          path: 'gcs',
          element: <GcsLayout />,
          children: gcsRoutes(queryClient),
        },
        {
          path: 'pubsub',
          element: <PubSubLayout />,
          children: pubsubRoutes(queryClient),
        },
        {
          path: 'secrets',
          element: <SecretsLayout />,
          children: secretsRoutes(queryClient),
        },
        {
          path: 'sql',
          element: <SqlLayout />,
          children: sqlRoutes(queryClient),
        },
        {
          path: 'lb',
          element: <LbLayout />,
          children: lbRoutes(queryClient),
        },
        {
          path: 'cdn',
          element: <CdnLayout />,
          children: cdnRoutes(queryClient),
        },
        {
          path: 'dns',
          element: <DnsLayout />,
          children: dnsRoutes(queryClient),
        },
        {
          path: 'nat',
          element: <NatLayout />,
          children: natRoutes(queryClient),
        },
        {
          path: 'compute',
          element: <VpcLayout />,
          children: vpcRoutes(queryClient),
        },
        {
          path: ':service/*',
          element: <ServicePage />,
          loader: async () => {
            await Promise.allSettled([queryClient.ensureQueryData(resourceCountsQuery())])
            return null
          },
        },
      ],
    },
  ]
}

// The IAM view (SRS 4.8.3). /iam is the policy of the Project, or of the
// resource in ?kind= and ?name=; service accounts and custom roles follow
// the API's names (by email and role ID), with the Project in ?project=;
// a predefined role roles/ID is /iam/roles/ID. Create pages are not under
// serviceAccounts/ and roles/, so that no name is shadowed.
function iamRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const account: RouteObject['loader'] = ({ request, params }) => {
    const project = projectOf(request)
    return project
      ? warm(queryClient.ensureQueryData(serviceAccountQuery(project, params.email ?? '')))
      : null
  }
  return [
    {
      index: true,
      element: <Policies />,
      loader: ({ request }) => {
        const project = projectOf(request)
        const kind = new URL(request.url).searchParams.get('kind')
        return project && !kind
          ? warm(
              queryClient.ensureQueryData(policyQuery({ project, kind: 'project', name: '' })),
              queryClient.ensureQueryData(predefinedRolesQuery()),
            )
          : null
      },
    },
    {
      path: 'serviceAccounts',
      element: <ServiceAccounts />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return project ? warm(queryClient.ensureQueryData(serviceAccountsQuery(project))) : null
      },
    },
    { path: 'create-service-account', element: <CreateServiceAccount /> },
    { path: 'serviceAccounts/:email/edit', element: <EditServiceAccount />, loader: account },
    {
      path: 'serviceAccounts/:email',
      element: <ServiceAccountPage />,
      loader: account,
      children: [
        { index: true, element: <AccountDetails /> },
        {
          path: 'keys',
          element: <Keys />,
          loader: ({ request, params }) => {
            const project = projectOf(request)
            return project
              ? warm(queryClient.ensureQueryData(keysQuery(project, params.email ?? '')))
              : null
          },
        },
        { path: 'permissions', element: <AccountPermissions /> },
      ],
    },
    {
      path: 'roles',
      element: <Roles />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return warm(
          queryClient.ensureQueryData(predefinedRolesQuery()),
          ...(project ? [queryClient.ensureQueryData(customRolesQuery(project))] : []),
        )
      },
    },
    { path: 'create-role', element: <CreateRole /> },
    {
      path: 'roles/:role',
      element: <PredefinedRolePage />,
      loader: ({ params }) =>
        warm(queryClient.ensureQueryData(predefinedRoleQuery(params.role ?? ''))),
    },
    {
      path: 'customRoles/:role',
      element: <CustomRolePage />,
      loader: ({ request, params }) => {
        const project = projectOf(request)
        return project
          ? warm(queryClient.ensureQueryData(customRoleQuery(project, params.role ?? '')))
          : null
      },
    },
    { path: 'customRoles/:role/edit', element: <EditRole /> },
    {
      path: 'simulator',
      element: <Simulator />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return project ? warm(queryClient.ensureQueryData(permissionsQuery(project))) : null
      },
    },
    {
      path: 'audit',
      element: <Audit />,
      loader: ({ request }) => warm(queryClient.ensureQueryData(denialsQuery(projectOf(request)))),
    },
  ]
}

// The GKE view (SRS 4.8.3). Paths follow the API's resource names, with
// the Project in ?project=.
function gkeRoutes(queryClient: QueryClient): RouteObject[] {
  const cluster = 'locations/:location/clusters/:cluster'
  return [
    {
      index: true,
      element: <Clusters />,
      loader: async ({ request }) => {
        const project = projectOf(request)
        if (project) await Promise.allSettled([queryClient.ensureQueryData(clustersQuery(project))])
        return null
      },
    },
    { path: 'create', element: <CreateCluster /> },
    { path: `${cluster}/edit`, element: <EditCluster /> },
    {
      path: cluster,
      element: <ClusterPage />,
      loader: async ({ request, params }) => {
        const project = projectOf(request)
        const { location = '', cluster = '' } = params
        if (project) {
          await Promise.allSettled([
            queryClient.ensureQueryData(clusterQuery({ project, location, cluster })),
          ])
        }
        return null
      },
      children: [
        { index: true, element: <ClusterOverview /> },
        { path: 'nodePools', element: <NodePools /> },
        { path: 'nodePools/create', element: <CreateNodePool /> },
        { path: 'nodePools/:pool', element: <NodePoolPage /> },
        { path: 'nodePools/:pool/edit', element: <EditNodePool /> },
        { path: 'nodes', element: <Nodes /> },
        { path: 'namespaces', element: <Namespaces /> },
        { path: 'workloads', element: <Workloads /> },
        { path: 'pods', element: <Pods /> },
        { path: 'pods/:namespace/:pod', element: <PodLogs /> },
        { path: 'services', element: <Services /> },
        { path: 'negs', element: <Negs /> },
      ],
    },
  ]
}

// The Artifact Registry view (SRS 4.8.3). Paths follow the API's resource
// names, with the Project in ?project=; an image is its path (slashes
// escaped) under images/, and a digest of it is one segment more. The
// list shows every location unless ?location= names one.
function arRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const repo = 'locations/:location/repositories/:repo'
  const refOf = (request: Request, params: Record<string, string | undefined>) => ({
    project: projectOf(request),
    location: params.location ?? '',
    repo: params.repo ?? '',
  })
  // Pull commands name the registry port, or the real host in host mode.
  const registry = () => [
    queryClient.ensureQueryData(endpointsQuery()),
    queryClient.ensureQueryData(hostModeQuery()),
  ]
  const images: RouteObject['loader'] = ({ request, params }) => {
    const r = refOf(request, params)
    return r.project ? warm(queryClient.ensureQueryData(dockerImagesQuery(r)), ...registry()) : null
  }
  return [
    {
      index: true,
      element: <Repositories />,
      loader: ({ request }) => {
        const project = projectOf(request)
        const location = new URL(request.url).searchParams.get('location') ?? ''
        return project
          ? warm(queryClient.ensureQueryData(repositoriesQuery(project, location)))
          : null
      },
    },
    { path: 'create', element: <CreateRepository /> },
    { path: `${repo}/edit`, element: <EditRepository /> },
    { path: `${repo}/images/:image`, element: <ImagePage />, loader: images },
    { path: `${repo}/images/:image/:digest`, element: <DigestPage />, loader: images },
    {
      path: repo,
      element: <RepositoryPage />,
      loader: ({ request, params }) => {
        const r = refOf(request, params)
        return r.project
          ? warm(
              queryClient.ensureQueryData(repositoryQuery(r)),
              queryClient.ensureQueryData(packagesQuery(r)),
              queryClient.ensureQueryData(dockerImagesQuery(r)),
              ...registry(),
            )
          : null
      },
      children: [
        { index: true, element: <Images /> },
        { path: 'overview', element: <RepositoryOverview /> },
      ],
    },
  ]
}

// The Cloud Storage view (SRS 4.8.3). Buckets are named globally, so only
// the bucket list takes ?project=; a folder of the browser is ?prefix=
// and an object's name is the rest of its path, slashes and all.
function gcsRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  return [
    {
      index: true,
      element: <Buckets />,
      loader: async ({ request }) => {
        const project = projectOf(request)
        if (project) await warm(queryClient.ensureQueryData(bucketsQuery(project)))
        return null
      },
    },
    { path: 'create', element: <CreateBucket /> },
    {
      path: 'b/:bucket',
      element: <BucketPage />,
      loader: ({ params }) => warm(queryClient.ensureQueryData(bucketQuery(params.bucket ?? ''))),
      children: [
        { index: true, element: <ObjectBrowser /> },
        { path: 'configuration', element: <BucketOverview /> },
        { path: 'edit', element: <EditBucket /> },
        { path: 'notifications', element: <Notifications /> },
        { path: 'notifications/create', element: <CreateNotification /> },
        { path: 'notifications/:id', element: <NotificationPage /> },
        {
          path: 'o/*',
          element: <ObjectPage />,
          loader: ({ params }) =>
            warm(queryClient.ensureQueryData(objectQuery(params.bucket ?? '', params['*'] ?? ''))),
        },
        { path: 'edit/*', element: <EditObject /> },
      ],
    },
  ]
}

// The Pub/Sub view (SRS 4.8.3). Paths follow the API's resource names,
// with the Project in ?project=; create pages are not under topics/ and
// subscriptions/, so that no ID is shadowed.
function pubsubRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const list: RouteObject['loader'] = ({ request }) => {
    const project = projectOf(request)
    return project
      ? warm(
          queryClient.ensureQueryData(topicsQuery(project)),
          queryClient.ensureQueryData(subscriptionsQuery(project)),
          queryClient.ensureQueryData(statsQuery(project)),
        )
      : null
  }
  return [
    { index: true, element: <Topics />, loader: list },
    { path: 'subscriptions', element: <Subscriptions />, loader: list },
    { path: 'create-topic', element: <CreateTopic /> },
    { path: 'create-subscription', element: <CreateSubscription /> },
    { path: 'topics/:topic/edit', element: <EditTopic /> },
    {
      path: 'topics/:topic',
      element: <TopicPage />,
      loader: ({ request, params }) => {
        const project = projectOf(request)
        const topic = params.topic ?? ''
        return project
          ? warm(
              queryClient.ensureQueryData(topicQuery(project, topic)),
              queryClient.ensureQueryData(topicSubscriptionsQuery(project, topic)),
            )
          : null
      },
      children: [
        { index: true, element: <TopicSubscriptions /> },
        { path: 'overview', element: <TopicOverview /> },
      ],
    },
    { path: 'subscriptions/:subscription/edit', element: <EditSubscription /> },
    {
      path: 'subscriptions/:subscription',
      element: <SubscriptionPage />,
      loader: ({ request, params }) => {
        const project = projectOf(request)
        return project
          ? warm(
              queryClient.ensureQueryData(subscriptionQuery(project, params.subscription ?? '')),
              queryClient.ensureQueryData(statsQuery(project)),
            )
          : null
      },
      children: [
        { index: true, element: <Messages /> },
        { path: 'overview', element: <SubscriptionOverview /> },
      ],
    },
  ]
}

// The Secret Manager view (SRS 4.8.3). Paths follow the API's resource
// names, with the Project in ?project=; the list shows one location
// (?location=, global by default).
function secretsRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const secret = (location: boolean): RouteObject => ({
    path: `${location ? 'locations/:location/' : ''}secrets/:secret`,
    element: <SecretPage />,
    loader: ({ request, params }) => {
      const r = {
        project: projectOf(request),
        location: params.location ?? '',
        secret: params.secret ?? '',
      }
      return r.project
        ? warm(
            queryClient.ensureQueryData(secretQuery(r)),
            queryClient.ensureQueryData(versionsQuery(r)),
          )
        : null
    },
    children: [
      { index: true, element: <Versions /> },
      { path: 'overview', element: <SecretOverview /> },
    ],
  })
  const edit = (location: boolean): RouteObject => ({
    path: `${location ? 'locations/:location/' : ''}secrets/:secret/edit`,
    element: <EditSecret />,
  })
  return [
    {
      index: true,
      element: <Secrets />,
      loader: async ({ request }) => {
        const project = projectOf(request)
        const location = new URL(request.url).searchParams.get('location') ?? ''
        if (project) {
          await warm(
            queryClient.ensureQueryData(secretsQuery(project, location)),
            queryClient.ensureQueryData(locationsQuery(project)),
          )
        }
        return null
      },
    },
    { path: 'create', element: <CreateSecret /> },
    edit(false),
    edit(true),
    secret(false),
    secret(true),
  ]
}

// The Cloud SQL view (SRS 4.8.3). Paths follow the API's resource names,
// with the Project in ?project=; the create page is not under instances/,
// so that no instance ID is shadowed.
function sqlRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  return [
    {
      index: true,
      element: <Instances />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return project ? warm(queryClient.ensureQueryData(instancesQuery(project))) : null
      },
    },
    { path: 'create', element: <CreateInstance /> },
    { path: 'instances/:instance/edit', element: <EditInstance /> },
    {
      path: 'instances/:instance',
      element: <InstancePage />,
      loader: ({ request, params }) => {
        const r = { project: projectOf(request), instance: params.instance ?? '' }
        return r.project
          ? warm(
              queryClient.ensureQueryData(instanceQuery(r)),
              queryClient.ensureQueryData(databasesQuery(r)),
            )
          : null
      },
      children: [
        { index: true, element: <InstanceOverview /> },
        { path: 'databases', element: <Databases /> },
        { path: 'databases/create', element: <CreateDatabase /> },
        { path: 'databases/:database/edit', element: <EditDatabase /> },
        { path: 'users', element: <Users /> },
        { path: 'users/create', element: <CreateUser /> },
        { path: 'users/:user/edit', element: <EditUser /> },
        { path: 'flags', element: <Flags /> },
        { path: 'query', element: <Query /> },
        { path: 'backups', element: <Backups /> },
      ],
    },
  ]
}

// The Load Balancer view (SRS 4.8.3). Each kind has a list (forwarding
// rules at /lb); resource paths follow the API's, global/C/N or
// regions/R/C/N, with the Project in ?project=; create pages are under
// create/, so that no name is shadowed. The traffic view is /lb/traffic,
// its filters in ?host= and ?path=.
function lbRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const list =
    (fixed?: Coll): RouteObject['loader'] =>
    ({ request, params }) => {
      const project = projectOf(request)
      const coll = fixed ?? (params.coll as Coll)
      return project && kindInfo(coll)
        ? warm(
            queryClient.ensureQueryData(kindQuery(project, coll)),
            ...(coll === 'forwardingRules'
              ? [queryClient.ensureQueryData(listenersQuery(project))]
              : []),
          )
        : null
    }
  const page: RouteObject['loader'] = ({ request, params }) => {
    const project = projectOf(request)
    const r = {
      project,
      region: params.region ?? '',
      coll: params.coll as Coll,
      name: params.name ?? '',
    }
    return project && kindInfo(r.coll) ? warm(queryClient.ensureQueryData(resourceQuery(r))) : null
  }
  return [
    { index: true, element: <Resources />, loader: list('forwardingRules') },
    { path: 'create/:coll', element: <CreateResource /> },
    {
      path: 'traffic',
      element: <Traffic />,
      loader: () => warm(queryClient.ensureQueryData(requestsQuery('lb'))),
    },
    { path: 'global/:coll/:name', element: <ResourcePage />, loader: page },
    { path: 'global/:coll/:name/edit', element: <EditResource /> },
    { path: 'regions/:region/:coll/:name', element: <ResourcePage />, loader: page },
    { path: 'regions/:region/:coll/:name/edit', element: <EditResource /> },
    { path: ':coll', element: <Resources />, loader: list() },
  ]
}

// The Cloud CDN view (SRS 4.8.3). An origin is a backend service or
// bucket; its path follows the API's, global/C/N or regions/R/C/N, with
// the Project in ?project=. The add page is not under global/, so that no
// name is shadowed.
function cdnRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const page: RouteObject['loader'] = ({ request, params }) => {
    const project = projectOf(request)
    const r = {
      project,
      region: params.region ?? '',
      coll: params.coll as Coll,
      name: params.name ?? '',
    }
    return project && kindInfo(r.coll)
      ? warm(
          queryClient.ensureQueryData(resourceQuery(r)),
          queryClient.ensureQueryData(cdnStatsQuery(project)),
        )
      : null
  }
  return [
    {
      index: true,
      element: <Origins />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return project
          ? warm(
              queryClient.ensureQueryData(backendsQuery(project)),
              queryClient.ensureQueryData(cdnStatsQuery(project)),
            )
          : null
      },
    },
    { path: 'add', element: <AddOrigin /> },
    { path: 'global/:coll/:name', element: <OriginPage />, loader: page },
    { path: 'global/:coll/:name/edit', element: <EditOrigin /> },
    { path: 'regions/:region/:coll/:name', element: <OriginPage />, loader: page },
    { path: 'regions/:region/:coll/:name/edit', element: <EditOrigin /> },
  ]
}

// The Cloud DNS view (SRS 4.8.3). Paths follow the API's resource names,
// with the Project in ?project=; a record set is its DNS name and type
// under rrsets/. The query box is /dns/query, its question in ?name=,
// ?type= and ?network=.
function dnsRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  return [
    {
      index: true,
      element: <Zones />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return project ? warm(queryClient.ensureQueryData(zonesQuery(project))) : null
      },
    },
    {
      path: 'query',
      element: <DnsQuery />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return project ? warm(queryClient.ensureQueryData(zonesQuery(project))) : null
      },
    },
    { path: 'create', element: <CreateZone /> },
    { path: 'zones/:zone/edit', element: <EditZone /> },
    {
      path: 'zones/:zone',
      element: <ZonePage />,
      loader: ({ request, params }) => {
        const r = { project: projectOf(request), zone: params.zone ?? '' }
        return r.project
          ? warm(
              queryClient.ensureQueryData(zoneQuery(r)),
              queryClient.ensureQueryData(rrsetsQuery(r)),
            )
          : null
      },
      children: [
        { index: true, element: <RecordSets /> },
        { path: 'rrsets/create', element: <CreateRecordSet /> },
        { path: 'rrsets/:name/:type/edit', element: <EditRecordSet /> },
        {
          path: 'changes',
          element: <Changes />,
          loader: ({ request, params }) => {
            const r = { project: projectOf(request), zone: params.zone ?? '' }
            return r.project ? warm(queryClient.ensureQueryData(changesQuery(r))) : null
          },
        },
        { path: 'overview', element: <ZoneOverview /> },
      ],
    },
  ]
}

// The Cloud NAT view (SRS 4.8.3). Routers follow the API's resource
// names, with the Project in ?project=; a NAT gateway is under its
// router's nats/. The translation log's filters are ?src=, ?dst= and
// ?outcome=.
function natRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const router: RouteObject['loader'] = ({ request, params }) => {
    const r = {
      project: projectOf(request),
      region: params.region ?? '',
      router: params.router ?? '',
    }
    return r.project
      ? warm(
          queryClient.ensureQueryData(routerQuery(r)),
          queryClient.ensureQueryData(statusQuery(r)),
          queryClient.ensureQueryData(subnetsQuery(r.project)),
          ...(params.nat ? [queryClient.ensureQueryData(mappingsQuery(r, params.nat))] : []),
        )
      : null
  }
  return [
    {
      index: true,
      element: <Routers />,
      loader: ({ request }) => {
        const project = projectOf(request)
        return project
          ? warm(
              queryClient.ensureQueryData(routersQuery(project)),
              queryClient.ensureQueryData(subnetsQuery(project)),
              queryClient.ensureQueryData(sinkQuery()),
            )
          : null
      },
    },
    {
      path: 'translations',
      element: <TranslationLogPage />,
      loader: () => warm(queryClient.ensureQueryData(requestsQuery('nat'))),
    },
    { path: 'create', element: <CreateRouter /> },
    { path: 'regions/:region/routers/:router', element: <RouterPage />, loader: router },
    { path: 'regions/:region/routers/:router/edit', element: <EditRouter /> },
    { path: 'regions/:region/routers/:router/nats/add', element: <AddNat /> },
    { path: 'regions/:region/routers/:router/nats/:nat', element: <NatPage />, loader: router },
    { path: 'regions/:region/routers/:router/nats/:nat/edit', element: <EditNat /> },
  ]
}

// The VPC network view (SRS 4.8.3), the compute Service. Paths follow the
// API's resource names, global/networks/N and regions/R/subnetworks/S,
// with the Project in ?project=; create pages are not under them, so
// that no name is shadowed, and the subnetwork one takes the network in
// ?network=. Offered ports are the Instance's, at /compute/offers.
function vpcRoutes(queryClient: QueryClient): RouteObject[] {
  const warm = (...queries: Promise<unknown>[]) => Promise.allSettled(queries).then(() => null)
  const lists: RouteObject['loader'] = ({ request }) => {
    const project = projectOf(request)
    return project
      ? warm(
          queryClient.ensureQueryData(networksQuery(project)),
          queryClient.ensureQueryData(subnetsQuery(project)),
        )
      : null
  }
  return [
    { index: true, element: <Networks />, loader: lists },
    { path: 'subnetworks', element: <Subnetworks />, loader: lists },
    {
      path: 'offers',
      element: <Offers />,
      loader: () => warm(queryClient.ensureQueryData(hostOffersQuery())),
    },
    { path: 'create-network', element: <CreateNetwork /> },
    { path: 'create-subnetwork', element: <CreateSubnetwork /> },
    {
      path: 'global/networks/:network',
      element: <NetworkPage />,
      loader: ({ request, params }) => {
        const project = projectOf(request)
        return project
          ? warm(
              queryClient.ensureQueryData(networkQuery({ project, network: params.network ?? '' })),
              queryClient.ensureQueryData(subnetsQuery(project)),
            )
          : null
      },
    },
    { path: 'global/networks/:network/edit', element: <EditNetwork /> },
    {
      path: 'regions/:region/subnetworks/:subnetwork',
      element: <SubnetworkPage />,
      loader: ({ request, params }) => {
        const r = {
          project: projectOf(request),
          region: params.region ?? '',
          subnetwork: params.subnetwork ?? '',
        }
        return r.project ? warm(queryClient.ensureQueryData(subnetQuery(r))) : null
      },
    },
    { path: 'regions/:region/subnetworks/:subnetwork/edit', element: <EditSubnetwork /> },
  ]
}

export function createRouter(queryClient: QueryClient) {
  return createBrowserRouter(routes(queryClient), { basename: BASENAME })
}
