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
import { Operations } from '@/features/operations/Operations'
import { ServicePage } from '@/features/services/ServicePage'

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

export function createRouter(queryClient: QueryClient) {
  return createBrowserRouter(routes(queryClient), { basename: BASENAME })
}
