import { expect, test } from '@playwright/test'
import { siteURL } from './env'
import { record, shot, signIn } from './pages'
import { api, demo, person, ref, scriptDialogs, since, useRoles } from './roles'

// Support, operations and the account page on the real services, and a
// refused Ref as staff and customers see it.
test.describe.serial('Staff and accounts on the real services', () => {
  useRoles()

  test('support answers and resolves a help request; markup in a request is shown as text and never runs', async ({ browser }) => {
    const customer1 = await person(browser, 'customer1@fetch.test')
    await customer1.goto(siteURL + '/help', { waitUntil: 'networkidle' })
    const markup = '<img src=x onerror=alert(1)> missing'
    await customer1.getByLabel('What is it about?').fill(markup)
    await customer1.getByLabel('What happened?').fill('The sambal was missing.')
    // ORD-1001 is a cash order that was never delivered: nothing was paid.
    await customer1.getByRole('combobox', { name: /^Order\b/ }).selectOption('ORD-1001')
    await customer1.getByRole('button', { name: 'Send to Fetch support' }).click()
    await customer1.waitForURL(/\/help\/TKT-\d+$/)
    const ticket = new URL(customer1.url()).pathname.split('/').pop()!

    const support1 = await person(browser, 'support1@fetch.test')
    await support1.goto(`${siteURL}/support?ticket=${ticket}`, { waitUntil: 'networkidle' })
    const panel = support1.getByRole('region', { name: 'Ticket', exact: true })
    await expect(panel.getByRole('heading', { name: markup })).toBeVisible()
    expect(await support1.locator('img[src="x"]').count()).toBe(0)
    // Staff read the customer's name, not the account ID.
    await expect(panel.getByRole('list', { name: 'Conversation' }).getByText('Customer 1', { exact: true })).toBeVisible()
    const linked = panel.getByRole('heading', { name: 'Linked order' }).locator('..')
    await expect(linked).toContainText('#1001')
    await expect(linked).toContainText('cash on delivery')
    await expect(linked).not.toContainText('Paid')
    await panel.getByLabel('Reply to the customer').fill('Sorry about that; we have refunded it.')
    await panel.getByRole('button', { name: 'Send reply' }).click()
    await expect(panel.getByRole('list', { name: 'Conversation' })).toContainText('we have refunded it')
    await expect(panel.getByRole('list', { name: 'Conversation' }).getByText('You · Fetch support', { exact: true })).toHaveCount(1)
    // Another agent sees the reply as Support 1's, never as their own.
    const admin2 = await person(browser, 'admin2@fetch.test')
    await admin2.goto(`${siteURL}/support?ticket=${ticket}`, { waitUntil: 'networkidle' })
    const seenByFarah = admin2.getByRole('region', { name: 'Ticket', exact: true }).getByRole('list', { name: 'Conversation' })
    await expect(seenByFarah.getByText('Support 1 · Fetch support', { exact: true })).toBeVisible()
    await expect(seenByFarah.getByText(/^You\b/)).toHaveCount(0)
    await panel.getByRole('button', { name: 'Mark resolved' }).click()
    await expect(panel).toContainText('This request is resolved.')
    await customer1.reload({ waitUntil: 'networkidle' })
    const seenByAisyah = customer1.getByRole('list', { name: 'Conversation' })
    await expect(seenByAisyah).toContainText('we have refunded it')
    await expect(seenByAisyah.getByText('You', { exact: true })).toBeVisible()
    await expect(seenByAisyah.getByText('Support agent from Fetch', { exact: true })).toBeVisible()
    expect(scriptDialogs()).toBe(0)
    record('support', { ticket })
  })

  test('another customer’s order is refused by Orders with one steady Ref, which the report form takes as given', async ({ browser }) => {
    const customer1 = await person(browser, 'customer1@fetch.test')
    const before = (await demo.receipts('orders')).length
    await customer1.goto(siteURL + '/my-orders/ORD-1004', { waitUntil: 'networkidle' })
    await expect(customer1.getByRole('heading', { name: 'We couldn’t find this order' })).toBeVisible()
    await expect(customer1.getByText('Answered by Orders · 404')).toBeVisible()
    const shown = await ref(customer1)
    // A missing order is not asked for again, so the Ref stays the one shown.
    await customer1.waitForTimeout(6000)
    const asked = (await since('orders', before)).filter((r) => r.target === '/orders/detail/ORD-1004')
    expect(asked.map((r) => `${r.request_id} ${r.status} ${r.subject}`)).toEqual([`${shown} 404 cust-001`])
    await shot(customer1, 'flows-08-not-yours')
    await customer1.getByRole('link', { name: 'Report this problem' }).click()
    await expect(customer1.getByLabel('Reference from an error message')).toHaveValue(shown)
    // Support is told where an administrator finds the request: the Dashboard's header search.
    await customer1.getByLabel('What is it about?').fill('Order not found')
    await customer1.getByLabel('What happened?').fill('The page said the order could not be found.')
    await customer1.getByRole('button', { name: 'Send to Fetch support' }).click()
    await customer1.waitForURL(/\/help\/TKT-\d+$/)
    const support1 = await person(browser, 'support1@fetch.test')
    await support1.goto(`${siteURL}/support?ticket=${new URL(customer1.url()).pathname.split('/').pop()}`, { waitUntil: 'networkidle' })
    const reference = support1.getByRole('heading', { name: 'Request reference' }).locator('..')
    await expect(reference).toContainText('paste it into the Dashboard’s header search')
    await expect(reference).not.toContainText(/request lookup/i)
  })

  test('operations reads platform reports and every order', async ({ browser }) => {
    const admin1 = await person(browser, 'admin1@fetch.test')
    await admin1.goto(siteURL + '/ops', { waitUntil: 'networkidle' })
    await expect(admin1.getByRole('heading', { level: 1, name: 'Platform reports' })).toBeVisible()
    await expect(admin1.getByText(/^RM [\d,]+\.\d\d$/).first()).toBeVisible()
    const before = (await demo.receipts('reports')).length
    await admin1.reload({ waitUntil: 'networkidle' })
    expect((await since('reports', before)).map((r) => `${r.target.split('?')[0]} ${r.status} ${r.subject}`).sort()).toEqual(
      ['/reports/sales 200 ops-01', '/reports/summary 200 ops-01', '/reports/top-items 200 ops-01'],
    )
    const ordersBefore = (await demo.receipts('orders')).length
    await admin1.goto(siteURL + '/ops?view=orders', { waitUntil: 'networkidle' })
    await expect(admin1.getByRole('row').filter({ hasText: '#1001' })).toBeVisible()
    expect((await since('orders', ordersBefore)).map((r) => `${r.method} ${r.target} ${r.status} ${r.subject}`)).toContain('GET /orders 200 ops-01')
  })

  test('the account page shows what Fetch Accounts returns, the wallet and this tab’s address, and signs out', async ({ browser }) => {
    // A page of its own: this test signs out, which must not touch the
    // shared Customer 3 page the other tests use.
    const customer3 = await person(browser, null)
    await signIn(customer3, siteURL, 'customer3@fetch.test')
    // The account button is named by its visible text, at any width.
    for (const width of [375, 1440]) {
      await customer3.setViewportSize({ width, height: 900 })
      await expect(customer3.getByRole('button', { name: 'Account: Customer 3, Customer', exact: true })).toBeVisible()
    }
    await customer3.getByRole('button', { name: /^Account: Customer 3/ }).click()
    await customer3.getByRole('menuitem', { name: 'Your account' }).click()
    await expect(customer3).toHaveURL(/\/account$/)
    const profile = customer3.getByRole('region', { name: 'Customer 3' })
    await expect(profile).toContainText('customer3@fetch.test')
    await expect(profile).toContainText('cust-003')
    await expect(customer3.getByRole('region', { name: 'Fetch wallet' })).toContainText(/RM\s?\d/)
    const address = customer3.getByRole('region', { name: 'Delivery address' })
    await address.getByRole('button', { name: /^(Add an address|Change)$/ }).click()
    await address.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await address.getByLabel('Postcode').fill('59100')
    await address.getByRole('button', { name: 'Save address' }).click()
    await expect(address).toContainText('12 Jalan Telawi 3')
    await shot(customer3, 'flows-08-account')
    await customer3.getByRole('button', { name: 'Sign out' }).click()
    await expect(customer3).toHaveURL(siteURL + '/')
    await customer3.goto(siteURL + '/account', { waitUntil: 'networkidle' })
    await expect(customer3.getByRole('heading', { name: 'Sign in to continue' })).toBeVisible()

    // Staff see their profile and their workspace, without a wallet.
    const rider1 = await person(browser, 'rider1@fetch.test')
    await rider1.goto(siteURL + '/account', { waitUntil: 'networkidle' })
    await expect(rider1.getByRole('region', { name: 'Rider 1' })).toContainText('rider-01')
    await expect(rider1.getByRole('region', { name: 'Fetch wallet' })).toHaveCount(0)
    await expect(rider1.getByRole('link', { name: 'Go to your workspace' })).toHaveAttribute('href', '/rider')
  })

  test('a board whose refresh is refused keeps its last list but says so, with who refused and the Ref, until it reads again', async ({ browser }) => {
    const rider1 = await person(browser, 'rider1@fetch.test')
    await rider1.goto(siteURL + '/rider', { waitUntil: 'networkidle' })
    const board = rider1.getByRole('region', { name: 'Ready for pickup' })
    await expect(board).toBeVisible()
    const baseline = JSON.parse(demo.file('baseline.json'))
    baseline.permits = baseline.permits.filter((permit: { role: string; operation: string }) => !(permit.role === 'RIDER' && permit.operation === 'orders.jobs'))
    await api.activate(await api.stage(JSON.stringify(baseline)), 300)
    const stale = rider1.getByRole('alert').filter({ hasText: 'What’s shown below is from the last successful check.' })
    await expect(stale).toContainText('Your account can’t do this right now.')
    await expect(stale).toContainText('Answered by the Fetch gateway · 403')
    await expect(stale.getByTitle(/^Full reference [0-9a-f]{32}$/)).toBeVisible()
    await expect(board).toBeVisible()
    await shot(rider1, 'flows-07-refresh-refused')
    await api.resolve('rollback')
    await expect(stale).toHaveCount(0)
  })
})
