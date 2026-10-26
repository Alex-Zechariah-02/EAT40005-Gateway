import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'
import AxeBuilder from '@axe-core/playwright'
import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { AdminApi, build, Demo, gatewayIdentity, siteURL, webRoot } from './env'
import { captureShop, record, shopPage, shopStatus, shot, useSlot } from './pages'

const password = 'controlled shop acceptance password 42'
let demo: Demo
let api: AdminApi
let context: BrowserContext
let page: Page
const tokens: Record<string, string> = {}
const requests: { method: string; url: string }[] = []
const violations: string[] = []
const consoleLines: string[] = []

function orderForm() {
  return page.getByRole('form', { name: 'Place an order', exact: true })
}
function stockRow(name = 'The Widget') {
  return page.getByRole('row').filter({ has: page.getByText(name, { exact: true }) })
}
async function product(id = 'ITM-001') {
  if (await page.evaluate((item) => location.hash === `#/product/${item}`, id)) {
    await page.getByRole('button', { name: 'Refresh stock', exact: true }).click()
  }
  await page.evaluate((item) => {
    location.hash = `#/product/${item}`
  }, id)
  await expect(page.getByRole('region', { name: 'Product stock' })).toBeVisible()
  await expect(page.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
}
async function choose(slot: 'Customer' | 'Staff' | 'Manager', subject: string) {
  if (!tokens[subject]) tokens[subject] = demo.token(subject, '1h')
  await useSlot(page, slot, tokens[subject])
}
async function stock(id: string) {
  return ((await demo.state('inventory')).data as { items: { id: string; stock: number }[] }).items.find((item) => item.id === id)!.stock
}
async function restart(fault?: string) {
  await demo.stop()
  await demo.start(fault ? { fault } : {})
  await api.signIn('admin', password)
}

test.describe.serial('Sprocket & Spindle on the native Gateway', () => {
  test.beforeAll(async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    const secret = demo.setup()
    await demo.start()
    api = new AdminApi()
    await api.enrol('admin', secret, password)
    await api.signIn('admin', password)
    await api.genesis(await api.stage(demo.file('deny-all.json')))
    await api.activate(await api.stage(demo.file('baseline.json')), 300)
    await api.resolve('confirm')
    context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    await context.exposeBinding('shopPolicyViolation', (_source, value: string) => {
      violations.push(value)
    })
    await context.addInitScript(() => {
      document.addEventListener('securitypolicyviolation', (event) => {
        void (window as unknown as { shopPolicyViolation: (value: string) => Promise<void> }).shopPolicyViolation(
          `${event.effectiveDirective}: ${event.blockedURI}`,
        )
      })
    })
    context.on('console', (entry) => consoleLines.push(entry.text()))
    context.on('request', (request) => requests.push({ method: request.method(), url: request.url() }))
    page = await context.newPage()
    await page.goto(siteURL + '/demo/')
    record('shop-source', { gateway: gatewayIdentity() })
  })
  test.afterAll(async () => {
    await context?.close()
    await demo?.stop()
  })

  test('Customer browses, sees a real stock refusal, places one order and looks up only individual orders', async () => {
    await choose('Customer', 'cust-001')
    const before = await demo.counts()
    await shopPage(page, 'Shop')
    await expect(page.locator('.product-card')).toHaveCount(3)
    expect((await demo.counts()).inventory).toBe(before.inventory)
    await captureShop(page, 'shop-customer-shelf')
    await page
      .getByRole('link')
      .filter({ has: page.getByText('ITM-001', { exact: true }) })
      .click()
    await expect(page.getByText('Stock levels are not shared with customers.')).toBeVisible()
    await expect(page.getByText('Refused by the Gateway: 403')).toBeVisible()
    expect((await demo.counts()).inventory).toBe(before.inventory)
    await expect(orderForm().getByRole('button', { name: 'Place order', exact: true })).toBeEnabled()
    await captureShop(page, 'shop-customer-product-refused')
    await orderForm().getByLabel('Quantity', { exact: true }).fill('2')
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(201)
    await expect(orderForm()).toContainText('Order ORD-1004 placed for 2 × The Widget.')
    const receipts = (await demo.receipts('orders')).slice(before.orders)
    expect(receipts).toHaveLength(1)
    expect(receipts[0]).toMatchObject({ operation: 'orders.create', status: 201, subject: 'cust-001' })
    expect(await stock('ITM-001')).toBe(40)
    await captureShop(page, 'shop-order-placed')
    await orderForm().getByRole('link', { name: 'My orders', exact: true }).click()
    await expect(page.getByRole('table')).toContainText('ORD-1004')
    await page.getByLabel('Order number', { exact: true }).fill('ORD-1002')
    await page.getByRole('button', { name: 'Find order', exact: true }).click()
    await expect(page.getByText('No order with that number in your account.')).toBeVisible()
    await expect(page.getByRole('table')).not.toContainText('ORD-1002')
    expect(requests.filter((request) => request.method === 'GET' && request.url === siteURL + '/orders')).toEqual([])
    await captureShop(page, 'shop-customer-orders-not-found')
    record('shop-customer', { stockStatus: 403, inventoryReceipts: 0, placed: receipts[0], stockUnchanged: 40, lookupStatus: 404 })
  })

  test('Staff sees all orders, receives stock once, cannot remove too much and reads the summary', async () => {
    await choose('Staff', 'staff-01')
    await shopPage(page, 'Orders')
    await expect(page.getByRole('table')).toContainText('ORD-1004')
    await captureShop(page, 'shop-staff-orders')
    await shopPage(page, 'Stock')
    const row = stockRow()
    await expect(row.locator('td.num')).toHaveText('40')
    const since = (await demo.receipts('inventory')).length
    await row.getByLabel('Change', { exact: true }).fill('5')
    await row.getByRole('button', { name: 'Receive', exact: true }).click()
    expect(await shopStatus(row)).toBe(200)
    await expect(row.locator('td.num')).toHaveText('45')
    const receipts = (await demo.receipts('inventory')).slice(since)
    expect(receipts).toHaveLength(1)
    expect(receipts[0]).toMatchObject({ operation: 'inventory.adjust', effect: 'ITM-001 stock 40->45', subject: 'staff-01' })
    await captureShop(page, 'shop-staff-stock-received')
    await row.getByLabel('Change', { exact: true }).fill('100')
    await row.getByRole('button', { name: 'Remove', exact: true }).click()
    expect(await shopStatus(row)).toBe(409)
    await expect(row.getByText('Not enough stock to remove that many.')).toBeVisible()
    expect(await stock('ITM-001')).toBe(45)
    // The refused change must not bring back the level read before the receipt.
    await expect(row.locator('td.num')).toHaveText('45')
    await captureShop(page, 'shop-staff-stock-refused')
    await shopPage(page, 'Reports')
    await expect(page.getByText('36 orders worth $5,357.50 over 3 months.')).toBeVisible()
    await captureShop(page, 'shop-staff-reports')
    record('shop-staff', { received: receipts[0], refusedRemoval: 409, stock: 45, summary: 200 })
  })

  test('Manager reads sales and summary and has no stock adjustment controls', async () => {
    await choose('Manager', 'mgr-01')
    await shopPage(page, 'Reports')
    await expect(page.getByText('36 orders worth $5,357.50 over 3 months.')).toBeVisible()
    await expect(page.getByRole('table')).toContainText('$1,845.00')
    await expect(page.getByRole('table')).toContainText('$2,302.50')
    await expect(page.getByRole('table')).toContainText('$1,210.00')
    await expect(page.getByText('Sales by month for 3 months.', { exact: true })).toBeVisible()
    await captureShop(page, 'shop-manager-reports')
    await shopPage(page, 'Stock')
    await expect(page.getByRole('button', { name: 'Receive', exact: true })).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Remove', exact: true })).toHaveCount(0)
    await expect(page.getByRole('table')).toContainText('45')
    await captureShop(page, 'shop-manager-stock')
    record('shop-manager', { salesMonths: 3, summary: 200, adjustControls: 0 })
  })

  test('changing slots cancels a pending lookup and clears results while tokens stay in memory only', async () => {
    await choose('Customer', 'cust-001')
    await shopPage(page, 'My orders')
    let release!: () => void
    const held = new Promise<void>((done) => {
      release = done
    })
    let finished!: () => void
    const complete = new Promise<void>((done) => {
      finished = done
    })
    await page.route('**/orders/ORD-1001', async (route) => {
      await held
      try {
        await route.fulfill({ response: await route.fetch() })
      } catch {
        /* Identity cancellation closes the browser request. */
      }
      finished()
    })
    await page.getByLabel('Order number', { exact: true }).fill('ORD-1001')
    await page.getByRole('button', { name: 'Find order', exact: true }).click()
    await expect(page.getByRole('button', { name: 'Finding order…', exact: true })).toBeDisabled()
    await captureShop(page, 'shop-customer-lookup-pending')
    await page.getByLabel('Signed in as', { exact: true }).selectOption('Staff')
    release()
    await complete
    await page.unroute('**/orders/ORD-1001')
    await expect(page.getByRole('heading', { level: 1, name: 'All orders' })).toBeVisible()
    await expect(page.getByRole('form', { name: 'Find an order' })).toHaveCount(0)
    await page.getByLabel('Signed in as', { exact: true }).selectOption('Customer')
    await expect(page.getByText(/No orders in this page session yet/)).toBeVisible()
    await expect(page.getByRole('table')).toHaveCount(0)
    expect(await page.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0)
    for (const value of Object.values(tokens)) {
      expect(requests.some((request) => request.url.includes(value))).toBe(false)
      expect(consoleLines.some((line) => line.includes(value))).toBe(false)
    }
    await captureShop(page, 'shop-customer-orders-empty')
    record('shop-slot-change', { lateAnswerDiscarded: true, storageEntries: 0 })
  })

  test('live permits, rollback, temporary exception, surviving rate counters and containment change real shop outcomes', async () => {
    await choose('Customer', 'cust-001')
    await product()
    await expect(page.getByText('Refused by the Gateway: 403')).toBeVisible()
    const document = JSON.parse(demo.file('baseline.json'))
    document.permits.push({ role: 'CUSTOMER', operation: 'inventory.get' })
    const before = (await demo.receipts('inventory')).length
    const revision = await api.activate(await api.stage(JSON.stringify(document)), 300)
    await product()
    await expect(page.getByText('In stock: 45', { exact: true })).toBeVisible()
    expect((await demo.receipts('inventory')).slice(before)).toHaveLength(1)
    await captureShop(page, 'shop-live-provisional-stock')
    await api.resolve('rollback')
    await product()
    await expect(page.getByText('Refused by the Gateway: 403')).toBeVisible()
    expect((await demo.receipts('inventory')).slice(before)).toHaveLength(1)
    await captureShop(page, 'shop-live-rollback-stock')
    const exception = await api.createOverride({
      type: 'exception',
      control: 'permit',
      role: 'CUSTOMER',
      operation: 'inventory.get',
      expires_in_seconds: 120,
    })
    await product()
    await expect(page.getByText('In stock: 45', { exact: true })).toBeVisible()
    await captureShop(page, 'shop-live-exception-stock')
    await api.revokeOverride(exception)
    await product()
    await expect(page.getByText('Refused by the Gateway: 403')).toBeVisible()
    expect((await demo.receipts('inventory')).slice(before)).toHaveLength(2)

    await restart()
    await choose('Staff', 'staff-01')
    const reportsBefore = (await demo.receipts('reports')).length
    await shopPage(page, 'Reports')
    await expect(page.getByText('36 orders worth $5,357.50 over 3 months.')).toBeVisible()
    const limited = JSON.parse(demo.file('baseline.json'))
    limited.operations.find((operation: { id: string }) => operation.id === 'reports.summary').rate.count = 2
    await api.activate(await api.stage(JSON.stringify(limited)), 300)
    await api.resolve('confirm')
    for (const status of [200, 429]) {
      const response = page.waitForResponse((reply) => reply.url() === siteURL + '/reports/summary')
      await page.getByRole('button', { name: 'Refresh reports', exact: true }).click()
      expect((await response).status()).toBe(status)
      await expect(page.getByRole('region', { name: 'Sales summary' })).toContainText(`HTTP ${status}`)
    }
    await expect(page.getByText('Too many requests in a short time. Try again shortly.')).toBeVisible()
    expect((await demo.receipts('reports')).slice(reportsBefore)).toHaveLength(2)
    await captureShop(page, 'shop-live-rate-refusal')
    await api.activate(await api.stage(demo.file('baseline.json')), 300)
    await api.resolve('confirm')

    const containment = await api.createOverride({
      type: 'containment',
      control: 'operation',
      operation: 'orders.create',
      expires_in_seconds: 120,
    })
    await choose('Customer', 'cust-001')
    await product()
    const ordersBefore = (await demo.receipts('orders')).length
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(403)
    await expect(orderForm().getByText("The Gateway did not allow this; nothing was sent to the shop's systems.")).toBeVisible()
    expect((await demo.receipts('orders')).slice(ordersBefore)).toHaveLength(0)
    await captureShop(page, 'shop-live-order-contained')
    await api.revokeOverride(containment)
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(201)
    expect((await demo.receipts('orders')).slice(ordersBefore)).toHaveLength(1)
    await captureShop(page, 'shop-live-order-restored')
    record('shop-live-policy', {
      provisional: revision,
      stockOutcomes: [403, 200, 403, 200, 403],
      exception,
      survivingRateCounter: [200, 200, 429],
      containment,
      orderOutcomes: [403, 201],
    })
  })

  test('real API and Gateway refusals use the specified shop wording', async () => {
    await choose('Customer', 'cust-001')
    await product()
    const cases: { status: number; text: string; quantity?: string; body?: Record<string, unknown>; headers?: Record<string, string> }[] = [
      { status: 422, text: 'Quantity must be 1 to 50.', quantity: '0' },
      {
        status: 403,
        text: 'You can only place orders for your own account.',
        body: { customer: 'cust-002', item: 'ITM-001', quantity: 1 },
      },
      { status: 422, text: 'That product is not sold here.', body: { customer: 'cust-001', item: 'ITM-999', quantity: 1 } },
      { status: 400, text: "The Gateway refused the request as invalid. Nothing was sent to the shop's systems.", quantity: '1.5' },
      {
        status: 415,
        text: "The Gateway refused the request as invalid. Nothing was sent to the shop's systems.",
        headers: { 'content-type': 'text/plain' },
      },
    ]
    const observations: { status: number; receipts: number }[] = []
    for (const [index, row] of cases.entries()) {
      await orderForm()
        .getByLabel('Quantity', { exact: true })
        .fill(row.quantity ?? '1')
      const before = (await demo.receipts('orders')).length
      if (row.body || row.headers)
        await page.route('**/orders', async (route) => {
          expect(route.request().method()).toBe('POST')
          expect(route.request().postDataJSON()).toEqual({ customer: 'cust-001', item: 'ITM-001', quantity: 1 })
          // Alter the request at the real boundary to reach API errors that a
          // correctly formed storefront request cannot naturally produce.
          await route.continue({
            headers: { ...route.request().headers(), ...row.headers },
            ...(row.body ? { postData: JSON.stringify(row.body) } : {}),
          })
        })
      await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
      expect(await shopStatus(orderForm())).toBe(row.status)
      await expect(orderForm().getByText(row.text, { exact: true })).toBeVisible()
      const receipts = (await demo.receipts('orders')).slice(before)
      expect(receipts).toHaveLength(index < 3 ? 1 : 0)
      if (receipts.length) expect(receipts[0]).toMatchObject({ operation: 'orders.create', status: row.status, subject: 'cust-001' })
      observations.push({ status: row.status, receipts: receipts.length })
      await captureShop(page, `shop-outcome-${index}-${row.status}`)
      await page.unroute('**/orders')
    }
    await page.route('**/orders', (route) =>
      route.continue({ postData: JSON.stringify({ customer: 'cust-001', item: 'ITM-001', quantity: 1, padding: 'x'.repeat(1100) }) }),
    )
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(413)
    await expect(orderForm()).toContainText("The Gateway refused the request as invalid. Nothing was sent to the shop's systems.")
    await captureShop(page, 'shop-outcome-413')
    await page.unroute('**/orders')
    await useSlot(page, 'Customer', '')
    await product()
    expect(await shopStatus(page.getByRole('region', { name: 'Product stock' }))).toBe(401)
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(401)
    await expect(orderForm().getByText('Please sign in: your sign-in is missing or has expired.')).toBeVisible()
    await captureShop(page, 'shop-outcome-401')
    await choose('Staff', 'staff-01')
    await shopPage(page, 'Stock')
    await stockRow().getByLabel('Change', { exact: true }).fill('0')
    await stockRow().getByRole('button', { name: 'Receive', exact: true }).click()
    expect(await shopStatus(stockRow())).toBe(422)
    await expect(stockRow().getByText('Change must be 1 to 100 and not zero.')).toBeVisible()
    await captureShop(page, 'shop-outcome-invalid-delta')
    await page.evaluate(() => {
      location.hash = '#/product/ITM-999'
    })
    await expect(page.getByText('That product is not in the inventory.')).toBeVisible()
    expect(await shopStatus(page.locator('main'))).toBe(404)
    await captureShop(page, 'shop-outcome-item-not-found')
    await product()
    for (const [method, path, status] of [
      ['GET', '/no-such-operation', 404],
      ['POST', '/inventory/items/ITM-001', 405],
    ] as const) {
      await page.route('**/inventory/items/ITM-001', (route) => route.continue({ method, url: siteURL + path }))
      await page.getByRole('button', { name: 'Refresh stock', exact: true }).click()
      await expect(page.getByText('The Gateway does not know this request.')).toBeVisible()
      expect(await shopStatus(page.getByRole('region', { name: 'Product stock' }))).toBe(status)
      await captureShop(page, `shop-outcome-route-${status}`)
      await page.unroute('**/inventory/items/ITM-001')
    }
    record('shop-refusals', {
      observations,
      additional: [413, 401, 422, 404, 405],
      transport: 'actual native responses, no fabricated response',
    })
  })

  test('a lost order answer and real 502 or 503 never claim a write result or resend', async () => {
    await choose('Customer', 'cust-001')
    await product()
    let release!: () => void
    const held = new Promise<void>((done) => (release = done))
    const before = (await demo.receipts('orders')).length
    const sends = requests.filter((request) => request.method === 'POST' && request.url === siteURL + '/orders').length
    await page.route('**/orders', async (route) => {
      await route.fetch()
      await held
      await route.abort('connectionreset')
    })
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    await expect(orderForm().getByRole('button', { name: 'Placing order…', exact: true })).toBeDisabled()
    await captureShop(page, 'shop-order-pending')
    release()
    await expect(orderForm().getByText('No answer. Your order may or may not have been applied: check before trying again.')).toBeVisible()
    expect(await shopStatus(orderForm())).toBe(0)
    await page.waitForTimeout(1500)
    expect(requests.filter((request) => request.method === 'POST' && request.url === siteURL + '/orders')).toHaveLength(sends + 1)
    expect((await demo.receipts('orders')).slice(before)).toHaveLength(1)
    await captureShop(page, 'shop-order-no-answer')
    await page.unroute('**/orders')
    process.kill(demo.pids.orders)
    await expect.poll(() => demo.log()).toContain('fixture orders stopped')
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(502)
    await expect(
      orderForm().getByText(
        'The shop could not confirm this. Your order may or may not have been applied: check My orders before trying again.',
      ),
    ).toBeVisible()
    await captureShop(page, 'shop-order-502')
    await restart('audit-partial')
    // This fault triggers on admission. A Customer stock request is refused
    // at RBAC first, so use an admitted Staff read to trigger the fault.
    await choose('Staff', 'staff-01')
    await product()
    expect(await shopStatus(page.getByRole('region', { name: 'Product stock' }))).toBe(503)
    await expect(page.getByText('The shop could not complete this right now. Try again later.')).toBeVisible()
    await choose('Customer', 'cust-001')
    await product()
    expect(await shopStatus(page.getByRole('region', { name: 'Product stock' }))).toBe(403)
    const auditBefore = (await demo.receipts('orders')).length
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(503)
    await expect(
      orderForm().getByText(
        'The shop could not confirm this. Your order may or may not have been applied: check My orders before trying again.',
      ),
    ).toBeVisible()
    expect((await demo.receipts('orders')).slice(auditBefore)).toHaveLength(0)
    await captureShop(page, 'shop-order-503')
    await restart()
    record('shop-uncertain', {
      noAnswer: { sends: 1, receipts: 1 },
      write502: true,
      write503: true,
      status504: 'NOT RUN: no existing native control produces a 504 response; same production branch as 502',
    })
  })

  test('real long subjects, stock zero, quantity fifty and many orders remain usable', async () => {
    const longSubject = 'customer-' + 'x'.repeat(119)
    const document = JSON.parse(demo.file('baseline.json'))
    document.subjects.push({ id: longSubject, roles: ['CUSTOMER'], status: 'active' })
    document.subjects.push({ id: 'shop-unknown', roles: ['CUSTOMER'], status: 'active' })
    await api.activate(await api.stage(JSON.stringify(document)), 300)
    await api.resolve('confirm')
    await choose('Customer', longSubject)
    await expect(page.getByLabel('Signed in as', { exact: true }).locator('option:checked')).toHaveText(
      `Customer slot: signed in as ${longSubject}`,
    )
    await product()
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    // The policy allows a 128-character subject. The existing order body
    // schema permits only 64 characters in customer, so this is a real 400.
    expect(await shopStatus(orderForm())).toBe(400)
    await expect(orderForm()).toContainText("The Gateway refused the request as invalid. Nothing was sent to the shop's systems.")
    await captureShop(page, 'shop-long-subject')
    await page.getByRole('banner').getByRole('button', { name: 'Manage sign-ins', exact: true }).click()
    await expect(page.getByRole('dialog')).toContainText(longSubject)
    await captureShop(page, 'shop-long-subject-dialog')
    await page.keyboard.press('Escape')
    await choose('Customer', 'shop-unknown')
    await product()
    await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
    expect(await shopStatus(orderForm())).toBe(422)
    await expect(orderForm().getByText('Your account is not a shop customer.')).toBeVisible()
    await captureShop(page, 'shop-unknown-customer')
    await api.activate(await api.stage(demo.file('baseline.json')), 300)
    await api.resolve('confirm')
    await choose('Staff', 'staff-01')
    await shopPage(page, 'Stock')
    const current = await stock('ITM-001')
    await stockRow().getByLabel('Change', { exact: true }).fill(String(current))
    await stockRow().getByRole('button', { name: 'Remove', exact: true }).click()
    expect(await shopStatus(stockRow())).toBe(200)
    expect(await stock('ITM-001')).toBe(0)
    await product()
    await expect(page.getByText('Out of stock (back-orders accepted)')).toBeVisible()
    await captureShop(page, 'shop-stock-zero')
    await choose('Customer', 'cust-001')
    await product()
    await orderForm().getByLabel('Quantity', { exact: true }).fill('50')
    const before = (await demo.receipts('orders')).length
    for (let index = 0; index < 20; index++) {
      await orderForm().getByRole('button', { name: 'Place order', exact: true }).click()
      expect(await shopStatus(orderForm())).toBe(201)
      await expect(orderForm()).toContainText('placed for 50 × The Widget.')
    }
    expect((await demo.receipts('orders')).slice(before)).toHaveLength(20)
    expect(await stock('ITM-001')).toBe(0)
    await captureShop(page, 'shop-quantity-fifty')
    await shopPage(page, 'My orders')
    await expect(page.getByRole('table').locator('tbody tr')).toHaveCount(20)
    await captureShop(page, 'shop-many-customer-orders')
    await choose('Staff', 'staff-01')
    await shopPage(page, 'Orders')
    await expect(page.getByRole('table').locator('tbody tr')).toHaveCount(23)
    await captureShop(page, 'shop-many-all-orders')
    await restart()
    record('shop-stress', {
      subjectLength: longSubject.length,
      unknownCustomerStatus: 422,
      stock: 0,
      quantity: 50,
      createdOrders: 20,
      listedOrders: 23,
    })
  })
  test('every shop surface fits all widths, passes axe, resizes text and supports keyboard ordering, stock adjustment and sign-ins', async () => {
    const scans: string[] = []
    for (const [slot, subject, names] of [
      ['Customer', 'cust-001', ['Shop', 'My orders']],
      ['Staff', 'staff-01', ['Shop', 'Orders', 'Stock', 'Reports']],
      ['Manager', 'mgr-01', ['Shop', 'Orders', 'Stock', 'Reports']],
    ] as const) {
      await choose(slot, subject)
      for (const name of names) {
        await shopPage(page, name)
        for (const width of [1440, 768, 375]) {
          await page.setViewportSize({ width, height: 900 })
          const result = await new AxeBuilder({ page })
            .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22a', 'wcag22aa'])
            .analyze()
          expect(
            result.violations.map((entry) => ({ id: entry.id, targets: entry.nodes.map((node) => node.target) })),
            `${slot} ${name} ${width}`,
          ).toEqual([])
          scans.push(`${slot} ${name} ${width}`)
          for (const link of await page.getByRole('navigation', { name: 'Client pages' }).getByRole('link').all()) {
            const box = (await link.boundingBox())!
            expect(box.x >= 0 && box.x + box.width <= width, `${slot} ${await link.textContent()} link on screen at ${width}`).toBe(true)
          }
        }
        await captureShop(page, `shop-axe-${slot.toLowerCase()}-${name.toLowerCase().replace(/ /g, '-')}`)
      }
      await product()
      for (const width of [1440, 768, 375]) {
        await page.setViewportSize({ width, height: 900 })
        expect(
          (await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22a', 'wcag22aa']).analyze())
            .violations,
        ).toEqual([])
        scans.push(`${slot} Product ${width}`)
      }
      await captureShop(page, `shop-axe-${slot.toLowerCase()}-product`)
    }
    await page.getByLabel('Signed in as', { exact: true }).selectOption('')
    await shopPage(page, 'Shop')
    await captureShop(page, 'shop-signed-out-shelf')
    await product()
    await captureShop(page, 'shop-signed-out-product')
    await expect(page.getByText('Please sign in: your sign-in is missing or has expired.')).toBeVisible()
    for (const hash of ['stock', 'reports', 'orders']) {
      await page.evaluate((route) => {
        location.hash = '#/' + route
      }, hash)
      await expect(page.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
      await captureShop(page, `shop-signed-out-${hash}`)
    }
    await choose('Customer', 'cust-001')
    await product()
    await orderForm().getByLabel('Quantity', { exact: true }).focus()
    await page.keyboard.press('ControlOrMeta+A')
    await page.keyboard.insertText('2')
    await page.keyboard.press('Tab')
    await expect(orderForm().getByRole('button', { name: 'Place order', exact: true })).toBeFocused()
    await page.keyboard.press('Enter')
    expect(await shopStatus(orderForm())).toBe(201)
    await captureShop(page, 'shop-keyboard-order')
    await page.getByRole('banner').getByRole('button', { name: 'Manage sign-ins', exact: true }).focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('dialog', { name: 'Manage sign-ins' })).toBeVisible()
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([])
    }
    await captureShop(page, 'shop-manage-sign-ins')
    await page.keyboard.press('Escape')
    await expect(page.getByRole('banner').getByRole('button', { name: 'Manage sign-ins', exact: true })).toBeFocused()
    await page.getByLabel('Signed in as', { exact: true }).focus()
    await page.keyboard.press('ArrowDown')
    await expect(page.getByLabel('Signed in as', { exact: true })).toHaveValue('Staff')
    await page.getByRole('navigation', { name: 'Client pages' }).getByRole('link', { name: 'Stock', exact: true }).focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('heading', { level: 1, name: 'Stock' })).toBeFocused()
    await expect(page.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
    await stockRow().getByLabel('Change', { exact: true }).focus()
    await page.keyboard.press('ControlOrMeta+A')
    await page.keyboard.insertText('1')
    await stockRow().getByRole('button', { name: 'Receive', exact: true }).focus()
    await page.keyboard.press('Enter')
    expect(await shopStatus(stockRow())).toBe(200)
    await captureShop(page, 'shop-keyboard-stock')
    await choose('Customer', 'cust-001')
    await product()
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      const button = orderForm().getByRole('button', { name: 'Place order', exact: true })
      await button.hover()
      await shot(page, `shop-place-hover-${width}`)
      await button.focus()
      await shot(page, `shop-place-focus-${width}`)
      const point = await button.boundingBox()
      await page.mouse.move(point!.x + point!.width / 2, point!.y + point!.height / 2)
      await page.mouse.down()
      await shot(page, `shop-place-pressed-${width}`)
      await page.mouse.up()
      expect(await shopStatus(orderForm())).toBe(201)
    }
    await page.emulateMedia({ reducedMotion: 'reduce' })
    expect(
      await orderForm()
        .getByRole('button', { name: 'Place order', exact: true })
        .evaluate((element) => getComputedStyle(element).transitionDuration),
    ).toBe('0s')
    expect(
      await orderForm()
        .locator('.shop-result')
        .evaluate((element) => getComputedStyle(element).animationName),
    ).toBe('none')
    await captureShop(page, 'shop-reduced-motion')
    await page.emulateMedia({ reducedMotion: 'no-preference' })
    await page.evaluate(() => {
      document.documentElement.style.fontSize = '200%'
    })
    await captureShop(page, 'shop-text-200-product')
    await shopPage(page, 'Shop')
    await captureShop(page, 'shop-text-200-shelf')
    await page.evaluate(() => {
      document.documentElement.style.fontSize = ''
    })
    record('shop-accessibility', { scans, violations: 0, keyboardOrder: 201, keyboardStock: 200, textResize: '200%' })
  })
  test('shop requests and assets disclose no credential and violate no content policy', async () => {
    const secrets = [password, ...Object.values(tokens), demo.file('fixture.secret').trim(), demo.file('issuer-key.pem').split('\n')[1]]
    const walk = (directory: string): string[] =>
      readdirSync(directory).flatMap((name) =>
        statSync(join(directory, name)).isDirectory() ? walk(join(directory, name)) : [join(directory, name)],
      )
    const leaks: string[] = []
    for (const file of walk(resolve('dist'))) {
      const text = readFileSync(file, 'utf8')
      if (secrets.some((secret) => secret && text.includes(secret)) || text.includes('PRIVATE KEY')) leaks.push(file)
      expect(text, file).not.toMatch(/5173|5174|localhost/)
    }
    expect(leaks).toEqual([])
    expect(await page.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0)
    expect(await context.cookies()).toEqual([])
    expect(requests.filter((request) => !request.url.startsWith(siteURL + '/'))).toEqual([])
    expect(requests.filter((request) => new URL(request.url).pathname.startsWith('/admin'))).toEqual([])
    expect(
      secrets.some(
        (secret) => secret && (consoleLines.join('\n').includes(secret) || requests.some((request) => request.url.includes(secret))),
      ),
    ).toBe(false)
    expect(violations).toEqual([])
    record('shop-secrets-and-policy', {
      credentialLeaks: 0,
      storage: 0,
      cookies: 0,
      externalRequests: 0,
      adminRequests: 0,
      contentPolicyViolations: 0,
      developmentAddressMatches: 0,
    })
  })
})
