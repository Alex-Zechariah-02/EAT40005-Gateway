import { randomUUID } from 'node:crypto'
import { expect, test } from '@playwright/test'
import { type Policy } from '../src/shared/api'
import { adminURL, raw, siteURL } from './env'
import { activate, createOverride, dialogButton, enrol, go, record, resolve, revokeOverride, signIn, stage } from './pages'
import { api, applyJson, baseline, demo, candidates, capture, currentPolicy, designer, expandAll, node, page, password, requests, restoreBaseline, selected, stagedDraft, open, ready, finish } from './designer'

// Designer drafts saved, put on trial and checked by the gateway.
// Each file starts its own gateway; designer.ts holds the shared steps.

test.describe.serial('Policy Designer on the native Gateway', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('granting a permission in the editor stages exactly one permit and actual activation changes enforcement', async () => {
    // The Fetch baseline gives customers no exact-stock permit.
    const token = demo.token('cust-001')
    const stock = siteURL + '/inventory/stock/ITM-501'
    const before = await demo.receipts('inventory')
    expect((await raw('GET', stock, { Authorization: `Bearer ${token}` })).status).toBe(403)
    expect((await demo.receipts('inventory')).length).toBe(before.length)
    await node('role:CUSTOMER').click()
    await selected().getByRole('checkbox', { name: 'GET /inventory/stock/* (inventory.stock.read)', exact: true }).check()
    await page.getByRole('button', { name: /^Inventory API Endpoints \d/ }).click()
    await expect(node('operation:inventory.stock.read')).toHaveClass(/is-allowed/)
    await capture('designer-permit-granted')
    const expected: Policy = structuredClone(baseline)
    expected.permits.push({ role: 'CUSTOMER', operation: 'inventory.stock.read' })
    const candidate = await stagedDraft()
    const stored = await api('GET', `/admin/candidates/${candidate}`)
    expect(stored.status).toBe(200)
    expect(stored.body.document).toEqual(expected)
    expect((await raw('GET', stock, { Authorization: `Bearer ${token}` })).status).toBe(403)
    expect((await demo.receipts('inventory')).length).toBe(before.length)
    await page.getByRole('button', { name: 'Preview Its Impact and Put It on Trial', exact: true }).click()
    await expect(page.getByRole('heading', { name: `Change ${candidate}`, exact: true })).toBeVisible()
    await activate(page, 300)
    await resolve(page, 'confirm')
    // The gateway now forwards the request to Inventory exactly once, and
    // Inventory, which leaves role decisions to the gateway, answers it.
    expect((await raw('GET', stock, { Authorization: `Bearer ${token}` })).status).toBe(200)
    const receipts = (await demo.receipts('inventory')).slice(before.length)
    expect(receipts).toHaveLength(1)
    expect(receipts[0]).toMatchObject({ subject: 'cust-001', target: '/inventory/stock/ITM-501', method: 'GET', status: 200 })
    // The staged draft now equals the new confirmed policy, so it follows it
    // instead of offering the permit again as an unstaged change.
    await designer()
    await expect(page.getByText(`No changes to version ${(await currentPolicy()).id}`, { exact: true })).toBeVisible()
    record('designer-permit-grant', {
      candidate,
      receipts: receipts.map(({ method, target, status, subject }) => ({ method, target, status, subject })),
    })
    await restoreBaseline()
  })

  test('another session causes a real 412 and rebasing preserves the draft without merging', async ({ browser }) => {
    const mine = structuredClone(baseline)
    mine.operations.find((operation) => operation.id === 'reports.summary')!.rate!.count = 2
    await applyJson(mine)
    const second = await browser.newContext()
    const secondPage = await second.newPage()
    await secondPage.goto(adminURL + '/')
    await signIn(secondPage, 'admin', password)
    const other = structuredClone(baseline)
    other.operations.find((operation) => operation.id === 'orders.all')!.access = 'deny'
    await stage(secondPage, { text: JSON.stringify(other) })
    await activate(secondPage, 300)
    await resolve(secondPage, 'confirm')
    const newRevision = (await currentPolicy()).id
    await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
    await dialogButton(page, 'Save Draft Change').click()
    await expect(page.getByText(`The active policy changed to version ${newRevision} since your draft was started from version`)).toBeVisible()
    await page.getByRole('button', { name: 'Show What Changed', exact: true }).click()
    await expect(page.getByText('endpoints: changed orders.all')).toBeVisible()
    expect(JSON.parse(await page.getByLabel('Complete Policy Document (JSON)').inputValue())).toEqual(mine)
    await capture('designer-stale-revision')
    await page.getByRole('button', { name: `Keep my draft and base it on r${newRevision}`, exact: true }).click()
    const candidate = await stagedDraft()
    expect((await api('GET', `/admin/candidates/${candidate}`)).body.document).toEqual(mine)
    await second.close()
    await restoreBaseline()
    record('designer-stale-revision', { status: 412, newRevision, candidate, draftKept: true })
  })

  test('live overrides and provisional markers refresh only on explicit reads and actual deadlines', async () => {
    const override = await createOverride(page, {
      action: 'Allow Temporarily: Let a Role Call an Endpoint',
      operation: 'inventory.stock.read',
      role: 'CUSTOMER',
      seconds: 120,
    })
    await designer()
    await expandAll()
    await expect(node('operation:inventory.stock.read')).toContainText('CUSTOMER allowed until')
    await capture('designer-temporary-permit')
    // Only the gateway's status is reread each second; temporary rules and
    // trials are read again only when the status says they changed.
    const policyReads = () => requests.filter((request) => /GET .*\/admin\//.test(request) && !/\/admin\/status\b/.test(request)).length
    const readCount = policyReads()
    await page.waitForTimeout(2_500)
    expect(policyReads()).toBe(readCount)
    const proposed = structuredClone(baseline)
    proposed.operations.find((operation) => operation.id === 'reports.summary')!.rate!.count = 2
    await stage(page, { text: JSON.stringify(proposed) })
    const revision = await activate(page, 300)
    await designer()
    await page.getByLabel('Viewing').selectOption('confirmed')
    await expandAll()
    await expect(node('operation:reports.summary')).toContainText(`Changed in the trial of version ${revision}`)
    await capture('designer-provisional-markers')
    await resolve(page, 'rollback')
    await revokeOverride(page, override)
    await designer()
    await expandAll()
    await expect(page.getByText(/CUSTOMER allowed until/)).toHaveCount(0)
    await expect(page.getByText(`Changed in the trial of version ${revision}`)).toHaveCount(0)
    record('designer-live-markers', { override, revision, noPolling: true, removedOnRevocationAndRollback: true })
  })

  test('trace explains the document with no requests and stops at the first refusal', async () => {
    // Apart from the status read each second, tracing sends nothing.
    const sent = () => requests.filter((request) => !/\/admin\/status\b/.test(request)).length
    const start = sent()
    await page.getByRole('button', { name: 'Trace', exact: true }).click()
    await page.getByLabel('What', { exact: true }).selectOption('orders.all')
    await page.getByLabel('Who', { exact: true }).selectOption('role:CUSTOMER')
    const trace = page.getByRole('complementary', { name: 'Trace policy document' })
    await expect(trace).toContainText('403: no permission')
    await expect(trace.locator('li').filter({ hasText: 'Blocked List' })).toContainText('403 account_blocked')
    await expect(trace.locator('li').last()).toContainText('Not Reached')
    await capture('designer-trace-refused')
    await page.getByLabel('Who', { exact: true }).selectOption('role:SUPPORT')
    await expect(trace.locator('li').last()).toContainText(`127.0.0.1:${9502 + Number(process.env.EAT_PORT_OFFSET ?? 0)}`)
    await capture('designer-trace-permitted')
    await page.getByLabel('What', { exact: true }).selectOption('inventory.categories')
    await page.getByLabel('Who', { exact: true }).selectOption('none')
    await expect(trace.locator('li').filter({ hasText: 'Sign-In' })).toContainText('Not Applicable')
    await expect(trace).toContainText('Public endpoint: no token is read.')
    await capture('designer-trace-public')
    expect(sent()).toBe(start)
    await page.getByRole('button', { name: 'Trace', exact: true }).click()
    record('designer-trace', { requests: 0, cases: ['no permit', 'forward', 'public'] })
  })

  test('a template only prepares the draft; the gateway checks it like any other change when it is saved', async () => {
    await designer()
    await page.getByText('Templates', { exact: true }).click()
    await page.getByRole('button', { name: 'Apply: limit sign-in attempts', exact: true }).click()
    await expect(page.getByText('Template applied to the draft: accounts.login limited to 5 requests per 60 s per client address.', { exact: false })).toBeVisible()
    // Nothing was sent: the draft only changed in this page.
    await expect(page.getByText(/^The gateway checked it/)).toHaveCount(0)
    const expected: Policy = structuredClone(baseline)
    expected.operations.find((operation) => operation.id === 'accounts.login')!.rate = { algorithm: 'sliding-window', count: 5, window_seconds: 60, key: 'client_address' }
    const candidate = await stagedDraft()
    const stored = await api('GET', `/admin/candidates/${candidate}`)
    expect(stored.body.document).toEqual(expected)
    // A template that cannot apply says why and leaves the draft as it is.
    await page.getByText('Templates', { exact: true }).click()
    await page.getByLabel('Endpoint', { exact: true }).selectOption('orders.create')
    await page.getByLabel('Requests per Window', { exact: true }).fill('100000')
    await page.getByRole('button', { name: 'Apply: tighten the limit', exact: true }).click()
    await expect(page.getByText(/already allows \d+ per window; enter a lower number\.$/)).toBeVisible()
    await capture('designer-template-applied')
    await page.getByRole('button', { name: 'Discard', exact: true }).click()
    await dialogButton(page, 'Discard Draft').click()
  })

  test('a step-by-step wizard ends on a review and only edits the draft; the gateway checks it when it is saved', async () => {
    await designer()
    // Guided changes open from a group's + menu on the canvas.
    await page.getByRole('button', { name: 'Add to Roles', exact: true }).click()
    await page.getByRole('menuitem', { name: 'Give a Role Access', exact: true }).click()
    const wizard = page.getByRole('dialog', { name: 'Give a Role Access' })
    await wizard.getByLabel('Role', { exact: true }).selectOption('CUSTOMER')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    // Each step's heading takes focus; Next pressed early stays on the step and says what to do.
    await expect(wizard.getByRole('heading', { name: 'Step 2 of 3: Choose Endpoints' })).toBeFocused()
    const early = wizard.getByRole('button', { name: 'Next', exact: true })
    await early.click()
    await expect(wizard.getByRole('heading', { name: 'Step 2 of 3: Choose Endpoints' })).toBeVisible()
    await expect(wizard.getByRole('alert')).toHaveText('Choose at least one endpoint CUSTOMER cannot call yet.')
    await expect(early).toHaveAccessibleDescription('Choose at least one endpoint CUSTOMER cannot call yet.')
    await wizard.getByRole('checkbox', { name: /^reports\.summary \(/ }).check()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    const added = wizard.getByRole('region', { name: 'Added (1)' })
    await expect(added).toContainText('CUSTOMER can now call reports.summary')
    await expect(wizard.getByRole('region', { name: /^(Changed|Removed)/ })).toHaveCount(0)
    await capture('designer-wizard-review')
    await wizard.getByRole('button', { name: 'Apply to the Draft', exact: true }).click()
    await expect(wizard).toHaveCount(0)
    await expect(page.getByText('Applied to the draft: CUSTOMER can now call 1 more endpoint.', { exact: false })).toBeVisible()
    // Nothing was sent: the draft only changed in this page.
    await expect(page.getByText(/^The gateway checked it/)).toHaveCount(0)
    const candidate = await stagedDraft()
    const stored = await api('GET', `/admin/candidates/${candidate}`)
    expect(stored.body.document.permits).toHaveLength(baseline.permits.length + 1)
    expect(stored.body.document.permits).toContainEqual({ role: 'CUSTOMER', operation: 'reports.summary' })
    // A rate limit left as it is cannot be applied, and Cancel changes nothing.
    await page.getByRole('button', { name: 'Add to Orders API Endpoints', exact: true }).click()
    await page.getByRole('menuitem', { name: 'Change a Rate Limit', exact: true }).click()
    const rate = page.getByRole('dialog', { name: 'Change a Rate Limit' })
    // Opened from the Orders group, it offers the Orders endpoints only.
    await expect(rate.getByLabel('Endpoint', { exact: true }).locator('option', { hasText: 'inventory.' })).toHaveCount(0)
    await rate.getByLabel('Endpoint', { exact: true }).selectOption('orders.create')
    await rate.getByRole('button', { name: 'Next', exact: true }).click()
    await rate.getByRole('button', { name: 'Next', exact: true }).click()
    await expect(rate.getByText('This is already the limit in the draft. Go back and choose another.')).toBeVisible()
    const unchanged = rate.getByRole('button', { name: 'Apply to the Draft', exact: true })
    await unchanged.click()
    await expect(rate.getByRole('alert')).toHaveText('This is already the limit in the draft. Go back and choose another.')
    await expect(unchanged).toHaveAccessibleDescription('This is already the limit in the draft. Go back and choose another.')
    await expect(rate).toBeVisible()
    await expect(page.getByText(/orders\.create limited to/)).toHaveCount(0)
    await rate.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(rate).toHaveCount(0)
    await page.getByRole('button', { name: 'Discard', exact: true }).click()
    await dialogButton(page, 'Discard Draft').click()
  })

  test('Add Endpoint names what to fix beside each field and adds the endpoint to the draft only', async () => {
    await designer()
    await page.getByRole('button', { name: 'Add to Reports API Endpoints', exact: true }).click()
    await page.getByRole('menuitem', { name: 'Add Endpoint', exact: true }).click()
    const wizard = page.getByRole('dialog', { name: 'Add an Endpoint' })
    await expect(wizard.getByLabel('API', { exact: true })).toHaveValue('reports')
    const next = wizard.getByRole('button', { name: 'Next', exact: true })
    const path = wizard.getByLabel('Path', { exact: true })
    const id = wizard.getByLabel('Endpoint ID', { exact: true })
    await next.click()
    await expect(path).toHaveAttribute('aria-invalid', 'true')
    await expect(path).toBeFocused()
    await expect(path).toHaveAccessibleDescription('Enter a path starting with /.')
    // The suggested ID follows the path's last fixed part; an ID already in the draft is named.
    await path.fill('/reports/daily/*')
    await expect(id).toHaveValue('reports.daily')
    await id.fill('reports.summary')
    await expect(id).toHaveAccessibleDescription('This draft already has an endpoint with this ID.')
    await id.fill('reports.daily')
    await next.click()
    await wizard.getByLabel('OPERATIONS', { exact: true }).check()
    await next.click()
    await wizard.getByLabel('Requests', { exact: true }).fill('15')
    await next.click()
    await expect(wizard.locator('.change-set li')).toHaveText(['Endpoint reports.daily added', 'OPERATIONS can now call reports.daily'])
    await wizard.getByRole('button', { name: 'Apply to the Draft', exact: true }).click()
    await expect(page.getByText('Applied to the draft: Endpoint reports.daily added (GET /reports/daily/*).', { exact: false })).toBeVisible()
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    const draft: Policy = JSON.parse(await page.getByLabel('Complete Policy Document (JSON)', { exact: true }).inputValue())
    const added = { id: 'reports.daily', api: 'reports', method: 'GET', target: '/reports/daily/*', access: 'allow', auth: 'required' }
    const rate = { algorithm: 'sliding-window', count: 15, window_seconds: 60, key: 'subject' }
    expect(draft.operations.find((operation) => operation.id === 'reports.daily')).toEqual({ ...added, rate })
    expect(draft.permits).toContainEqual({ role: 'OPERATIONS', operation: 'reports.daily' })
    expect((await currentPolicy()).document.operations.some((operation) => operation.id === 'reports.daily')).toBe(false)
    await page.getByRole('button', { name: 'Discard', exact: true }).click()
    await dialogButton(page, 'Discard Draft').click()
  })

  test('a read-only administrator has no editing controls and a crafted staging request is refused', async ({ browser }) => {
    const secret = await demo.enrol('reader', 'read')
    const reader = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    const readerPage = await reader.newPage()
    await readerPage.goto(adminURL + '/')
    await enrol(readerPage, 'reader', secret, password)
    await signIn(readerPage, 'reader', password)
    await go(readerPage, 'Policy')
    await expect(readerPage.getByText(/Read-only. Editing needs/)).toBeVisible()
    await expect(readerPage.getByRole('button', { name: 'Save Changes', exact: true })).toHaveCount(0)
    await node('role:CUSTOMER', readerPage).click()
    await expect(selected(readerPage).getByRole('checkbox').first()).toBeDisabled()
    await capture('designer-read-only-canvas', readerPage)
    await readerPage.getByRole('tab', { name: 'Form', exact: true }).click()
    await expect(readerPage.getByRole('button', { name: /^Add / })).toHaveCount(0)
    await capture('designer-read-only-form', readerPage)
    await readerPage.getByRole('tab', { name: 'JSON', exact: true }).click()
    expect(await readerPage.getByLabel('Complete Policy Document (JSON)').isEditable()).toBe(false)
    const confirmed = await api('GET', '/admin/policy', undefined, {}, reader)
    const candidateCount = (await candidates(reader)).length
    const refused = await api(
      'POST',
      '/admin/candidates',
      { policy: baseline },
      { 'If-Match': confirmed.etag!, 'Idempotency-Key': randomUUID() },
      reader,
    )
    expect(refused.status).toBe(403)
    expect(refused.body.error).toBe('forbidden')
    expect(await candidates(reader)).toHaveLength(candidateCount)
    await capture('designer-read-only-json', readerPage)
    await reader.close()
    record('designer-read-only', { status: refused.status, editingControls: 0 })
  })
})
