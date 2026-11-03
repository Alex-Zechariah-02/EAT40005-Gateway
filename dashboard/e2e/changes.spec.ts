import { expect, test } from '@playwright/test'
import { activate, dialogButton, go, moreDetail, nav, record, resolve, revisions, shot, stage, trial, trialStep, versionsPart } from './pages'
import { admin, adminCall, admitted, demo, fact, finish, listAll, open, propose, ready, rowFor, scriptDialogs, status, summary } from './setup'

// Draft changes, trials, keep and undo, earlier versions and refused drafts.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('markup in policy documents is shown as text and never runs', async () => {
    const document = JSON.parse(demo.file('baseline.json'))
    const create = document.operations.find((o: { id: string }) => o.id === 'orders.create')
    create.body.schema.properties.payment.enum = ['<img src=x onerror=alert(2)>', 'cash', 'wallet']
    const id = await stage(admin, { text: JSON.stringify(document, null, 2) })
    await admin.getByText('Full Document').click()
    await expect(admin.locator('pre').filter({ hasText: '<img src=x onerror=alert(2)>' })).toBeVisible()
    await expect(admin.getByText('endpoints: changed orders.create')).toBeVisible()
    expect(await admin.locator('img').count()).toBe(0)
    await admin.getByRole('button', { name: 'Withdraw', exact: true }).click()
    await dialogButton(admin, 'Withdraw Change').click()
    await expect(admin.getByText(`Withdrawn: change ${id} can no longer be put on trial.`)).toBeVisible()
    expect((await adminCall('GET', `/admin/candidates/${id}`)).body.status).toBe('withdrawn')
    expect(scriptDialogs).toBe(0)
    record('markup-as-text', { candidate: id, scriptDialogs })
  })

  test('a provisional change takes effect at once and a rollback removes it', async () => {
    const before = await status()
    const denied = await summary('cust-001')
    expect(denied.status).toBe(403)
    expect(denied.receipts).toHaveLength(0)

    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    await expect(admin.getByText('permissions: added CUSTOMER → reports.summary')).toBeVisible()
    // Before anything is sent, the trial dialog says in a plain sentence what
    // the change adds, and Cancel sends nothing.
    await trialStep(admin)
    await admin.getByLabel('Trial Length (Seconds)').fill('600')
    await admin.getByRole('main').getByRole('button', { name: 'Put on Trial', exact: true }).click()
    const review = admin.getByRole('dialog')
    await expect(review.getByRole('region', { name: 'Added (1)' }).getByRole('listitem')).toHaveText(['CUSTOMER can now call reports.summary'])
    await expect(review.getByRole('region', { name: /^(Changed|Removed) / })).toHaveCount(0)
    await expect(review).toContainText('One difference from the active version.')
    await shot(admin, '10a-trial-change-set')
    await dialogButton(admin, 'Cancel').click()
    expect(await status()).toMatchObject({ confirmed: before.confirmed, revision: before.confirmed })
    const provisional = await activate(admin, 600)
    const during = await status()
    expect(during).toMatchObject({ confirmed: before.confirmed, provisional, revision: provisional })
    expect(during.generation).toMatch(new RegExp(`^r${provisional}-`))

    await go(admin, 'Changes')
    const view = (await adminCall('GET', '/admin/provisional')).body as { changes: { target: string }[]; activation: { rollback_target: number } }
    expect(view.activation.rollback_target).toBe(before.confirmed)
    expect(view.changes.length).toBeGreaterThan(0)
    await expect(trial(admin).getByRole('heading', { name: `Version ${provisional} is on trial` })).toBeVisible()
    await expect(trial(admin).getByRole('heading', { name: `What Changed from Version ${before.confirmed}` })).toBeVisible()
    await expect(trial(admin).getByRole('button', { name: 'Undo Change', exact: true })).toBeVisible()
    await expect(trial(admin)).toContainText(`returns to version ${before.confirmed} by itself`)
    await expect(trial(admin).getByText('Active Generation', { exact: true }).locator('xpath=following-sibling::dd[1]')).toHaveText(during.generation!)
    for (const change of view.changes) await expect(trial(admin).getByRole('listitem').filter({ hasText: change.target })).toBeVisible()
    await expect(trial(admin).getByRole('listitem').filter({ hasText: 'CUSTOMER:reports.summary' })).toHaveText(/^permission\s*CUSTOMER:reports.summary$/)
    await expect(trial(admin).getByText(/^\d{2}:\d{2}$/)).toBeVisible()

    const allowed = await summary('cust-001')
    admitted(allowed)
    expect(allowed.receipts[0]).toMatchObject({ subject: 'cust-001', roles: 'CUSTOMER', generation: during.generation })

    await admin.getByRole('main').getByRole('button', { name: 'Undo Change', exact: true }).click()
    await expect(admin.getByRole('dialog')).toContainText(`Version ${before.confirmed} is active again from the next request`)
    await shot(admin, '11-rollback-dialog')
    await dialogButton(admin, 'Undo Change').click()
    await expect(admin.getByText(`Undone: version ${before.confirmed} is active again.`)).toBeVisible()
    const after = await status()
    expect(after).toMatchObject({ confirmed: before.confirmed, revision: before.confirmed, generation: before.generation })
    expect(after.provisional).toBeUndefined()
    const again = await summary('cust-001')
    expect(again.status).toBe(403)
    expect(again.receipts).toHaveLength(0)
    // The timeline names the version the undo brought back, not the one undone.
    await go(admin, 'Overview')
    const newest = admin.getByRole('main').getByRole('region', { name: 'Policy Timeline' }).getByRole('listitem').first()
    await expect(newest).toContainText(`Version ${before.confirmed}`)
    await expect(newest).toContainText('Active Again After an Undo')
    record('cpa-rollback', { before, during, changes: view.changes, receipt: allowed.receipts[0], after })
  })

  test('a confirmed change stays active, and the baseline is restored the same way', async () => {
    const before = await status()
    // The change also gives orders.create an optional list of lists that may
    // be null, so the confirmed policy's request rules must show every level.
    const document = JSON.parse(demo.file('baseline.json'))
    document.permits.push({ role: 'CUSTOMER', operation: 'reports.summary' })
    document.operations.find((o: { id: string }) => o.id === 'orders.create').body.schema.properties.slots = {
      type: ['array', 'null'],
      maxItems: 3,
      items: { type: 'array', maxItems: 2, items: { type: 'string', maxLength: 5 } },
    }
    await stage(admin, { text: JSON.stringify(document, null, 2) })
    const provisional = await activate(admin, 600)
    await resolve(admin, 'confirm')
    await revisions(admin)
    await versionsPart(admin, 'Endpoints')
    const createRules = admin.getByRole('main').getByRole('region', { name: 'Request Rules of orders.create' })
    await expect(createRules.getByRole('row', { name: /^slots\s/ })).toHaveText(/slots\s*body\s*array or null\s*optional\s*at most 3 items/)
    await expect(createRules.getByRole('row', { name: /^slots\[\]\s/ })).toHaveText(/slots\[\]\s*body\s*array\s*every element\s*at most 2 items/)
    await expect(createRules.getByRole('row', { name: /^slots\[\]\[\]\s/ })).toHaveText(/slots\[\]\[\]\s*body\s*string\s*every element\s*at most 5 characters/)
    const confirmed = await status()
    expect(confirmed).toMatchObject({ confirmed: provisional, revision: provisional })
    expect(confirmed.provisional).toBeUndefined()
    const allowed = await summary('cust-001')
    admitted(allowed)
    expect(allowed.receipts[0]).toMatchObject({ generation: confirmed.generation })

    await stage(admin, { file: demo.path('baseline.json') })
    const restoredRevision = await activate(admin, 600)
    await resolve(admin, 'confirm')
    const restored = await status()
    expect(restored).toMatchObject({ confirmed: restoredRevision })
    const denied = await summary('cust-001')
    expect(denied.status).toBe(403)
    expect(denied.receipts).toHaveLength(0)
    record('cpa-confirm', { before, confirmed, receipt: allowed.receipts[0], restored })
  })

  test('an earlier revision can be inspected and proposed again, but only a staged and activated candidate changes anything', async () => {
    const before = await status()
    const candidatesBefore = (await listAll('/admin/candidates')).length
    const genesis = (await adminCall('GET', '/admin/revisions/1')).body as { document: unknown }
    await revisions(admin)
    await versionsPart(admin, 'Version History')
    await admin.getByRole('button', { name: 'Inspect version 1', exact: true }).click()
    const detail = admin.getByRole('region', { name: 'Version 1' })
    await expect(detail.getByRole('heading', { name: 'Version 1', exact: true })).toBeFocused()
    await expect(fact(admin, 'Origin')).toContainText('first policy')
    await expect(fact(admin, 'Origin').locator('code')).toHaveText('genesis')
    const listChange = detail.getByRole('listitem').filter({ hasText: 'endpoints: changed orders.all' })
    await expect(listChange.getByRole('listitem')).toHaveText(['access: "allow" → "deny"'])
    await expect(detail.getByText('permissions: removed SUPPORT → orders.all')).toBeVisible()
    await shot(admin, '10b-revision')
    await detail.getByRole('button', { name: 'Start a Draft Change from This Version' }).click()
    await expect(admin.getByRole('heading', { level: 1, name: 'Changes' })).toBeVisible()
    const editor = admin.getByLabel('Complete Policy Document (JSON)')
    expect(JSON.parse(await editor.inputValue())).toEqual(genesis.document)
    // Nothing was sent: the gateway's state and its candidates are unchanged.
    expect(await status()).toEqual(before)
    expect(await listAll('/admin/candidates')).toHaveLength(candidatesBefore)

    // Starting again from the confirmed policy asks before replacing the text.
    await admin.getByRole('button', { name: /^Start from version \d+$/ }).click()
    await expect(admin.getByRole('dialog')).toContainText('The text in the editor is replaced by version')
    await dialogButton(admin, 'Replace It').click()
    expect(JSON.parse(await editor.inputValue())).toEqual((await adminCall('GET', '/admin/policy')).body.document)

    // A changed rate limit is listed member by member in the review.
    const changed = JSON.parse(await editor.inputValue())
    changed.operations.find((o: { id: string }) => o.id === 'reports.summary').rate.count = 10
    await editor.fill(JSON.stringify(changed, null, 2))
    await admin.getByRole('button', { name: 'Save Draft and Check' }).click()
    const staged = admin.getByText(/^The gateway checked it: saved as draft change \d+/)
    await expect(staged).toBeVisible()
    const id = Number((await staged.textContent())!.match(/change (\d+)/)![1])
    await admin.getByRole('region', { name: 'Save a Draft Change' }).getByRole('button', { name: `Review Change ${id}` }).click()
    const summaryChange = admin.getByRole('listitem').filter({ hasText: 'endpoints: changed reports.summary' })
    await expect(summaryChange.getByRole('listitem')).toHaveText(['rate.count: 30 → 10'])
    await admin.getByRole('button', { name: 'Withdraw', exact: true }).click()
    await dialogButton(admin, 'Withdraw Change').click()
    await expect(admin.getByText(`Withdrawn: change ${id} can no longer be put on trial.`)).toBeVisible()
    expect(await status()).toEqual(before)
    record('revision-inspection', { candidate: id, status: before })
  })

  test('an unconfirmed change rolls back by itself at its deadline', async () => {
    const before = await status()
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const provisional = await activate(admin, 3)
    admitted(await summary('cust-001'))

    await go(admin, 'Changes')
    // The countdown only prompts one reread; the gateway's own timeout is what
    // the page then shows.
    await expect(trial(admin)).toHaveCount(0, { timeout: 20_000 })
    const history = admin.getByRole('region', { name: 'Past Trials' })
    await expect(rowFor(history, provisional)).toContainText('Undone at the Deadline')
    await expect(nav(admin, 'Changes')).not.toContainText('On Trial')
    await shot(admin, '12-provisional-empty')

    const after = await status()
    expect(after).toMatchObject({ confirmed: before.confirmed, revision: before.confirmed })
    expect(after.provisional).toBeUndefined()
    const activations = await listAll<{ revision: number; disposition?: string }>('/admin/activations')
    expect(activations.find((a) => a.revision === provisional)?.disposition).toBe('timed_out')
    const denied = await summary('cust-001')
    expect(denied.status).toBe(403)
    expect(denied.receipts).toHaveLength(0)
    record('cpa-timeout', { provisional, after })
  })

  test('an invalid candidate is refused with the field named and changes nothing', async () => {
    const before = await status()
    const candidates = (await listAll('/admin/candidates')).length
    const results: Record<string, { text: string; code: string | null; field: string | null }> = {}

    // A document that is not JSON is stopped by the page before sending.
    await go(admin, 'Changes')
    await admin.getByLabel('Complete Policy Document (JSON)').fill('{"schema_version": 4,')
    await expect(admin.getByText('This is not valid JSON.')).toBeVisible()
    await admin.getByRole('button', { name: 'Save Draft and Check' }).click()

    const upstream = JSON.parse(demo.file('baseline.json'))
    upstream.upstreams[0].dial = '10.0.0.5:80'
    const missing = JSON.parse(demo.file('baseline.json'))
    delete missing.operations
    const unsupported = JSON.parse(demo.file('baseline.json'))
    unsupported.operations[0].method = 'TRACE'
    // A version 3 document, which named accounts and their roles, is refused.
    const legacy = { ...JSON.parse(demo.file('baseline.json')), schema_version: 3, subjects: [{ id: 'cust-001', status: 'active', roles: ['CUSTOMER'] }] }
    for (const [name, text] of [
      ['non-loopback upstream', JSON.stringify(upstream)],
      ['missing operations', JSON.stringify(missing)],
      ['unsupported method', JSON.stringify(unsupported)],
      ['version 3 with subjects', JSON.stringify(legacy)],
    ]) {
      await go(admin, 'Changes')
      await admin.getByLabel('Complete Policy Document (JSON)').fill(text)
      await admin.getByRole('button', { name: 'Save Draft and Check' }).click()
      const refusal = admin.getByRole('region', { name: 'Save a Draft Change' }).getByRole('alert')
      await expect(refusal).toHaveClass(/tone-refused/)
      await expect(moreDetail(refusal, 'HTTP status')).toHaveText('400')
      results[name] = {
        text: await refusal.innerText(),
        code: await moreDetail(refusal, 'Error code').textContent(),
        field: (await moreDetail(refusal, 'Field').count()) ? await moreDetail(refusal, 'Field').textContent() : null,
      }
      if (name === 'non-loopback upstream') await shot(admin, '19-candidate-refused')
    }
    expect(results['non-loopback upstream'].code).toBe('INVALID_UPSTREAM')
    expect(results['non-loopback upstream'].text).toContain('A service address is not allowed')
    expect(results['non-loopback upstream'].field).toBe('upstreams[0].dial')
    expect(results['unsupported method'].field).toBe('operations[0].method')
    expect(results['version 3 with subjects'].field).toBe('schema_version')
    expect(await listAll('/admin/candidates')).toHaveLength(candidates)
    expect(await status()).toEqual(before)
    record('invalid-candidates', results)
  })
})
