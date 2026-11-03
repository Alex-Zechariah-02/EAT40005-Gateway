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
  await expect(node('role:CUSTOMER')).toBeVisible()
}

async function discard() {
  const button = page.getByRole('button', { name: 'Discard', exact: true })
  if (await button.isEnabled()) {
    await button.click()
    await dialogButton(page, 'Discard Draft').click()
  }
}

async function applyJson(document: Policy) {
  await page.getByRole('tab', { name: 'JSON', exact: true }).click()
  await page.getByLabel('Complete Policy Document (JSON)', { exact: true }).fill(JSON.stringify(document, null, 2))
  await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
  await expect(page.locator('#designer-json-document-error')).toHaveCount(0)
}

async function stagedDraft(): Promise<number> {
  await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
  await dialogButton(page, 'Save Draft Change').click()
  const result = page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is active yet\.$/)
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

// formItem opens one box's editor from the Form view's list.
async function formItem(name: string) {
  const list = page.getByRole('navigation', { name: 'Policy objects' })
  const group = ['Service address', 'Endpoint', 'Role', 'API'].find((kind) => name.startsWith(kind + ' '))!
  const toggle = list.getByRole('button', { name: new RegExp(`^${group === 'Service address' ? 'Service Addresses' : group + 's'}\\b`) })
  if ((await toggle.getAttribute('aria-expanded')) === 'false') await toggle.click()
  await list.getByRole('button', { name, exact: true }).click()
}

// node is a box's row on the canvas; an endpoint's row shows once its API's
// group is open.
function node(id: string, target = page) {
  return target.locator(`.designer-row[data-box="${id}"]`)
}

async function expandAll(target = page) {
  await target.getByRole('button', { name: 'Expand All', exact: true }).click()
}

function selected(target = page) {
  return target.getByRole('complementary', { name: 'Selected policy box' })
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
    await page.getByRole('button', { name: 'Establish as the First Policy' }).click()
    await page.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
    await dialogButton(page, 'Establish').click()
    await expect(page.getByText('Established: version 1 is the active policy.')).toBeVisible()
    await stage(page, { file: demo.path('baseline.json') })
    await activate(page, 300)
    await resolve(page, 'confirm')
    await designer()
    const confirmed = await currentPolicy()
    expect(confirmed.document).toEqual(baseline)
    // The canvas opens with every group in view and the endpoint lists
    // closed, joined by a few connections that each stand for many.
    const canvas = (await page.locator('.designer-canvas').boundingBox())!
    const inView = async (selector: string) => {
      const box = (await page.locator(selector).boundingBox())!
      return box.x >= canvas.x && box.x + box.width <= canvas.x + canvas.width && box.y >= canvas.y && box.y + box.height <= canvas.y + canvas.height
    }
    const zoom = await page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a)
    expect(zoom).toBeGreaterThanOrEqual(0.85)
    for (const id of await page.locator('.react-flow__node').evaluateAll((nodes) => nodes.map((n) => n.getAttribute('data-id')!)))
      expect(await inView(`.react-flow__node[data-id="${id}"]`), `${id} in the opening view`).toBe(true)
    for (const api of baseline.apis)
      await expect(page.getByRole('button', { name: new RegExp(`^${api.id[0].toUpperCase()}${api.id.slice(1)} API Endpoints,`) })).toHaveAttribute('aria-expanded', 'false')
    const reached = baseline.apis.filter((api) =>
      baseline.operations.some((operation) => operation.api === api.id && baseline.permits.some((permit) => permit.operation === operation.id)),
    )
    await expect(page.locator('.react-flow__edge')).toHaveCount(reached.length + baseline.apis.length + 1)
    expect(reached.length + baseline.apis.length + 1).toBeLessThan(baseline.permits.length)
    await capture('designer-baseline-canvas')
    await expandAll()
    for (const [kind, collection] of [
      ['role', baseline.roles],
      ['operation', baseline.operations],
      ['api', baseline.apis],
      ['upstream', baseline.upstreams],
    ] as const) {
      await expect(page.locator(`.designer-row[data-box^="${kind}:"]`)).toHaveCount(collection.length)
      for (const item of collection) await expect(node(`${kind}:${item.id}`)).toBeVisible()
    }
    // A policy names roles, never accounts: role rows list nobody.
    await expect(node('role:CUSTOMER')).not.toContainText('cust-001')
    // Each endpoint's editor lists the gateway's checks in their fixed order.
    for (const operation of baseline.operations) {
      await node(`operation:${operation.id}`).click()
      // Each check reads as its name and value; the step number is drawn, not read.
      const checkLabels = (await selected().locator('.designer-check').allTextContents()).map((label) => label.replace(/^\d+/, ''))
      expect(checkLabels.map((label) => label.split(' ')[0])).toEqual(['Route', 'Access', 'Sign-In', 'Role', 'Rate', 'Body', 'Record', 'Forward'])
      expect(checkLabels[0]).toBe(`Route ${operation.method} ${operation.target}`)
      expect(checkLabels[1]).toBe(`Access ${operation.access}`)
      expect(checkLabels[2]).toBe(`Sign-In ${operation.auth === 'none' ? 'public' : 'required'}`)
      // Selecting an endpoint brings it into view at a readable size; closing
      // the form returns the canvas to the view it had before.
      await expect
        .poll(() => page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a))
        .toBeGreaterThanOrEqual(0.85)
      await selected().getByRole('button', { name: 'Close', exact: true }).click()
      await expect(selected()).toHaveCount(0)
    }
    // Selecting a role marks exactly the endpoints it may call.
    await node('role:CUSTOMER').click()
    const granted = baseline.permits.filter((permit) => permit.role === 'CUSTOMER')
    await expect(page.locator('.designer-row.is-allowed')).toHaveCount(granted.length)
    for (const permit of granted) await expect(node(`operation:${permit.operation}`)).toHaveClass(/is-allowed/)
    await page.getByRole('button', { name: 'Fit', exact: true }).click()
    await capture('designer-baseline-fit')
    await selected().getByRole('button', { name: 'Close', exact: true }).click()
    await page.getByRole('button', { name: 'Collapse All', exact: true }).click()
    await expect(node('role:CUSTOMER')).toBeHidden()
    await page.getByRole('button', { name: /^Roles,/ }).click()
    await expect(node('role:CUSTOMER')).toBeVisible()
    record('designer-baseline', {
      revision: confirmed.id,
      boxes: baseline.roles.length + baseline.operations.length + baseline.apis.length + baseline.upstreams.length,
      connections: reached.length + baseline.apis.length + 1,
      permits: baseline.permits.length,
    })
  })

  test('granting a permission in the editor stages exactly one permit and actual activation changes enforcement', async () => {
    // The Fetch baseline gives customers no exact-stock permit.
    const token = demo.token('cust-001')
    const stock = siteURL + '/inventory/stock/ITM-501'
    const before = await demo.receipts('inventory')
    expect((await raw('GET', stock, { Authorization: `Bearer ${token}` })).status).toBe(403)
    expect((await demo.receipts('inventory')).length).toBe(before.length)
    await node('role:CUSTOMER').click()
    await selected().getByRole('checkbox', { name: 'GET /inventory/stock/* (inventory.stock.read)', exact: true }).check()
    await page.getByRole('button', { name: /^Inventory API Endpoints,/ }).click()
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

  test('a public endpoint takes no role permission and the document stays unchanged', async () => {
    const before = await currentPolicy()
    await node('role:CUSTOMER').click()
    await expect(selected().getByRole('checkbox', { name: /\(inventory\.categories\)$/ })).toHaveCount(0)
    await expandAll()
    await node('operation:inventory.categories').click()
    await expect(selected()).toContainText('Public endpoints do not use role permissions.')
    await expect(selected().getByRole('checkbox', { name: 'CUSTOMER', exact: true })).toHaveCount(0)
    await capture('designer-public-endpoint')
    await expect(page.getByRole('button', { name: 'Save Changes', exact: true })).toBeDisabled()
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    expect(JSON.parse(await page.getByLabel('Complete Policy Document (JSON)').inputValue())).toEqual(before.document)
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-public-endpoint', { documentUnchanged: true })
  })

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
    // Each step's heading takes focus; Next waits until a new endpoint is chosen.
    await expect(wizard.getByRole('heading', { name: 'Step 2 of 3: Choose Endpoints' })).toBeFocused()
    await expect(wizard.getByRole('button', { name: 'Next', exact: true })).toBeDisabled()
    await expect(wizard.getByText('Choose at least one endpoint CUSTOMER cannot call yet.')).toBeVisible()
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
    await expect(rate.getByRole('button', { name: 'Apply to the Draft', exact: true })).toBeDisabled()
    await rate.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(rate).toHaveCount(0)
    await page.getByRole('button', { name: 'Discard', exact: true }).click()
    await dialogButton(page, 'Discard Draft').click()
  })

  test('groups open and close, full screen fills the window, and the canvas stores nothing in the browser', async () => {
    const inventory = page.getByRole('button', { name: /^Inventory API Endpoints,/ })
    await page.getByRole('button', { name: 'Collapse All', exact: true }).click()
    await expect(inventory).toHaveAttribute('aria-expanded', 'false')
    await expect(node('operation:inventory.stock.read')).toBeHidden()
    await inventory.click()
    await expect(inventory).toHaveAttribute('aria-expanded', 'true')
    await expect(node('operation:inventory.stock.read')).toBeVisible()
    // Opening a group brings it into view at a readable size, its heading
    // inside the canvas, even with another group already open.
    const orders = page.getByRole('button', { name: /^Orders API Endpoints,/ })
    await orders.click()
    await expect(orders).toHaveAttribute('aria-expanded', 'true')
    const focused = async () => {
      const canvas = (await page.locator('.designer-canvas').boundingBox())!
      const heading = (await orders.boundingBox())!
      const zoom = await page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a)
      return zoom >= 0.85 && heading.y >= canvas.y && heading.y + heading.height <= canvas.y + canvas.height && heading.x >= canvas.x
    }
    await expect.poll(focused).toBe(true)
    await capture('designer-group-open')
    await orders.click()
    await expect(orders).toHaveAttribute('aria-expanded', 'false')
    // Fit shows every card again, so the Inventory group is in reach.
    await page.getByRole('button', { name: 'Fit', exact: true }).click()
    await inventory.click()
    await expect(node('operation:inventory.stock.read')).toBeHidden()
    await page.getByRole('button', { name: 'Full Screen', exact: true }).click()
    await expect(page.locator('.designer-workspace')).toHaveClass(/is-expanded/)
    const size = page.viewportSize()!
    const area = (await page.locator('.designer-workspace').boundingBox())!
    expect(area.width).toBeGreaterThanOrEqual(size.width - 1)
    await shot(page, 'designer-full-screen')
    await page.locator('.designer-canvas').press('Escape')
    await expect(page.locator('.designer-workspace')).not.toHaveClass(/is-expanded/)
    const cookie = (await context.cookies(adminURL)).find((entry) => entry.name === '__Host-gateway-admin')!
    const session = await api('GET', '/admin/session')
    await expectLayoutOnlyStorage(page, [password, enrolment, cookie.value, String(session.body.csrf_token)])
    expect(await page.evaluate(() => localStorage.getItem('eat40005.policy-designer.layout.v1'))).toBeNull()
    record('designer-groups', { collapsedByDefault: true, fullScreen: true, layoutStored: false })
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
    await page.getByRole('button', { name: 'Permissions Matrix', exact: true }).focus()
    await page.keyboard.press('Enter')
    const permit = page.getByRole('checkbox', { name: 'CUSTOMER may call inventory.stock.read', exact: true })
    await permit.focus()
    await page.keyboard.press('Space')
    await expect(permit).toBeChecked()
    await page.getByRole('button', { name: 'Save Changes', exact: true }).focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('dialog')).toBeVisible()
    await page.keyboard.press('Tab')
    await page.keyboard.press('Enter')
    await expect(page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is active yet\.$/)).toBeVisible()
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
