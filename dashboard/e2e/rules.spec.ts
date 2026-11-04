import { expect, test } from '@playwright/test'
import { activate, createOverride, go, nav, record, resolve, revokeOverride, shot } from './pages'
import { admin, adminCall, adjust, admitted, finish, open, propose, ready, rowFor, sales, status, stock, strip } from './setup'

// Temporary rules: the dialog, restrictions, allowances, expiry and refusal during a trial.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('with no temporary rule ever added, the page shows one empty state', async () => {
    await go(admin, 'Temporary Rules')
    const main = admin.getByRole('main')
    await expect(main.getByText('No temporary rules are active.')).toBeVisible()
    await expect(main.locator('.empty, .card-empty')).toHaveCount(1)
    await expect(main.getByRole('heading', { name: 'Past Rules' })).toHaveCount(0)
  })

  test('Versions states what each role can do under the active policy, with temporary rules beside it', async () => {
    const policy = (await adminCall('GET', '/admin/policy')).body as { id: number; document: { roles: { id: string }[]; permits: { role: string; operation: string }[] } }
    await go(admin, 'Versions')
    const main = admin.getByRole('main')
    await expect(main.getByText(new RegExp(`^Version ${policy.id},`))).toBeVisible()
    // One tab per role; the chosen role's endpoints are listed in its panel.
    const panel = main.getByRole('tabpanel', { name: 'Roles', exact: true }).getByRole('tabpanel')
    for (const role of policy.document.roles) {
      const permitted = policy.document.permits.filter((p) => p.role === role.id).map((p) => p.operation)
      await main.getByRole('tab', { name: new RegExp(`^${role.id} ${permitted.length}$`) }).click()
      await expect(panel).toContainText(permitted.length === 0 ? `${role.id} has no permissions.` : `${role.id} can call ${permitted.length} ${permitted.length === 1 ? 'endpoint' : 'endpoints'}.`)
      for (const operation of permitted) await expect(panel.getByText(operation, { exact: true })).toBeVisible()
    }
    // The filter narrows the endpoints shown and says how many are left; it reads nothing new.
    await main.getByRole('tab', { name: /^OPERATIONS \d+$/ }).click()
    const total = policy.document.permits.filter((p) => p.role === 'OPERATIONS').length
    const filter = main.getByRole('searchbox', { name: 'Filter endpoints' })
    await filter.fill('inventory.stock.adjust')
    await expect(main.getByText(`1 of ${total} endpoints`, { exact: true })).toBeVisible()
    await expect(panel.getByText('reports.summary', { exact: true })).toHaveCount(0)
    await filter.fill('')
    await expect(main.getByText(`${total} endpoints`, { exact: true })).toBeVisible()
    // A temporary rule is listed beside the role it changes; the permission
    // in the policy stays listed, because the policy itself is unchanged.
    const id = await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 120 })
    await go(admin, 'Versions')
    await main.getByRole('tab', { name: /^OPERATIONS \d+$/ }).click()
    await expect(panel).toContainText(`Rule ${id}: OPERATIONS may not call inventory.stock.adjust, until`)
    await expect(panel.getByText('inventory.stock.adjust', { exact: true })).toBeVisible()
    await shot(admin, '47-roles')
    await revokeOverride(admin, id)
    await go(admin, 'Versions')
    await main.getByRole('tab', { name: /^OPERATIONS \d+$/ }).click()
    await expect(panel).not.toContainText(`Rule ${id}:`)
  })

  test('the temporary rule dialog ends on a review of every choice, adds the rule from it and is listed by the filter', async () => {
    await go(admin, 'Temporary Rules')
    const main = admin.getByRole('main')
    await main.getByRole('button', { name: 'Add a Temporary Rule', exact: true }).click()
    const wizard = admin.getByRole('dialog', { name: 'Add a Temporary Rule' })
    await wizard.getByRole('radio', { name: 'Restrict Temporarily: Stop a Role Calling an Endpoint', exact: true }).check()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByLabel('Endpoint', { exact: true }).selectOption('inventory.stock.adjust')
    await wizard.getByLabel('Role', { exact: true }).selectOption('OPERATIONS')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByLabel('Lifetime (Seconds)').fill('0')
    // Next pressed with an invalid lifetime stays on the step and says why.
    const early = wizard.getByRole('button', { name: 'Next', exact: true })
    await early.click()
    await expect(wizard.getByRole('alert')).toHaveText('Enter a whole number of seconds from 1 to 86400.')
    await expect(early).toHaveAccessibleDescription('Enter a whole number of seconds from 1 to 86400.')
    await expect(wizard.getByLabel('Lifetime (Seconds)')).toHaveValue('0')
    await wizard.getByLabel('Lifetime (Seconds)').fill('120')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await expect(wizard).toContainText('OPERATIONS may not call inventory.stock.adjust, for 2 min.')
    // The review lists every choice, not only the summary.
    const facts = wizard.locator('.review-facts')
    await expect(facts).toContainText('Restrict Temporarily: Stop a Role Calling an Endpoint')
    await expect(facts).toContainText('inventory.stock.adjust')
    await expect(facts).toContainText('OPERATIONS')
    await expect(facts).toContainText('2 min from the next request')
    await wizard.getByRole('button', { name: 'Check with the Gateway' }).click()
    await expect(wizard.getByText('The gateway would accept it now. Nothing has been created.')).toBeVisible()
    await shot(admin, '48-rule-wizard-review')
    // The review is the confirmation: Add rule on it sends the rule.
    await wizard.getByRole('button', { name: 'Add Rule', exact: true }).click()
    await expect(wizard).toHaveCount(0)
    const done = admin.getByText(/^Rule \d+ added\. It ends at /)
    await expect(done).toBeVisible()
    const id = Number((await done.textContent())!.match(/Rule (\d+)/)![1])
    expect((await status()).overrides).toContain(id)
    const filter = main.getByRole('searchbox', { name: 'Filter active temporary rules' })
    await filter.fill(`rule ${id}`)
    await expect(main.getByText('1 of 1 active rule', { exact: true })).toBeVisible()
    await filter.fill('no-such-endpoint')
    await expect(main.getByText('No active rule matches the filter.')).toBeVisible()
    await filter.fill('')
    await revokeOverride(admin, id)
  })

  test('a temporary containment stops OPERATIONS adjusting stock without changing the confirmed base, and revocation lifts it', async () => {
    const before = await status()
    const id = await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 300 })
    const during = await status()
    expect(during).toMatchObject({ confirmed: before.confirmed, revision: before.confirmed, overrides: [id] })
    expect(during.generation).toContain('+o')
    // A notification names the rule from the status the gateway reports,
    // without moving focus. It closes by itself after a few seconds unless
    // pointed at, and can be dismissed.
    const started = admin.getByRole('status').filter({ hasText: `Temporary Rule ${id} is active.` })
    await expect(started).toBeVisible({ timeout: 15_000 })
    await started.locator('p', { hasText: `Temporary rule ${id} is active.` }).hover()
    await admin.getByRole('button', { name: `Dismiss: Temporary rule ${id} is active.` }).click()
    await expect(started).toHaveCount(0)
    // Screenshots finish CSS animations, which would end the notice's
    // countdown, so the page is captured once it is dismissed.
    await shot(admin, '15-overrides-active')

    const row = rowFor(admin.getByRole('main').getByRole('region', { name: 'Active Rules' }), id)
    await expect(row).toContainText('Restrict Temporarily')
    await expect(row).toContainText('OPERATIONS may not call inventory.stock.adjust')

    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText('1 active')
    await expect(strip(admin)).toContainText('OPERATIONS may not call inventory.stock.adjust')
    await expect(strip(admin).getByText(during.generation!, { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText(`Version ${before.confirmed}`)
    await expect(nav(admin, 'Temporary Rules')).toContainText('1')
    await shot(admin, '16-overview-override')

    const stockBefore = await stock('ITM-501')
    const contained = await adjust('ops-01', -1)
    expect(contained.status).toBe(403)
    expect(contained.receipts).toHaveLength(0)
    expect(await stock('ITM-501')).toBe(stockBefore)

    // A second override on the same rule is refused as a conflict.
    await go(admin, 'Temporary Rules')
    await admin.getByRole('main').getByRole('button', { name: 'Add a Temporary Rule', exact: true }).click()
    const wizard = admin.getByRole('dialog', { name: 'Add a Temporary Rule' })
    await wizard.getByRole('radio', { name: 'Restrict Temporarily: Stop a Role Calling an Endpoint', exact: true }).check()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByLabel('Endpoint', { exact: true }).selectOption('inventory.stock.adjust')
    await wizard.getByLabel('Role', { exact: true }).selectOption('OPERATIONS')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByRole('button', { name: 'Check with the Gateway' }).click()
    await expect(wizard.getByRole('alert').filter({ hasText: 'OVERRIDE_CONFLICT' })).toBeVisible()
    await wizard.getByRole('button', { name: 'Cancel', exact: true }).click()
    expect((await status()).overrides).toEqual([id])

    await revokeOverride(admin, id)
    const after = await status()
    expect(after).toMatchObject({ confirmed: before.confirmed, overrides: [] })
    await expect(admin.getByRole('status').filter({ hasText: `Temporary Rule ${id} ended.` })).toBeVisible({ timeout: 15_000 })
    const lifted = await adjust('ops-01', -1)
    expect(lifted.status).toBe(200)
    expect(lifted.receipts).toHaveLength(1)
    expect(lifted.receipts[0].generation).toBe(after.generation)
    expect(await stock('ITM-501')).toBe(stockBefore - 1)
    record('override-containment', { id, during, contained: contained.status, after, lifted: lifted.receipts[0] })
  })

  test('a temporary exception lets CUSTOMER read sales until the gateway expires it', async () => {
    const beforeGrant = await sales('cust-001')
    expect(beforeGrant.status).toBe(403)
    expect(beforeGrant.receipts).toHaveLength(0)
    const id = await createOverride(admin, { action: 'Allow Temporarily: Let a Role Call an Endpoint', operation: 'reports.sales', role: 'CUSTOMER', seconds: 4 })
    const allowed = await sales('cust-001')
    admitted(allowed)
    expect(allowed.receipts[0].generation).toContain('+o')

    await go(admin, 'Temporary Rules')
    await expect(admin.getByText('No temporary rules are active.')).toBeVisible({ timeout: 20_000 })
    const history = admin.getByRole('region', { name: 'Past Rules' })
    await expect(rowFor(history, id)).toContainText('Ended')
    expect((await adminCall('GET', `/admin/overrides/${id}`)).body.status).toBe('expired')
    await shot(admin, '17-overrides-history')

    const denied = await sales('cust-001')
    expect(denied.status).toBe(403)
    expect(denied.receipts).toHaveLength(0)
    record('override-expiry', { id, allowed: allowed.receipts[0], after: await status() })
  })

  test('no override can be created while a provisional policy waits', async () => {
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    await activate(admin, 600)
    await go(admin, 'Temporary Rules')
    await expect(admin.getByText('A change is on trial: the gateway refuses new temporary rules until it is kept or undone.')).toBeVisible()
    // The page offers no way to add one while a change is on trial, and the
    // gateway itself refuses the same check.
    await expect(admin.getByRole('main').getByRole('button', { name: 'Add a Temporary Rule', exact: true })).toBeDisabled()
    const policy = await adminCall('GET', '/admin/overrides')
    const refused = await adminCall(
      'POST',
      '/admin/overrides/validate',
      { type: 'containment', control: 'operation', operation: 'orders.all', expires_in_seconds: 300 },
      { 'If-Match': policy.etag ?? '' },
    )
    expect(JSON.stringify(refused.body).toUpperCase()).toContain('PROVISIONAL_ACTIVE')
    await shot(admin, '18-override-refused')
    expect((await status()).overrides).toEqual([])
    await resolve(admin, 'rollback')
    record('override-during-provisional', { status: await status() })
  })
})
