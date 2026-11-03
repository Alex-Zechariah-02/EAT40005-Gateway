import { expect, test } from '@playwright/test'
import { go, record, revisions, shot, versionsPart } from './pages'
import { admin, adminCall, adminRequests, finish, open, ready, status, strip, summary } from './setup'

// Overview, APIs, Versions, Accounts and Team, the Security Log account search and page addresses.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
    // The Security Log finds this account's requests.
    await summary('cust-001')
  })

  test.afterAll(finish)

  test('the overview, policy revisions, accounts and team views show the controller state as the Admin API reports it, and only traffic figures and the status refresh by themselves', async () => {
    await go(admin, 'Overview')
    const now = await status()
    const main = admin.getByRole('main')
    await expect(strip(admin).getByText('Accepting Requests', { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText(`Version ${now.confirmed}`)
    await expect(strip(admin).getByText(now.generation!, { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText('None active')
    await expect(strip(admin).getByText('None', { exact: true })).toBeVisible()
    // The policy timeline is the newest administrative history.
    const timeline = main.getByRole('region', { name: 'Policy Timeline' })
    await expect(timeline.getByRole('listitem').first()).toContainText(`Version ${now.confirmed}`)
    await expect(timeline.getByRole('listitem').first()).toContainText('Kept')

    // The Getting started guide ticks each step from what the gateway reports:
    // the baseline trial was kept, so all three are done. Hidden, it stays
    // hidden on the Overview until My account brings it back.
    const guide = main.locator('details.getting-started')
    await expect(guide.locator('summary')).toHaveText('Getting Started · 3 of 3 done')
    await guide.locator('summary').click()
    await expect(guide.getByRole('listitem')).toHaveCount(3)
    await expect(guide.getByText('To Do', { exact: true })).toHaveCount(0)
    await shot(admin, '08a-getting-started')
    await guide.getByRole('button', { name: 'Hide This Guide' }).click()
    await expect(guide).toHaveCount(0)
    await go(admin, 'Changes')
    await go(admin, 'Overview')
    await expect(timeline).toBeVisible()
    await expect(guide).toHaveCount(0)
    await go(admin, 'My Account')
    await main.getByRole('button', { name: 'Show the Getting Started Guide' }).click()
    await expect(guide.locator('summary')).toHaveText('Getting Started · 3 of 3 done')

    // The policy revisions view shows the document the server holds.
    const confirmed = (await adminCall('GET', '/admin/policy')).body as {
      id: number
      document: {
        upstreams: { id: string; dial: string }[]
        operations: { id: string; method: string; target: string; access: string; auth: string; body?: unknown; rate?: { count: number; window_seconds: number; key: string } }[]
        roles: { id: string }[]
        permits: { role: string; operation: string }[]
      }
    }
    const doc = confirmed.document
    await shot(admin, '08-overview')

    // The APIs view lists every operation of each API with its limit, from
    // the same document.
    await go(admin, 'APIs')
    for (const o of doc.operations) {
      const row = main.getByRole('row').filter({ has: admin.getByRole('link', { name: o.id, exact: true }) })
      await expect(row).toContainText(`${o.method} ${o.target}`)
      const span: Record<number, string> = { 1: 's', 60: 'min', 3600: 'h' }
      if (o.rate) await expect(row).toContainText(`${o.rate.count}/${span[o.rate.window_seconds] ?? `${o.rate.window_seconds} s`}`)
    }

    await revisions(admin)
    await expect(main.getByText(`Version ${confirmed.id},`)).toBeVisible()
    await versionsPart(admin, 'Service Addresses')
    for (const u of confirmed.document.upstreams) await expect(main.getByRole('row').filter({ hasText: u.id })).toContainText(u.dial)
    // The Endpoints tab lists each endpoint once; the role tables list them per role.
    await versionsPart(admin, 'Endpoints')
    const endpoints = main.getByRole('region', { name: 'Endpoints', exact: true })
    for (const o of confirmed.document.operations) {
      await expect(endpoints.getByRole('row').filter({ has: admin.getByRole('cell', { name: o.id, exact: true }) })).toContainText(`${o.method} ${o.target}`)
    }
    // The permit matrix has a column per role and, for every operation that
    // needs a token, exactly the permits the document holds.
    await versionsPart(admin, 'Roles')
    const matrix = main.getByRole('region', { name: 'Role Permissions' })
    for (const r of doc.roles) await expect(matrix.getByRole('columnheader', { name: new RegExp(`^${r.id}\\b`) })).toBeVisible()
    for (const o of doc.operations) {
      const row = matrix.getByRole('row').filter({ has: admin.getByRole('rowheader', { name: o.id, exact: true }) })
      if (o.access !== 'allow') {
        await expect(row).toContainText('closed to everyone by its access flag')
        continue
      }
      if (o.auth === 'none') {
        await expect(row).toContainText('public: no token or role needed')
        continue
      }
      const cells = row.getByRole('cell')
      await expect(cells).toHaveText(doc.roles.map((r) => (doc.permits.some((p) => p.role === r.id && p.operation === o.id) ? /permitted/ : /no permission/)))
    }
    // The request rules state each body field the schema declares, including
    // nested objects and each element of an array.
    await versionsPart(admin, 'Endpoints')
    const adjustRules = main.getByRole('region', { name: 'Request Rules of inventory.stock.adjust' })
    await expect(adjustRules.getByRole('row', { name: /^delta/ })).toHaveText(/delta\s*body\s*integer\s*required\s*-100 to 100/)
    const createRules = main.getByRole('region', { name: 'Request Rules of orders.create' })
    await expect(createRules.getByRole('row')).toHaveCount(14)
    await expect(createRules.getByRole('row', { name: /^items\s/ })).toContainText('1 to 20 items')
    await expect(createRules.getByRole('row', { name: /^items\[\]\s/ })).toContainText('every element')
    await expect(createRules.getByRole('row', { name: /^items\[\]\.quantity/ })).toContainText('1 to 50')
    await expect(createRules.getByRole('row', { name: /^address\.postcode/ })).toContainText('5 to 5 characters')
    await expect(main.getByRole('region', { name: 'Request Rules of orders.all' })).toHaveCount(0)
    await shot(admin, '09-policy')

    // The APIs page states the same rules in words, with the exact schema one
    // click away; it describes the policy and checks nothing itself.
    await go(admin, 'APIs')
    const ordersApi = main.getByRole('region', { name: 'Orders', exact: true })
    await ordersApi.getByText('Request Rules', { exact: true }).click()
    const apiRules = ordersApi.getByRole('region', { name: 'Request Rules of orders.create' })
    await expect(apiRules.getByRole('row')).toHaveCount(14)
    await expect(apiRules.getByRole('row', { name: /^items\s/ })).toContainText('1 to 20 items')
    await expect(ordersApi.locator('.more-details dt', { hasText: 'Body Schema (JSON)' }).first()).toBeAttached()

    // Accounts: nothing is blocked yet, and the accounts seen in the latest
    // requests carry the roles their verified tokens held.
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([])
    expect((await summary('ops-01')).status).toBe(200)
    await go(admin, 'Accounts')
    // One table lists every known account; none carries a Blocked badge yet.
    await expect(main.getByRole('row').filter({ has: admin.getByText('Blocked', { exact: true }) })).toHaveCount(0)
    await expect(main.getByRole('row').filter({ hasText: 'ops-01' })).toContainText('OPERATIONS')
    await shot(admin, '10-accounts')

    // Team lists the administrators and signing keys exactly as the Admin
    // API reports them.
    await go(admin, 'Team')
    for (const a of (await adminCall('GET', '/admin/administrators')).body.items as { name: string; status: string }[]) {
      await expect(main.getByRole('row').filter({ has: admin.getByRole('cell', { name: a.name, exact: true }) })).toContainText(a.status === 'active' ? 'Active' : a.status)
    }
    const keys = (await adminCall('GET', '/admin/keys')).body.items as { kid: string; fingerprint: string }[]
    expect(keys.length).toBeGreaterThan(0)
    for (const k of keys) await expect(main.getByRole('row').filter({ hasText: k.kid })).toContainText(k.fingerprint)
    await shot(admin, '10-team')

    // Every visible view rereads the gateway's status each second, so what
    // it shows is never more than a second old. A policy view reads nothing
    // else by itself.
    const idleReads = async (view: string) => {
      await go(admin, view)
      await admin.waitForTimeout(2_000)
      const start = adminRequests.length
      await admin.waitForTimeout(6_000)
      return adminRequests.slice(start).map((url) => new URL(url).pathname)
    }
    const changesIdle = await idleReads('Changes')
    expect(changesIdle.filter((path) => path !== '/admin/status')).toEqual([])
    expect(changesIdle.length).toBeGreaterThanOrEqual(4)
    expect(changesIdle.length).toBeLessThanOrEqual(8)
    // The Overview also rereads its traffic figures and recent refusals each
    // second and its alerts every 5 seconds, and nothing more.
    const idle = await idleReads('Overview')
    expect(idle.filter((path) => path !== '/admin/traffic' && path !== '/admin/events' && path !== '/admin/status' && path !== '/admin/alerts')).toEqual([])
    expect(idle.filter((path) => path === '/admin/alerts').length).toBeLessThanOrEqual(2)
    for (const path of ['/admin/traffic', '/admin/events', '/admin/status']) {
      expect(idle.filter((p) => p === path).length, path).toBeGreaterThanOrEqual(4)
      expect(idle.filter((p) => p === path).length, path).toBeLessThanOrEqual(8)
    }
    record('overview', { status: now, changesIdle: changesIdle.length, overviewIdle: idle.length, seconds: 6 })
  })

  test('the security log keeps an account search while moving between views without putting the account in the address, and a missing Ref says what was searched', async () => {
    const search = admin.getByRole('searchbox', { name: 'Search', exact: true })
    const account = admin.getByRole('main').getByLabel('Account', { exact: true })
    await go(admin, 'Overview')
    await search.fill('cust-001')
    await search.press('Enter')
    await expect(admin.getByRole('heading', { level: 1, name: 'Security Log' })).toBeVisible()
    await expect(account).toHaveValue('cust-001')
    await expect(admin.getByRole('heading', { name: /^[1-9]\d* requests?$/ })).toBeVisible()
    await expect(admin.getByText('No request matches.')).toHaveCount(0)
    await go(admin, 'Overview')
    await go(admin, 'Security Log')
    await expect(account).toHaveValue('cust-001')
    await expect(admin.getByRole('heading', { name: /^[1-9]\d* requests?$/ })).toBeVisible()
    await expect(admin.getByText('No request matches.')).toHaveCount(0)
    expect(admin.url()).not.toContain('cust-001')
    await search.fill('0'.repeat(32))
    await search.press('Enter')
    await expect(admin.getByRole('dialog')).toContainText(/Searched the latest 10,000 requests since the gateway started/)
    await admin.keyboard.press('Escape')
    await expect(admin.getByRole('dialog')).toHaveCount(0)
  })

  test('an address that names no view, an inherited name such as constructor included, opens the Overview', async () => {
    for (const name of ['constructor', '__proto__', 'toString', 'no-such-view']) {
      await go(admin, 'Live Traffic')
      await expect(admin).toHaveTitle('Live Traffic – Fetch Gateway')
      await admin.evaluate((fragment) => (window.location.hash = fragment), `#/${name}`)
      await expect(admin, name).toHaveTitle('Overview – Fetch Gateway')
      await expect(admin.locator('#page-title'), name).toBeVisible()
    }
  })

  test('returning to a hidden tab rereads the gateway at once, even on a page that does not poll', async () => {
    await go(admin, 'Changes')
    await admin.waitForTimeout(1500)
    const setVisibility = (state: 'hidden' | 'visible') =>
      admin.evaluate((value) => {
        Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value })
        document.dispatchEvent(new Event('visibilitychange'))
      }, state)
    try {
      const start = adminRequests.length
      await setVisibility('hidden')
      await admin.waitForTimeout(2000)
      expect(adminRequests.slice(start)).toEqual([])
      const shownAt = Date.now()
      await setVisibility('visible')
      await expect.poll(() => adminRequests.slice(start).map((url) => new URL(url).pathname)).toContain('/admin/status')
      const reread = Date.now() - shownAt
      expect(reread).toBeLessThan(2000)
      record('tab-return', { rereadMs: reread })
    } finally {
      await admin.evaluate(() => delete (document as unknown as Record<string, unknown>).visibilityState)
    }
  })

  test('a status answer saying the stored policy could not be read is shown as not known, never as no policy', async () => {
    // A display check of the contract field only: the gateway's own answer
    // is proven by its Go test, because a failing store read cannot be
    // caused safely in this deployment. The real status answer is fetched
    // and given the form the gateway sends when that read fails.
    await admin.route('**/admin/status', async (route) => {
      const response = await route.fetch()
      const body = (await response.json()) as Record<string, unknown>
      delete body.confirmed
      delete body.provisional
      delete body.deadline
      body.stored_authority = 'unreadable'
      await route.fulfill({ response, json: body })
    })
    try {
      await go(admin, 'Overview')
      await expect(strip(admin)).toContainText('Not known')
      await expect(strip(admin)).not.toContainText('None yet')
      await expect(admin.getByText('The gateway could not read its stored policy.', { exact: true })).toBeVisible()
      await shot(admin, '46-stored-policy-unreadable')
    } finally {
      await admin.unroute('**/admin/status')
    }
  })
})
