import { expect, test } from '@playwright/test'
import { adminURL, raw } from './env'
import { createOverride, enrol, go, moreDetail, nav, record, revisions, revokeOverride, shot, signIn } from './pages'
import { admin, adminCall, adminContext, auditEvents, demo, finish, handedOut, listAll, newContext, open, password, ready, sales, sessionCookie, signedInPage, status, strip } from './setup'

// History, password change, sign-out, an outage and a read-only administrator.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
    // History holds a temporary rule.
    await revokeOverride(admin, await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 120 }))
  })

  test.afterAll(finish)

  test('history pages through every recorded transition, filters and orders them, and shows no secrets', async () => {
    // More recorded transitions than one page holds: candidates staged
    // through the Admin API, each recorded as one transition.
    const confirmed = await adminCall('GET', '/admin/policy')
    const recorded = (await listAll('/admin/history')).length
    for (let i = recorded; i <= 100; i++) {
      const reply = await adminCall('POST', '/admin/candidates', { policy: confirmed.body.document }, { 'If-Match': confirmed.etag, 'Idempotency-Key': crypto.randomUUID() })
      expect(reply.status).toBe(201)
    }
    const entries = await listAll<{ seq: number; kind: string; outcome: string; actor: string; transition_id: string }>('/admin/history')
    expect(entries.length).toBeGreaterThan(100)

    // The newest entries come first; every page can be read.
    await go(admin, 'History')
    const table = admin.getByRole('region', { name: 'Administrative History' })
    const rows = table.locator('tbody tr')
    await expect(rows).toHaveCount(100)
    await expect(rows.first().locator('td').first()).toHaveText(String(Math.max(...entries.map((e) => e.seq))))
    await expect(admin.getByText('Showing 100 entries, newest first. More entries exist.')).toBeVisible()
    await admin.getByRole('button', { name: 'Load All' }).click()
    await expect(rows).toHaveCount(entries.length)
    await expect(admin.getByText(`Showing ${entries.length} entries, the whole history.`)).toBeVisible()
    await expect(admin.getByRole('button', { name: 'Load All' })).toHaveCount(0)
    const kinds = [...new Set(entries.map((e) => e.kind))]
    for (const kind of kinds) await expect(table.getByText(kind, { exact: true }).first()).toBeVisible()
    await shot(admin, '22-history')

    // The gateway applies the filters, so a count covers the whole history,
    // not just the entries read so far.
    await admin.getByLabel('Area').selectOption('overrides')
    const overrides = entries.filter((e) => ['tpo.create', 'tpo.revoke', 'tpo.expire'].includes(e.kind))
    expect(overrides.length).toBeGreaterThan(0)
    await expect(rows).toHaveCount(overrides.length)
    await expect(admin.getByText(`Showing ${overrides.length} matching entries: every match in the history.`)).toBeVisible()
    await expect(rows.first().locator('td').first()).toHaveText(String(overrides[overrides.length - 1].seq))
    await admin.getByLabel('Area').selectOption('all')
    await admin.getByLabel('Outcome', { exact: true }).selectOption('rejected')
    const rejected = entries.filter((e) => e.outcome === 'rejected')
    if (rejected.length > 0) await expect(rows).toHaveCount(Math.min(rejected.length, 100))
    else await expect(admin.getByText('No recorded transition matches these filters.')).toBeVisible()
    await admin.getByLabel('Outcome', { exact: true }).selectOption('all')
    await admin.getByLabel('Area').selectOption('policy')
    await shot(admin, '22b-history-filtered')
    await admin.getByLabel('Area').selectOption('all')
    await admin.getByLabel('Newest First').uncheck()
    await expect(rows.first().locator('td').first()).toHaveText(String(entries[0].seq))
    // The choices stay while moving between views, and only in this page.
    await go(admin, 'Overview')
    await go(admin, 'History')
    await expect(admin.getByLabel('Newest First')).not.toBeChecked()
    await expect(rows.first().locator('td').first()).toHaveText(String(entries[0].seq))
    expect(admin.url()).not.toContain('order')

    await table.getByRole('button', { name: 'Details of entry 1', exact: true }).click()
    const entry = admin.getByRole('region', { name: /^Transition / })
    await expect(entry).toContainText(entries[0].transition_id)
    await expect(entry.getByText('Kind', { exact: true }).locator('xpath=following-sibling::dd[1]')).toHaveText(entries[0].kind)
    await shot(admin, '23-history-entry')
    const shown = await admin.locator('body').innerText()
    for (const secret of [password, ...handedOut, demo.file('service.secret').trim()]) expect(shown).not.toContain(secret)
    await admin.getByLabel('Newest First').check()
    record('history', { entries: entries.length, kinds, overrides: overrides.length, rejected: rejected.length })
  })

  test('a wrong current password keeps the session; a password change ends every session of the administrator', async ({ browser }) => {
    const other = await signedInPage(browser, 'admin')
    const otherCookie = await sessionCookie(other.context)
    const changed = 'a different correct horse battery staple'

    await admin.getByRole('navigation', { name: 'Administration' }).getByRole('link', { name: 'My Account' }).click()
    await admin.getByLabel('Current Password').fill('not the current password at all')
    await admin.getByLabel('New Password', { exact: true }).fill(changed)
    await admin.getByLabel('Repeat the New Password').fill(changed)
    await admin.getByRole('button', { name: 'Change Password' }).click()
    // The gateway refuses the change (401) but the session goes on: the page
    // stays signed in and says what was wrong.
    await expect(admin.getByText('The current password is not correct.')).toBeVisible()
    await expect(admin.getByText('Signed in as admin')).toBeAttached()
    await shot(admin, '29-password-refused')
    expect((await raw('GET', adminURL + '/admin/session', { Cookie: await sessionCookie() })).status).toBe(200)

    await admin.getByLabel('Current Password').fill(password)
    await admin.getByLabel('New Password', { exact: true }).fill(changed)
    await admin.getByLabel('Repeat the New Password').fill(changed)
    await admin.getByRole('button', { name: 'Change Password' }).click()
    await expect(admin.getByText('Password changed. Every session of this administrator has ended; sign in with the new password.')).toBeVisible()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    // The other session ended too, at the gateway and in its page.
    expect((await raw('GET', adminURL + '/admin/session', { Cookie: otherCookie })).status).toBe(401)
    // The page rereads the gateway's status every second, so it notices by
    // itself; no click is needed, and one could race the sign-out.
    await expect(other.page.getByText('Your session has ended. Sign in again.')).toBeVisible()
    await other.context.close()

    // The old password no longer works; the new one does. It is then set back.
    await admin.getByLabel('Administrator Name').fill('admin')
    await admin.getByLabel('Password', { exact: true }).fill(password)
    await admin.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(admin.getByRole('alert').filter({ has: admin.locator('.more-details dd code', { hasText: /^401$/ }) })).toHaveClass(/tone-refused/)
    await signIn(admin, 'admin', changed)
    await admin.getByRole('navigation', { name: 'Administration' }).getByRole('link', { name: 'My Account' }).click()
    await admin.getByLabel('Current Password').fill(changed)
    await admin.getByLabel('New Password', { exact: true }).fill(password)
    await admin.getByLabel('Repeat the New Password').fill(password)
    await admin.getByRole('button', { name: 'Change Password' }).click()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    await signIn(admin, 'admin', password)
    const kinds = auditEvents()
      .filter((e) => e.path === '/admin/password')
      .map((e) => `${e.kind} ${e.reason ?? ''}`.trim())
    expect(kinds).toContain('admin.denied bad_credentials')
    record('password-change', { passwordEvents: kinds })
  })

  test('signing out ends the session in every tab and back navigation shows no privileged view', async () => {
    const cookie = await sessionCookie()
    const second = await adminContext.newPage()
    await second.goto(adminURL + '/#/accounts')
    await expect(second.getByText('Signed in as admin')).toBeAttached()
    await expect(second.getByRole('heading', { level: 1, name: 'Accounts' })).toBeVisible()
    await expect(second.getByRole('button', { name: 'Block an Account' })).toBeVisible()

    await admin.getByRole('button', { name: 'Sign Out' }).click()
    await expect(admin.getByText('Signed out.')).toBeVisible()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    expect((await raw('GET', adminURL + '/admin/status', { Cookie: cookie })).status).toBe(401)

    // The other tab rereads the status each second; the next read is
    // refused and the tab drops every privileged view by itself.
    await expect(second.getByText('Your session has ended. Sign in again.')).toBeVisible({ timeout: 5_000 })
    await expect(second.getByText('Signed in as admin')).toHaveCount(0)
    await expect(second.getByRole('navigation', { name: 'Administration' })).toHaveCount(0)
    await expect(second.getByRole('button', { name: 'Block an Account' })).toHaveCount(0)
    await second.close()

    await admin.goto('about:blank')
    await admin.goBack()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    await expect(admin.getByText('Signed in as admin')).toHaveCount(0)
    await signIn(admin, 'admin', password)
    record('sign-out', { oldCookieStatus: 401 })
  })

  test('an unreachable gateway, a restart and a read-only administrator are shown truthfully', async ({ browser }) => {
    const confirmed = (await status()).confirmed
    // An override that expires while the gateway is stopped, and a read the
    // dashboard makes while it is unreachable.
    const expiring = await createOverride(admin, { action: 'Allow Temporarily: Let a Role Call an Endpoint', operation: 'reports.sales', role: 'CUSTOMER', seconds: 5 })
    const expiresAt = Date.parse((await adminCall('GET', `/admin/overrides/${expiring}`)).body.expires_at)
    await go(admin, 'Overview')
    await demo.stop()
    await admin.getByRole('button', { name: 'Refresh', exact: true }).click()
    const unreachable = admin.getByRole('alert').filter({ has: admin.getByText('No answer from the gateway', { exact: true }) }).first()
    await expect(unreachable).toBeVisible()
    await expect(unreachable).toHaveClass(/tone-unknown/)
    await expect(moreDetail(unreachable, 'Reason')).toHaveText('the server could not be reached')
    await expect(admin.getByText('Signed in as admin')).toBeAttached()
    await shot(admin, '30-unreachable')
    while (Date.now() < expiresAt + 1000) await admin.waitForTimeout(250)
    await demo.start()
    // Restarting ended every session.
    await admin.reload()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    await expect(admin.locator('.entry-state')).toContainText('Gateway: Ready')
    await expect(admin.locator('.entry-state code')).toHaveText('normal')
    await signIn(admin, 'admin', password)
    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText(`Version ${confirmed}`)
    expect(await status()).toMatchObject({ state: 'normal', confirmed, overrides: [] })
    await expect(strip(admin)).toContainText('None active')
    expect((await adminCall('GET', `/admin/overrides/${expiring}`)).body.status).toBe('expired')
    const expiredGrant = await sales('cust-001')
    expect(expiredGrant.status).toBe(403)
    expect(expiredGrant.receipts).toHaveLength(0)
    expect((await sales('ops-01')).status).toBe(200)

    const viewerSecret = await demo.enrol('viewer', 'read')
    handedOut.push(viewerSecret)
    const viewerContext = await newContext(browser)
    const viewer = await viewerContext.newPage()
    await viewer.goto(adminURL + '/')
    await enrol(viewer, 'viewer', viewerSecret, password)
    await signIn(viewer, 'viewer', password)
    await expect(nav(viewer, 'History')).toHaveCount(0)
    await go(viewer, 'Changes')
    await expect(viewer.getByRole('heading', { name: 'Draft Changes' })).toBeVisible()
    await expect(viewer.getByRole('region', { name: 'Save a Draft Change' })).toHaveCount(0)
    await revisions(viewer)
    await expect(viewer.getByRole('button', { name: 'Start a Change from This' })).toHaveCount(0)
    // No block form, Block or Unblock button, quick action or key reload
    // without the permissions.
    await go(viewer, 'Accounts')
    await expect(viewer.getByRole('region', { name: 'Accounts', exact: true })).toBeVisible()
    await expect(viewer.getByRole('button', { name: 'Block an Account' })).toHaveCount(0)
    // The Blocked filter only narrows the list; no action to block or unblock is offered.
    await expect(viewer.getByRole('button', { name: /^(Block( an Account| Now|…)|Unblock)/ })).toHaveCount(0)
    await go(viewer, 'Team')
    await expect(viewer.getByRole('button', { name: 'Reload Keys from Settings' })).toHaveCount(0)
    await go(viewer, 'Temporary Rules')
    await expect(viewer.getByRole('heading', { name: 'Active Rules' })).toBeVisible()
    await expect(viewer.getByRole('heading', { name: 'Add a Temporary Rule' })).toHaveCount(0)
    await expect(viewer.getByRole('button', { name: 'Add a Temporary Rule' })).toHaveCount(0)
    await expect(viewer.getByRole('heading', { name: 'Past Rules' })).toHaveCount(0)
    await shot(viewer, '31-viewer-overrides')

    // Hiding the buttons is only a convenience: the server refuses the change.
    const session = (await (await viewerContext.request.get(adminURL + '/admin/session')).json()) as { csrf_token: string }
    expect((await viewerContext.request.get(adminURL + '/admin/blocked-accounts')).status()).toBe(200)
    const attempt = await viewerContext.request.put(adminURL + '/admin/blocked-accounts/cust-002', {
      headers: { Origin: adminURL, 'X-CSRF-Token': session.csrf_token, 'Idempotency-Key': crypto.randomUUID() },
      data: { reason: 'Read-only attempt' },
      failOnStatusCode: false,
    })
    expect(attempt.status()).toBe(403)
    const reload = await viewerContext.request.post(adminURL + '/admin/keys/reload', {
      headers: { Origin: adminURL, 'X-CSRF-Token': session.csrf_token, 'Idempotency-Key': crypto.randomUUID() },
      failOnStatusCode: false,
    })
    expect(reload.status()).toBe(403)
    expect((await viewerContext.request.get(adminURL + '/admin/history', { failOnStatusCode: false })).status()).toBe(403)
    await viewerContext.close()

    await admin.reload()
    await signIn(admin, 'admin', password)
    expect(await status()).toMatchObject({ confirmed })
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([])
    const denied = auditEvents()
      .filter((e) => e.kind === 'admin.denied' && e.actor === 'viewer')
      .map((e) => `${e.method} ${e.path} ${e.reason}`)
    expect(denied).toEqual(
      expect.arrayContaining(['PUT /admin/blocked-accounts/cust-002 permission', 'POST /admin/keys/reload permission', 'GET /admin/history permission']),
    )
    record('outage-restart-viewer', { confirmed, viewerDenied: denied })
  })
})
