import { expect, test, type Browser, type BrowserContext, type Page } from '@playwright/test'
import { AdminApi, build, Demo, raw, siteURL, webRoot, type Receipt } from './env'
import { accessible, addToBasket, demoPassword, orderCard, placeOrder, record, shot, signIn, storage } from './pages'

// Every Fetch role on the real gateway, services and Fetch Accounts, with the
// Fetch baseline policy confirmed. Each step is checked against the services'
// own receipts or the Admin API as well as what the page shows.

let demo: Demo
const api = new AdminApi()
const adminPassword = 'controlled client flows password 42'
const opened: BrowserContext[] = []
// Each person signs in once and keeps their tab, as in the demonstration:
// the baseline allows ten sign-ins a minute from one address, and every
// person here signs in from this machine.
const people = new Map<string, Page>()
const violations: string[] = []
const pageErrors: string[] = []
let dialogs = 0

// person returns this person's tab, signed in, opening a new browser context
// for someone not seen before (or for nobody, when email is null).
async function person(browser: Browser, email: string | null): Promise<Page> {
  const known = email ? people.get(email) : undefined
  if (known) {
    await known.goto(siteURL + '/', { waitUntil: 'networkidle' })
    return known
  }
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  if (!email) opened.push(context)
  await context.exposeBinding('reportViolation', (_source, text: string) => {
    violations.push(text)
  })
  await context.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (event) => {
      void (window as unknown as { reportViolation: (text: string) => Promise<void> }).reportViolation(`${event.effectiveDirective} ${event.blockedURI}`)
    })
  })
  const page = await context.newPage()
  page.on('pageerror', (error) => pageErrors.push(error.message))
  page.on('dialog', (dialog) => {
    dialogs++
    void dialog.dismiss()
  })
  if (email) {
    await signIn(page, siteURL, email)
    people.set(email, page)
  }
  return page
}

// leave closes a person's tab; they sign in again if they come back.
async function leave(email: string) {
  await people.get(email)?.context().close()
  people.delete(email)
}

// since returns the receipts a service recorded after the given count.
async function since(service: string, count: number): Promise<Receipt[]> {
  return (await demo.receipts(service)).slice(count)
}

// ref reads the full reference a Ref shows in its tooltip (the page shows a
// short form, and Copy full reference copies this value).
const ref = async (page: Page) => ((await page.getByTitle(/^Full reference [0-9a-f]{32}$/).first().getAttribute('title')) ?? '').replace(/^Full reference /, '')

test.describe.serial('Fetch roles on the real services', () => {
  test.beforeAll(async () => {
    demo = new Demo(build(), webRoot())
    const secret = demo.setup()
    await demo.start()
    await api.enrol('admin', secret, adminPassword)
    await api.signIn('admin', adminPassword)
    await api.baseline(demo)
  })

  // Between tests every kept tab rests on a blank page, so no open board (a
  // merchant's or rider's rereads every few seconds) adds requests to the
  // next test; the tab keeps its session.
  test.afterEach(async () => {
    for (const context of opened.splice(0)) await context.close()
    for (const page of people.values()) await page.goto('about:blank')
  })

  test.afterAll(async () => {
    for (const email of [...people.keys()]) await leave(email)
    await demo?.stop()
  })

  test('a customer browses, sorts, filters and searches, and checks out with cash and with the wallet; each order reaches Orders once', async ({ browser }) => {
    const priya = await person(browser, 'priya@fetch.test')
    await expect(priya.getByRole('heading', { level: 1, name: 'What can we fetch for you?' })).toBeVisible()
    await expect(priya.getByRole('link', { name: /Warung Kak Ros/ })).toBeVisible()
    await priya.goto(siteURL + '/?q=nasi', { waitUntil: 'networkidle' })
    await expect(priya.getByText('Nasi Lemak Ayam Goreng').first()).toBeVisible()
    // Results sort by price, and the choice stays in the address.
    const prices = async () => (await priya.locator('a[href^="/item/"]').allTextContents()).map((text) => Number(/RM\s*([\d.]+)/.exec(text)?.[1]))
    await priya.getByLabel('Sort by').selectOption({ label: 'Price, high to low' })
    await expect(priya).toHaveURL(/\/\?q=nasi&sort=price-desc$/)
    const highToLow = await prices()
    expect(highToLow.length).toBeGreaterThan(1)
    expect(highToLow).toEqual([...highToLow].sort((a, b) => b - a))
    // The results name each shop they come from, and each opens the shop.
    const shops = priya.getByRole('navigation', { name: 'Shops in these results' })
    await expect(shops.getByRole('link')).toHaveCount(1)
    await shops.getByRole('link', { name: 'Warung Kak Ros' }).click()
    await expect(priya).toHaveURL(/\/store\/MER-001$/)

    // Fetch Food sorts by delivery fee and can show only open shops; both
    // choices survive a reload.
    await priya.goto(siteURL + '/food', { waitUntil: 'networkidle' })
    await priya.getByLabel('Sort by').selectOption({ label: 'Lowest delivery fee' })
    await expect(priya).toHaveURL(/\/food\?sort=fee$/)
    const fees = (await priya.locator('a[href^="/store/"]').filter({ hasText: 'Delivery fee' }).allTextContents()).map((text) => Number(/Delivery fee\s*RM\s*([\d.]+)/.exec(text)?.[1]))
    expect(fees.length).toBeGreaterThan(1)
    expect(fees).toEqual([...fees].sort((a, b) => a - b))
    await priya.getByLabel('Open now').check()
    await expect(priya).toHaveURL(/\/food\?sort=fee&open=1$/)
    await expect(priya.getByRole('status').filter({ hasText: /open now/ })).toBeVisible()
    await expect(priya.getByText('Closed now')).toHaveCount(0)
    await priya.reload({ waitUntil: 'networkidle' })
    await expect(priya.getByLabel('Sort by')).toHaveValue('fee')
    await expect(priya.getByLabel('Open now')).toBeChecked()
    await shot(priya, 'flows-00-food-sorted')

    const before = (await demo.receipts('orders')).length
    await addToBasket(priya, siteURL, 'MER-001', 'Nasi Lemak Ayam Goreng')
    const cash = await placeOrder(priya, siteURL, 'Cash on delivery')
    await expect(priya.getByRole('heading', { level: 1 })).toHaveText('Placed')
    const created = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === '/orders')
    expect(created).toHaveLength(1)
    expect(created[0]).toMatchObject({ status: 201, subject: 'cust-003' })

    // Priya's first Fetch Mart order has free delivery, from Orders' own rule.
    await addToBasket(priya, siteURL, 'MER-005', 'Full Cream Milk 1 L')
    await priya.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await expect(priya.getByText('Free because this is your first Fetch Mart order.')).toBeVisible()
    await expect(priya.getByText('Balance RM 100.00. Charged when you place the order.')).toBeVisible()
    await shot(priya, 'flows-01-checkout')
    const wallet = await placeOrder(priya, siteURL, 'Fetch wallet')
    await priya.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    for (const id of [cash, wallet]) await expect(priya.getByRole('link', { name: new RegExp(id) })).toBeVisible()
    await priya.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await expect(priya.getByText('Nothing to check out yet')).toBeVisible()
    record('customer-checkout', { cash, wallet, receipt: created[0] })
  })

  test('the merchant, a rider and the customer move an order through every status, each seeing it change', async ({ browser }) => {
    const aisyah = await person(browser, 'aisyah@fetch.test')
    await addToBasket(aisyah, siteURL, 'MER-001', 'Teh Tarik')
    const order = await placeOrder(aisyah, siteURL, 'Cash on delivery')

    const ros = await person(browser, 'ros@fetch.test')
    await ros.goto(siteURL + '/merchant', { waitUntil: 'networkidle' })
    await orderCard(ros, order).getByRole('button', { name: 'Accept order' }).click()
    await expect(orderCard(ros, order).getByRole('button', { name: 'Mark ready for pickup' })).toBeVisible()
    // Her open order page follows the change by itself and says so; a status
    // read when the page opens is not announced.
    await expect(aisyah.getByRole('status').filter({ hasText: /^Update: Being prepared at .+\.$/ })).toBeVisible({ timeout: 15_000 })
    await expect(aisyah.getByRole('heading', { level: 1 })).toHaveText('Being prepared')
    await aisyah.reload({ waitUntil: 'networkidle' })
    await expect(aisyah.getByRole('heading', { level: 1 })).toHaveText('Being prepared')
    await expect(aisyah.getByText(/^Update:/)).toHaveCount(0)
    await orderCard(ros, order).getByRole('button', { name: 'Mark ready for pickup' }).click()
    await expect(orderCard(ros, order).getByRole('button')).toHaveCount(0)

    const amir = await person(browser, 'amir@fetch.test')
    await amir.goto(siteURL + '/rider', { waitUntil: 'networkidle' })
    await expect(orderCard(amir, order)).toContainText(/Collect RM [\d.]+ in cash/)
    await orderCard(amir, order).getByRole('button', { name: 'Take this job' }).click()
    await aisyah.reload({ waitUntil: 'networkidle' })
    await expect(aisyah.getByRole('heading', { level: 1 })).toHaveText('On the way')
    await orderCard(amir, order).getByRole('button', { name: 'Mark delivered' }).click()
    await expect(orderCard(amir, order)).toHaveCount(0)
    await aisyah.reload({ waitUntil: 'networkidle' })
    await expect(aisyah.getByRole('heading', { level: 1 })).toHaveText('Delivered')

    // The receipt prints without the site's header, footer or buttons.
    const header = aisyah.locator('header').first()
    const print = aisyah.locator('button', { hasText: 'Print receipt' })
    await expect(header).toBeVisible()
    await expect(print).toBeVisible()
    await aisyah.emulateMedia({ media: 'print' })
    await expect(aisyah.getByText(`Receipt · ${order} · placed `)).toBeVisible()
    await expect(header).toBeHidden()
    await expect(aisyah.locator('footer').first()).toBeHidden()
    await expect(print).toBeHidden()
    await expect(aisyah.getByText(/^Order placed\./)).toBeHidden()
    await shot(aisyah, 'flows-02-receipt-print')
    await aisyah.emulateMedia({ media: null })

    // Orders recorded exactly one change per step.
    const statuses = (await demo.receipts('orders'))
      .filter((r) => r.method === 'POST' && r.target.endsWith(`/${order}`))
      .map((r) => `${r.method} ${r.target.split('/')[2]} ${r.status}`)
    expect(statuses).toEqual(['POST accept 200', 'POST ready 200', 'POST pickup 200', 'POST deliver 200'])
    record('order-statuses', { order, statuses })
  })

  test('a merchant changes a price, availability and stock, and customers see it at once', async ({ browser }) => {
    const hafiz = await person(browser, 'mart@fetch.test')
    await hafiz.goto(siteURL + '/merchant?view=menu', { waitUntil: 'networkidle' })
    const before = (await demo.receipts('inventory')).length
    const bread = hafiz.getByRole('listitem').filter({ hasText: 'Wholemeal Bread' })
    await bread.getByLabel('Price in ringgit').fill('4.80')
    await bread.getByRole('button', { name: 'Save' }).click()
    await expect(bread.getByText('New price saved.')).toBeVisible()
    await bread.getByLabel('Change stock by').fill('-5')
    await bread.getByRole('button', { name: 'Adjust' }).click()
    await expect(bread.getByText('Stock updated.')).toBeVisible()
    // The switch is operated through its visible label, as a person would.
    await expect(bread.getByRole('switch')).toBeChecked()
    await bread.getByText('Available', { exact: true }).click()
    await expect(bread.getByText('Marked unavailable.')).toBeVisible()
    await expect(bread.getByRole('switch')).not.toBeChecked()
    const changes = (await since('inventory', before)).filter((r) => r.method === 'PATCH').map((r) => `${r.target} ${r.status} ${r.subject}`)
    expect(changes).toEqual([
      '/inventory/items/ITM-505 200 merchant-005',
      '/inventory/stock/ITM-505 200 merchant-005',
      '/inventory/items/ITM-505 200 merchant-005',
    ])

    const daniel = await person(browser, 'daniel@fetch.test')
    await daniel.goto(siteURL + '/item/ITM-505', { waitUntil: 'networkidle' })
    await expect(daniel.getByText('RM 4.80').first()).toBeVisible()
    await expect(daniel.getByRole('button', { name: /Add to basket/ })).toHaveCount(0)
    await bread.getByText('Unavailable', { exact: true }).click()
    await expect(bread.getByText('Now available to order.')).toBeVisible()
    record('merchant-menu', { item: 'ITM-505' })
  })

  test('a cancel after the shop accepted is refused as already moved on, with Orders named and the Ref; a placed order cancels', async ({ browser }) => {
    const daniel = await person(browser, 'daniel@fetch.test')
    await addToBasket(daniel, siteURL, 'MER-002', 'Roti Canai')
    const accepted = await placeOrder(daniel, siteURL, 'Cash on delivery')
    const bala = await person(browser, 'bintang@fetch.test')
    await bala.goto(siteURL + '/merchant', { waitUntil: 'networkidle' })
    await orderCard(bala, accepted).getByRole('button', { name: 'Accept order' }).click()
    await expect(orderCard(bala, accepted).getByRole('button', { name: 'Mark ready for pickup' })).toBeVisible()

    // Daniel's page still shows the order as placed; his cancel reaches Orders,
    // which refuses it.
    await daniel.getByRole('button', { name: 'Cancel order' }).click()
    await daniel.getByRole('dialog').getByRole('button', { name: 'Cancel order' }).click()
    const refusal = daniel.getByRole('alert').filter({ hasText: 'This has already moved on.' })
    await expect(refusal).toContainText('Answered by Orders · 409')
    const shown = await ref(daniel)
    expect((await demo.receipts('orders')).find((r) => r.request_id === shown)).toMatchObject({ status: 409 })
    await shot(daniel, 'flows-02-cancel-refused')

    await addToBasket(daniel, siteURL, 'MER-002', 'Milo Ais')
    const placed = await placeOrder(daniel, siteURL, 'Cash on delivery')
    await daniel.getByRole('button', { name: 'Cancel order' }).click()
    await daniel.getByRole('dialog').getByRole('button', { name: 'Cancel order' }).click()
    await expect(daniel.getByRole('heading', { level: 1 })).toHaveText('Order cancelled')
    record('cancel', { accepted, refusedRef: shown, cancelled: placed })
  })

  test('support answers and resolves a help request; markup in a request is shown as text and never runs', async ({ browser }) => {
    const aisyah = await person(browser, 'aisyah@fetch.test')
    await aisyah.goto(siteURL + '/help', { waitUntil: 'networkidle' })
    const markup = '<img src=x onerror=alert(1)> missing'
    await aisyah.getByLabel('What is it about?').fill(markup)
    await aisyah.getByLabel('What happened?').fill('The sambal was missing.')
    // ORD-1001 is a cash order that was never delivered: nothing was paid.
    await aisyah.getByRole('combobox', { name: /^Order\b/ }).selectOption('ORD-1001')
    await aisyah.getByRole('button', { name: 'Send to Fetch support' }).click()
    await aisyah.waitForURL(/\/help\/TKT-\d+$/)
    const ticket = new URL(aisyah.url()).pathname.split('/').pop()!

    const nurul = await person(browser, 'nurul@fetch.test')
    await nurul.goto(`${siteURL}/support?ticket=${ticket}`, { waitUntil: 'networkidle' })
    const panel = nurul.getByRole('region', { name: 'Ticket', exact: true })
    await expect(panel.getByRole('heading', { name: markup })).toBeVisible()
    expect(await nurul.locator('img[src="x"]').count()).toBe(0)
    const linked = panel.getByRole('heading', { name: 'Linked order' }).locator('..')
    await expect(linked).toContainText('ORD-1001')
    await expect(linked).toContainText('cash on delivery')
    await expect(linked).not.toContainText('Paid')
    await panel.getByLabel('Reply to the customer').fill('Sorry about that; we have refunded it.')
    await panel.getByRole('button', { name: 'Send reply' }).click()
    await expect(panel.getByRole('list', { name: 'Conversation' })).toContainText('we have refunded it')
    await expect(panel.getByRole('list', { name: 'Conversation' }).getByText('You · Fetch support', { exact: true })).toHaveCount(1)
    // Another agent sees the reply as Nurul's, never as their own.
    const farah = await person(browser, 'farah@fetch.test')
    await farah.goto(`${siteURL}/support?ticket=${ticket}`, { waitUntil: 'networkidle' })
    const seenByFarah = farah.getByRole('region', { name: 'Ticket', exact: true }).getByRole('list', { name: 'Conversation' })
    await expect(seenByFarah.getByText('support-01 · Fetch support', { exact: true })).toBeVisible()
    await expect(seenByFarah.getByText(/^You\b/)).toHaveCount(0)
    await panel.getByRole('button', { name: 'Mark resolved' }).click()
    await expect(panel).toContainText('This request is resolved.')
    await aisyah.reload({ waitUntil: 'networkidle' })
    const seenByAisyah = aisyah.getByRole('list', { name: 'Conversation' })
    await expect(seenByAisyah).toContainText('we have refunded it')
    await expect(seenByAisyah.getByText('You', { exact: true })).toBeVisible()
    await expect(seenByAisyah.getByText('Fetch support', { exact: true })).toBeVisible()
    expect(dialogs).toBe(0)
    record('support', { ticket })
  })

  test('another customer’s order is refused by Orders with one steady Ref, which the report form takes as given', async ({ browser }) => {
    const aisyah = await person(browser, 'aisyah@fetch.test')
    const before = (await demo.receipts('orders')).length
    await aisyah.goto(siteURL + '/my-orders/ORD-1004', { waitUntil: 'networkidle' })
    await expect(aisyah.getByRole('heading', { name: 'We couldn’t find this order' })).toBeVisible()
    await expect(aisyah.getByText('Answered by Orders · 404')).toBeVisible()
    const shown = await ref(aisyah)
    // A missing order is not asked for again, so the Ref stays the one shown.
    await aisyah.waitForTimeout(6000)
    const asked = (await since('orders', before)).filter((r) => r.target === '/orders/detail/ORD-1004')
    expect(asked.map((r) => `${r.request_id} ${r.status} ${r.subject}`)).toEqual([`${shown} 404 cust-001`])
    await shot(aisyah, 'flows-08-not-yours')
    await aisyah.getByRole('link', { name: 'Report this problem' }).click()
    await expect(aisyah.getByLabel('Reference from an error message')).toHaveValue(shown)
  })

  test('operations reads platform reports and every order', async ({ browser }) => {
    const jason = await person(browser, 'jason@fetch.test')
    await jason.goto(siteURL + '/ops', { waitUntil: 'networkidle' })
    await expect(jason.getByRole('heading', { level: 1, name: 'Platform reports' })).toBeVisible()
    await expect(jason.getByText(/^RM [\d,]+\.\d\d$/).first()).toBeVisible()
    const before = (await demo.receipts('reports')).length
    await jason.reload({ waitUntil: 'networkidle' })
    expect((await since('reports', before)).map((r) => `${r.target.split('?')[0]} ${r.status} ${r.subject}`).sort()).toEqual(
      ['/reports/sales 200 ops-01', '/reports/summary 200 ops-01', '/reports/top-items 200 ops-01'],
    )
    const ordersBefore = (await demo.receipts('orders')).length
    await jason.goto(siteURL + '/ops?view=orders', { waitUntil: 'networkidle' })
    await expect(jason.getByRole('row').filter({ hasText: 'ORD-1001' })).toBeVisible()
    expect((await since('orders', ordersBefore)).map((r) => `${r.method} ${r.target} ${r.status} ${r.subject}`)).toContain('GET /orders 200 ops-01')
  })

  test('a checkout whose answer is lost says it may or may not have been placed, is never resent by itself, and the explicit resend places it once', async ({ browser }) => {
    const priya = await person(browser, 'priya@fetch.test')
    await priya.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    const kopitiam = priya.getByRole('link').filter({ hasText: 'Kopitiam Lama' })
    const earlier = await kopitiam.count()
    await addToBasket(priya, siteURL, 'MER-003', 'Kopi O Peng')
    const before = (await demo.receipts('orders')).length
    let releaseResponse!: () => void
    let received!: () => void
    const pendingResponse = new Promise<void>((done) => { releaseResponse = done })
    const reached = new Promise<void>((done) => { received = done })
    await priya.route('**/orders', async (route) => {
      if (route.request().method() !== 'POST') return route.continue()
      await route.fetch()
      received()
      await pendingResponse
      await route.abort('connectionreset')
    })
    await priya.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await priya.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await priya.getByLabel('Postcode').fill('59100')
    try {
      await priya.getByRole('button', { name: /^Place order · RM / }).click()
      await reached
      await expect(priya.getByRole('button', { name: 'Placing your order…' })).toBeDisabled()
    } finally {
      releaseResponse()
    }
    const unknown = priya.getByRole('alert').filter({ hasText: 'No answer about your order.' })
    await expect(unknown).toContainText('It may or may not have been placed.')
    await priya.waitForTimeout(5000)
    const sent = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === '/orders')
    expect(sent).toHaveLength(1)
    await shot(priya, 'flows-03-unknown-outcome')

    await priya.unroute('**/orders')
    await priya.getByRole('button', { name: 'Send the same order again' }).click()
    await priya.waitForURL(/\/my-orders\/ORD-\d+\?placed=1$/)
    const both = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === '/orders')
    expect(both).toHaveLength(2)
    expect(both.map((r) => r.status)).toEqual([201, 201])
    const placed = new URL(priya.url()).pathname.split('/').pop()!
    await priya.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    await expect(kopitiam).toHaveCount(earlier + 1)
    record('unknown-outcome', { placed, receipts: both.map((r) => r.request_id) })
  })

  test('an item that runs out before checkout is refused by Orders with its line named; nothing is charged or reserved', async ({ browser }) => {
    const aisyah = await person(browser, 'aisyah@fetch.test')
    await addToBasket(aisyah, siteURL, 'MER-005', 'Cooking Oil 1 kg')
    const hafiz = await person(browser, 'mart@fetch.test')
    await hafiz.goto(siteURL + '/merchant?view=menu', { waitUntil: 'networkidle' })
    const oil = hafiz.getByRole('listitem').filter({ hasText: 'Cooking Oil 1 kg' })
    await oil.getByLabel('Change stock by').fill('-12')
    await oil.getByRole('button', { name: 'Adjust' }).click()
    await expect(oil.getByText('Stock updated.')).toBeVisible()

    const before = (await demo.receipts('orders')).length
    await aisyah.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await aisyah.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await aisyah.getByLabel('Postcode').fill('59100')
    await aisyah.getByRole('button', { name: /^Place order · RM / }).click()
    const refusal = aisyah.getByRole('alert').filter({ hasText: 'Something in your basket has run out.' })
    await expect(refusal).toContainText('Answered by Orders · 409')
    expect((await since('orders', before)).filter((r) => r.method === 'POST').map((r) => r.status)).toEqual([409])
    await oil.getByLabel('Change stock by').fill('12')
    await oil.getByRole('button', { name: 'Adjust' }).click()
    await expect(oil.getByText('Stock updated.')).toBeVisible()
  })

  test('a blocked customer is refused by the gateway at once, before any service, with its Ref; unblocking restores them', async ({ browser }) => {
    const daniel = await person(browser, 'daniel@fetch.test')
    const before = await demo.counts()
    await api.block('cust-002', 'Chargeback under review')
    await daniel.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    const refusal = daniel.getByRole('alert').filter({ hasText: 'This account is blocked.' })
    await expect(refusal).toContainText('Answered by the Fetch gateway · 403')
    expect(await demo.counts()).toEqual(before)
    await shot(daniel, 'flows-04-blocked')
    await api.unblock('cust-002')
    await daniel.reload({ waitUntil: 'networkidle' })
    await expect(daniel.getByRole('heading', { level: 1, name: 'My orders' })).toBeVisible()
  })

  test('the security lab: every attack is answered as expected, refusals reach no service, and forged identity headers change nothing', async ({ browser }) => {
    const daniel = await person(browser, 'daniel@fetch.test')
    await daniel.goto(siteURL + '/lab', { waitUntil: 'networkidle' })
    const before = await demo.receipts('orders')
    for (const send of await daniel.getByRole('button', { name: 'Send', exact: true }).all()) {
      await send.click()
      await expect(send).toBeEnabled()
    }
    await expect(daniel.getByRole('status').filter({ hasText: /^15 of 15 as expected\./ })).toBeVisible()
    await shot(daniel, 'flows-05-lab')
    // Only the forged-header read reached Orders, as the gateway's identity.
    const reached = (await demo.receipts('orders')).slice(before.length)
    expect(reached.map((r) => `${r.method} ${r.target} ${r.subject} ${r.roles}`)).toEqual(['GET /orders/mine cust-002 CUSTOMER'])
    record('lab', { reached })
  })

  test('a policy change takes effect in the app at once: a trial grants customers exact stock, its rollback removes it', async ({ browser }) => {
    const aisyah = await person(browser, 'aisyah@fetch.test')
    await aisyah.goto(siteURL + '/lab', { waitUntil: 'networkidle' })
    const card = aisyah.getByRole('listitem').filter({ hasText: 'Read exact stock' })
    await card.getByRole('button', { name: 'Send', exact: true }).click()
    await expect(card).toContainText('Answered by the gateway · 403')
    const baseline = JSON.parse(demo.file('baseline.json'))
    baseline.permits.push({ role: 'CUSTOMER', operation: 'inventory.stock.read' })
    await api.activate(await api.stage(JSON.stringify(baseline)), 300)
    const generation = (await api.status()).generation
    // The gateway now admits the request and Inventory, which leaves role
    // decisions to the gateway, answers it. The lab's expectation is the
    // baseline's, so it reports the difference.
    const before = (await demo.receipts('inventory')).length
    await card.getByRole('button', { name: 'Send', exact: true }).click()
    await expect(card).toContainText('Answered by Inventory · 200')
    await expect(card).toContainText('Differs')
    expect((await since('inventory', before)).map((r) => `${r.target} ${r.subject} ${r.generation} ${r.status}`)).toEqual([`/inventory/stock/ITM-502 cust-001 ${generation} 200`])
    await api.resolve('rollback')
    await card.getByRole('button', { name: 'Send', exact: true }).click()
    await expect(card).toContainText('Answered by the gateway · 403')
  })

  test('a board whose refresh is refused keeps its last list but says so, with who refused and the Ref, until it reads again', async ({ browser }) => {
    const amir = await person(browser, 'amir@fetch.test')
    await amir.goto(siteURL + '/rider', { waitUntil: 'networkidle' })
    const board = amir.getByRole('region', { name: 'Ready for pickup' })
    await expect(board).toBeVisible()
    const baseline = JSON.parse(demo.file('baseline.json'))
    baseline.permits = baseline.permits.filter((permit: { role: string; operation: string }) => !(permit.role === 'RIDER' && permit.operation === 'orders.jobs'))
    await api.activate(await api.stage(JSON.stringify(baseline)), 300)
    const stale = amir.getByRole('alert').filter({ hasText: 'What’s shown below is from the last successful check.' })
    await expect(stale).toContainText('Your account can’t do this right now.')
    await expect(stale).toContainText('Answered by the Fetch gateway · 403')
    await expect(stale.getByTitle(/^Full reference [0-9a-f]{32}$/)).toBeVisible()
    await expect(board).toBeVisible()
    await shot(amir, 'flows-07-refresh-refused')
    await api.resolve('rollback')
    await expect(stale).toHaveCount(0)
  })

  test('the session lives in this tab only: a new tab is signed out, nothing but the theme is in localStorage, signing out clears the person', async ({ browser }) => {
    const priya = await person(browser, 'priya@fetch.test')
    await addToBasket(priya, siteURL, 'MER-001', 'Teh Tarik')
    // Establish this test's saved-address precondition even in an isolated run.
    const addAddress = priya.getByRole('button', { name: 'Add a delivery address' })
    if (await addAddress.isVisible()) {
      await addAddress.click()
      await priya.getByLabel('Street address').fill('12 Jalan Telawi 3')
      await priya.getByLabel('Postcode').fill('59100')
      await priya.getByRole('button', { name: 'Save address' }).click()
    }
    await priya.getByRole('button', { name: /^Account: Priya/ }).click()
    await priya.getByRole('menuitemradio', { name: 'Dark' }).click()
    await priya.keyboard.press('Escape')
    const kept = await storage(priya)
    expect(kept.session).toEqual(['fetch.address', 'fetch.basket', 'fetch.session'])
    expect(kept.local).toEqual([['fetch.theme', 'dark']])
    expect(kept.cookie).toBe('')
    expect(await priya.context().cookies(siteURL)).toEqual([])
    // The token never appears in the page's text or address.
    const token = await priya.evaluate(() => JSON.parse(sessionStorage.getItem('fetch.session') ?? '{}').token as string)
    expect(token.split('.')).toHaveLength(3)
    expect(await priya.content()).not.toContain(token)
    expect(priya.url()).not.toContain(token)

    const other = await priya.context().newPage()
    await other.goto(siteURL + '/', { waitUntil: 'networkidle' })
    await expect(other.getByRole('link', { name: 'Sign in' })).toBeVisible()
    await other.close()

    // Changing person cancels what is pending: a slow read is aborted when
    // Priya signs out, and nothing of hers stays in the tab.
    const diagnostics: { event: string; at: number; error?: string }[] = []
    const startedAt = Date.now()
    let releaseRead: () => void = () => {}
    const held = new Promise<void>((resolve) => { releaseRead = resolve })
    const pending = priya.waitForRequest((request) => new URL(request.url()).pathname === '/orders/mine')
    await priya.route('**/orders/mine*', async (route) => {
      diagnostics.push({ event: 'read held', at: Date.now() - startedAt })
      await held
      await route.continue().catch((error: unknown) => {
        diagnostics.push({ event: 'continue after cancellation', at: Date.now() - startedAt, error: String(error) })
      })
    })
    try {
      await priya.goto(siteURL + '/my-orders')
      await pending
      const aborted = priya.waitForEvent('requestfailed', (request) => new URL(request.url()).pathname === '/orders/mine')
      await priya.getByRole('button', { name: /^Account: Priya/ }).click()
      await priya.getByRole('menuitem', { name: 'Sign out' }).click()
      diagnostics.push({ event: 'signed out', at: Date.now() - startedAt })
      releaseRead()
      const failure = (await aborted).failure()?.errorText
      diagnostics.push({ event: 'request failed', at: Date.now() - startedAt, error: failure })
      expect(failure).toMatch(/abort/i)
    } finally {
      releaseRead()
      record('sign-out-abort-diagnostics', { diagnostics })
      await priya.unroute('**/orders/mine*')
    }
    await expect(priya.getByRole('link', { name: 'Sign in' })).toBeVisible()
    expect((await storage(priya)).session).toEqual([])
    await expect(priya.getByText('Priya')).toHaveCount(0)
    await leave('priya@fetch.test')
  })

  test('signed-in pages pass the accessibility rules at desktop and phone widths', async ({ browser }) => {
    for (const [email, routes] of [
      ['aisyah@fetch.test', ['/', '/store/MER-001', '/my-orders', '/my-orders/ORD-1001', '/help', '/lab']],
      ['ros@fetch.test', ['/merchant', '/merchant?view=menu', '/merchant?view=sales']],
      ['amir@fetch.test', ['/rider']],
      ['nurul@fetch.test', ['/support']],
      ['jason@fetch.test', ['/ops', '/ops?view=orders']],
    ] as const) {
      const page = await person(browser, email)
      for (const width of [1440, 375]) {
        await page.setViewportSize({ width, height: 900 })
        for (const route of routes) {
          await page.goto(siteURL + route, { waitUntil: 'networkidle' })
          await accessible(page, `${email} ${route} at ${width}px`)
        }
      }
      await page.setViewportSize({ width: 1440, height: 900 })
    }
  })

  test('a cancellation with Inventory unavailable stays uncertain while a failed checkout is a definite refusal', async ({ browser }) => {
    const daniel = await person(browser, 'daniel@fetch.test')
    await addToBasket(daniel, siteURL, 'MER-005', 'Full Cream Milk 1 L')
    const order = await placeOrder(daniel, siteURL, 'Fetch wallet')
    const before = (await demo.receipts('orders')).length
    const charged = (await demo.state('orders')).data as { wallets: Record<string, number> }

    const priya = await person(browser, 'priya@fetch.test')
    await addToBasket(priya, siteURL, 'MER-005', 'Full Cream Milk 1 L')
    await priya.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await priya.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await priya.getByLabel('Postcode').fill('59100')

    // Stop only this test's Inventory process. Orders and its independent
    // control surface remain live; no response or application is patched.
    process.kill(demo.pids['inventory'])
    try {
      await daniel.getByRole('button', { name: 'Cancel order' }).click()
      await daniel.getByRole('dialog').getByRole('button', { name: 'Cancel order' }).click()
      await expect(daniel.getByRole('heading', { level: 1 })).toHaveText('Order cancelled')
      const state = (await demo.state('orders')).data as { orders: { id: string; status: string; total_sen: number }[]; wallets: Record<string, number> }
      const cancelled = state.orders.find((entry) => entry.id === order)!
      expect(cancelled.status).toBe('cancelled')
      expect(state.wallets['cust-002']).toBe(charged.wallets['cust-002'] + cancelled.total_sen)
      const unknown = daniel.getByRole('alert').filter({ hasText: 'Fetch couldn’t confirm cancelling this order.' })
      await expect(unknown).toContainText('It may or may not have been cancelled.')
      await expect(unknown).toContainText('Answered by Orders · 503')
      await daniel.waitForTimeout(3000)
      const sent = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === `/orders/cancel/${order}`)
      expect(sent).toHaveLength(1)
      expect(sent[0].status).toBe(503)

      await priya.getByRole('button', { name: /^Place order · RM / }).click()
      await expect(priya.getByRole('alert').filter({ hasText: 'Inventory could not confirm this request.' })).toContainText('Answered by Orders · 503')
      await expect(priya.getByRole('button', { name: 'Send the same order again' })).toHaveCount(0)
      record('inventory-unavailable-outcomes', { order, cancellationReceipt: sent[0].request_id })
    } finally {
      await demo.stop()
      await demo.start()
    }
  })

  test('a service that is down and a gateway that cannot record are reported truthfully, and nothing is resent', async ({ browser }) => {
    const aisyah = await person(browser, 'aisyah@fetch.test')
    let posts = 0
    aisyah.on('request', (request) => {
      if (request.method() === 'POST' && new URL(request.url()).pathname === '/support/tickets') posts++
    })
    const ask = async () => {
      await aisyah.getByLabel('What is it about?').fill('Where is my order')
      await aisyah.getByLabel('What happened?').fill('It has been an hour.')
      await aisyah.getByRole('button', { name: 'Send to Fetch support' }).click()
    }

    // Support stops: the gateway answers 502 for its reads and writes, and a
    // write is never described as not having reached it.
    process.kill(demo.pids['support'])
    await aisyah.goto(siteURL + '/help', { waitUntil: 'networkidle' })
    await expect(aisyah.getByRole('alert').filter({ hasText: 'That part of Fetch isn’t answering right now.' })).toContainText('Answered by the Fetch gateway · 502')
    await ask()
    const lost = aisyah.getByRole('alert').filter({ hasText: 'Fetch couldn’t confirm your request.' })
    await expect(lost).toContainText('It may or may not have been sent.')
    await aisyah.waitForTimeout(3000)
    expect(posts).toBe(1)
    await shot(aisyah, 'flows-06-service-down')

    // The gateway restarts unable to write its security record: protected
    // requests are refused 503 and reach no service; the write still says it
    // may or may not have been sent, and is not resent.
    await demo.stop()
    await demo.start({ fault: 'audit-partial' })
    const before = await demo.counts()
    await aisyah.reload({ waitUntil: 'networkidle' })
    await expect(aisyah.getByRole('alert').filter({ hasText: 'Fetch can’t take requests right now.' })).toContainText('Answered by the Fetch gateway · 503')
    await ask()
    await expect(aisyah.getByRole('alert').filter({ hasText: 'Fetch couldn’t confirm your request.' })).toContainText('Answered by the Fetch gateway · 503')
    await aisyah.waitForTimeout(3000)
    expect(posts).toBe(2)
    expect(await demo.counts()).toEqual(before)
    await demo.stop()
    await demo.start()
    record('service-down-and-unrecordable', { posts })
  })

  test('no content-policy violation, page error or script dialog in any role flow, and the demo password never left the sign-in form', async () => {
    expect(violations).toEqual([])
    expect(pageErrors).toEqual([])
    expect(dialogs).toBe(0)
    for (const service of ['orders', 'inventory', 'reports', 'support', 'accounts']) {
      const receipts = JSON.stringify(await demo.receipts(service))
      expect(receipts).not.toContain(demoPassword)
    }
    expect((await raw('GET', siteURL + '/')).body).not.toContain(demoPassword)
  })
})
