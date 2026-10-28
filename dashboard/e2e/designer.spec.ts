import { randomUUID } from 'node:crypto'
import AxeBuilder from '@axe-core/playwright'
import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import type { Candidate, Policy, Revision } from '../src/api'
import { adminURL, build, Demo, raw, siteURL, webRoot } from './env'
import {
  activate,
  createOverride,
  dialogButton,
  enrol,
  expectLayoutOnlyStorage,
  go,
  moreDetail,
  record,
  resolve,
  revokeOverride,
  shot,
  signIn,
  stage,
} from './pages'

const password = 'designer controlled local password 42'
let demo: Demo
let context: BrowserContext
let page: Page
let baseline: Policy
let enrolment: string
const policyViolations: string[] = []
const browserMessages: string[] = []
const requests: string[] = []

async function api(method: string, path: string, data?: unknown, headers: Record<string, string> = {}, session = context) {
  const sessionAnswer = await session.request.get(adminURL + '/admin/session')
  const csrf = sessionAnswer.ok() ? String((await sessionAnswer.json()).csrf_token) : ''
  const reply = await session.request.fetch(adminURL + path, {
    method,
    headers: method === 'GET' ? headers : { Origin: adminURL, 'X-CSRF-Token': csrf, ...headers },
    data,
  })
  const text = await reply.text()
  return { status: reply.status(), etag: reply.headers()['etag'], body: text ? JSON.parse(text) : undefined }
}

async function currentPolicy(): Promise<Revision & { document: Policy }> {
  const reply = await api('GET', '/admin/policy')
  expect(reply.status).toBe(200)
  return reply.body
}

async function candidates(session = context): Promise<Candidate[]> {
  const reply = await api('GET', '/admin/candidates?limit=100', undefined, {}, session)
  expect(reply.status).toBe(200)
  return reply.body.items
}

async function capture(name: string, target = page) {
  const original = target.viewportSize()!
  for (const width of [1440, 768, 375]) {
    await target.setViewportSize({ width, height: 900 })
    await shot(target, `${name}-${width}`)
    expect(await target.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${name} width ${width}`).toBe(true)
  }
  await target.setViewportSize(original)
}

async function designer() {
  await go(page, 'Policy')
  await expect(page.getByRole('tab', { name: 'Canvas', exact: true })).toBeVisible()
  await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
  await expect(page.locator('.react-flow__node[data-id="role:CUSTOMER"]')).toBeVisible()
}

async function discard() {
  const button = page.getByRole('button', { name: 'Discard draft', exact: true })
  if (await button.isEnabled()) {
    await button.click()
    await dialogButton(page, 'Discard draft').click()
  }
}

async function applyJson(document: Policy) {
  await page.getByRole('tab', { name: 'JSON', exact: true }).click()
  await page.getByLabel('Complete policy document (JSON)', { exact: true }).fill(JSON.stringify(document, null, 2))
  await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
  await expect(page.locator('#designer-json-document-error')).toHaveCount(0)
}

async function stagedDraft(): Promise<number> {
  await page.getByRole('button', { name: 'Save draft change', exact: true }).click()
  await dialogButton(page, 'Save draft change').click()
  const result = page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is in force yet\.$/)
  await expect(result).toBeVisible()
  return Number((await result.textContent())!.match(/change (\d+)/)![1])
}

async function restoreBaseline() {
  await stage(page, { text: JSON.stringify(baseline) })
  await activate(page, 300)
  await resolve(page, 'confirm')
  await designer()
  await discard()
}

function node(id: string, target = page) {
  return target.locator(`.react-flow__node[data-id="${id}"]`)
}

async function connectHandles(source: string, destination: string) {
  // The canvas opens at a readable zoom, which can leave the far columns
  // outside it; Fit brings both handles into view.
  await page.getByRole('button', { name: 'Fit', exact: true }).click()
  const from = await node(source).locator('.react-flow__handle.source').boundingBox()
  const to = await node(destination).locator('.react-flow__handle.target').boundingBox()
  expect(from).not.toBeNull()
  expect(to).not.toBeNull()
  await page.mouse.move(from!.x + from!.width / 2, from!.y + from!.height / 2)
  await page.mouse.down()
  await page.mouse.move(from!.x + from!.width / 2 + 8, from!.y + from!.height / 2, { steps: 4 })
  await page.mouse.move(to!.x + to!.width / 2, to!.y + to!.height / 2, { steps: 28 })
  await capture(`designer-connecting-${source}-${destination}`.replace(/[^\w.-]/g, '-'))
  await page.mouse.up()
}

test.describe.serial('Policy Designer on the native Gateway', () => {
  test.beforeAll(async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    enrolment = demo.setup()
    baseline = JSON.parse(demo.file('baseline.json'))
    await demo.start()
    context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    await context.exposeBinding('reportPolicyViolation', (_source, message: string) => {
      policyViolations.push(message)
    })
    await context.addInitScript(() => {
      document.addEventListener('securitypolicyviolation', (event) => {
        void (window as unknown as { reportPolicyViolation: (value: string) => Promise<void> }).reportPolicyViolation(
          `${event.effectiveDirective}: ${event.blockedURI}`,
        )
      })
    })
    context.on('console', (entry) => browserMessages.push(entry.text()))
    context.on('request', (request) => requests.push(`${request.method()} ${request.url()}`))
    page = await context.newPage()
    await page.goto(adminURL + '/')
    await enrol(page, 'admin', enrolment, password)
    await signIn(page, 'admin', password)
  })

  test.afterAll(async () => {
    await context?.close()
    await demo?.stop()
  })

  test('first-policy guidance and exact baseline boxes, references and fixed checks', async () => {
    await go(page, 'Policy')
    await expect(page.getByText('Establish the first policy before designing changes.')).toBeVisible()
    await expect(page.locator('.designer-canvas')).toHaveCount(0)
    await capture('designer-no-confirmed-policy')
    await stage(page, { file: demo.path('deny-all.json') })
    await page.getByRole('button', { name: 'Establish as the first policy' }).click()
    await page.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
    await dialogButton(page, 'Establish').click()
    await expect(page.getByText('Established: version 1 is the policy in force.')).toBeVisible()
    await stage(page, { file: demo.path('baseline.json') })
    await activate(page, 300)
    await resolve(page, 'confirm')
    await designer()
    const confirmed = await currentPolicy()
    expect(confirmed.document).toEqual(baseline)
    for (const [kind, collection] of [
      ['role', baseline.roles],
      ['operation', baseline.operations],
      ['api', baseline.apis],
      ['upstream', baseline.upstreams],
    ] as const) {
      await expect(page.locator(`.designer-box[data-kind="${kind}"]`)).toHaveCount(collection.length)
      for (const item of collection) await expect(node(`${kind}:${item.id}`)).toBeAttached()
    }
    await expect(page.locator('.react-flow__edge')).toHaveCount(baseline.permits.length + baseline.operations.length + baseline.apis.length)
    // A policy names roles, never accounts: role boxes list nobody.
    await expect(node('role:CUSTOMER')).not.toContainText('cust-001')
    for (const operation of baseline.operations) {
      const checkLabels = await node(`operation:${operation.id}`).locator('.designer-check').allTextContents()
      expect(checkLabels.map((label) => label.split(' ')[0])).toEqual([
        'Route',
        'Access',
        'Sign-in',
        'Role',
        'Rate',
        'Body',
        'Record',
        'Forward',
      ])
      expect(checkLabels[0]).toBe(`Route ${operation.method} ${operation.target}`)
      expect(checkLabels[1]).toBe(`Access ${operation.access}`)
      expect(checkLabels[2]).toBe(`Sign-in ${operation.auth === 'none' ? 'public' : 'required'}`)
    }
    await capture('designer-baseline-canvas')
    const zoom = await page
      .locator('.react-flow__viewport')
      .evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a)
    expect(zoom).toBeGreaterThanOrEqual(0.85)
    const canvas = (await page.locator('.designer-canvas').boundingBox())!
    const inView = async (id: string) => {
      const box = (await node(id).boundingBox())!
      return (
        box.x >= canvas.x &&
        box.x + box.width <= canvas.x + canvas.width &&
        box.y >= canvas.y &&
        box.y + box.height <= canvas.y + canvas.height
      )
    }
    // The baseline is too tall to fit at a readable zoom, so the canvas opens
    // on its middle with every role in view; Fit then shows every box.
    for (const role of baseline.roles) expect(await inView(`role:${role.id}`), `role:${role.id} in the opening view`).toBe(true)
    const operationsInView = []
    for (const operation of baseline.operations) if (await inView(`operation:${operation.id}`)) operationsInView.push(operation.id)
    expect(operationsInView.length, 'whole operations in the opening view').toBeGreaterThan(0)
    await page.getByRole('button', { name: 'Fit', exact: true }).click()
    await expect
      .poll(() => page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a))
      .toBeLessThan(zoom)
    for (const id of await page.locator('.react-flow__node').evaluateAll((nodes) => nodes.map((n) => n.getAttribute('data-id')!)))
      expect(await inView(id), `${id} after Fit`).toBe(true)
    await capture('designer-baseline-fit')
    record('designer-baseline', {
      revision: confirmed.id,
      boxes: baseline.roles.length + baseline.operations.length + baseline.apis.length + baseline.upstreams.length,
      edges: baseline.permits.length + baseline.operations.length + baseline.apis.length,
    })
  })

  test('a real handle drag stages exactly one permit and actual activation changes enforcement', async () => {
    // The Fetch baseline gives customers no exact-stock permit.
    const token = demo.token('cust-001')
    const stock = siteURL + '/inventory/stock/ITM-501'
    const before = await demo.receipts('inventory')
    expect((await raw('GET', stock, { Authorization: `Bearer ${token}` })).status).toBe(403)
    expect((await demo.receipts('inventory')).length).toBe(before.length)
    await connectHandles('role:CUSTOMER', 'operation:inventory.stock.read')
    await expect(page.getByText('Connection added to the draft.')).toBeVisible()
    await capture('designer-permit-drawn')
    const expected: Policy = structuredClone(baseline)
    expected.permits.push({ role: 'CUSTOMER', operation: 'inventory.stock.read' })
    const candidate = await stagedDraft()
    const stored = await api('GET', `/admin/candidates/${candidate}`)
    expect(stored.status).toBe(200)
    expect(stored.body.document).toEqual(expected)
    expect((await raw('GET', stock, { Authorization: `Bearer ${token}` })).status).toBe(403)
    expect((await demo.receipts('inventory')).length).toBe(before.length)
    await page.getByRole('button', { name: 'Preview its impact and put it on trial', exact: true }).click()
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
    await expect(page.getByText(`No draft changes against version ${(await currentPolicy()).id}`, { exact: true })).toBeVisible()
    record('designer-permit-drag', {
      candidate,
      receipts: receipts.map(({ method, target, status, subject }) => ({ method, target, status, subject })),
    })
    await restoreBaseline()
  })

  test('public, wrong-kind and reversed connections preserve the document and show the reason', async () => {
    const before = await currentPolicy()
    await connectHandles('role:CUSTOMER', 'operation:inventory.categories')
    await expect(page.getByText('This endpoint is public (no sign-in), so no role can be given permission for it.')).toBeVisible()
    await capture('designer-refused-public-connection')
    await connectHandles('role:CUSTOMER', 'api:inventory')
    await expect(page.getByText('Arrows go roles → operations → APIs → upstreams.')).toBeVisible()
    await connectHandles('operation:inventory.stock.read', `upstream:${baseline.apis.find((api) => api.id === 'inventory')!.upstream}`)
    await expect(page.getByRole('button', { name: 'Save draft change', exact: true })).toBeDisabled()
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    expect(JSON.parse(await page.getByLabel('Complete policy document (JSON)').inputValue())).toEqual(before.document)
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-refused-connections', { refusedConnections: 3, documentUnchanged: true })
  })

  test('Form, JSON and canvas stay synchronised and invalid JSON prevents switching', async () => {
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await page
      .getByRole('region', { name: 'Endpoint reports.summary', exact: true })
      .getByLabel('Request count', { exact: true })
      .fill('2')
    await capture('designer-form-rate')
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    const editor = page.getByLabel('Complete policy document (JSON)', { exact: true })
    const document: Policy = JSON.parse(await editor.inputValue())
    expect(document.operations.find((operation) => operation.id === 'reports.summary')!.rate!.count).toBe(2)
    document.operations.find((operation) => operation.id === 'reports.summary')!.access = 'deny'
    await editor.fill(JSON.stringify(document, null, 2))
    await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
    await capture('designer-json-applied')
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    await expect(node('operation:reports.summary').getByRole('button', { name: 'Rate 2/60s', exact: true })).toBeVisible()
    await expect(node('operation:reports.summary').getByRole('button', { name: 'Access deny', exact: true })).toBeVisible()
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
    await expect(page.getByRole('button', { name: 'Discard draft', exact: true })).toBeEnabled()
    await discard()
    expect(JSON.parse(await editor.inputValue())).toEqual((await currentPolicy()).document)
    await expect(page.locator('#designer-json-document-error')).toHaveCount(0)
    await capture('designer-discarded-json')
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-views-in-sync', { formRate: 2, jsonAccess: 'deny', invalidJsonBlockedSwitch: true })
  })

  test('Renaming a role renames its permits, and the Gateway refuses a permit for a role the document lacks', async () => {
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await page.getByRole('region', { name: 'Role CUSTOMER', exact: true }).getByLabel('Role ID', { exact: true }).fill('BUYER')
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    const editor = page.getByLabel('Complete policy document (JSON)', { exact: true })
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
    await page.getByRole('button', { name: 'Save draft change', exact: true }).click()
    const response = page.waitForResponse((reply) => reply.request().method() === 'POST' && reply.url() === adminURL + '/admin/candidates')
    await dialogButton(page, 'Save draft change').click()
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
    const field = page.getByRole('region', { name: 'Role RIDER', exact: true }).getByLabel('Role ID', { exact: true })
    await field.fill('')
    await field.pressSequentially('CUSTOMER')
    // Each valid prefix was applied as it was typed; the clashing one is not.
    await expect(page.getByText('Another role already uses the ID CUSTOMER. The draft keeps the ID CUSTOME until this is changed.')).toBeVisible()
    await expect(page.getByRole('button', { name: 'Save draft change', exact: true })).toBeDisabled()
    // The region is named after the box's current ID, so keep typing in the focused field.
    await page.keyboard.press('X')
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    const document: Policy = JSON.parse(await page.getByLabel('Complete policy document (JSON)', { exact: true }).inputValue())
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
    await page.getByRole('button', { name: 'Save draft change', exact: true }).click()
    await dialogButton(page, 'Save draft change').click()
    await expect(page.getByRole('alert').filter({ hasText: 'INVALID_UPSTREAM' })).toBeVisible()
    await expect(node(`upstream:${document.upstreams[0].id}`).locator('.designer-box')).toHaveClass(/is-refused/)
    await expect(page.getByRole('complementary', { name: 'Selected policy box' })).toContainText(document.upstreams[0].id)
    expect((await candidates()).length).toBe(before)
    await capture('designer-compiler-refusal')
    await discard()
    record('designer-compiler-refusal', { status: 400, reason: 'INVALID_UPSTREAM', candidateCountUnchanged: true })
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
    await page.getByRole('button', { name: 'Save draft change', exact: true }).click()
    await dialogButton(page, 'Save draft change').click()
    await expect(page.getByText(`The policy in force changed to version ${newRevision} since your draft was started from version`)).toBeVisible()
    await page.getByRole('button', { name: 'Show what changed', exact: true }).click()
    await expect(page.getByText('endpoints: changed orders.all')).toBeVisible()
    expect(JSON.parse(await page.getByLabel('Complete policy document (JSON)').inputValue())).toEqual(mine)
    await capture('designer-stale-revision')
    await page.getByRole('button', { name: `Keep my draft and base it on r${newRevision}`, exact: true }).click()
    const candidate = await stagedDraft()
    expect((await api('GET', `/admin/candidates/${candidate}`)).body.document).toEqual(mine)
    await second.close()
    await restoreBaseline()
    record('designer-stale-revision', { status: 412, newRevision, candidate, draftKept: true })
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
    await page.getByRole('button', { name: 'Save draft change', exact: true }).click()
    await dialogButton(page, 'Save draft change').click()
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
    await page.getByRole('button', { name: 'Send the same request again', exact: true }).click()
    await expect(page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is in force yet\.$/)).toBeVisible()
    expect(posts).toHaveLength(2)
    expect(posts[1]).toEqual(posts[0])
    expect((await candidates()).length).toBe(before + 1)
    await page.unroute('**/admin/candidates')
    await discard()
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-lost-staging-answer', { posts: posts.length, newCandidates: 1, identicalResend: true })
  })

  test('live overrides and provisional markers refresh only on explicit reads and actual deadlines', async () => {
    const override = await createOverride(page, {
      action: 'Allow temporarily: let a role call an endpoint',
      operation: 'inventory.stock.read',
      role: 'CUSTOMER',
      seconds: 120,
    })
    await designer()
    await expect(page.locator(`.react-flow__edge[data-id="override:${override}"]`)).toHaveCount(1)
    await capture('designer-temporary-permit')
    const readCount = requests.filter((request) => /GET .*\/admin\//.test(request)).length
    await page.waitForTimeout(700)
    expect(requests.filter((request) => /GET .*\/admin\//.test(request)).length).toBe(readCount)
    const proposed = structuredClone(baseline)
    proposed.operations.find((operation) => operation.id === 'reports.summary')!.rate!.count = 2
    await stage(page, { text: JSON.stringify(proposed) })
    const revision = await activate(page, 300)
    await designer()
    await page.getByRole('button', { name: /^In force · version \d+$/ }).click()
    await expect(node('operation:reports.summary')).toContainText(`changed in the trial of version ${revision}`)
    await capture('designer-provisional-markers')
    await resolve(page, 'rollback')
    await revokeOverride(page, override)
    await designer()
    await expect(page.locator(`.react-flow__edge[data-id="override:${override}"]`)).toHaveCount(0)
    await expect(page.getByText(`changed in the trial of version ${revision}`)).toHaveCount(0)
    record('designer-live-markers', { override, revision, noPolling: true, removedOnRevocationAndRollback: true })
  })

  test('trace explains the document with no requests and stops at the first refusal', async () => {
    const start = requests.length
    await page.getByRole('button', { name: 'Trace', exact: true }).click()
    await page.getByLabel('What', { exact: true }).selectOption('orders.all')
    await page.getByLabel('Who', { exact: true }).selectOption('role:CUSTOMER')
    const trace = page.getByRole('complementary', { name: 'Trace policy document' })
    await expect(trace).toContainText('403: no permission')
    await expect(trace.locator('li').filter({ hasText: 'Blocked list' })).toContainText('403 account_blocked')
    await expect(trace.locator('li').last()).toContainText('not reached')
    await capture('designer-trace-refused')
    await page.getByLabel('Who', { exact: true }).selectOption('role:SUPPORT')
    await expect(trace.locator('li').last()).toContainText(`127.0.0.1:${9502 + Number(process.env.EAT_PORT_OFFSET ?? 0)}`)
    await capture('designer-trace-permitted')
    await page.getByLabel('What', { exact: true }).selectOption('inventory.categories')
    await page.getByLabel('Who', { exact: true }).selectOption('none')
    await expect(trace).toContainText('not checked (public)')
    await capture('designer-trace-public')
    expect(requests.length).toBe(start)
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
    await page.getByLabel('Requests per window', { exact: true }).fill('100000')
    await page.getByRole('button', { name: 'Apply: tighten the limit', exact: true }).click()
    await expect(page.getByText(/already allows \d+ per window; enter a lower number\.$/)).toBeVisible()
    await capture('designer-template-applied')
    await page.getByRole('button', { name: 'Discard draft', exact: true }).click()
    await dialogButton(page, 'Discard draft').click()
  })

  test('a step-by-step wizard ends on a review and only edits the draft; the gateway checks it when it is saved', async () => {
    await designer()
    await page.getByText('Step by step', { exact: true }).click()
    await page.getByRole('button', { name: 'Give a role access', exact: true }).click()
    const wizard = page.getByRole('dialog', { name: 'Give a role access' })
    await wizard.getByLabel('Role', { exact: true }).selectOption('CUSTOMER')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    // Each step's heading takes focus; Next waits until a new endpoint is chosen.
    await expect(wizard.getByRole('heading', { name: 'Step 2 of 3: Choose endpoints' })).toBeFocused()
    await expect(wizard.getByRole('button', { name: 'Next', exact: true })).toBeDisabled()
    await expect(wizard.getByText('Choose at least one endpoint CUSTOMER cannot call yet.')).toBeVisible()
    await wizard.getByRole('checkbox', { name: /^reports\.summary \(/ }).check()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    const added = wizard.getByRole('region', { name: 'Added (1)' })
    await expect(added).toContainText('CUSTOMER can now call reports.summary')
    await expect(wizard.getByRole('region', { name: /^(Changed|Removed)/ })).toHaveCount(0)
    await capture('designer-wizard-review')
    await wizard.getByRole('button', { name: 'Apply to the draft', exact: true }).click()
    await expect(wizard).toHaveCount(0)
    await expect(page.getByText('Applied to the draft: CUSTOMER can now call 1 more endpoint.', { exact: false })).toBeVisible()
    // Nothing was sent: the draft only changed in this page.
    await expect(page.getByText(/^The gateway checked it/)).toHaveCount(0)
    const candidate = await stagedDraft()
    const stored = await api('GET', `/admin/candidates/${candidate}`)
    expect(stored.body.document.permits).toHaveLength(baseline.permits.length + 1)
    expect(stored.body.document.permits).toContainEqual({ role: 'CUSTOMER', operation: 'reports.summary' })
    // A rate limit left as it is cannot be applied, and Cancel changes nothing.
    await page.getByText('Step by step', { exact: true }).click()
    await page.getByRole('button', { name: 'Change a rate limit', exact: true }).click()
    const rate = page.getByRole('dialog', { name: 'Change a rate limit' })
    await rate.getByLabel('Endpoint', { exact: true }).selectOption('orders.create')
    await rate.getByRole('button', { name: 'Next', exact: true }).click()
    await rate.getByRole('button', { name: 'Next', exact: true }).click()
    await expect(rate.getByText('This is already the limit in the draft. Go back and choose another.')).toBeVisible()
    await expect(rate.getByRole('button', { name: 'Apply to the draft', exact: true })).toBeDisabled()
    await rate.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(rate).toHaveCount(0)
    await page.getByRole('button', { name: 'Discard draft', exact: true }).click()
    await dialogButton(page, 'Discard draft').click()
  })

  test('only box positions survive reload and Reset restores the automatic layout', async () => {
    const source = node('role:CUSTOMER')
    const automatic = await source.getAttribute('style')
    const box = await source.boundingBox()
    await page.mouse.move(box!.x + box!.width / 2, box!.y + 8)
    await page.mouse.down()
    await page.mouse.move(box!.x + box!.width / 2 + 24, box!.y + 32, { steps: 16 })
    await page.mouse.up()
    await expect.poll(() => page.evaluate(() => localStorage.getItem('eat40005.policy-designer.layout.v1'))).not.toBeNull()
    const saved = await page.evaluate(() => JSON.parse(localStorage.getItem('eat40005.policy-designer.layout.v1')!))
    const cookie = (await context.cookies(adminURL)).find((entry) => entry.name === '__Host-gateway-admin')!
    const session = await api('GET', '/admin/session')
    await expectLayoutOnlyStorage(page, [password, enrolment, cookie.value, String(session.body.csrf_token)])
    await page.reload()
    await expect(node('role:CUSTOMER')).toBeVisible()
    const position = await source.evaluate((element) => {
      const transform = new DOMMatrix(getComputedStyle(element).transform)
      return { x: transform.m41, y: transform.m42 }
    })
    // Computed transforms keep six significant digits, so a box thousands of
    // pixels down reads back to the hundredth; the drag moved it 24 pixels.
    expect(position.x).toBeCloseTo(saved.positions['role:CUSTOMER'].x, 1)
    expect(position.y).toBeCloseTo(saved.positions['role:CUSTOMER'].y, 1)
    await capture('designer-remembered-layout')
    await page.getByRole('button', { name: 'Reset layout', exact: true }).click()
    expect(await page.evaluate(() => localStorage.getItem('eat40005.policy-designer.layout.v1'))).toBeNull()
    await expect.poll(() => source.getAttribute('style')).toBe(automatic)
    await capture('designer-reset-layout')
    record('designer-layout-storage', { positionKept: true, resetRemovedKey: true, storageShapeAndSecretsChecked: true })
  })

  test('a read-only administrator has no editing controls and a crafted staging request is refused', async ({ browser }) => {
    const secret = await demo.enrol('reader', 'read')
    const reader = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    const readerPage = await reader.newPage()
    await readerPage.goto(adminURL + '/')
    await enrol(readerPage, 'reader', secret, password)
    await signIn(readerPage, 'reader', password)
    await go(readerPage, 'Policy')
    await expect(readerPage.getByText(/Read-only administrator/)).toBeVisible()
    await expect(readerPage.getByRole('button', { name: 'Save draft change', exact: true })).toHaveCount(0)
    await expect(readerPage.locator('.react-flow__handle')).toHaveCount(0)
    await capture('designer-read-only-canvas', readerPage)
    await readerPage.getByRole('tab', { name: 'Form', exact: true }).click()
    await expect(readerPage.getByRole('button', { name: /^Add / })).toHaveCount(0)
    await capture('designer-read-only-form', readerPage)
    await readerPage.getByRole('tab', { name: 'JSON', exact: true }).click()
    expect(await readerPage.getByLabel('Complete policy document (JSON)').isEditable()).toBe(false)
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

  test('every view passes axe, fits all widths and a keyboard permit change is staged', async () => {
    await page.reload()
    if (await page.getByRole('heading', { name: 'Sign in', exact: true }).isVisible()) await signIn(page, 'admin', password)
    await designer()
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      const fit = page.getByRole('button', { name: 'Fit', exact: true })
      await page.keyboard.press('Tab')
      await fit.focus()
      await shot(page, `designer-control-focus-${width}`)
      await fit.hover()
      await shot(page, `designer-control-hover-${width}`)
      await page.mouse.down()
      await shot(page, `designer-control-active-${width}`)
      await page.mouse.up()
    }
    await page.setViewportSize({ width: 1440, height: 900 })
    // Scan the canvas with its side form showing a selected box.
    await node('role:CUSTOMER').click()
    await expect(page.getByRole('heading', { name: 'Role CUSTOMER', level: 2 })).toBeVisible()
    for (const view of ['Canvas', 'Form', 'JSON']) {
      await page.getByRole('tab', { name: view, exact: true }).click()
      for (const width of [1440, 768, 375]) {
        await page.setViewportSize({ width, height: 900 })
        const scan = await new AxeBuilder({ page }).analyze()
        expect(
          scan.violations.map((violation) => ({ id: violation.id, nodes: violation.nodes.map((entry) => entry.target) })),
          `${view} ${width}`,
        ).toEqual([])
        await shot(page, `designer-axe-${view.toLowerCase()}-${width}`)
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      }
      await page.setViewportSize({ width: 1440, height: 900 })
    }
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    await page.getByRole('button', { name: 'Trace', exact: true }).click()
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      const scan = await new AxeBuilder({ page }).analyze()
      expect(
        scan.violations.map((violation) => ({ id: violation.id, nodes: violation.nodes.map((entry) => entry.target) })),
        `Trace ${width}`,
      ).toEqual([])
      await shot(page, `designer-axe-trace-${width}`)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    }
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.getByRole('button', { name: 'Trace', exact: true }).click()
    await page.getByRole('tab', { name: 'Form', exact: true }).focus()
    await page.keyboard.press('Enter')
    const permit = page.getByRole('checkbox', { name: 'CUSTOMER may call inventory.stock.read', exact: true })
    await permit.focus()
    await page.keyboard.press('Space')
    await expect(permit).toBeChecked()
    await page.getByRole('button', { name: 'Save draft change', exact: true }).focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('dialog')).toBeVisible()
    await page.keyboard.press('Tab')
    await page.keyboard.press('Enter')
    await expect(page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is in force yet\.$/)).toBeVisible()
    await capture('designer-keyboard-staged')
    await discard()
    await page.evaluate(() => {
      document.documentElement.style.fontSize = '200%'
    })
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      expect(await page.evaluate(() => getComputedStyle(document.documentElement).fontSize)).toBe('32px')
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await shot(page, `designer-text-200-${width}`)
    }
    await page.evaluate(() => {
      document.documentElement.style.fontSize = ''
    })
    await page.setViewportSize({ width: 1440, height: 900 })
    record('designer-accessibility', { axeScans: 12, violations: 0, widths: [1440, 768, 375], keyboardStage: true, textResize: '200%' })
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
    await expect(page.locator('.designer-box-operation')).toHaveCount(40)
    await capture('designer-worst-canvas')
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await expect(page.getByRole('region', { name: `Role ${longRole}`, exact: true })).toBeVisible()
    await capture('designer-worst-form')
    await expect(page.getByRole('region', { name: 'API unused-api', exact: true })).toBeVisible()
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    await expect(page.getByLabel('Complete policy document (JSON)', { exact: true })).toHaveValue(JSON.stringify(document, null, 2))
    await capture('designer-worst-json')
    await discard()
    record('designer-stress', { operations: 40, idCharacters: 64, targetCharacters: 256, unusedAPI: true })
  })

  test('all observed Designer interactions retain the content policy and secrets remain outside storage', async () => {
    expect(policyViolations).toEqual([])
    expect(
      browserMessages.filter((line) => /Content Security Policy|Refused to (load|execute|apply|connect)|violates.*directive/i.test(line)),
    ).toEqual([])
    await expectLayoutOnlyStorage(page, [password, enrolment])
    record('designer-content-policy', {
      policyEvents: policyViolations.length,
      requests: requests.length,
      browserMessages: [...new Set(browserMessages.map((message) => message.replace(/\d+/g, 'N')))],
    })
  })
})
