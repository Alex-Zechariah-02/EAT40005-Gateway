import { expect, test } from '@playwright/test'
import { adminURL, raw, siteURL } from './env'
import { activate, dialogButton, go, record, resolve, shot, signIn } from './pages'
import { admin, adminCall, adminContext, adjust, demo, enrolled, establish, finish, listAll, open, password, propose, signedInPage, status } from './setup'

// Blocking and unblocking accounts, request checks and unknown outcomes.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await enrolled()
    // A request refused before the first policy, as at cold start.
    await raw('GET', siteURL + '/admin/status')
    await establish(admin, demo)
  })

  test.afterAll(finish)

  test('blocking an account refuses its next request at once, a policy rollback cannot lift it, and unblocking restores it', async ({ browser }) => {
    const before = await status()
    expect((await adjust('ops-01', -1)).status).toBe(200)

    // The Recently seen shortcut fills in the account; the block needs a reason.
    await go(admin, 'Accounts')
    const main = admin.getByRole('main')
    const blockedList = main.getByRole('region', { name: 'Account List', exact: true })
    await main.getByRole('button', { name: 'Block… ops-01' }).click()
    await expect(admin.getByLabel('Account ID')).toHaveValue('ops-01')
    await expect(admin.getByRole('button', { name: 'Block Now' })).toBeDisabled()
    await admin.getByLabel('Reason').fill('Reported a stolen phone')
    await admin.getByRole('button', { name: 'Block Now' }).click()
    await expect(admin.getByText(/^ops-01 is blocked: its requests are refused from now on$/)).toBeVisible()
    const row = blockedList.getByRole('row').filter({ hasText: 'ops-01' })
    await expect(row).toContainText('Reported a stolen phone')
    await expect(row).toContainText('by admin')
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([
      expect.objectContaining({ subject: 'ops-01', reason: 'Reported a stolen phone', blocked_by: 'admin' }),
    ])
    await shot(admin, '20-account-blocked')

    // A block is not a policy change: the active policy is unchanged, and
    // the next request is refused before it reaches any service.
    expect(await status()).toEqual(before)
    const refused = await adjust('ops-01', -1)
    expect(refused.status).toBe(403)
    expect(refused.receipts).toHaveLength(0)
    const [event] = (await adminCall('GET', '/admin/events?subject=ops-01&limit=1')).body.items
    expect(event).toMatchObject({ decision: 'refused', status: 403, stage: 'blocked', code: 'account_blocked', roles: ['OPERATIONS'] })

    // The refusal's Ref finds the request and the check that decided it.
    await admin.getByRole('searchbox', { name: 'Search', exact: true }).fill(event.request_id)
    await admin.getByRole('searchbox', { name: 'Search', exact: true }).press('Enter')
    await expect(admin.getByRole('heading', { level: 1, name: 'Accounts' })).toBeVisible()
    expect(admin.url()).toContain(`#/accounts?ref=${event.request_id}`)
    await expect(admin.getByRole('dialog').getByRole('heading', { name: 'PATCH /inventory/stock/ITM-501' })).toBeVisible()
    const checks = admin.getByRole('dialog').getByRole('list', { name: "The gateway's checks, in order" })
    await expect(checks.getByRole('listitem').filter({ hasText: 'Blocked List' })).toContainText('Refused Here')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Sign-In' })).toContainText('Passed')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Role' })).toContainText('Not Reached')
    await shot(admin, '21-drawer-blocked')
    await admin.keyboard.press('Escape')

    // The same header search finds an account's requests without putting the
    // account into the address, opens a draft change by its number, and says
    // plainly that a short Ref is not enough.
    const search = admin.getByRole('searchbox', { name: 'Search', exact: true })
    await go(admin, 'Overview')
    await expect(search).toHaveAttribute('placeholder', 'Search by Ref, account or endpoint')
    await search.fill('ops-01')
    await search.press('Enter')
    await expect(admin.getByRole('heading', { level: 1, name: 'Security Log' })).toBeVisible()
    await expect(main.getByLabel('Account', { exact: true })).toHaveValue('ops-01')
    expect(admin.url()).not.toContain('ops-01')
    await go(admin, 'Overview')
    await search.fill(`${event.request_id.slice(0, 4)}-${event.request_id.slice(4, 8)}`)
    await search.press('Enter')
    await expect(admin.getByText(/^That is a short Ref\./)).toBeVisible()
    await search.fill('change 1')
    await search.press('Enter')
    await expect(admin.getByRole('heading', { name: 'Change 1', exact: true })).toBeVisible()

    // A request decided before routing (at cold start, when the gateway
    // admitted nothing) never shows the checks it did not meet as passed.
    const early = (await listAll<{ request_id: string; operation: string | null; code: string | null; stage: string }>('/admin/events')).find(
      (e) => e.operation === null && e.code === 'gateway_unavailable',
    )!
    expect(early.stage).toBe('record')
    // Its address opens its drawer, as a link pasted from elsewhere would.
    await admin.evaluate((ref) => (window.location.hash = `#/overview?ref=${ref}`), early.request_id)
    await expect(checks.getByRole('listitem').filter({ hasText: 'Route' })).toContainText('Not Checked')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Record' })).toContainText('Refused Here')
    await expect(admin.getByRole('dialog').getByText('Decided before endpoint matching; earlier checks did not run.')).toBeVisible()
    await admin.keyboard.press('Escape')

    // A trial and its rollback leave the block in place.
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    await activate(admin, 600)
    const duringTrial = await adjust('ops-01', -1)
    expect(duringTrial.status).toBe(403)
    expect(duringTrial.receipts).toHaveLength(0)
    await resolve(admin, 'rollback')
    const rolledBack = await adjust('ops-01', -1)
    expect(rolledBack.status).toBe(403)
    expect(rolledBack.receipts).toHaveLength(0)

    // A second session reads the list; the first unblocks, after a
    // confirmation; the second session's unblock of the same account is then
    // refused, and its Refresh shows the current list.
    const other = await signedInPage(browser, 'admin')
    await go(other.page, 'Accounts')
    const otherMain = other.page.getByRole('main')
    await expect(otherMain.getByRole('row').filter({ hasText: 'ops-01' }).getByText('Blocked', { exact: true })).toBeVisible()
    await go(admin, 'Accounts')
    await blockedList.getByRole('button', { name: 'Unblock ops-01' }).click()
    await expect(admin.getByRole('dialog')).toContainText('ops-01 can use Fetch again at once')
    await dialogButton(admin, 'Unblock').click()
    await expect(admin.getByText(/^ops-01 is unblocked and can use Fetch again$/)).toBeVisible()
    await expect(main.getByRole('row').filter({ has: admin.getByText('Blocked', { exact: true }) })).toHaveCount(0)
    await otherMain.getByRole('button', { name: 'Unblock ops-01' }).click()
    await dialogButton(other.page, 'Unblock').click()
    const stale = otherMain.getByRole('alert').filter({ has: other.page.locator('.more-details dd code', { hasText: /^404$/ }) })
    await expect(stale).toBeVisible()
    await expect(stale).toHaveClass(/tone-refused/)
    const staleText = await stale.textContent()
    await other.page.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(otherMain.getByRole('row').filter({ has: other.page.getByText('Blocked', { exact: true }) })).toHaveCount(0)
    await other.context.close()

    const restored = await adjust('ops-01', -1)
    expect(restored.status).toBe(200)
    expect(restored.receipts).toHaveLength(1)
    expect(restored.receipts[0]).toMatchObject({ subject: 'ops-01', roles: 'OPERATIONS' })

    // A block whose answer is lost: the gateway applies it, the page says the
    // outcome is unknown and sends nothing more; the explicit identical resend
    // returns the recorded receipt with exactly one transition.
    let lostReceipt: { subject?: string } = {}
    await admin.route('**/admin/blocked-accounts/cust-002', async (route) => {
      const response = await route.fetch()
      lostReceipt = await response.json()
      await route.abort('connectionreset')
    })
    await admin.getByRole('main').getByRole('button', { name: 'Block an Account' }).click()
    await admin.getByLabel('Account ID').fill('cust-002')
    await admin.getByLabel('Reason').fill('Chargeback under review')
    await admin.getByRole('button', { name: 'Block Now' }).click()
    await expect(admin.getByText('No answer from the gateway: outcome unknown')).toBeVisible()
    await expect(admin.getByText(/^cust-002 is blocked/)).toHaveCount(0)
    await shot(admin, '22-block-outcome-unknown')
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([expect.objectContaining({ subject: 'cust-002' })])
    await admin.unroute('**/admin/blocked-accounts/cust-002')
    await admin.getByRole('button', { name: 'Send the Identical Request Again' }).click()
    await expect(admin.getByText(/^cust-002 is blocked: its requests are refused from now on$/)).toBeVisible()
    expect(lostReceipt.subject).toBe('cust-002')
    const blocks = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'account.block' && h.target === 'cust-002')
    expect(blocks).toHaveLength(1)
    await blockedList.getByRole('button', { name: 'Unblock cust-002' }).click()
    await dialogButton(admin, 'Unblock').click()
    await expect(admin.getByText(/^cust-002 is unblocked and can use Fetch again$/)).toBeVisible()

    // History names each change in words.
    await go(admin, 'History')
    await expect(main.getByText('Account Blocked').first()).toBeVisible()
    await expect(main.getByText('Account Unblocked').first()).toBeVisible()
    record('blocked-accounts', { event, refused: refused.status, afterRollback: rolledBack.status, stale: staleText, restored: restored.receipts[0], blockTransitions: blocks.length })
  })

  test('a change whose answer is lost stays listed on every page and after signing in again, and is resolved from its exact history record without being sent again', async () => {
    const subject = 'cust-004'
    await go(admin, 'Accounts')
    // The gateway acts on the block but its answer is lost, and the
    // history look-up is held back too, so the outcome stays unknown.
    await admin.route('**/admin/blocked-accounts/*', async (route) => {
      if (route.request().method() !== 'PUT') return route.continue()
      await route.fetch()
      await route.abort('connectionreset')
    })
    await admin.route('**/admin/history/admin*', (route) => route.abort('connectionreset'))
    try {
      await admin.getByRole('main').getByRole('button', { name: 'Block an Account' }).click()
      await admin.getByLabel('Account ID').fill(subject)
      await admin.getByLabel('Reason').fill('lost answer test')
      await admin.getByRole('button', { name: 'Block Now' }).click()
      await expect(admin.getByText('No answer from the gateway: outcome unknown')).toBeVisible()
      const unresolved = admin.getByRole('region', { name: /^(A change has an unknown outcome|Outcome found)$/ })
      await expect(unresolved).toContainText(`Block ${subject}`)
      await expect(unresolved).toContainText('It could not be looked up')
      await admin.unroute('**/admin/blocked-accounts/*')

      // Still listed on another view.
      await go(admin, 'Overview')
      await expect(unresolved).toContainText(`Block ${subject}`)
      await shot(admin, '42-unknown-outcome-listed')

      // The session ends elsewhere; signing in again as the same
      // administrator still lists it.
      const tab = await adminContext.newPage()
      await tab.goto(adminURL + '/')
      await expect(tab.getByText('Signed in as admin')).toBeAttached()
      await tab.getByRole('button', { name: 'Sign Out' }).click()
      await expect(tab.getByRole('heading', { name: 'Sign in' })).toBeVisible()
      await tab.close()
      // The Overview's next status read, a second later, finds the session ended.
      await expect(admin.getByText(/^Your session has ended\. Sign in again\./)).toBeVisible({ timeout: 20_000 })
      await signIn(admin, 'admin', password)
      await expect(unresolved).toContainText(`Block ${subject}`)

      // Look-ups work again: the exact transition shows it applied, once.
      await admin.unroute('**/admin/history/admin*')
      await unresolved.getByRole('button', { name: /^Check Its Outcome/ }).click()
      await expect(unresolved.getByText(/^Applied: the gateway recorded it at /)).toBeVisible()
      const blocks = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'account.block' && h.target === subject)
      expect(blocks).toHaveLength(1)
      await unresolved.getByRole('button', { name: /^Dismiss/ }).click()
      await expect(unresolved).toHaveCount(0)
      record('unknown-outcome-register', { subject, blockTransitions: blocks.length })
    } finally {
      await admin.unroute('**/admin/blocked-accounts/*')
      await admin.unroute('**/admin/history/admin*')
    }
    expect((await adminCall('DELETE', `/admin/blocked-accounts/${subject}`, undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
  })
})
