import { expect, test } from '@playwright/test'
import { adminURL } from './env'
import { drawer, drawerFact, full, openLink, send, type RequestRecord } from './drawer'
import { go, nav, record, shot } from './pages'
import { admin, finish, open, ready } from './setup'

// The request drawer: one request's facts and checks, opened from every
// request row, from the top search and from its own address.
// Each file starts its own gateway; setup.ts builds the state the tests read.

let anonymous: RequestRecord
let refused: RequestRecord

test.describe.serial('request drawer', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
    anonymous = await send('/reports/summary')
    refused = await send('/reports/sales', 'cust-001')
  })

  test.afterAll(finish)

  test('an Overview row opens its request in a drawer at its own address, with the facts the gateway recorded', async () => {
    expect(anonymous).toMatchObject({ decision: 'refused', status: 401, subject: null })
    await go(admin, 'Overview')
    const row = admin.getByRole('region', { name: 'Recent Refusals' }).getByRole('row').filter({ hasText: anonymous.request_id.slice(0, 8) })
    await expect(row).toContainText('Refused · 401')
    await openLink(admin, anonymous.request_id).click()
    expect(admin.url()).toBe(`${adminURL}/#/overview?ref=${anonymous.request_id}`)
    const d = drawer(admin)
    await expect(d.getByRole('heading', { name: 'GET /reports/summary' })).toBeVisible()
    await expect(d.locator('dt')).toHaveText(['Time', 'Method and Path', 'Response', 'Policy Version', 'Account', 'Client Address', 'Reason', 'Ref'])
    const time = drawerFact(admin, 'Time').locator('time')
    await expect(time).toHaveText(full)
    await expect(time).toHaveAttribute('datetime', anonymous.time)
    await expect(drawerFact(admin, 'Method and Path')).toContainText('GET /reports/summary')
    await expect(drawerFact(admin, 'Response')).toContainText('Refused · 401')
    const version = /^r(\d+)/.exec(anonymous.generation)![1]
    await expect(drawerFact(admin, 'Policy Version')).toContainText(`Version ${version}`)
    await expect(drawerFact(admin, 'Policy Version')).toContainText(anonymous.generation)
    await expect(drawerFact(admin, 'Account')).toHaveText('None (no token)')
    await expect(drawerFact(admin, 'Client Address')).toHaveText(anonymous.client_address)
    const checks = d.getByRole('list', { name: "The gateway's checks, in order" })
    await expect(checks.getByRole('listitem').filter({ hasText: 'Route' })).toContainText('Passed')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Sign-In' })).toContainText('Refused Here')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Role' })).toContainText('Not Reached')
    await expect(drawerFact(admin, 'Reason')).toContainText('Not Signed In or Token Not Accepted')
    await expect(drawerFact(admin, 'Reason')).toContainText('unauthenticated')
    await expect(drawerFact(admin, 'Ref')).toContainText(anonymous.request_id)
    await expect(drawerFact(admin, 'Ref').getByRole('button', { name: 'Copy Ref' })).toBeVisible()
    await shot(admin, '60-drawer-overview')
  })

  test('Back closes the drawer, a reload or a pasted address opens it, and Escape or Close return focus to the row', async () => {
    await admin.goBack()
    await expect(drawer(admin)).toHaveCount(0)
    expect(admin.url()).toBe(`${adminURL}/#/overview`)
    const link = openLink(admin, anonymous.request_id)
    await expect(link).toBeFocused()

    await link.click()
    await expect(drawer(admin)).toBeVisible()
    await drawer(admin).getByRole('button', { name: 'Close', exact: true }).click()
    await expect(drawer(admin)).toHaveCount(0)
    await expect(link).toBeFocused()
    expect(admin.url()).toBe(`${adminURL}/#/overview`)

    // By keyboard: Enter on the row's link opens it, Escape closes it.
    await link.focus()
    await admin.keyboard.press('Enter')
    await expect(drawer(admin)).toBeVisible()
    await admin.keyboard.press('Escape')
    await expect(drawer(admin)).toHaveCount(0)
    await expect(link).toBeFocused()

    // A pasted address, and a reload of it, show the page with the drawer open.
    await admin.goto(`${adminURL}/#/live-traffic?ref=${refused.request_id}`)
    await expect(admin.getByRole('heading', { level: 1, name: 'Live Traffic' })).toBeVisible()
    await expect(drawer(admin).getByRole('heading', { name: 'GET /reports/sales' })).toBeVisible()
    await admin.reload()
    await expect(drawer(admin).getByRole('heading', { name: 'GET /reports/sales' })).toBeVisible()
    await expect(drawerFact(admin, 'Response')).toContainText('Refused · 403')
    await drawer(admin).getByRole('button', { name: 'Close', exact: true }).click()
    await expect(drawer(admin)).toHaveCount(0)
    expect(admin.url()).toBe(`${adminURL}/#/live-traffic`)
  })

  test('Live Traffic and Security Log rows open the drawer; the stream keeps running beneath it without taking focus', async () => {
    await go(admin, 'Live Traffic')
    const stream = admin.getByRole('region', { name: /^Request Stream/ })
    await expect(stream.getByRole('status')).toHaveText('Live')
    const allowed = await send('/reports/sales', 'ops-01')
    await expect(stream.getByRole('row').filter({ hasText: allowed.request_id.slice(0, 8) })).toContainText('Allowed · 200')
    await openLink(admin, allowed.request_id).click()
    expect(admin.url()).toBe(`${adminURL}/#/live-traffic?ref=${allowed.request_id}`)
    await expect(drawerFact(admin, 'Response')).toContainText('Allowed · 200')
    await expect(drawerFact(admin, 'Account')).toContainText('ops-01')
    const checks = drawer(admin).getByRole('list', { name: "The gateway's checks, in order" })
    await expect(checks.getByRole('listitem').filter({ hasText: 'Forward' })).toContainText('Passed')
    const focused = () => admin.evaluate(() => !!document.activeElement?.closest('dialog'))
    expect(await focused()).toBe(true)
    const later = await send('/reports/sales', 'ops-01')
    await admin.waitForTimeout(1500)
    expect(await focused()).toBe(true)
    await admin.keyboard.press('Escape')
    await expect(stream.getByRole('row').filter({ hasText: later.request_id.slice(0, 8) })).toBeVisible()

    await go(admin, 'Security Log')
    await admin.getByRole('main').getByLabel('Account', { exact: true }).fill('cust-001')
    // The log writer is asynchronous: search until the record is there.
    await expect(async () => {
      await admin.getByRole('button', { name: 'Search', exact: true }).click()
      await expect(openLink(admin, refused.request_id)).toBeVisible({ timeout: 1000 })
    }).toPass()
    await openLink(admin, refused.request_id).click()
    expect(admin.url()).toBe(`${adminURL}/#/security-log?ref=${refused.request_id}`)
    await expect(drawerFact(admin, 'Response')).toContainText('Refused · 403')
    await admin.goBack()
    await expect(drawer(admin)).toHaveCount(0)
    await expect(openLink(admin, refused.request_id)).toBeFocused()
  })

  test('the top search opens a Ref over the current page, and a Ref not found says what was searched', async () => {
    await go(admin, 'Versions')
    const search = admin.getByRole('searchbox', { name: 'Search', exact: true })
    await search.fill(refused.request_id)
    await search.press('Enter')
    expect(admin.url()).toBe(`${adminURL}/#/versions?ref=${refused.request_id}`)
    await expect(admin.getByRole('heading', { level: 1, name: 'Versions' })).toBeVisible()
    await expect(drawer(admin).getByRole('heading', { name: 'GET /reports/sales' })).toBeVisible()
    await admin.keyboard.press('Escape')
    await expect(search).toBeFocused()

    const missing = '0'.repeat(32)
    await search.fill(missing)
    await search.press('Enter')
    const d = drawer(admin)
    await expect(d.getByRole('heading', { name: 'No Request with This Ref' })).toBeVisible()
    await expect(d).toContainText(missing)
    await expect(d).toContainText('Searched the latest 10,000 requests since the gateway started, then the Security Log files the gateway still holds.')
    await shot(admin, '61-drawer-not-found')
    await admin.keyboard.press('Escape')
  })

  test('rows show the short outcome and status on one line at 1280 px, and the old Request Lookup address opens the Overview', async () => {
    await admin.setViewportSize({ width: 1280, height: 860 })
    await go(admin, 'Overview')
    const results = admin.getByRole('region', { name: 'Recent Refusals' }).locator('tbody td.result')
    expect(await results.count()).toBeGreaterThan(0)
    const lines: number[] = []
    for (const cell of await results.all()) {
      await expect(cell).toHaveText(/^(Allowed|Refused|Outcome Unknown) · \d{3}$/)
      lines.push(await cell.evaluate((td) => td.firstElementChild!.getClientRects().length))
    }
    expect(lines.every((n) => n === 1)).toBe(true)
    const height = await admin.getByRole('region', { name: 'Recent Refusals' }).locator('tbody tr').first().evaluate((tr) => tr.getBoundingClientRect().height)
    expect(height).toBeLessThan(56)
    record('request-row', { width: 1280, height })

    await admin.evaluate(() => (window.location.hash = '#/request-lookup'))
    await expect(admin).toHaveTitle('Overview – Fetch Gateway')
    await expect(nav(admin, 'Request Lookup')).toHaveCount(0)
  })
})
