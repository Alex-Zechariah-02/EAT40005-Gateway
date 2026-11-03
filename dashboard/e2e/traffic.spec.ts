import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { adminURL, build, Demo, raw, siteURL, webRoot } from './env'
import { dialogButton, enrol, go, record, shot, signIn } from './pages'
import { admin, adminCall, adjust, finish, listAll, open, password, propose, ready, sales, status, token } from './setup'

// Live Traffic, the impact preview, export, Security Log, Access Checker, Limits and alerts.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('live traffic shows each request as it happens and holds new ones while paused; impact, key reload and export show what the gateway reports', async () => {
    const before = await status()
    const main = admin.getByRole('main')
    await go(admin, 'Live Traffic')
    const stream = main.getByRole('region', { name: /^Request Stream/ })
    await expect(stream.getByRole('status')).toHaveText('Live')
    const row = (ref: string) => stream.getByRole('row').filter({ hasText: ref.slice(0, 8) })

    // A request appears as the gateway decides it.
    const allowed = await sales('ops-01')
    expect(allowed.status).toBe(200)
    const allowedRef = allowed.receipts[0].request_id
    await expect(row(allowedRef)).toContainText('ops-01')
    await expect(row(allowedRef)).toContainText('All Passed')

    // While paused a new request is counted, not shown; Resume adds it.
    await stream.getByRole('button', { name: 'Pause' }).click()
    await expect(stream.getByRole('status')).toHaveText('Paused')
    const refusedRead = await sales('cust-001')
    expect(refusedRead.status).toBe(403)
    expect(refusedRead.receipts).toHaveLength(0)
    const [refusedEvent] = (await adminCall('GET', '/admin/events?subject=cust-001&limit=1')).body.items
    await expect(stream.getByRole('button', { name: 'Resume (1 new)' })).toBeVisible()
    await expect(row(refusedEvent.request_id)).toHaveCount(0)
    await stream.getByRole('button', { name: 'Resume (1 new)' }).click()
    await expect(row(refusedEvent.request_id)).toContainText('Refused · 403')
    await row(refusedEvent.request_id).getByRole('link', { name: `Open request ${refusedEvent.request_id.slice(0, 8)}`, exact: true }).click()
    await expect(admin.getByRole('dialog')).toContainText('No Permission')
    await admin.keyboard.press('Escape')
    await stream.getByRole('button', { name: 'Refused Only' }).click()
    await expect(row(allowedRef)).toHaveCount(0)
    await expect(row(refusedEvent.request_id)).toBeVisible()
    // Changes only hides reads: an attempted stock change stays, both sales
    // reads go.
    const refusedWrite = await adjust('cust-001', 1)
    expect(refusedWrite.status).toBe(403)
    expect(refusedWrite.receipts).toHaveLength(0)
    const [changeEvent] = (await adminCall('GET', '/admin/events?subject=cust-001&limit=1')).body.items
    await stream.getByRole('button', { name: 'Changes Only' }).click()
    await expect(stream.getByRole('button', { name: 'Changes Only' })).toHaveAttribute('aria-pressed', 'true')
    await expect(row(changeEvent.request_id)).toContainText('PATCH')
    await expect(row(allowedRef)).toHaveCount(0)
    await expect(row(refusedEvent.request_id)).toHaveCount(0)
    await stream.getByRole('button', { name: 'All', exact: true }).click()
    await shot(admin, '23-live-traffic')

    // The impact preview replays recent requests against a staged change and
    // shows what the gateway itself reports; nothing changes.
    const id = await propose(admin, { role: 'CUSTOMER', operation: 'reports.sales' })
    await main.getByRole('button', { name: 'Next: Preview Impact', exact: true }).click()
    const impact = main.getByRole('region', { name: 'Impact on Recent Traffic' })
    await impact.getByRole('button', { name: 'Preview Impact' }).click()
    await expect(impact.getByText(/^Replayed \d+ of \d+ requests/)).toBeVisible()
    const direct = (await adminCall('POST', `/admin/candidates/${id}/impact`)).body as {
      window: { requests: number; previewed: number; identity_unknown: number }
      flips: { operation: string; subject: string | null; before: string; after: string; count: number }[]
    }
    expect(direct.window.requests).toBe(direct.window.previewed + direct.window.identity_unknown)
    const flip = direct.flips.find((f) => f.operation === 'reports.sales' && f.subject === 'cust-001')
    expect(flip).toMatchObject({ before: 'refused', after: 'allowed' })
    const flipRow = impact.getByRole('row').filter({ hasText: 'reports.sales' }).filter({ hasText: 'cust-001' })
    await expect(flipRow).toContainText('Refused')
    await expect(flipRow).toContainText('Allowed')
    await expect(flipRow.getByRole('cell').last()).toHaveText(String(flip!.count))
    await shot(admin, '24-impact-preview')
    await admin.getByRole('button', { name: 'Withdraw', exact: true }).click()
    await dialogButton(admin, 'Withdraw Change').click()
    await expect(admin.getByText(`Withdrawn: change ${id} can no longer be put on trial.`)).toBeVisible()
    expect(await status()).toEqual(before)

    // Reloading the signing keys reads them again from the operator settings;
    // tokens keep working.
    await go(admin, 'Team')
    const keys = (await adminCall('GET', '/admin/keys')).body.items as unknown[]
    await admin.getByRole('button', { name: 'Reload Keys from Settings' }).click()
    await expect(admin.getByText(`Reloaded: ${keys.length} key${keys.length === 1 ? '' : 's'} trusted.`)).toBeVisible()
    // The settings were not changed, so the reload reports the same keys by
    // ID; no key material is shown.
    await expect(admin.getByText('The same keys as before: nothing changed.')).toBeVisible()
    expect((await listAll<{ kind: string }>('/admin/history')).filter((h) => h.kind === 'keys.reload')).toHaveLength(1)
    expect((await sales('ops-01')).status).toBe(200)

    // The Policy page's JSON view shows and downloads exactly the active policy.
    const policy = (await adminCall('GET', '/admin/policy')).body as { id: number; document: unknown }
    await go(admin, 'Policy')
    await admin.getByLabel('Viewing').selectOption('confirmed')
    await admin.getByRole('tab', { name: 'JSON' }).click()
    const code = main.getByLabel('Complete Policy Document (JSON)')
    expect(JSON.parse(await code.inputValue())).toEqual(policy.document)
    const [download] = await Promise.all([admin.waitForEvent('download'), main.getByRole('link', { name: 'Download JSON' }).click()])
    expect(download.suggestedFilename()).toBe(`gateway-policy-r${policy.id}.json`)
    expect(JSON.parse(readFileSync((await download.path())!, 'utf8'))).toEqual(policy.document)
    // Scripts are shown the product's own Admin API, not a repository tool.
    await main.getByText('Save a Draft Change with the Admin API').click()
    const requests = main.getByLabel('Admin API requests')
    await expect(requests).toContainText('POST /admin/candidates')
    await expect(requests).toContainText('If-Match: "r<active version>"')
    await expect(main).not.toContainText('go run')
    // The sidebar names the gateway by its admin address once widened; the
    // narrow strip, the default, keeps it for screen readers only.
    await expect(admin.getByText(new URL(adminURL).host, { exact: true })).toBeAttached()
    await admin.getByRole('button', { name: 'Expand Sidebar' }).click()
    await expect(admin.getByText(new URL(adminURL).host, { exact: true })).toBeVisible()
    await admin.getByRole('button', { name: 'Collapse Sidebar' }).click()
    record('monitor-pages', { allowedRef, refusedRef: refusedEvent.request_id, impactWindow: direct.window, flip, keys: keys.length, exported: policy.id })
  })

  test('the security log finds a request in the log files, the access checker answers with the gateway, limits show their use and keys show their use', async () => {
    const refused = await raw('GET', siteURL + '/reports/summary', { Authorization: `Bearer ${token('cust-001')}` })
    expect(refused.status).toBe(403)
    const ref = String(refused.headers['x-request-id'])

    await go(admin, 'Security Log')
    await admin.getByLabel('Reference (Ref)').fill(ref)
    // The log writer is asynchronous: search until the record is there.
    await expect(async () => {
      await admin.getByRole('button', { name: 'Search', exact: true }).click()
      await expect(admin.getByRole('heading', { name: '1 request', exact: true })).toBeVisible({ timeout: 1000 })
    }).toPass()
    await expect(admin.getByText(/This is everything the gateway still holds\./)).toBeVisible()
    await expect(admin.getByRole('link', { name: 'Export CSV' })).toHaveAttribute('href', new RegExp(`ref=${ref}.*format=csv`))
    await shot(admin, '44-security-log')

    await go(admin, 'Access Checker')
    await admin.getByLabel('Role', { exact: true }).selectOption('CUSTOMER')
    await admin.getByLabel('Endpoint', { exact: true }).selectOption('reports.summary')
    await admin.getByRole('button', { name: 'Check Access' }).click()
    const verdict = admin.getByRole('region', { name: /^Refused/ })
    await expect(verdict).toContainText('None of these roles has permission for the endpoint.')
    await admin.getByLabel('Role', { exact: true }).selectOption('OPERATIONS')
    await admin.getByRole('button', { name: 'Check Access' }).click()
    const allowed = admin.getByRole('region', { name: /^Allowed/ })
    await expect(allowed).toContainText('These were not checked.')
    await shot(admin, '45-access-checker')

    await go(admin, 'Limits')
    await expect(admin.getByRole('row', { name: /Administrator Sessions/ })).toContainText(/\d+ of 64/)
    await expect(admin.getByRole('row', { name: /Security Log Files/ })).toContainText(/1 of \d+/)
    await shot(admin, '46-limits')

    await go(admin, 'Team')
    await expect(admin.getByRole('region', { name: 'Signing Keys' }).getByText('In Use', { exact: true }).first()).toBeVisible()

    // Advice is the gateway's fixed checks; unused permissions wait for
    // enough traffic and say why.
    await go(admin, 'Advice')
    await expect(admin.getByRole('heading', { name: 'Settings to Review' })).toBeVisible()
    await expect(admin.getByText(/Shown once the security log covers 60 minutes/)).toBeVisible()
    await expect(admin.getByText(/Checked with rule set checks-0\.2/)).toBeVisible()
    await shot(admin, '47-advice')

    // The Overview shows exactly the alerts the gateway reports. Whether a
    // refusal spike is raised depends on this suite's earlier traffic, so the
    // spike rule itself is tested in the gateway against exact traffic.
    const reported = (await adminCall('GET', '/admin/alerts')).body as { alerts: { message: string }[] }
    await go(admin, 'Overview')
    const alerts = admin.getByRole('status').filter({ hasText: /^\d+ alerts?/ })
    if (reported.alerts.length) await expect(alerts).toContainText(reported.alerts[0].message)
    else await expect(alerts).toHaveCount(0)
    // Rendering of a critical alert, from an answer in the gateway's shape.
    await admin.route('**/admin/alerts', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ rules: 'checks-0.2', evaluated_at: new Date().toISOString(), alerts: [{ id: 'A2', severity: 'critical', object: 'security_log', message: 'The security log cannot be written, so the gateway refuses requests it cannot record.' }] }),
      }),
    )
    await go(admin, 'Changes')
    await go(admin, 'Overview')
    await expect(alerts).toContainText('Critical')
    await expect(alerts).toContainText('The security log cannot be written')
    await shot(admin, '48-overview-alert')
    await admin.unroute('**/admin/alerts')
  })
})

test('a live stream reconnect discloses the gap and still receives later requests', async ({ browser }) => {
  const deployment = new Demo(build(), webRoot())
  const secret = deployment.setup()
  const context = await browser.newContext()
  let resume: () => void = () => {}
  try {
    await deployment.start()
    const page = await context.newPage()
    await page.goto(adminURL + '/')
    await enrol(page, 'admin', secret, password)
    await signIn(page, 'admin', password)
    let connections = 0
    let retryStarted: () => void = () => {}
    const retrying = new Promise<void>((resolve) => { retryStarted = resolve })
    const released = new Promise<void>((resolve) => { resume = resolve })
    // A transport-only EOF triggers native EventSource auto-reconnect. No
    // request event is fabricated; the retry goes to the real protected stream.
    await page.route('**/admin/events/stream', async (route) => {
      connections++
      if (connections === 1) {
        await route.fulfill({ status: 200, contentType: 'text/event-stream', body: 'retry: 100\n: controlled EOF\n\n' })
      } else {
        retryStarted()
        await released
        await route.continue()
      }
    })
    await go(page, 'Live Traffic')
    const stream = page.getByRole('region', { name: /^Request Stream/ })
    await expect(page.getByText(/events may have been missed/i)).toHaveCount(0)
    await retrying
    // This independent request occurs while the reconnect is held. No policy exists:
    // this request is recorded, refused and absent from every service receipt.
    const missed = await raw('GET', siteURL + '/orders')
    expect(missed.status).toBe(503)
    for (const count of Object.values(await deployment.counts())) expect(count).toBe(0)
    resume()
    await expect(stream.getByRole('status')).toHaveText('Live', { timeout: 20_000 })
    const later = await raw('GET', siteURL + '/orders')
    const laterRef = String(later.headers['x-request-id'])
    await expect(stream.getByRole('row').filter({ hasText: laterRef.slice(0, 8) })).toBeVisible()
    await expect(page.getByText(/events may have been missed/i)).toBeVisible()
    record('stream-reconnect-gap', { connections, missed: missed.headers['x-request-id'], later: laterRef })
  } finally {
    resume()
    await context.close()
    await deployment.stop()
  }
})
