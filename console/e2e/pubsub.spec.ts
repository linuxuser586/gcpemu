import type { APIRequestContext, Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Pub/Sub view (SRS 4.8.3, FR-UI-011) against the Instance's pubsub
// Service. Each attempt has its own Project, so a retry does not meet the
// topics of the one before.

// The console's paths: its bundle, the admin API and public GCP API paths.
const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

/** tile is the value of one figure of the subscription's backlog. */
const tile = (page: Page, label: string) =>
  page.getByLabel('Backlog').locator('div').filter({ hasText: label }).locator('dd').first()

async function publish(page: Page, body: string, attributes: string, orderingKey: string) {
  await page.getByRole('button', { name: 'Publish message' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Message body').fill(body)
  await dialog.getByLabel('Attributes').fill(attributes)
  await dialog.getByLabel('Ordering key').fill(orderingKey)
  const published = page.waitForResponse((r) => r.url().endsWith(':publish') && r.ok())
  await dialog.getByRole('button', { name: 'Publish' }).click()
  await published
  await dialog.getByRole('button', { name: 'Done' }).click()
}

/** localTime is t as a datetime-local input's value, in the browser's time zone. */
function localTime(t: Date): string {
  const d = new Date(t)
  d.setMinutes(d.getMinutes() - d.getTimezoneOffset())
  return d.toISOString().slice(0, 19)
}

test('Pub/Sub: topics, subscriptions, publish, pull, ack, peek, seek and purge', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-ps${testInfo.retry}`, browserName)
  await page.goto(`/console/pubsub?project=${id}`)
  await expect(page.getByText('No topics in this Project yet.')).toBeVisible()

  // Topics: the form's validation, then two topics.
  await page.getByRole('link', { name: 'Create topic' }).click()
  await page.getByLabel('Topic ID').fill('1-orders')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByText(`Invalid [topics] name: (name=projects/${id}/topics/1-orders)`),
  ).toBeVisible()
  await page.getByLabel('Topic ID').fill('orders-dlq')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/pubsub/topics/orders-dlq\\?project=${id}$`))
  await page.getByRole('link', { name: 'Topics', exact: true }).click()
  await page.getByRole('link', { name: 'Create topic' }).click()
  await page.getByLabel('Topic ID').fill('orders')
  await page.getByLabel('Labels').fill('team=e2e')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/pubsub/topics/orders\\?project=${id}$`))
  await expect(page.getByText('No subscriptions yet')).toBeVisible()

  // A subscription to it: the API rejects what the form cannot check.
  await page.getByRole('link', { name: 'Create subscription' }).click()
  await page.getByLabel('Subscription ID').fill('orders-worker')
  await expect(page.getByLabel('Topic', { exact: true })).toHaveValue(
    `projects/${id}/topics/orders`,
  )
  await page.getByLabel('Filter').fill('attributes.kind ==')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByRole('alert').filter({ hasText: 'Invalid filter expression' }),
  ).toBeVisible()
  await page.getByLabel('Filter').fill('')
  await page.getByLabel('Retain acknowledged messages').check()
  await page.getByLabel('Dead-letter topic').fill(`projects/${id}/topics/orders-dlq`)
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/pubsub/subscriptions/orders-worker\\?project=${id}$`),
  )
  await expect(tile(page, 'Unacked messages')).toHaveText('0')
  const before = new Date()

  // Publish two messages with attributes and an ordering key.
  await page.getByRole('link', { name: 'Overview' }).click()
  await page.getByRole('link', { name: 'orders', exact: true }).click()
  await publish(page, 'first', 'kind=new', 'cust-1')
  await publish(page, 'second', 'kind=paid', 'cust-1')
  const row = page.getByTestId('subscription').filter({ hasText: 'orders-worker' })
  await expect(row).toContainText('2 (')

  // Peek returns them without acking them; pull leases them in order.
  await row.getByRole('link', { name: 'orders-worker' }).click()
  await expect(tile(page, 'Unacked messages')).toHaveText('2')
  const messages = page.getByTestId('message')
  await page.getByRole('button', { name: 'Peek' }).click()
  await expect(messages).toHaveCount(2)
  await expect(messages.filter({ hasText: 'Peeked' })).toHaveCount(2)
  await expect(tile(page, 'Unacked messages')).toHaveText('2')
  await page.getByLabel('Messages at most').fill('10')
  await page.getByRole('button', { name: 'Pull' }).click()
  await expect(messages.getByRole('button', { name: /^Ack message/ })).toHaveCount(2)
  const first = messages.filter({ hasText: 'first' })
  await expect(first).toContainText('kind=new')
  await expect(first).toContainText('cust-1')
  await first.getByRole('button', { name: /^Ack message/ }).click()
  await expect(messages).toHaveCount(1)
  await expect(tile(page, 'Unacked messages')).toHaveText('1')

  // Purge acks the rest; seeking back replays the retained messages.
  await page.getByRole('button', { name: 'Purge messages' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Purge messages' }).click()
  await expect(tile(page, 'Unacked messages')).toHaveText('0')
  await page.getByRole('button', { name: 'Seek' }).click()
  await page
    .getByRole('dialog')
    .getByLabel('Time')
    .fill(localTime(new Date(before.getTime() - 60_000)))
  await page.getByRole('dialog').getByRole('button', { name: 'Seek' }).click()
  await expect(tile(page, 'Unacked messages')).toHaveText('2')

  // Edit: labels and a longer acknowledgement deadline.
  await page.getByRole('link', { name: 'Edit subscription' }).click()
  await page.getByLabel('Acknowledgement deadline (seconds)').fill('60')
  await page.getByLabel('Labels').fill('tier=gold')
  await page.getByRole('button', { name: 'Save' }).click()
  const details = page.getByLabel('Subscription details')
  await expect(details).toContainText('60 s')
  await expect(details).toContainText('tier=gold')
  await expect(details).toContainText(`projects/${id}/topics/orders-dlq after 5 delivery attempts`)

  // Delete the subscription, then the topic.
  await page.getByRole('button', { name: 'Delete subscription' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete subscription' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/pubsub/subscriptions\\?project=${id}$`))
  await expect(page.getByText('No subscriptions in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Topics', exact: true }).click()
  await page.getByRole('link', { name: 'orders', exact: true }).click()
  await page.getByRole('button', { name: 'Delete topic' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete topic' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/pubsub\\?project=${id}$`))
  await expect(page.getByTestId('topic')).toHaveCount(1)

  for (const r of requests) {
    expect(new URL(r.url()).pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
})

async function put(request: APIRequestContext, path: string, data: unknown) {
  expect((await request.put(`/pubsub/v1/${path}`, { data })).ok()).toBe(true)
}

test('Pub/Sub: a message peeked at too often is dead-lettered', async ({
  page,
  request,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-psd${testInfo.retry}`, browserName)
  const topic = (t: string) => `projects/${id}/topics/${t}`
  await put(request, topic('jobs'), {})
  await put(request, topic('jobs-dlq'), {})
  await put(request, `projects/${id}/subscriptions/jobs-dlq-sub`, { topic: topic('jobs-dlq') })
  await put(request, `projects/${id}/subscriptions/jobs-worker`, {
    topic: topic('jobs'),
    deadLetterPolicy: { deadLetterTopic: topic('jobs-dlq'), maxDeliveryAttempts: 5 },
  })
  const res = await request.post(`/pubsub/v1/${topic('jobs')}:publish`, {
    data: { messages: [{ data: Buffer.from('poison').toString('base64') }] },
  })
  expect(res.ok()).toBe(true)

  await page.goto(`/console/pubsub/subscriptions/jobs-worker?project=${id}`)
  await expect(tile(page, 'Unacked messages')).toHaveText('1')
  for (let attempt = 1; attempt <= 5; attempt++) {
    await page.getByRole('button', { name: 'Peek' }).click()
    await expect(page.getByTestId('message')).toContainText('poison')
    await expect(page.getByTestId('message').getByRole('cell').nth(5)).toHaveText(String(attempt))
  }
  await expect(tile(page, 'Dead-lettered')).toHaveText('1')
  await expect(tile(page, 'Unacked messages')).toHaveText('0')

  await page.getByRole('link', { name: 'Subscriptions', exact: true }).click()
  const dlq = page.getByTestId('subscription').filter({ hasText: 'jobs-dlq-sub' })
  await expect(dlq).toContainText('1 (')
})
