import { spawnSync } from 'node:child_process'
import { createServer } from 'node:http'
import { dirname, join } from 'node:path'
import { expect, test } from '@playwright/test'
import { adminURL, raw, siteURL } from './env'
import { advanced, dialogButton, enrol, go, moreDetail, record, shot, signIn, stage } from './pages'
import { admin, adminCall, adminContext, adminRequests, admitted, auditEvents, call, demo, expectNoSecrets, finish, handedOut, open, password, ready, scriptDialogs, sessionCookie, status, strip, token } from './setup'

// Admin API refusals, security log failures, a restore and the secret scan.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('other origins, missing CSRF tokens, wrong hosts and API tokens cannot use the Admin API', async () => {
    const before = await status()
    const cookie = await sessionCookie()
    const csrf = ((await adminCall('GET', '/admin/session')).body as { csrf_token: string }).csrf_token
    const change = JSON.stringify({ reason: 'Blocked from another site' })
    const put = (headers: Record<string, string>) =>
      raw('PUT', adminURL + '/admin/blocked-accounts/cust-002', { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID(), ...headers }, change)

    const cases = {
      'website origin': await put({ Cookie: cookie, Origin: siteURL, 'X-CSRF-Token': csrf }),
      'no origin': await put({ Cookie: cookie, 'X-CSRF-Token': csrf }),
      'no CSRF token': await put({ Cookie: cookie, Origin: adminURL }),
      'wrong CSRF token': await put({ Cookie: cookie, Origin: adminURL, 'X-CSRF-Token': 'x'.repeat(csrf.length) }),
      'wrong host': await put({ Cookie: cookie, Origin: adminURL, 'X-CSRF-Token': csrf, Host: `localhost:${new URL(adminURL).port}` }),
      'API token, no session': await put({ Authorization: `Bearer ${token('ops-01')}`, Origin: adminURL }),
    }
    expect(Object.fromEntries(Object.entries(cases).map(([k, v]) => [k, v.status]))).toEqual({
      'website origin': 403,
      'no origin': 403,
      'no CSRF token': 403,
      'wrong CSRF token': 403,
      'wrong host': 400,
      'API token, no session': 401,
    })
    expect((await raw('GET', adminURL + '/admin/status', { Authorization: `Bearer ${token('ops-01')}` })).status).toBe(401)
    for (const reply of Object.values(cases)) expect(reply.headers['access-control-allow-origin']).toBeUndefined()

    // A page on another site, in the browser that holds the session.
    const attackStarted = Date.now()
    const otherSite = createServer((_, response) => {
      response.writeHead(200, { 'Content-Type': 'text/html' })
      response.end('<!doctype html><title>Another site</title>')
    })
    await new Promise<void>((done) => otherSite.listen(9999, '127.0.0.3', done))
    const attacker = await adminContext.newPage()
    await attacker.goto('http://127.0.0.3:9999/')
    const read = await attacker.evaluate(
      (url) =>
        fetch(url + '/admin/status', { credentials: 'include' }).then(
          (r) => `read ${r.status}`,
          (e: unknown) => `blocked: ${String(e)}`,
        ),
      adminURL,
    )
    expect(read).toMatch(/^blocked/)
    await attacker.evaluate(
      (url) =>
        fetch(url + '/admin/provisional/rollback', {
          method: 'POST',
          mode: 'no-cors',
          credentials: 'include',
          headers: { 'Content-Type': 'text/plain' },
          body: '{"revision":1}',
        }).catch(() => undefined),
      adminURL,
    )
    await attacker.close()
    await new Promise((done) => otherSite.close(done))

    expect(await status()).toEqual(before)
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([])
    const refused = auditEvents().filter((e) => (e.kind === 'admin.rejected' || e.kind === 'admin.denied') && typeof e.path === 'string')
    const reasons = (path: string) => refused.filter((e) => e.path === path).map((e) => e.reason)
    expect(reasons('/admin/blocked-accounts/cust-002')).toEqual(expect.arrayContaining(['bad_origin', 'csrf', 'bad_host', 'no_session']))
    // The session cookie is SameSite=Strict, so the other site's requests
    // arrived without it. Refusals are written to the audit file shortly after
    // the answer.
    const fromAttacker = (path: string) =>
      auditEvents()
        .filter((e) => e.path === path && e.kind !== 'admin.read' && Date.parse(String(e.time)) >= attackStarted)
        .map((e) => `${e.kind} ${e.reason}`)
    await expect.poll(() => fromAttacker('/admin/provisional/rollback')).toEqual(['admin.rejected no_session'])
    await expect.poll(() => fromAttacker('/admin/status')).toEqual(['admin.rejected no_session'])
    record('admin-api-refusals', { statuses: Object.fromEntries(Object.entries(cases).map(([k, v]) => [k, v.status])), read, reasons: reasons('/admin/blocked-accounts/cust-002') })
  })

  test('a failing security event log is shown as such, nothing is admitted, and a restart recovers', async () => {
    const failure = 'The security log cannot be written, so every protected request is refused.'
    const main = admin.getByRole('main')
    const startedAt = Date.now()

    // The gateway starts normally; the first admission record it writes is
    // cut off part-way, as when a disk fails.
    await demo.stop()
    await demo.start({ fault: 'audit-partial' })
    await admin.reload()
    await signIn(admin, 'admin', password)
    await go(admin, 'Overview')
    expect(await status()).toMatchObject({ state: 'normal', admission: 'ready', audit_failed: false })
    await expect(main.getByText('Accepting Requests', { exact: true })).toBeVisible()
    await expect(admin.getByText(failure)).toHaveCount(0)

    const refused = await call('orders', 'GET', '/orders', 'ops-01')
    expect(refused.status).toBe(503)
    expect(refused.receipts).toHaveLength(0)
    const again = await call('orders', 'GET', '/orders/detail/ORD-1001', 'cust-001')
    expect(again.status).toBe(503)
    expect(again.receipts).toHaveLength(0)

    // The dashboard shows the failure from the server's status, after a read.
    expect(await status()).toMatchObject({ audit_failed: true, admission: 'audit_failed' })
    await admin.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(main.getByRole('alert').filter({ hasText: failure })).toBeVisible()
    await expect(main.getByText('Not Accepting Requests', { exact: true })).toBeVisible()
    await expect(moreDetail(main.getByRole('alert').filter({ hasText: 'The gateway is not accepting requests' }), 'Admission value')).toHaveText('audit_failed')
    await shot(admin, '32-audit-failed')
    // Every other page says so across its top.
    await go(admin, 'Changes')
    await expect(admin.getByRole('status').filter({ hasText: 'The gateway is not accepting requests.' })).toContainText(failure)
    await go(admin, 'Overview')

    // Restarting with a working log recovers: the incomplete record is
    // terminated and named, and requests are admitted again.
    await demo.stop()
    await demo.start()
    await admin.reload()
    await signIn(admin, 'admin', password)
    await go(admin, 'Overview')
    expect(await status()).toMatchObject({ state: 'normal', admission: 'ready', audit_failed: false })
    await expect(admin.getByText(failure)).toHaveCount(0)
    const admitted = await call('orders', 'GET', '/orders', 'ops-01')
    expect(admitted.status).toBe(200)
    expect(admitted.receipts).toHaveLength(1)
    await expect.poll(() => auditEvents().filter((e) => e.kind === 'audit.recovered' && Date.parse(String(e.time)) >= startedAt).length).toBe(1)
    record('audit-failure', { refused: [refused.status, again.status], recovered: admitted.receipts[0] })
  })

  test('a change committed while its audit record cannot be written is shown as applied with a problem, never as refused', async () => {
    const subject = 'cust-005'
    // The gateway commits a block but cannot write its outcome record.
    await demo.stop()
    await demo.start({ fault: 'audit-fail-published' })
    await admin.reload()
    await signIn(admin, 'admin', password)
    await go(admin, 'Accounts')
    await admin.getByRole('main').getByRole('button', { name: 'Block an Account' }).click()
    await admin.getByLabel('Account ID').fill(subject)
    await admin.getByLabel('Reason').fill('audit pending test')
    await admin.getByRole('button', { name: 'Block Now' }).click()
    const notice = admin.getByRole('alert').filter({ hasText: 'outcome_audit_pending' })
    await expect(notice).toHaveClass(/tone-pending/)
    await expect(notice).toContainText('Applied, but the security log could not record it')
    await expect(notice).toContainText('The change is active. The gateway stops accepting requests until it restarts and records it.')
    await expect(moreDetail(notice, 'HTTP status')).toHaveText('503')
    await expect(moreDetail(notice, 'Error code')).toHaveText('outcome_audit_pending')
    await expect(admin.locator('.notice.tone-refused')).toHaveCount(0)
    const blocked = await adminCall('GET', '/admin/blocked-accounts')
    expect((blocked.body.items as { subject: string }[]).map((b) => b.subject)).toContain(subject)
    await shot(admin, '44-applied-audit-pending')

    // A normal restart records the missing outcome; the block is removed
    // again for the tests that follow.
    await demo.stop()
    await demo.start()
    await admin.reload()
    await signIn(admin, 'admin', password)
    expect(await status()).toMatchObject({ state: 'normal', admission: 'ready', audit_failed: false })
    expect((await adminCall('DELETE', `/admin/blocked-accounts/${subject}`, undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
    record('audit-pending-shown-applied', { subject, status: blocked.status })
  })

  test('a draft saved while the gateway cannot read it back is shown as applied with a problem, never as refused', async () => {
    // The gateway stores the draft but cannot read its record for the answer.
    await demo.stop()
    await demo.start({ fault: 'candidate-read-unavailable' })
    await admin.reload()
    await signIn(admin, 'admin', password)
    const drafts = async () => ((await adminCall('GET', '/admin/candidates?limit=100')).body.items as { id: number }[]).map((c) => c.id)
    const before = await drafts()
    const policy = (await adminCall('GET', '/admin/policy')).body.document
    await go(admin, 'Changes')
    await advanced(admin)
    await admin.getByLabel('Complete Policy Document (JSON)').fill(JSON.stringify(policy))
    await admin.getByRole('button', { name: 'Save Draft and Check' }).click()
    const notice = admin.getByRole('alert').filter({ hasText: 'Applied, but the answer reports a problem' })
    await expect(notice).toHaveClass(/tone-pending/)
    await expect(moreDetail(notice, 'HTTP status')).toHaveText('503')
    await expect(moreDetail(notice, 'Error code')).toHaveText('STORE_ERROR')
    await expect(admin.locator('.notice.tone-refused')).toHaveCount(0)
    const saved = (await drafts()).filter((id) => !before.includes(id))
    expect(saved).toHaveLength(1)

    // A normal restart; the draft is withdrawn again for the tests that follow.
    await demo.stop()
    await demo.start()
    await admin.reload()
    await signIn(admin, 'admin', password)
    expect((await adminCall('DELETE', `/admin/candidates/${saved[0]}`, undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
    record('draft-read-failure-shown-applied', { candidate: saved[0] })
  })

  test('a restore from a backup is shown as recovery required, and a deny-all recovery base established in the dashboard makes the gateway ready again', async () => {
    const before = await status()
    expect(before).toMatchObject({ state: 'normal', admission: 'ready' })

    // The operator backs the database up and restores it with the gateway
    // stopped, then issues a new enrolment: a restore quarantines every
    // earlier administrator.
    await demo.stop()
    const program = join(demo.bin, process.platform === 'win32' ? 'gateway.exe' : 'gateway')
    const backup = join(dirname(demo.dir), 'recovery-backup.db')
    for (const args of [
      ['backup', '--config', demo.path('gateway.json'), '--output', backup],
      ['restore', '--config', demo.path('gateway.json'), '--backup', backup],
    ]) {
      const result = spawnSync(program, args, { encoding: 'utf8' })
      expect(result.status, `${args[0]}: ${result.stderr}`).toBe(0)
    }
    const secret = await demo.enrol('recovery', 'read,read_history,manage_subjects,manage_policy')
    handedOut.push(secret)

    await admin.reload()
    await expect(admin.locator('.entry-state')).toContainText('Gateway: Restored from a backup, waiting for a starting policy')
    await expect(admin.locator('.entry-state code')).toHaveText('recovery_required')
    // The earlier administrator no longer gets in.
    await admin.getByLabel('Administrator Name').fill('admin')
    await admin.getByLabel('Password', { exact: true }).fill(password)
    await admin.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(admin.getByRole('alert').filter({ has: admin.locator('.more-details dd code', { hasText: /^401$/ }) })).toHaveClass(/tone-refused/)
    await enrol(admin, 'recovery', secret, password)
    await signIn(admin, 'recovery', password)

    // The overview says what state this is and how to leave it, and shows no
    // active policy.
    await go(admin, 'Overview')
    const main = admin.getByRole('main')
    await expect(main.getByText('Not Accepting Requests', { exact: true })).toBeVisible()
    await expect(moreDetail(main.getByRole('alert').filter({ hasText: 'The gateway is not accepting requests' }), 'Admission value')).toHaveText('recovery_required')
    await expect(main.getByText('Restored from a backup: every request is refused until an administrator establishes a starting policy after the restore (Changes).')).toBeVisible()
    const restoreCard = admin.getByRole('region', { name: 'Establish a starting policy after the restore' })
    await expect(restoreCard).toBeVisible()
    await expect(strip(admin)).toContainText('None yet')
    await shot(admin, '32b-overview-recovery')
    expect(await status()).toMatchObject({ state: 'recovery_required', admission: 'recovery_required' })
    const locked = await call('orders', 'GET', '/orders', 'ops-01')
    expect(locked.status).toBe(503)
    expect(locked.receipts).toHaveLength(0)

    // The recovery base is a confirmed policy that denies everything.
    await restoreCard.getByRole('button', { name: 'Go to Changes' }).click()
    await expect(admin.getByRole('heading', { level: 1, name: 'Changes' })).toBeVisible()
    await stage(admin, { file: demo.path('deny-all.json') })
    await admin.getByRole('button', { name: 'Establish as the Starting Policy After a Restore' }).click()
    await expect(admin.getByRole('dialog')).toContainText('becomes the starting policy after a restore, active at once')
    await expect(dialogButton(admin, 'Establish')).toBeDisabled()
    await admin.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
    await dialogButton(admin, 'Establish').click()
    const established = admin.getByText(/^Established: version \d+ is the active policy\.$/)
    await expect(established).toBeVisible()
    const after = await status()
    expect(after).toMatchObject({ state: 'normal', admission: 'ready' })
    await go(admin, 'Overview')
    await expect(admin.getByRole('region', { name: 'Establish a starting policy after the restore' })).toHaveCount(0)
    await expect(strip(admin)).toContainText(`Version ${after.confirmed}`)
    const denied = await call('orders', 'GET', '/orders', 'ops-01')
    expect(denied.status).toBe(403)
    expect(denied.receipts).toHaveLength(0)

    // The history names both steps.
    await go(admin, 'History')
    const loadAll = admin.getByRole('button', { name: 'Load All' })
    if (await loadAll.isVisible()) await loadAll.click()
    await expect(admin.getByRole('region', { name: 'Administrative History' }).getByText('Restored from a Backup by the Operator')).toBeVisible()
    await expect(admin.getByRole('region', { name: 'Administrative History' }).getByText('Starting Policy After a Restore Established')).toBeVisible()
    record('recovery-base', { before, locked: locked.status, after, denied: denied.status })
  })

  test('no secret appears in browser storage, URLs, console output, built files, logs or evidence, and no content policy violation occurred', async () => {
    const scan = await expectNoSecrets()
    record('secret-scan', { secretsChecked: scan.secrets, builtFiles: scan.built, requests: adminRequests.length, consoleMessages: scan.consoleMessages, scriptDialogs })
  })
})
