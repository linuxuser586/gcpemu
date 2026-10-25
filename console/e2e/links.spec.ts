import { expect, expectConsoleRequests, test } from './fixtures'
import { startInstance, type Instance } from './instance'

// #622: two Instances of their own per browser, a laptop and an office,
// pair through their consoles, approved on the office's side; the office
// shares a Project, the laptop delegates it and browses its resources,
// and once the office stops, the laptop shows that Project as
// unavailable. The laptop finds the office at a --link-discover address,
// and both use an mDNS port of their own so that they meet no other
// Instances.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/
const project = 'team-shared'

test.describe.configure({ mode: 'serial' })

test.describe('#622 Links, Delegated Projects and Pairing requests', () => {
  let office: Instance
  let laptop: Instance
  test.beforeAll(async () => {
    const env = { GCPEMU_LINK_MDNS_PORT: String(40000 + Math.floor(Math.random() * 20000)) }
    office = await startInstance(
      ['iam', 'gcs', 'pubsub'],
      ['--instance', 'office', '--link-bind', '127.0.0.1'],
      { env },
    )
    laptop = await startInstance(
      ['iam', 'gcs', 'pubsub'],
      ['--instance', 'laptop', '--link-discover', office.link ?? ''],
      {
        env,
      },
    )
  })
  test.afterAll(async () => {
    await laptop?.stop()
    await office?.stop()
  })

  test('pairs two Instances, approved on the other side', async ({ page, context, requests }) => {
    await page.goto(`${laptop.url}/console/links`)
    await expect(page.getByRole('heading', { name: 'Links', level: 1 })).toBeVisible()
    await expect(page.getByText(/No links yet/)).toBeVisible()
    const found = page.getByRole('table', { name: 'Discovered Instances' })
    await found.getByRole('button', { name: /^Pair office@/ }).click()
    const dialog = page.getByRole('dialog', { name: /^Pair with office@/ })
    await dialog.getByRole('button', { name: 'Send Pairing request' }).click()
    await expect(dialog.getByRole('status')).toContainText('Waiting for office@')

    const other = await context.newPage()
    await other.goto(`${office.url}/console/links`)
    const waiting = other.getByRole('table', { name: 'Pairing requests' })
    await waiting.getByRole('button', { name: /^Approve / }).click()
    await other.getByRole('dialog').getByRole('button', { name: 'Approve' }).click()
    await expect(other.getByText(/^Paired with /)).toBeVisible()
    await expect(other.getByText('No Pairing requests waiting.')).toBeVisible()
    await expect(other.getByRole('region', { name: /^Link / })).toBeVisible()

    await expect(page.getByText('Paired with office.')).toBeVisible()
    const link = page.getByRole('region', { name: 'Link office' })
    await expect(link.getByText('reachable', { exact: true })).toBeVisible()
    await expect(link.getByText('paired', { exact: true })).toBeVisible()
    await expect(found.getByText('paired as office')).toBeVisible()

    await expectConsoleRequests(requests, { allowed })
  })

  test('delegates a Project the other shares, and browses its resources', async ({
    page,
    context,
    request,
  }) => {
    expect(
      (
        await request.post(`${office.url}/storage/v1/b?project=${project}`, {
          data: { name: `${project}-office-bucket` },
        })
      ).status(),
    ).toBe(200)

    const other = await context.newPage()
    await other.goto(`${office.url}/console/links`)
    const form = other.getByRole('form', { name: 'Share a Project' })
    await form.getByRole('textbox', { name: 'Project ID' }).fill(project)
    await form.getByRole('button', { name: 'Share' }).click()
    await expect(other.getByText(`Shared ${project} with every link.`)).toBeVisible()

    await page.goto(`${laptop.url}/console/links`)
    const sharedByIt = page
      .getByRole('region', { name: 'Link office' })
      .getByRole('list', { name: 'Shared by office' })
    await sharedByIt.getByRole('button', { name: `Delegate ${project}` }).click()
    await expect(page.getByText(`Delegated ${project} to office.`)).toBeVisible()
    await expect(sharedByIt.getByText('delegated', { exact: true })).toBeVisible()

    await page.getByRole('button', { name: 'Project' }).click()
    await page
      .getByRole('list', { name: 'Delegated Projects' })
      .getByRole('button', { name: new RegExp(project) })
      .click()
    await expect(page.getByText('via office')).toBeVisible()
    await page
      .getByRole('navigation', { name: 'Services' })
      .getByRole('link', { name: 'Cloud Storage' })
      .click()
    await expect(
      page.getByTestId('bucket').filter({ hasText: `${project}-office-bucket` }),
    ).toBeVisible()
    // The laptop reads the office's bucket through the link too.
    expect(
      (await request.get(`${laptop.url}/storage/v1/b/${project}-office-bucket`)).status(),
    ).toBe(200)
  })

  test('shows the Projects of an unreachable Linked Instance as unavailable', async ({ page }) => {
    await office.stop()
    await page.goto(`${laptop.url}/console/links?project=${project}`)
    const link = page.getByRole('region', { name: 'Link office' })
    await expect(link.getByText('unreachable', { exact: true })).toBeVisible()
    const row = page.getByRole('table', { name: 'Delegated Projects' }).getByRole('row', {
      name: new RegExp(project),
    })
    await expect(row.getByText('unavailable')).toBeVisible()
    await expect(page.getByText('via office')).toBeVisible()

    await page.getByRole('button', { name: 'Project' }).click()
    await expect(
      page
        .getByRole('list', { name: 'Delegated Projects' })
        .getByRole('button', { name: new RegExp(project) }),
    ).toContainText('unavailable')
  })
})
