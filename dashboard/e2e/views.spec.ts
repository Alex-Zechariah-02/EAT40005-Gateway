import { expect, test } from '@playwright/test'
import { type Policy } from '../src/shared/api'
import { adminURL } from './env'
import { dialogButton, moreDetail, record, shot } from './pages'
import { api, applyJson, baseline, candidates, capture, currentPolicy, designer, discard, expandAll, formItem, node, page, selected, stagedDraft, open, ready, finish } from './designer'

// Form, JSON and canvas views of the draft, and the refusals the gateway names.
// Each file starts its own gateway; designer.ts holds the shared steps.

test.describe.serial('Policy Designer on the native Gateway', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('Form, JSON and canvas stay synchronised and invalid JSON prevents switching', async () => {
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await formItem('Endpoint reports.summary')
    await page
      .getByRole('region', { name: 'Endpoint reports.summary', exact: true })
      .getByLabel('Request count', { exact: true })
      .fill('2')
    await capture('designer-form-rate')
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    const editor = page.getByLabel('Complete Policy Document (JSON)', { exact: true })
    const document: Policy = JSON.parse(await editor.inputValue())
    expect(document.operations.find((operation) => operation.id === 'reports.summary')!.rate!.count).toBe(2)
    document.operations.find((operation) => operation.id === 'reports.summary')!.access = 'deny'
    await editor.fill(JSON.stringify(document, null, 2))
    await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
    await capture('designer-json-applied')
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    // The box edited in the Form is the one selected on the canvas.
    await expect(selected()).toContainText('Endpoint reports.summary')
    await expandAll()
    await node('operation:reports.summary').click()
    await expect(selected().getByRole('button', { name: 'Rate 2/60s', exact: true })).toBeVisible()
    await expect(selected().getByRole('button', { name: 'Access deny', exact: true })).toBeVisible()
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    await editor.fill('{ invalid')
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await expect(page.getByRole('tab', { name: 'JSON', exact: true })).toHaveAttribute('aria-selected', 'true')
    await expect(page.locator('#designer-json-document-error')).toContainText('$:')
    await capture('designer-invalid-json')
    await editor.fill(JSON.stringify(document))
    await page.getByRole('button', { name: 'Apply JSON' }).click()
    await discard()
    await editor.fill('{ invalid again')
    await page.getByRole('button', { name: 'Apply JSON' }).click()
    await expect(page.locator('#designer-json-document-error')).toContainText('$:')
    await expect(page.getByRole('button', { name: 'Discard', exact: true })).toBeEnabled()
    await discard()
    expect(JSON.parse(await editor.inputValue())).toEqual((await currentPolicy()).document)
    await expect(page.locator('#designer-json-document-error')).toHaveCount(0)
    await capture('designer-discarded-json')
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-views-in-sync', { formRate: 2, jsonAccess: 'deny', invalidJsonBlockedSwitch: true })
  })

  test('Renaming a role renames its permits, and the Gateway refuses a permit for a role the document lacks', async () => {
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await formItem('Role CUSTOMER')
    await page.getByRole('region', { name: 'Role CUSTOMER', exact: true }).getByLabel('Role ID', { exact: true }).fill('BUYER')
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    const editor = page.getByLabel('Complete Policy Document (JSON)', { exact: true })
    const document: Policy = JSON.parse(await editor.inputValue())
    expect(Object.keys(document)).not.toContain('subjects')
    expect(document.permits.filter((permit) => permit.role === 'BUYER')).toEqual(
      baseline.permits.filter((permit) => permit.role === 'CUSTOMER').map((permit) => ({ ...permit, role: 'BUYER' })),
    )
    // A permit for the old name, which no role in the document has, is the
    // Gateway's to refuse; it names the field.
    document.permits.push({ role: 'CUSTOMER', operation: 'orders.all' })
    await editor.fill(JSON.stringify(document, null, 2))
    await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
    await expect(page.locator('#designer-json-document-error')).toHaveCount(0)
    const before = (await candidates()).length
    await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
    const response = page.waitForResponse((reply) => reply.request().method() === 'POST' && reply.url() === adminURL + '/admin/candidates')
    await dialogButton(page, 'Save Draft Change').click()
    expect((await response).status()).toBe(400)
    const refusal = page.getByRole('alert').filter({ has: page.locator('.more-details dd code', { hasText: /^400$/ }) })
    await expect(refusal).toHaveClass(/tone-refused/)
    await expect(moreDetail(refusal, 'Field')).toHaveText(`permits[${document.permits.length - 1}].role`)
    expect(await candidates()).toHaveLength(before)
    await capture('designer-missing-role-refused')
    await discard()
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-missing-role', { renamedPermits: true, stagingStatus: 400 })
  })

  test('Typing a new ID never merges a box into another box with the same ID on the way', async () => {
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await formItem('Role RIDER')
    const field = page.getByRole('region', { name: 'Role RIDER', exact: true }).getByLabel('Role ID', { exact: true })
    await field.fill('')
    await field.pressSequentially('CUSTOMER')
    // Each valid prefix was applied as it was typed; the clashing one is not.
    await expect(page.getByText('Another role already uses the ID CUSTOMER. The draft keeps the ID CUSTOME until this is changed.')).toBeVisible()
    await expect(page.getByRole('button', { name: 'Save Changes', exact: true })).toBeDisabled()
    // The region is named after the box's current ID, so keep typing in the focused field.
    await page.keyboard.press('X')
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    const document: Policy = JSON.parse(await page.getByLabel('Complete Policy Document (JSON)', { exact: true }).inputValue())
    expect(document.roles.map((role) => role.id)).toEqual(baseline.roles.map((role) => (role.id === 'RIDER' ? 'CUSTOMERX' : role.id)))
    expect(document.permits).toEqual(
      baseline.permits.map((permit) => (permit.role === 'RIDER' ? { ...permit, role: 'CUSTOMERX' } : permit)),
    )
    await discard()
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-rename-collision', { merged: false })
  })

  test('the Gateway compiler refuses an invalid dial and identifies its box', async () => {
    const document = structuredClone(baseline)
    document.upstreams[0].dial = 'localhost:9501'
    const before = (await candidates()).length
    await applyJson(document)
    await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
    await dialogButton(page, 'Save Draft Change').click()
    await expect(page.getByRole('alert').filter({ hasText: 'INVALID_UPSTREAM' })).toBeVisible()
    await expect(node(`upstream:${document.upstreams[0].id}`)).toContainText('Refused: INVALID_UPSTREAM')
    await expect(page.getByRole('complementary', { name: 'Selected policy box' })).toContainText(document.upstreams[0].id)
    expect((await candidates()).length).toBe(before)
    await capture('designer-compiler-refusal')
    await discard()
    record('designer-compiler-refusal', { status: 400, reason: 'INVALID_UPSTREAM', candidateCountUnchanged: true })
  })

  test('an endpoint with host and header conditions is saved, and the Access Checker checks it with them', async () => {
    const document = structuredClone(baseline)
    const plain = document.operations.find((operation) => operation.id === 'inventory.categories')!
    const when = { hosts: ['*.example.com'], headers: [{ name: 'X-Version', value: '2' }] }
    document.operations.push({ ...plain, id: 'inventory.categories-v2', access: 'deny', when })
    await applyJson(document)
    const candidate = await stagedDraft()
    const stored = await api('GET', `/admin/candidates/${candidate}`)
    expect(stored.body.document.operations.at(-1).when).toEqual(when)
    await page.getByRole('link', { name: 'Access Checker', exact: true }).click()
    // A draft's own endpoints are offered once the draft is chosen.
    await page.getByLabel('Policy', { exact: true }).selectOption(String(candidate))
    await page.getByLabel('Endpoint', { exact: true }).selectOption('inventory.categories-v2')
    await page.getByRole('button', { name: 'Check Access' }).click()
    const verdict = page.getByRole('region', { name: /^Refused/ })
    await expect(verdict).toContainText('host example.example.com and header X-Version: 2')
    await expect(verdict).toContainText('The gateway selected inventory.categories-v2.')
    await capture('access-checker-conditions')
    await designer()
    await discard()
  })

  test('losing the staging answer never resends automatically and an identical manual resend creates one candidate', async () => {
    const document = structuredClone(baseline)
    document.operations.find((operation) => operation.id === 'reports.summary')!.rate!.count = 3
    await applyJson(document)
    const before = (await candidates()).length
    const posts: { key: string; body: string | null }[] = []
    let drop = true
    let releaseAnswer: () => void = () => {}
    const heldAnswer = new Promise<void>((resolve) => {
      releaseAnswer = resolve
    })
    await page.route('**/admin/candidates', async (route) => {
      if (route.request().method() !== 'POST') return route.continue()
      posts.push({ key: route.request().headers()['idempotency-key'], body: route.request().postData() })
      if (drop) {
        drop = false
        await route.fetch()
        await heldAnswer
        await route.abort('connectionreset')
      } else await route.continue()
    })
    await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
    await dialogButton(page, 'Save Draft Change').click()
    await expect(page.getByText("Sent; waiting for the gateway's answer…")).toBeVisible()
    await capture('designer-staging-pending')
    releaseAnswer()
    await expect(page.getByText('No answer from the gateway: the draft may or may not have been saved.')).toBeVisible()
    await page.waitForTimeout(500)
    expect(posts).toHaveLength(1)
    expect((await candidates()).length).toBe(before + 1)
    await capture('designer-staging-answer-lost')
    await page.getByRole('button', { name: 'Check', exact: true }).click()
    await expect(page.getByText(/draft changes listed\. Look for this draft on the Changes page/)).toBeVisible()
    await page.getByRole('button', { name: 'Send the Same Request Again', exact: true }).click()
    await expect(page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is active yet\.$/)).toBeVisible()
    expect(posts).toHaveLength(2)
    expect(posts[1]).toEqual(posts[0])
    expect((await candidates()).length).toBe(before + 1)
    await page.unroute('**/admin/candidates')
    await discard()
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-lost-staging-answer', { posts: posts.length, newCandidates: 1, identicalResend: true })
  })

  test('Designer handles a large policy document through JSON Apply', async () => {
    await designer()
    await discard()
    const document = structuredClone(baseline)
    const longRole = 'warehouse.inventory.audit.readers.'.padEnd(64, 'x')
    document.roles.push({ id: longRole })
    document.apis.push({ id: 'unused-api', upstream: document.upstreams[0].id })
    for (let index = document.operations.length; index < 40; index++) {
      document.operations.push({
        id: `warehouse.inventory.audit.operation.${index}.`.padEnd(64, 'x'),
        api: document.apis[0].id,
        method: 'GET',
        target: `/api/warehouse/audit/${index}/`.padEnd(256, 'x'),
        access: 'allow',
        auth: 'required',
      })
    }
    await applyJson(document)
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    await expandAll()
    await expect(page.locator('.designer-row[data-box^="operation:"]')).toHaveCount(40)
    await capture('designer-worst-canvas')
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await formItem(`Role ${longRole}`)
    await expect(page.getByRole('region', { name: `Role ${longRole}`, exact: true })).toBeVisible()
    await capture('designer-worst-form')
    await formItem('API unused-api')
    await expect(page.getByRole('region', { name: 'API unused-api', exact: true })).toBeVisible()
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    await expect(page.getByLabel('Complete Policy Document (JSON)', { exact: true })).toHaveValue(JSON.stringify(document, null, 2))
    await capture('designer-worst-json')
    await discard()
    record('designer-stress', { operations: 40, idCharacters: 64, targetCharacters: 256, unusedAPI: true })
  })

  test('the first fit beside the open sidebar is readable at 1280 px and cards never overlap', async () => {
    const original = page.viewportSize()!
    await page.setViewportSize({ width: 1280, height: 800 })
    await page.reload()
    await designer()
    await expect(page.locator('.shell')).not.toHaveClass(/is-rail/)
    const zoom = () => page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a)
    await expect.poll(zoom).toBeGreaterThanOrEqual(0.85)
    const canvas = (await page.locator('.designer-canvas').boundingBox())!
    const cards = await page.locator('.react-flow__node').evaluateAll((nodes) => nodes.map((n) => n.getBoundingClientRect().toJSON() as DOMRect))
    for (const [index, card] of cards.entries()) {
      expect(card.x >= canvas.x && card.right <= canvas.x + canvas.width && card.y >= canvas.y && card.bottom <= canvas.y + canvas.height, `card ${index} in view`).toBe(true)
      for (const other of cards.slice(index + 1))
        expect(card.right <= other.x || other.right <= card.x || card.bottom <= other.y || other.bottom <= card.y, `card ${index} overlaps another`).toBe(true)
    }
    await shot(page, 'designer-fit-1280')
    record('designer-fit-1280', { zoom: await zoom(), cards: cards.length })
    await page.setViewportSize(original)
  })
})
