import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { adminURL } from './env'
import { drawer, drawerFact, openLink, send, type RequestRecord } from './drawer'
import { enrol, record, shot, signIn } from './pages'
import { admin, adminCall, demo, finish, handedOut, newContext, open, password, ready } from './setup'

// The request drawer's links to other pages, its Block action and who is
// offered it, and its accessibility at three widths and by keyboard.
// Each file starts its own gateway; setup.ts builds the state the tests read.

let refused: RequestRecord
let blocked: RequestRecord
let viewerSecret: string

// openAt opens a page with one request's drawer over it, as a pasted address would.
async function openAt(page: Page, view: string, ref: string) {
  await page.goto(`${adminURL}/#/${view}?ref=${ref}`)
  await expect(drawer(page).getByRole('heading', { level: 2 }).first()).toBeVisible()
  await expect(page.getByText(/^Reading/)).toHaveCount(0)
}

test.describe.serial('request drawer links', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    // Enrolling restarts the gateway, so it comes before any sign-in or request.
    viewerSecret = await demo.enrol('viewer', 'read')
    handedOut.push(viewerSecret)
    await ready()
    refused = await send('/reports/sales', 'cust-001')
    expect((await adminCall('PUT', '/admin/blocked-accounts/cust-002', { reason: 'Drawer check' }, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
    blocked = await send('/reports/sales', 'cust-002')
  })

  test.afterAll(finish)

  test('the endpoint opens the Policy page, the version opens Versions, the account opens the Security Log, and the client address is text', async () => {
    const version = /^r(\d+)/.exec(refused.generation)![1]
    await openAt(admin, 'overview', refused.request_id)
    await expect(drawerFact(admin, 'Client Address').locator('code')).toHaveText(refused.client_address)
    await expect(drawerFact(admin, 'Client Address').getByRole('link')).toHaveCount(0)

    await drawerFact(admin, 'Method and Path').getByRole('link').click()
    expect(admin.url()).toBe(`${adminURL}/#/policy?select=operation%3A${refused.operation}`)
    await expect(admin.getByRole('heading', { level: 1, name: 'Policy' })).toBeVisible()
    await expect(drawer(admin)).toHaveCount(0)
    await admin.goBack()
    await expect(drawerFact(admin, 'Response')).toContainText('Refused · 403')

    await drawerFact(admin, 'Policy Version').getByRole('link', { name: `Version ${version}` }).click()
    expect(admin.url()).toBe(`${adminURL}/#/versions?version=${version}`)
    await expect(admin.getByRole('heading', { name: `Version ${version}`, exact: true })).toBeVisible()
    await admin.goBack()

    await drawerFact(admin, 'Account').getByRole('link', { name: 'cust-001' }).click()
    await expect(admin.getByRole('heading', { level: 1, name: 'Security Log' })).toBeVisible()
    await expect(admin.getByRole('main').getByLabel('Account', { exact: true })).toHaveValue('cust-001')
    await expect(openLink(admin, refused.request_id)).toBeVisible()
    expect(admin.url()).not.toContain('cust-001')
  })

  test('Block is offered only for an account not yet blocked and only with the Block Accounts permission; it opens the usual confirmation', async ({ browser }) => {
    await openAt(admin, 'overview', refused.request_id)
    await drawer(admin).getByRole('button', { name: 'Block Account…' }).click()
    await expect(admin.getByRole('heading', { level: 1, name: 'Accounts' })).toBeVisible()
    await expect(admin.getByRole('dialog').getByRole('heading', { name: 'Block an Account' })).toBeVisible()
    await expect(admin.getByLabel('Account ID')).toHaveValue('cust-001')
    await admin.getByRole('dialog').getByRole('button', { name: 'Cancel', exact: true }).click()
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([expect.objectContaining({ subject: 'cust-002' })])
    expect(admin.url()).not.toContain('cust-001')

    await openAt(admin, 'overview', blocked.request_id)
    await expect(drawerFact(admin, 'Account')).toContainText('cust-002')
    await expect(drawerFact(admin, 'Account')).toContainText('Blocked')
    await expect(drawer(admin).getByRole('button', { name: 'Block Account…' })).toHaveCount(0)

    // An administrator who may only view status gets no Block action, no
    // account links and a plain answer to an account search.
    const context = await newContext(browser)
    try {
      const viewer = await context.newPage()
      await viewer.goto(adminURL + '/')
      await enrol(viewer, 'viewer', viewerSecret, password)
      await signIn(viewer, 'viewer', password)
      await openAt(viewer, 'overview', refused.request_id)
      await expect(drawerFact(viewer, 'Account')).toContainText('cust-001')
      await expect(drawerFact(viewer, 'Account').getByRole('link')).toHaveCount(0)
      await expect(drawer(viewer).getByRole('button', { name: 'Block Account…' })).toHaveCount(0)
      await viewer.keyboard.press('Escape')
      // Escape closes the drawer on a later history change; until then the search is inert.
      await expect(drawer(viewer)).toBeHidden()
      await expect(viewer.getByRole('main').getByRole('link', { name: 'cust-001', exact: true })).toHaveCount(0)
      const search = viewer.getByRole('searchbox', { name: 'Search', exact: true })
      await search.fill('cust-001')
      await search.press('Enter')
      await expect(viewer.getByText('Searching by account or endpoint needs the View History permission.')).toBeVisible()
      await search.fill('f'.repeat(32))
      await search.press('Enter')
      await expect(drawer(viewer)).toContainText(
        'Only the latest 10,000 requests since the gateway started are searched. Older requests are in the Security Log, which needs the View History permission.',
      )
    } finally {
      await context.close()
    }
  })

  test('the open drawer passes the accessibility rules at 1440, 768 and 375 px with no sideways scrolling, keeps keyboard focus inside and shows a time in UTC on focus', async () => {
    const results: Record<string, string[]> = {}
    for (const width of [1440, 768, 375]) {
      await admin.setViewportSize({ width, height: 900 })
      await openAt(admin, 'live-traffic', refused.request_id)
      // Contrast is judged on the settled screen, after the drawer's entrance.
      await admin.evaluate(() => Promise.all(document.getAnimations().map((animation) => animation.finished.catch(() => undefined))))
      const report = await new AxeBuilder({ page: admin }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22a', 'wcag22aa']).analyze()
      results[width] = report.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`)
      expect(results[width], `${width} px`).toEqual([])
      const overflow = await admin.evaluate(() => {
        const dialog = document.querySelector('dialog[open]')!
        return { page: document.documentElement.scrollWidth - innerWidth, drawer: dialog.scrollWidth - dialog.clientWidth, right: dialog.getBoundingClientRect().right - innerWidth }
      })
      expect(overflow, `${width} px`).toEqual({ page: 0, drawer: 0, right: 0 })
      await shot(admin, `62-drawer-${width}`)
    }
    record('drawer-accessibility', results)

    await admin.setViewportSize({ width: 1280, height: 860 })
    await admin.goto(`${adminURL}/#/overview`)
    const link = openLink(admin, refused.request_id)
    await link.focus()
    await admin.keyboard.press('Enter')
    await expect(drawer(admin)).toBeVisible()
    const inside = () => admin.evaluate(() => !!document.activeElement?.closest('dialog[open]'))
    for (let i = 0; i < 16; i++) {
      await admin.keyboard.press(i < 12 ? 'Tab' : 'Shift+Tab')
      expect(await inside(), `Tab ${i + 1}`).toBe(true)
    }
    const time = drawerFact(admin, 'Time').locator('time')
    await time.focus()
    await admin.keyboard.press('Shift+Tab')
    await admin.keyboard.press('Tab')
    await expect(time).toBeFocused()
    const tip = admin.locator('.tooltip')
    await expect(tip).toHaveText(/^\d{1,2} \w{3} \d{4}, \d{2}:\d{2}:\d{2} UTC\+00:00$/)
    expect(await tip.evaluate((element) => !!element.closest('dialog[open]'))).toBe(true)
    // Focus that scrolls the drawer under a resting pointer keeps the focused time's tip.
    await admin.setViewportSize({ width: 1280, height: 520 })
    const box = (await drawer(admin).boundingBox())!
    await admin.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await drawer(admin).evaluate((element) => element.scrollTo(0, element.scrollHeight))
    await time.focus()
    await admin.evaluate(() => new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done))))
    await expect(tip).toHaveText(/ UTC\+00:00$/)
    await admin.keyboard.press('Escape')
    await expect(drawer(admin)).toHaveCount(0)
    await expect(link).toBeFocused()
  })
})
