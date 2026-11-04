import { expect, test } from '@playwright/test'
import { siteURL } from './env'
import { addToBasket, orderCard, placeOrder, record, shot } from './pages'
import { demo, person, ref, since, useRoles } from './roles'

// Customers, merchants and riders on the real services: browsing, checkout,
// every order status, price and stock changes, cancels, a lost checkout
// answer and an item that runs out.
test.describe.serial('Orders on the real services', () => {
  useRoles()

  test('a customer browses, sorts, filters and searches, and checks out with cash and with the wallet; each order reaches Orders once', async ({ browser }) => {
    const customer3 = await person(browser, 'customer3@fetch.test')
    await expect(customer3.getByRole('heading', { level: 1, name: 'What can we fetch for you?' })).toBeVisible()
    await expect(customer3.getByRole('link', { name: /Warung Nasi Lemak/ })).toBeVisible()
    await customer3.goto(siteURL + '/?q=nasi', { waitUntil: 'networkidle' })
    await expect(customer3.getByText('Nasi Lemak Ayam Goreng').first()).toBeVisible()
    // Results sort by price, and the choice stays in the address.
    const prices = async () => (await customer3.locator('a[href*="?item="]').allTextContents()).map((text) => Number(/RM\s*([\d.]+)/.exec(text)?.[1]))
    await customer3.getByLabel('Sort by').selectOption({ label: 'Price, high to low' })
    await expect(customer3).toHaveURL(/\/\?q=nasi&sort=price-desc$/)
    const highToLow = await prices()
    expect(highToLow.length).toBeGreaterThan(1)
    expect(highToLow).toEqual([...highToLow].sort((a, b) => b - a))
    // The results name each shop they come from, and each opens the shop.
    const shops = customer3.getByRole('navigation', { name: 'Shops in these results' })
    await expect(shops.getByRole('link')).toHaveCount(1)
    await shops.getByRole('link', { name: 'Warung Nasi Lemak' }).click()
    await expect(customer3).toHaveURL(/\/store\/warung-nasi-lemak$/)

    // Fetch Food sorts by delivery fee and can show only open shops; both
    // choices survive a reload.
    await customer3.goto(siteURL + '/food', { waitUntil: 'networkidle' })
    await customer3.getByLabel('Sort by').selectOption({ label: 'Lowest delivery fee' })
    await expect(customer3).toHaveURL(/\/food\?sort=fee$/)
    const fees = (await customer3.locator('a[href^="/store/"]').filter({ hasText: 'Delivery fee' }).allTextContents()).map((text) => Number(/Delivery fee\s*RM\s*([\d.]+)/.exec(text)?.[1]))
    expect(fees.length).toBeGreaterThan(1)
    expect(fees).toEqual([...fees].sort((a, b) => a - b))
    await customer3.getByLabel('Open now').check()
    await expect(customer3).toHaveURL(/\/food\?sort=fee&open=1$/)
    await expect(customer3.getByRole('status').filter({ hasText: /open now/ })).toBeVisible()
    await expect(customer3.getByText('Closed now')).toHaveCount(0)
    await customer3.reload({ waitUntil: 'networkidle' })
    await expect(customer3.getByLabel('Sort by')).toHaveValue('fee')
    await expect(customer3.getByLabel('Open now')).toBeChecked()
    await shot(customer3, 'flows-00-food-sorted')

    const before = (await demo.receipts('orders')).length
    await addToBasket(customer3, siteURL, 'MER-001', 'Nasi Lemak Ayam Goreng')
    const cash = await placeOrder(customer3, siteURL, 'Cash on delivery')
    await expect(customer3.getByRole('heading', { level: 1 })).toHaveText('Placed')
    const created = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === '/orders')
    expect(created).toHaveLength(1)
    expect(created[0]).toMatchObject({ status: 201, subject: 'cust-003' })

    // Customer 3's first Fetch Mart order has free delivery, from Orders' own rule.
    await addToBasket(customer3, siteURL, 'MER-005', 'Full Cream Milk 1 L')
    await customer3.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await expect(customer3.getByText('Free because this is your first Fetch Mart order.')).toBeVisible()
    await expect(customer3.getByText('Balance RM 100.00. Charged when you place the order.')).toBeVisible()
    await shot(customer3, 'flows-01-checkout')
    const wallet = await placeOrder(customer3, siteURL, 'Fetch wallet')
    await customer3.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    for (const id of [cash, wallet]) await expect(customer3.getByRole('link', { name: new RegExp(`#${id.replace(/^ORD-/, '')}(?!\\d)`) })).toBeVisible()
    await customer3.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await expect(customer3.getByText('Nothing to check out yet')).toBeVisible()
    record('customer-checkout', { cash, wallet, receipt: created[0] })
  })

  test('the merchant, a rider and the customer move an order through every status, each seeing it change', async ({ browser }) => {
    const customer1 = await person(browser, 'customer1@fetch.test')
    await addToBasket(customer1, siteURL, 'MER-001', 'Teh Tarik')
    const order = await placeOrder(customer1, siteURL, 'Cash on delivery')

    const merchant1 = await person(browser, 'merchant1@fetch.test')
    await merchant1.goto(siteURL + '/merchant', { waitUntil: 'networkidle' })
    await orderCard(merchant1, order).getByRole('button', { name: 'Accept order' }).click()
    await expect(orderCard(merchant1, order).getByRole('button', { name: 'Mark ready for pickup' })).toBeVisible()
    // Her open order page follows the change by itself and says so; a status
    // read when the page opens is not announced.
    await expect(customer1.getByRole('status').filter({ hasText: /^Update: Being prepared at .+\.$/ })).toBeVisible({ timeout: 15_000 })
    await expect(customer1.getByRole('heading', { level: 1 })).toHaveText('Being prepared')
    await customer1.reload({ waitUntil: 'networkidle' })
    await expect(customer1.getByRole('heading', { level: 1 })).toHaveText('Being prepared')
    await expect(customer1.getByText(/^Update:/)).toHaveCount(0)
    await orderCard(merchant1, order).getByRole('button', { name: 'Mark ready for pickup' }).click()
    await expect(orderCard(merchant1, order).getByRole('button')).toHaveCount(0)

    const rider1 = await person(browser, 'rider1@fetch.test')
    await rider1.goto(siteURL + '/rider', { waitUntil: 'networkidle' })
    await expect(orderCard(rider1, order)).toContainText(/Collect RM [\d.]+ in cash/)
    await orderCard(rider1, order).getByRole('button', { name: 'Take this job' }).click()
    await customer1.reload({ waitUntil: 'networkidle' })
    await expect(customer1.getByRole('heading', { level: 1 })).toHaveText('On the way')
    await orderCard(rider1, order).getByRole('button', { name: 'Mark delivered' }).click()
    await expect(orderCard(rider1, order)).toHaveCount(0)
    await customer1.reload({ waitUntil: 'networkidle' })
    await expect(customer1.getByRole('heading', { level: 1 })).toHaveText('Delivered')

    // The receipt prints without the site's header, footer or buttons.
    const header = customer1.locator('header').first()
    const print = customer1.locator('button', { hasText: 'Print receipt' })
    await expect(header).toBeVisible()
    await expect(print).toBeVisible()
    await customer1.emulateMedia({ media: 'print' })
    await expect(customer1.getByText(`Receipt · Order #${order.replace(/^ORD-/, '')} · placed `)).toBeVisible()
    await expect(header).toBeHidden()
    await expect(customer1.locator('footer').first()).toBeHidden()
    await expect(print).toBeHidden()
    await expect(customer1.getByText(/^Order placed\./)).toBeHidden()
    await shot(customer1, 'flows-02-receipt-print')
    await customer1.emulateMedia({ media: null })

    // Orders recorded exactly one change per step.
    const statuses = (await demo.receipts('orders'))
      .filter((r) => r.method === 'POST' && r.target.endsWith(`/${order}`))
      .map((r) => `${r.method} ${r.target.split('/')[2]} ${r.status}`)
    expect(statuses).toEqual(['POST accept 200', 'POST ready 200', 'POST pickup 200', 'POST deliver 200'])
    record('order-statuses', { order, statuses })
  })

  test('a merchant changes a price, availability and stock, and customers see it at once', async ({ browser }) => {
    const merchant5 = await person(browser, 'merchant5@fetch.test')
    await merchant5.goto(siteURL + '/merchant?view=menu', { waitUntil: 'networkidle' })
    const before = (await demo.receipts('inventory')).length
    const bread = merchant5.getByRole('listitem').filter({ hasText: 'Wholemeal Bread' })
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

    const customer2 = await person(browser, 'customer2@fetch.test')
    await customer2.goto(siteURL + '/item/ITM-505', { waitUntil: 'networkidle' })
    await expect(customer2.getByText('RM 4.80').first()).toBeVisible()
    await expect(customer2.getByRole('button', { name: /Add to basket/ })).toHaveCount(0)
    await bread.getByText('Unavailable', { exact: true }).click()
    await expect(bread.getByText('Now available to order.')).toBeVisible()
    record('merchant-menu', { item: 'ITM-505' })
  })

  test('a cancel after the shop accepted is refused as already moved on, with Orders named and the Ref; a placed order cancels', async ({ browser }) => {
    const customer2 = await person(browser, 'customer2@fetch.test')
    await addToBasket(customer2, siteURL, 'MER-002', 'Roti Canai')
    const accepted = await placeOrder(customer2, siteURL, 'Cash on delivery')
    const bala = await person(browser, 'merchant2@fetch.test')
    await bala.goto(siteURL + '/merchant', { waitUntil: 'networkidle' })
    await orderCard(bala, accepted).getByRole('button', { name: 'Accept order' }).click()
    await expect(orderCard(bala, accepted).getByRole('button', { name: 'Mark ready for pickup' })).toBeVisible()

    // Customer 2's page still shows the order as placed; his cancel reaches Orders,
    // which refuses it.
    await customer2.getByRole('button', { name: 'Cancel order' }).click()
    await customer2.getByRole('dialog').getByRole('button', { name: 'Cancel order' }).click()
    const refusal = customer2.getByRole('alert').filter({ hasText: 'This has already moved on.' })
    await expect(refusal).toContainText('Answered by Orders · 409')
    const shown = await ref(customer2)
    expect((await demo.receipts('orders')).find((r) => r.request_id === shown)).toMatchObject({ status: 409 })
    await shot(customer2, 'flows-02-cancel-refused')

    await addToBasket(customer2, siteURL, 'MER-002', 'Milo Ais')
    const placed = await placeOrder(customer2, siteURL, 'Cash on delivery')
    await customer2.getByRole('button', { name: 'Cancel order' }).click()
    await customer2.getByRole('dialog').getByRole('button', { name: 'Cancel order' }).click()
    await expect(customer2.getByRole('heading', { level: 1 })).toHaveText('Order cancelled')
    record('cancel', { accepted, refusedRef: shown, cancelled: placed })
  })

  test('a checkout whose answer is lost says it may or may not have been placed, is never resent by itself, and the explicit resend places it once', async ({ browser }) => {
    const customer3 = await person(browser, 'customer3@fetch.test')
    await customer3.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    const kopitiam = customer3.getByRole('link').filter({ hasText: 'Kopitiam Lama' })
    const earlier = await kopitiam.count()
    await addToBasket(customer3, siteURL, 'MER-003', 'Kopi O Peng')
    const before = (await demo.receipts('orders')).length
    let releaseResponse!: () => void
    let received!: () => void
    const pendingResponse = new Promise<void>((done) => { releaseResponse = done })
    const reached = new Promise<void>((done) => { received = done })
    await customer3.route('**/orders', async (route) => {
      if (route.request().method() !== 'POST') return route.continue()
      await route.fetch()
      received()
      await pendingResponse
      await route.abort('connectionreset')
    })
    await customer3.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await customer3.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await customer3.getByLabel('Postcode').fill('59100')
    try {
      await customer3.getByRole('button', { name: /^Place order · RM / }).click()
      await reached
      await expect(customer3.getByRole('button', { name: 'Placing your order…' })).toBeDisabled()
    } finally {
      releaseResponse()
    }
    const unknown = customer3.getByRole('alert').filter({ hasText: 'No answer about your order.' })
    await expect(unknown).toContainText('It may or may not have been placed.')
    await customer3.waitForTimeout(5000)
    const sent = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === '/orders')
    expect(sent).toHaveLength(1)
    await shot(customer3, 'flows-03-unknown-outcome')

    await customer3.unroute('**/orders')
    await customer3.getByRole('button', { name: 'Send the same order again' }).click()
    await customer3.waitForURL(/\/my-orders\/ORD-\d+\?placed=1$/)
    const both = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === '/orders')
    expect(both).toHaveLength(2)
    expect(both.map((r) => r.status)).toEqual([201, 201])
    const placed = new URL(customer3.url()).pathname.split('/').pop()!
    await customer3.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    await expect(kopitiam).toHaveCount(earlier + 1)
    record('unknown-outcome', { placed, receipts: both.map((r) => r.request_id) })
  })

  test('an item that runs out before checkout is refused by Orders with its line named; nothing is charged or reserved', async ({ browser }) => {
    const customer1 = await person(browser, 'customer1@fetch.test')
    await addToBasket(customer1, siteURL, 'MER-005', 'Cooking Oil 1 kg')
    const merchant5 = await person(browser, 'merchant5@fetch.test')
    await merchant5.goto(siteURL + '/merchant?view=menu', { waitUntil: 'networkidle' })
    const oil = merchant5.getByRole('listitem').filter({ hasText: 'Cooking Oil 1 kg' })
    await oil.getByLabel('Change stock by').fill('-12')
    await oil.getByRole('button', { name: 'Adjust' }).click()
    await expect(oil.getByText('Stock updated.')).toBeVisible()

    const before = (await demo.receipts('orders')).length
    await customer1.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await customer1.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await customer1.getByLabel('Postcode').fill('59100')
    await customer1.getByRole('button', { name: /^Place order · RM / }).click()
    const refusal = customer1.getByRole('alert').filter({ hasText: 'Something in your basket has run out.' })
    await expect(refusal).toContainText('Answered by Orders · 409')
    expect((await since('orders', before)).filter((r) => r.method === 'POST').map((r) => r.status)).toEqual([409])
    await oil.getByLabel('Change stock by').fill('12')
    await oil.getByRole('button', { name: 'Adjust' }).click()
    await expect(oil.getByText('Stock updated.')).toBeVisible()
  })
})
