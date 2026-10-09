import type { QueryClient } from '@tanstack/react-query'
import { createBrowserRouter, type RouteObject } from 'react-router'

import {
  containersQuery,
  endpointsQuery,
  envQuery,
  infoQuery,
  operationsQuery,
  readinessQuery,
  resourceCountsQuery,
} from '@/api/queries'
import { AppShell } from '@/components/shell/AppShell'
import { RouteError } from '@/components/shell/RouteError'
import { Dashboard } from '@/features/dashboard/Dashboard'
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
import { Namespaces, Nodes, Pods, Workloads } from '@/features/gke/Kube'
import { Negs } from '@/features/gke/Negs'
import { CreateNodePool, EditNodePool } from '@/features/gke/NodePoolForm'
import { NodePoolPage, NodePools } from '@/features/gke/NodePools'
import { PodLogs } from '@/features/gke/PodLogs'
import { kindInfo, kindQuery, listenersQuery, resourceQuery, type Coll } from '@/features/lb/api'
import { LbLayout } from '@/features/lb/LbLayout'
import { CreateResource, EditResource } from '@/features/lb/ResourceForm'
import { ResourcePage } from '@/features/lb/ResourcePage'
import { Resources } from '@/features/lb/Resources'
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
        { path: 'negs', element: <Negs /> },
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
// create/, so that no name is shadowed.
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
    { path: 'global/:coll/:name', element: <ResourcePage />, loader: page },
    { path: 'global/:coll/:name/edit', element: <EditResource /> },
    { path: 'regions/:region/:coll/:name', element: <ResourcePage />, loader: page },
    { path: 'regions/:region/:coll/:name/edit', element: <EditResource /> },
    { path: ':coll', element: <Resources />, loader: list() },
  ]
}

export function createRouter(queryClient: QueryClient) {
  return createBrowserRouter(routes(queryClient), { basename: BASENAME })
}
