import { expect, test } from '@playwright/test'
import { siteURL } from './env'
import { accessible, addToBasket, placeOrder, record, shot, storage } from './pages'
import { api, demo, leave, person, since, useRoles } from './roles'

// What the app shows when the gateway refuses or a service fails: blocked
// accounts, the security lab, a policy trial, the per-tab session,
// accessibility, Inventory or Support down and an unrecordable gateway.
test.describe.serial('Failures and safeguards on the real services', () => {
  useRoles()

  test('a blocked customer is refused by the gateway at once, before any service, with its Ref; unblocking restores them', async ({ browser }) => {
    const customer2 = await person(browser, 'customer2@fetch.test')
    const before = await demo.counts()
    await api.block('cust-002', 'Chargeback under review')
    await customer2.goto(siteURL + '/my-orders', { waitUntil: 'networkidle' })
    const refusal = customer2.getByRole('alert').filter({ hasText: 'This account is blocked.' })
    await expect(refusal).toContainText('Answered by the Fetch gateway · 403')
    expect(await demo.counts()).toEqual(before)
    await shot(customer2, 'flows-04-blocked')
    await api.unblock('cust-002')
    await customer2.reload({ waitUntil: 'networkidle' })
    await expect(customer2.getByRole('heading', { level: 1, name: 'My orders' })).toBeVisible()
  })

  test('the security lab: every attack matches the baseline, refusals reach no service, forged identity headers change nothing and a blocked account is named', async ({ browser }) => {
    const customer2 = await person(browser, 'customer2@fetch.test')
    await customer2.goto(siteURL + '/lab', { waitUntil: 'networkidle' })
    await expect(customer2.getByText(/paste it into the Dashboard’s header search/)).toBeVisible()
    await expect(customer2.getByRole('main')).not.toContainText(/request lookup/i)
    const before = await demo.receipts('orders')
    for (const send of await customer2.getByRole('button', { name: 'Send', exact: true }).all()) {
      await send.click()
      await expect(send).toBeEnabled()
    }
    await expect(customer2.getByRole('status').filter({ hasText: /^15 of 15 match the baseline\./ })).toBeVisible()
    await shot(customer2, 'flows-05-lab')
    // Only the forged-header read reached Orders, as the gateway's identity.
    const reached = (await demo.receipts('orders')).slice(before.length)
    expect(reached.map((r) => `${r.method} ${r.target} ${r.subject} ${r.roles}`)).toEqual(['GET /orders/mine cust-002 CUSTOMER'])
    record('lab', { reached })

    // A blocked account is refused before the role and body checks. The lab
    // names that from the gateway's answer instead of reporting a difference,
    // and the token checks before it still match the baseline.
    await api.block('cust-002', 'Security lab check')
    try {
      const forged = customer2.getByRole('listitem').filter({ hasText: 'Forged identity headers' })
      await forged.getByRole('button', { name: 'Send', exact: true }).click()
      await expect(forged).toContainText('Account blocked')
      await expect(forged).toContainText('Answered by the gateway · 403 account_blocked')
      await expect(forged).not.toContainText('Differs')
      const tampered = customer2.getByRole('listitem').filter({ hasText: 'Tampered token' })
      await tampered.getByRole('button', { name: 'Send', exact: true }).click()
      await expect(tampered).toContainText('Matches the baseline')
      await expect(customer2.getByRole('status').filter({ hasText: /^14 of 15 match the baseline\. 1 refused because this account is blocked\./ })).toBeVisible()
      await shot(customer2, 'flows-05b-lab-blocked')
    } finally {
      await api.unblock('cust-002')
    }
  })

  test('a policy change takes effect in the app at once: a trial grants customers exact stock, its rollback removes it', async ({ browser }) => {
    const customer1 = await person(browser, 'customer1@fetch.test')
    await customer1.goto(siteURL + '/lab', { waitUntil: 'networkidle' })
    const card = customer1.getByRole('listitem').filter({ hasText: 'Read exact stock' })
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
    await expect(card).toContainText('Differs from the baseline')
    await expect(card).toContainText('a change on trial or a temporary rule can decide differently')
    expect((await since('inventory', before)).map((r) => `${r.target} ${r.subject} ${r.generation} ${r.status}`)).toEqual([`/inventory/stock/ITM-502 cust-001 ${generation} 200`])
    await api.resolve('rollback')
    await card.getByRole('button', { name: 'Send', exact: true }).click()
    await expect(card).toContainText('Answered by the gateway · 403')
  })

  test('the session lives in this tab only: a new tab is signed out, nothing but the theme is in localStorage, signing out clears the person', async ({ browser }) => {
    const customer3 = await person(browser, 'customer3@fetch.test')
    await addToBasket(customer3, siteURL, 'MER-001', 'Teh Tarik')
    // Establish this test's saved-address precondition even in an isolated run.
    const addAddress = customer3.getByRole('button', { name: 'Add a delivery address' })
    if (await addAddress.isVisible()) {
      await addAddress.click()
      await customer3.getByLabel('Street address').fill('12 Jalan Telawi 3')
      await customer3.getByLabel('Postcode').fill('59100')
      await customer3.getByRole('button', { name: 'Save address' }).click()
    }
    await customer3.getByRole('button', { name: /^Account: Customer 3/ }).click()
    await customer3.getByRole('menuitemradio', { name: 'Dark' }).click()
    await customer3.keyboard.press('Escape')
    const kept = await storage(customer3)
    expect(kept.session).toEqual(['fetch.address', 'fetch.basket', 'fetch.session'])
    expect(kept.local).toEqual([['fetch.theme', 'dark']])
    expect(kept.cookie).toBe('')
    expect(await customer3.context().cookies(siteURL)).toEqual([])
    // The token never appears in the page's text or address.
    const token = await customer3.evaluate(() => JSON.parse(sessionStorage.getItem('fetch.session') ?? '{}').token as string)
    expect(token.split('.')).toHaveLength(3)
    expect(await customer3.content()).not.toContain(token)
    expect(customer3.url()).not.toContain(token)

    const other = await customer3.context().newPage()
    await other.goto(siteURL + '/', { waitUntil: 'networkidle' })
    await expect(other.getByRole('link', { name: 'Sign in' })).toBeVisible()
    await other.close()

    // Changing person cancels what is pending: a slow read is aborted when
    // Customer 3 signs out, and nothing of hers stays in the tab.
    const diagnostics: { event: string; at: number; error?: string }[] = []
    const startedAt = Date.now()
    let releaseRead: () => void = () => {}
    const held = new Promise<void>((resolve) => { releaseRead = resolve })
    const pending = customer3.waitForRequest((request) => new URL(request.url()).pathname === '/orders/mine')
    await customer3.route('**/orders/mine*', async (route) => {
      diagnostics.push({ event: 'read held', at: Date.now() - startedAt })
      await held
      await route.continue().catch((error: unknown) => {
        diagnostics.push({ event: 'continue after cancellation', at: Date.now() - startedAt, error: String(error) })
      })
    })
    try {
      await customer3.goto(siteURL + '/my-orders')
      await pending
      const aborted = customer3.waitForEvent('requestfailed', (request) => new URL(request.url()).pathname === '/orders/mine')
      await customer3.getByRole('button', { name: /^Account: Customer 3/ }).click()
      await customer3.getByRole('menuitem', { name: 'Sign out' }).click()
      diagnostics.push({ event: 'signed out', at: Date.now() - startedAt })
      releaseRead()
      const failure = (await aborted).failure()?.errorText
      diagnostics.push({ event: 'request failed', at: Date.now() - startedAt, error: failure })
      expect(failure).toMatch(/abort/i)
    } finally {
      releaseRead()
      record('sign-out-abort-diagnostics', { diagnostics })
      await customer3.unroute('**/orders/mine*')
    }
    await expect(customer3.getByRole('link', { name: 'Sign in' })).toBeVisible()
    expect((await storage(customer3)).session).toEqual([])
    await expect(customer3.getByText('Customer 3')).toHaveCount(0)
    await leave('customer3@fetch.test')
  })

  test('signed-in pages pass the accessibility rules at desktop and phone widths', async ({ browser }) => {
    for (const [email, routes] of [
      ['customer1@fetch.test', ['/', '/store/warung-nasi-lemak', '/my-orders', '/my-orders/ORD-1001', '/help', '/lab']],
      ['merchant1@fetch.test', ['/merchant', '/merchant?view=menu', '/merchant?view=sales']],
      ['rider1@fetch.test', ['/rider']],
      ['support1@fetch.test', ['/support']],
      ['admin1@fetch.test', ['/ops', '/ops?view=orders']],
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
    const customer1 = await person(browser, 'customer1@fetch.test')
    await addToBasket(customer1, siteURL, 'MER-005', 'Full Cream Milk 1 L')
    const order = await placeOrder(customer1, siteURL, 'Fetch wallet')
    const before = (await demo.receipts('orders')).length
    const charged = (await demo.state('orders')).data as { wallets: Record<string, number> }

    const customer3 = await person(browser, 'customer3@fetch.test')
    await addToBasket(customer3, siteURL, 'MER-005', 'Full Cream Milk 1 L')
    await customer3.goto(siteURL + '/checkout', { waitUntil: 'networkidle' })
    await customer3.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await customer3.getByLabel('Postcode').fill('59100')

    // Stop only this test's Inventory process. Orders and its independent
    // control surface remain live; no response or application is patched.
    process.kill(demo.pids['inventory'])
    try {
      await customer1.getByRole('button', { name: 'Cancel order' }).click()
      await customer1.getByRole('dialog').getByRole('button', { name: 'Cancel order' }).click()
      await expect(customer1.getByRole('heading', { level: 1 })).toHaveText('Order cancelled')
      const state = (await demo.state('orders')).data as { orders: { id: string; status: string; total_sen: number }[]; wallets: Record<string, number> }
      const cancelled = state.orders.find((entry) => entry.id === order)!
      expect(cancelled.status).toBe('cancelled')
      expect(state.wallets['cust-001']).toBe(charged.wallets['cust-001'] + cancelled.total_sen)
      const unknown = customer1.getByRole('alert').filter({ hasText: 'Fetch couldn’t confirm cancelling this order.' })
      await expect(unknown).toContainText('It may or may not have been cancelled.')
      await expect(unknown).toContainText('Answered by Orders · 503')
      await customer1.waitForTimeout(3000)
      const sent = (await since('orders', before)).filter((r) => r.method === 'POST' && r.target === `/orders/cancel/${order}`)
      expect(sent).toHaveLength(1)
      expect(sent[0].status).toBe(503)

      await customer3.getByRole('button', { name: /^Place order · RM / }).click()
      await expect(customer3.getByRole('alert').filter({ hasText: 'Inventory could not confirm this request.' })).toContainText('Answered by Orders · 503')
      await expect(customer3.getByRole('button', { name: 'Send the same order again' })).toHaveCount(0)
      record('inventory-unavailable-outcomes', { order, cancellationReceipt: sent[0].request_id })
    } finally {
      await demo.stop()
      await demo.start()
    }
  })

  test('a service that is down and a gateway that cannot record are reported truthfully, and nothing is resent', async ({ browser }) => {
    const customer1 = await person(browser, 'customer1@fetch.test')
    let posts = 0
    customer1.on('request', (request) => {
      if (request.method() === 'POST' && new URL(request.url()).pathname === '/support/tickets') posts++
    })
    const ask = async () => {
      await customer1.getByLabel('What is it about?').fill('Where is my order')
      await customer1.getByLabel('What happened?').fill('It has been an hour.')
      await customer1.getByRole('button', { name: 'Send to Fetch support' }).click()
    }

    // Support stops: the gateway answers 502 for its reads and writes, and a
    // write is never described as not having reached it.
    process.kill(demo.pids['support'])
    await customer1.goto(siteURL + '/help', { waitUntil: 'networkidle' })
    await expect(customer1.getByRole('alert').filter({ hasText: 'That part of Fetch isn’t answering right now.' })).toContainText('Answered by the Fetch gateway · 502')
    await ask()
    const lost = customer1.getByRole('alert').filter({ hasText: 'Fetch couldn’t confirm your request.' })
    await expect(lost).toContainText('It may or may not have been sent.')
    await customer1.waitForTimeout(3000)
    expect(posts).toBe(1)
    await shot(customer1, 'flows-06-service-down')

    // The gateway restarts unable to write its security record: protected
    // requests are refused 503 and reach no service; the write still says it
    // may or may not have been sent, and is not resent.
    await demo.stop()
    await demo.start({ fault: 'audit-partial' })
    const before = await demo.counts()
    await customer1.reload({ waitUntil: 'networkidle' })
    await expect(customer1.getByRole('alert').filter({ hasText: 'Fetch can’t take requests right now.' })).toContainText('Answered by the Fetch gateway · 503')
    await ask()
    await expect(customer1.getByRole('alert').filter({ hasText: 'Fetch couldn’t confirm your request.' })).toContainText('Answered by the Fetch gateway · 503')
    await customer1.waitForTimeout(3000)
    expect(posts).toBe(2)
    expect(await demo.counts()).toEqual(before)
    await demo.stop()
    await demo.start()
    record('service-down-and-unrecordable', { posts })
  })
})
