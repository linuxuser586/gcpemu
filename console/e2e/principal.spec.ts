import { expect, projectId, test } from './fixtures'

// The Principal switcher (FR-UI-004) against the Instance's gateway. Other
// tests share the Instance, so it stays at the audit IAM enforcement
// level: the service account's calls are allowed but the denial log names
// it, which proves the gateway attributed them to it. What enforce shows
// is left to the component tests.

interface Denial {
  principal: string
  permission: string
}

test('Principal switcher: the console acts as the chosen service account', async ({
  page,
  request,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-prin${testInfo.retry}`, browserName)
  const email = `viewer@${id}.iam.gserviceaccount.com`
  expect(
    (
      await request.post(`/iam/v1/projects/${id}/serviceAccounts`, {
        data: { accountId: 'viewer' },
      })
    ).ok(),
  ).toBe(true)
  const deniedTo = async (principal: string) => {
    const res = await request.get('/_emu/v1/iam/denials')
    const { denials } = (await res.json()) as { denials: Denial[] }
    return denials.some((d) => d.principal === principal && d.permission === 'storage.buckets.list')
  }

  await page.goto(`/console/gcs?project=${id}`)
  await expect(page.getByRole('button', { name: 'Acting as' })).toHaveText(/Default principal/)
  await page.getByRole('button', { name: 'Acting as' }).click()
  await page
    .getByRole('list', { name: 'Service accounts' })
    .getByRole('button', { name: email })
    .click()
  await expect(page.getByRole('button', { name: 'Acting as' })).toHaveText(new RegExp(email))
  await expect.poll(() => deniedTo(`serviceAccount:${email}`)).toBe(true)

  // The choice is per browser: it survives a reload but not the link.
  expect(new URL(page.url()).search).toBe(`?project=${id}`)
  await page.reload()
  await expect(page.getByRole('button', { name: 'Acting as' })).toHaveText(new RegExp(email))
})
