import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, resolve as resolvePath } from 'node:path'
import AxeBuilder from '@axe-core/playwright'
import { expect, test, type BrowserContext, type Locator, type Page } from '@playwright/test'
import { AdminApi, adminURL, build, controls, Demo, gatewayIdentity, raw, siteURL, webRoot } from './env'
import {
  hops,
  inspect,
  observed,
  operation,
  record,
  runAnswers,
  scenarioGroup,
  sendScenario,
  shot,
  shopStatus,
  shopPage,
  traceStates,
  useSlot,
} from './pages'

// One demonstration deployment, driven in order as two ordinary API users
// would drive the client, while test tooling plays the operator and the
// administrator. Every step checks what the client shows against independent
// evidence: the fixtures' own receipts and state and the gateway's security
// events.

const password = 'correct horse battery staple 42'
const uiPolicy =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
const seeds = {
  orders: '89b88f883eaf03f4690644c108f6eb28c51adeb3e0e2879140fb8f872a90ccd5',
  inventory: 'c9be637744336ba749507ff90ca3acd32a04f956a4dea88be48300a87b27369c',
  reports: '1ea7c90454da4c0ece4bcb056145408d674a3ed9f764c33272f905feee6cdaba',
}

let demo: Demo
let api: AdminApi
let siteContext: BrowserContext
let site: Page
const tokens: Record<string, string> = {}
// Every request the client's pages made, with its method and cookie header,
// every console line and whether a script dialog ever opened.
const siteRequests: Promise<{ url: string; method: string; cookie?: string }>[] = []
const consoleLines: string[] = []
let scriptDialogs = 0
// Every token and one-time secret handed out, for the final secret scan.
const handedOut: string[] = []
// Requests the tests themselves send from the client's page, to probe what
// its origin allows; the client's own code sends none of them.
const pageProbes: string[] = []

function token(subject: string): string {
  if (!tokens[subject]) {
    tokens[subject] = demo.token(subject)
    handedOut.push(tokens[subject])
  }
  return tokens[subject]
}

async function stock(item: string): Promise<number> {
  const state = await demo.state('inventory')
  const items = (state.data as { items: { id: string; stock: number }[] }).items
  return items.find((i) => i.id === item)!.stock
}

// received returns the receipts a fixture recorded after the first `since`.
async function received(service: string, since: number) {
  return (await demo.receipts(service)).slice(since)
}

// shownRequestId reads the gateway request id the client displays for an
// answer the gateway produced itself.
async function shownRequestId(region: Locator): Promise<string> {
  const id = await region.locator('.request-id').textContent()
  expect(id).toMatch(/^[0-9a-f]{32}$/)
  return id!
}

// The gateway's checks as the client's trace names them, in the order of the
// gateway's request handler, and the check each security event stage belongs
// to.
const checkOrder = ['Request form', 'Route', 'Operation access', 'Token', 'Role permit', 'Rate limit', 'Body and schema', 'Event record']
const stageCheck: Record<string, string> = {
  INGRESS: 'Request form',
  CANONICAL: 'Request form',
  ROUTE: 'Route',
  ACCESS: 'Operation access',
  JWT: 'Token',
  RBAC: 'Role permit',
  RATE: 'Rate limit',
  BODY: 'Body and schema',
  VALIDATION: 'Body and schema',
}

// expectTrace checks the trace an answer shows against the gateway's own
// security event for the request: the stage that refused it is marked as the
// check that refused it, or as one that may have; every check before the
// first marked one is cleared and every check after the last is not reached.
async function expectTrace(region: Locator, stage: string) {
  const states = await traceStates(region)
  expect(states.map((s) => s.check)).toEqual(checkOrder)
  const marked = states.flatMap((s, i) => (s.state === 'refused the request' || s.state === 'may have refused it' ? [i] : []))
  expect(marked, stage).toContain(checkOrder.indexOf(stageCheck[stage]))
  expect(states.slice(0, marked[0]).map((s) => s.state)).toEqual(Array(marked[0]).fill('cleared'))
  expect(states.slice(marked.at(-1)! + 1).every((s) => s.state === 'not reached')).toBe(true)
}

// expectCleared checks that an answer's trace shows every check cleared.
async function expectCleared(region: Locator) {
  expect(await traceStates(region)).toEqual(checkOrder.map((check) => ({ check, state: 'cleared' })))
}

// Shop helpers exercise the actual page controls. Identity setup happens on
// the inspector so incidental catalogue reads cannot pollute receipt windows.
function stockRow() {
  return site.getByRole('row').filter({ has: site.getByText('The Gadget', { exact: true }) })
}
async function listOrders(subject: string) {
  await useSlot(site, 'Staff', token(subject))
  const since = (await demo.receipts('orders')).length
  await shopPage(site, 'Orders')
  const region = site.locator('main')
  return { region, status: await shopStatus(region), receipts: await received('orders', since) }
}
async function lookupOrder(subject: string, number: string) {
  await useSlot(site, 'Customer', token(subject))
  await shopPage(site, 'My orders')
  const since = (await demo.receipts('orders')).length
  await site.getByLabel('Order number', { exact: true }).fill(number)
  await site.getByRole('button', { name: 'Find order', exact: true }).click()
  const region = site.locator('main')
  return { region, status: await shopStatus(region), receipts: await received('orders', since) }
}
async function product(item = 'ITM-002') {
  await site.evaluate((id) => {
    location.hash = `#/product/${id}`
  }, item)
  const region = site.getByRole('region', { name: 'Product stock' })
  await expect(region.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
  await shopStatus(region)
  return region
}
async function adjust(subject: string, delta: string) {
  await useSlot(site, 'Staff', token(subject))
  await shopPage(site, 'Stock')
  const since = (await demo.receipts('inventory')).length
  const region = stockRow()
  await region.getByLabel('Change', { exact: true }).fill(String(Math.abs(Number(delta))))
  await region.getByRole('button', { name: Number(delta) < 0 ? 'Remove' : 'Receive', exact: true }).click()
  return { region, status: await shopStatus(region), receipts: await received('inventory', since) }
}
async function summary(subject: string) {
  await useSlot(site, 'Staff', token(subject))
  const since = (await demo.receipts('reports')).length
  await shopPage(site, 'Reports')
  const region = site.getByRole('region', { name: 'Sales summary', exact: true })
  return { region, status: await shopStatus(region), receipts: await received('reports', since) }
}
async function sales(subject: string) {
  await useSlot(site, 'Manager', token(subject))
  const since = (await demo.receipts('reports')).length
  await shopPage(site, 'Reports')
  const region = site.getByRole('region', { name: 'Monthly sales', exact: true })
  return { region, status: await shopStatus(region), receipts: await received('reports', since) }
}

// withPermit returns the baseline policy document with one extra permit.
function withPermit(permit: { role: string; operation: string }): string {
  const document = JSON.parse(demo.file('baseline.json'))
  document.permits.push(permit)
  return JSON.stringify(document)
}

// canonical writes a JSON value with object members in sorted order, so two
// documents compare equal whatever order their members were written in.
function canonical(value: unknown): string {
  return JSON.stringify(value, (_, v: unknown) =>
    v && typeof v === 'object' && !Array.isArray(v) ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : 1))) : v,
  )
}

// restart stops and starts the demonstration processes. Sessions and rate
// counters end, the database and audit file remain, and the fixtures start
// again from their seed data. The tooling signs in again.
async function restart(options: { fault?: string } = {}) {
  await demo.stop()
  await demo.start(options)
  await api.signIn('admin', password)
}

test.describe.serial('client', () => {
  test.beforeAll(async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    const secret = demo.setup()
    handedOut.push(secret)
    await demo.start()
    api = new AdminApi()
    await api.enrol('admin', secret, password)
    await api.signIn('admin', password)
    siteContext = await browser.newContext()
    site = await siteContext.newPage()
    site.on('request', (r) => siteRequests.push(r.allHeaders().then((h) => ({ url: r.url(), method: r.method(), cookie: h['cookie'] }))))
    siteContext.on('console', (m) => consoleLines.push(m.text()))
    site.on('dialog', (dialog) => {
      scriptDialogs++
      dialog.dismiss()
    })
  })

  test.afterAll(async () => {
    await siteContext?.close()
    await demo?.stop()
  })

  test('cold start serves the client and its assets from the data plane at /demo/ only, under the content policy', async () => {
    record('cold-start', { gateway: gatewayIdentity(), pids: Object.keys(demo.pids), web: demo.web })
    expect(Object.keys(demo.pids).sort()).toEqual(['gateway', 'inventory', 'orders', 'reports'])

    await site.goto(siteURL + '/demo/')
    await expect(site.getByRole('heading', { level: 1, name: 'The workshop shelf' })).toBeVisible()
    await expect(site.locator('.product-card')).toHaveCount(3)
    await expect(site.getByLabel('Signed in as', { exact: true })).toHaveValue('')
    // The three typefaces load under the content policy, from the page's own
    // script, and the page's text and code are set in them.
    expect(
      await site.evaluate(() => {
        const first = (element: Element | null) =>
          element ? getComputedStyle(element).fontFamily.split(',')[0].replace(/["']/g, '').trim() : ''
        return {
          faces: [...document.fonts].map((face) => `${face.family.replace(/["']/g, '')}:${face.status}`),
          text: first(document.body) === 'Instrument Sans' && document.fonts.check('16px "Instrument Sans"'),
          code: first(document.querySelector('code')) === 'Martian Mono' && document.fonts.check('16px "Martian Mono"'),
          display: first(document.querySelector('h1')) === 'Big Shoulders Display' && document.fonts.check('26px "Big Shoulders Display"'),
        }
      }),
    ).toEqual({
      faces: ['Big Shoulders Display:loaded', 'Instrument Sans:loaded', 'Martian Mono:loaded'],
      text: true,
      code: true,
      display: true,
    })
    await shot(site, '01-cold-start')

    // Every built file is served from /demo/assets/ with a fixed type and the
    // content policy; none is served by the admin listener.
    const index = readFileSync(resolvePath('dist', 'index.html'), 'utf8')
    const assets = [...index.matchAll(/(?:src|href)="\/demo\/assets\/([^"]+)"/g)].map((m) => m[1])
    expect(assets.map((a) => a.split('.').pop()).sort()).toEqual(['css', 'js', 'svg'])
    const types: Record<string, string> = { js: 'text/javascript; charset=utf-8', css: 'text/css; charset=utf-8', svg: 'image/svg+xml' }
    const served = []
    for (const asset of assets) {
      const reply = await raw('GET', `${siteURL}/demo/assets/${asset}`)
      expect(reply.status, asset).toBe(200)
      expect(reply.headers['content-type'], asset).toBe(types[asset.split('.').pop()!])
      expect(reply.headers['content-security-policy'], asset).toBe(uiPolicy)
      expect(reply.body, asset).toBe(readFileSync(resolvePath('dist', 'assets', asset), 'utf8'))
      const onAdmin = await raw('GET', `${adminURL}/demo/assets/${asset}`)
      expect(onAdmin.status, asset).toBe(404)
      served.push({ asset, status: reply.status, adminListener: onAdmin.status })
    }

    // Only the page and its flat assets are the website's; every other path,
    // spelling and method goes to enforcement, which is bootstrap-locked now.
    const probes = [
      { method: 'GET', url: siteURL + '/demo/assets/no-such-file.js', status: 404 },
      { method: 'GET', url: siteURL + '/demo/no-such-file.js', status: 503 },
      { method: 'GET', url: siteURL + '/demo/index.html', status: 503 },
      { method: 'GET', url: siteURL + '/demo', status: 503 },
      { method: 'GET', url: siteURL + '/demo/assets/../index.html', status: 503 },
      { method: 'GET', url: siteURL + '/demo/assets/picture.png', status: 503 },
      { method: 'POST', url: siteURL + '/demo/', status: 503 },
      { method: 'GET', url: siteURL + '/orders', status: 503 },
      { method: 'GET', url: siteURL + '/admin/status', status: 503 },
      { method: 'GET', url: adminURL + '/demo/', status: 404 },
    ]
    const seen = []
    for (const probe of probes) {
      const reply = await raw(probe.method, probe.url)
      seen.push({ method: probe.method, url: probe.url, status: reply.status, type: reply.headers['content-type'] })
      expect(reply.status, `${probe.method} ${probe.url}`).toBe(probe.status)
      expect(String(reply.headers['content-type'] ?? ''), probe.url).not.toContain('text/html')
      expect(reply.body, probe.url).not.toContain('<!doctype html>')
    }

    const page = await raw('GET', siteURL + '/demo/')
    expect(page.status).toBe(200)
    expect(page.headers['content-security-policy']).toBe(uiPolicy)
    expect(page.headers['x-frame-options']).toBe('DENY')
    expect(page.headers['referrer-policy']).toBe('no-referrer')
    expect(page.headers['x-content-type-options']).toBe('nosniff')
    expect(page.headers['cache-control']).toBe('no-store')

    // Each interface listens only on its own loopback host.
    await expect(raw('GET', 'http://127.0.0.1:9480/demo/')).rejects.toThrow(/ECONNREFUSED/)
    await expect(raw('GET', 'http://127.0.0.2:9440/')).rejects.toThrow(/ECONNREFUSED/)
    expect(await demo.counts()).toEqual({ orders: 0, inventory: 0, reports: 0 })
    record('website-paths', { assets: served, probes: seen })
  })

  test('the administrator establishes the demonstration baseline, which the client then meets', async () => {
    // Before any policy, the gateway admits nothing, and the client says so.
    const locked = await listOrders('staff-01')
    expect(locked.status).toBe(503)
    await expect(locked.region.getByText('The shop could not complete this right now. Try again later.')).toBeVisible()

    await api.genesis(await api.stage(demo.file('deny-all.json')))
    await api.activate(await api.stage(demo.file('baseline.json')), 600)
    await api.resolve('confirm')
    const now = await api.status()
    expect(now).toMatchObject({ state: 'normal', admission: 'ready', overrides: [] })
    expect(canonical((await api.get('/admin/policy')).body.document)).toBe(canonical(JSON.parse(demo.file('baseline.json'))))
    expect(await demo.counts()).toEqual({ orders: 0, inventory: 0, reports: 0 })
    record('baseline', { status: now })
  })

  test('the client reads real data from all three fixtures through the gateway', async () => {
    const generation = (await api.status()).generation!
    const before = await demo.counts()
    const list = await listOrders('staff-01')
    expect(list.status).toBe(200)
    await expect(site.getByRole('table')).toContainText('ORD-1001')
    await shot(site, '02-orders-list')
    const item = await product()
    expect(await shopStatus(item)).toBe(200)
    await expect(item).toContainText('In stock: 15')
    const report = await sales('mgr-01')
    expect(report.status).toBe(200)
    await expect(report.region.getByRole('table')).toContainText('2026-08')
    await expect(site.getByLabel('Signed in as', { exact: true })).toHaveValue('Manager')
    const expected = {
      orders: [{ operation: 'orders.list', subject: 'staff-01', roles: 'STAFF' }],
      inventory: [{ operation: 'inventory.get', subject: 'staff-01', roles: 'STAFF' }],
      reports: [
        { operation: 'reports.sales', subject: 'mgr-01', roles: 'MANAGER' },
        { operation: 'reports.summary', subject: 'mgr-01', roles: 'MANAGER' },
      ],
    }
    const evidence: Record<string, unknown> = {}
    for (const [service, want] of Object.entries(expected)) {
      const receipts = await received(service, before[service])
      expect(receipts, service).toHaveLength(want.length)
      for (const target of want) {
        const receipt = receipts.find((r) => r.operation === target.operation)!
        expect(receipt).toMatchObject({ ...target, status: 200, generation })
        const events = demo.events(receipt.request_id)
        expect(events.map((e) => e.kind)).toEqual(['request.admitted', 'request.forwarded'])
        expect(events[0]).toMatchObject({ subject: target.subject, operation: target.operation, generation })
      }
      evidence[service] = receipts
    }
    // Detailed technical traces remain in the inspector. Assert the same
    // admitted path for each service there as well as the shop's real data.
    for (const [slot, subject, group, title, apiName] of [
      ['Staff', 'staff-01', 'Authentication', 'Valid token', 'Orders'],
      ['Staff', 'staff-01', 'Authorisation', 'Role with a permit', 'Inventory'],
      ['Manager', 'mgr-01', 'Authorisation', 'Role without a permit', 'Reports'],
    ] as const) {
      await useSlot(site, slot, token(subject))
      await scenarioGroup(site, group)
      const region = operation(site, title)
      expect((await sendScenario(region)).status).toBe(200)
      await inspect(region)
      expect(await hops(region)).toEqual(['Browser sent', 'Gateway admitted', `${apiName} API answered 200`])
      await expectCleared(region)
      await expect(region.getByText(`the ${apiName.toLowerCase()} API, through the gateway`)).toBeVisible()
    }
    expect(await siteContext.cookies()).toHaveLength(0)
    const requests = await Promise.all(siteRequests)
    expect(requests.filter((r) => !r.url.startsWith(siteURL + '/'))).toEqual([])
    expect(requests.filter((r) => r.cookie !== undefined)).toEqual([])
    expect(requests.filter((r) => new URL(r.url).pathname.startsWith('/admin'))).toEqual([])
    record('reads-three-fixtures', evidence)
    record('site-requests', { count: requests.length, hosts: [...new Set(requests.map((r) => new URL(r.url).host))] })
  })

  test('a permitted write changes the fixture state exactly once', async () => {
    const change = await adjust('staff-01', '-3')
    expect(change.status).toBe(200)
    await expect(change.region.getByText('Stock updated to 12.')).toBeVisible()
    await shot(site, '03-inventory-write')
    expect(change.receipts).toHaveLength(1)
    expect(change.receipts[0]).toMatchObject({
      operation: 'inventory.adjust',
      status: 200,
      effect: 'ITM-002 stock 15->12',
      subject: 'staff-01',
    })
    expect(await stock('ITM-002')).toBe(12)
    const readBack = await product()
    expect(await shopStatus(readBack)).toBe(200)
    await expect(readBack).toContainText('In stock: 12')
    record('write-once', { receipt: change.receipts[0], stock: 12 })
  })

  test('the same operation is allowed for STAFF and refused for CUSTOMER, before the fixture', async () => {
    // A Customer token in the Staff slot deliberately leaves the controls
    // visible. Only the Gateway decides whether the write can proceed.
    const denied = await adjust('cust-001', '-1')
    expect(denied.status).toBe(403)
    await expect(denied.region).toContainText("The Gateway did not allow this; nothing was sent to the shop's systems.")
    const deniedEvents = demo.events(await shownRequestId(denied.region.locator('td').last()))
    expect(deniedEvents).toHaveLength(1)
    expect(deniedEvents[0]).toMatchObject({
      kind: 'request.rejected',
      stage: 'RBAC',
      status: '403',
      subject: 'cust-001',
      operation: 'inventory.adjust',
    })
    expect(denied.receipts).toHaveLength(0)
    expect(await stock('ITM-002')).toBe(12)
    await shot(site, '04-forbidden')
    await scenarioGroup(site, 'Authorisation')
    const trace = operation(site, 'Role with a permit')
    expect((await sendScenario(trace)).status).toBe(403)
    await inspect(trace)
    expect(await hops(trace)).toEqual(['Browser sent', 'Gateway refused 403', 'Inventory API not forwarded'])
    await expect(trace.getByText('operation access or role permits', { exact: true })).toBeVisible()
    await expectTrace(trace, deniedEvents[0].stage)
    expect((await traceStates(trace)).filter((s) => s.state === 'may have refused it').map((s) => s.check)).toEqual([
      'Operation access',
      'Role permit',
    ])
    const allowed = await adjust('staff-01', '-1')
    expect(allowed.status).toBe(200)
    expect(allowed.receipts).toHaveLength(1)
    expect(allowed.receipts[0]).toMatchObject({ subject: 'staff-01', effect: 'ITM-002 stock 12->11' })
    expect(await stock('ITM-002')).toBe(11)
    const other = await lookupOrder('cust-001', 'ORD-1002')
    expect(other.status).toBe(404)
    await expect(other.region.getByText('No order with that number in your account.')).toBeVisible()
    expect(other.receipts).toHaveLength(1)
    expect(other.receipts[0]).toMatchObject({ operation: 'orders.get', status: 404, subject: 'cust-001' })
    await scenarioGroup(site, 'Authorisation')
    const ownership = operation(site, 'Another customer’s order')
    expect((await sendScenario(ownership)).status).toBe(404)
    await inspect(ownership)
    expect(await hops(ownership)).toEqual(['Browser sent', 'Gateway admitted', 'Orders API answered 404'])
    await expectCleared(ownership)
    await expect(ownership.getByRole('alert').getByText('Not found (the orders API: order_not_found)')).toBeVisible()
    record('two-slots', { denied: deniedEvents[0], allowed: allowed.receipts[0] })
  })

  test('the authentication scenarios: missing, malformed, altered and expired tokens are refused before any fixture', async () => {
    const since = (await demo.receipts('orders')).length
    await useSlot(site, 'Staff', token('cust-001'))
    await scenarioGroup(site, 'Authentication')
    const valid = operation(site, 'Valid token')
    expect(await sendScenario(valid)).toEqual({ status: 200, asExpected: true })
    // The expectation names staff-01; the page says the active caller differs.
    await expect(valid.getByText('Expected for staff-01; the active caller is cust-001.')).toBeVisible()

    const reasons: Record<string, string> = {}
    for (const [title, reason] of [
      ['No token', 'missing_credential'],
      ['Malformed token', 'malformed_credential'],
      ['Altered signature', 'invalid_signature'],
    ]) {
      const region = operation(site, title)
      expect(await sendScenario(region), title).toEqual({ status: 401, asExpected: true })
      await inspect(region)
      await expect(region.getByText('token check (JWT)', { exact: true })).toBeVisible()
      expect(await hops(region)).toEqual(['Browser sent', 'Gateway refused 401', 'Orders API not forwarded'])
      if (title === 'No token') await expect(region.locator('.compare-observed')).toContainText('from the gateway, sent with no token')
      const events = demo.events(await shownRequestId(region))
      expect(events[0]).toMatchObject({ kind: 'request.rejected', stage: 'JWT', reason })
      await expectTrace(region, events[0].stage)
      expect((await traceStates(region)).find((s) => s.check === 'Token')?.state).toBe('refused the request')
      reasons[title] = events[0].reason
    }
    await shot(site, '06-authentication')

    // The expired-token scenario waits for a token past its own expiry time.
    const expiredScenario = operation(site, 'Expired token')
    await expect(expiredScenario.getByText('The active token’s own expiry time has not passed.')).toBeVisible()
    const expired = demo.token('cust-001', '-1m')
    handedOut.push(expired)
    await useSlot(site, 'Customer', expired)
    await site.getByRole('banner').getByRole('button', { name: 'Manage sign-ins', exact: true }).click()
    await expect(site.getByRole('dialog')).toContainText('cust-001')
    await expect(site.getByRole('dialog')).toContainText('expired')
    await site.getByRole('dialog').getByRole('button', { name: 'Done', exact: true }).click()
    await scenarioGroup(site, 'Authentication')
    await expect(expiredScenario.getByText('The active token’s own expiry time has passed.')).toBeVisible()
    expect(await sendScenario(expiredScenario)).toEqual({ status: 401, asExpected: true })
    await inspect(expiredScenario)
    const lateEvents = demo.events(await shownRequestId(expiredScenario))
    expect(lateEvents[0]).toMatchObject({ stage: 'JWT', reason: 'expired' })
    reasons.expired = lateEvents[0].reason

    // A public operation needs no token at all.
    expect(await sendScenario(operation(site, 'Public operation without a token'))).toEqual({ status: 200, asExpected: true })

    // The same refusals from a client other than the browser.
    const validToken = token('cust-001')
    const signature = validToken.split('.')[2]
    const middle = Math.floor(signature.length / 2)
    const alteredToken =
      validToken.slice(0, validToken.length - signature.length + middle) +
      (signature[middle] === 'A' ? 'B' : 'A') +
      signature.slice(middle + 1)
    for (const authorization of [undefined, 'Bearer not-a-token', `Bearer ${alteredToken}`, `Bearer ${expired}`]) {
      const reply = await raw('GET', siteURL + '/orders/ORD-1001', authorization ? { Authorization: authorization } : {})
      expect(reply.status).toBe(401)
      expect(JSON.parse(reply.body).error).toBe('unauthenticated')
      expect(reply.headers['www-authenticate']).toBe('Bearer')
    }

    // Only the valid token reached the Orders API.
    const receipts = await received('orders', since)
    expect(receipts).toHaveLength(1)
    expect(receipts[0]).toMatchObject({ operation: 'orders.get', status: 200, subject: 'cust-001' })
    record('token-refusals', { reasons, fixtureReceipts: receipts.length })
  })

  test('forged identity headers, direct fixture calls and the Admin API give the client no access', async () => {
    const reportsSince = (await demo.receipts('reports')).length
    const ordersSince = (await demo.receipts('orders')).length
    await useSlot(site, 'Staff', token('staff-01'))
    await scenarioGroup(site, 'Authorisation')
    const forged = operation(site, 'Forged identity headers')
    expect(await sendScenario(forged)).toEqual({ status: 403, asExpected: true })
    await inspect(forged)
    // The request as sent shows the forged headers, and never the token.
    const sentText = (await forged.locator('pre.http').first().textContent()) ?? ''
    expect(sentText).toContain('X-Gateway-Subject: mgr-01')
    expect(sentText).toContain('Authorization: Bearer ‹Staff slot (staff-01) token›')
    expect(sentText).not.toContain(token('staff-01'))
    const forgedEvents = demo.events(await shownRequestId(forged))
    expect(forgedEvents[0]).toMatchObject({ stage: 'RBAC', subject: 'staff-01', operation: 'reports.sales' })
    await expectTrace(forged, forgedEvents[0].stage)

    const forgedHeaders = { 'X-Gateway-Subject': 'mgr-01', 'X-Gateway-Roles': 'MANAGER' }
    expect((await raw('GET', siteURL + '/reports/sales', { ...forgedHeaders, Authorization: `Bearer ${token('staff-01')}` })).status).toBe(
      403,
    )
    expect((await raw('GET', siteURL + '/reports/sales', forgedHeaders)).status).toBe(401)
    // An admitted request carries the gateway's identity, not the forged one.
    expect(
      (
        await raw('GET', siteURL + '/orders', {
          'X-Gateway-Subject': 'cust-001',
          'X-Gateway-Roles': 'CUSTOMER',
          Authorization: `Bearer ${token('staff-01')}`,
        })
      ).status,
    ).toBe(200)
    const ordersReceipts = await received('orders', ordersSince)
    expect(ordersReceipts).toHaveLength(1)
    expect(ordersReceipts[0]).toMatchObject({ subject: 'staff-01', roles: 'STAFF' })
    expect(await received('reports', reportsSince)).toHaveLength(0)

    // The client's scripts can reach only their own origin: the content
    // policy blocks a direct call to a fixture or the Admin API before it
    // leaves the browser.
    const attempts = await site.evaluate(
      async (targets) => {
        const results: string[] = []
        for (const url of targets) {
          results.push(
            await fetch(url).then(
              (r) => `answered ${r.status}`,
              (e: unknown) => `blocked: ${String(e)}`,
            ),
          )
        }
        return results
      },
      ['http://127.0.0.1:9503/reports/summary', 'http://127.0.0.1:9440/admin/status'],
    )
    expect(attempts.every((a) => a.startsWith('blocked'))).toBe(true)
    expect(await received('reports', reportsSince)).toHaveLength(0)
    // On the client's own origin, Admin API paths reach enforcement, which
    // has no such operation.
    pageProbes.push(siteURL + '/admin/status')
    const adminOnSite = await site.evaluate(() => fetch('/admin/status').then((r) => r.status))
    expect(adminOnSite).toBe(404)

    // No native Caddy administration endpoint, and the fixture control
    // listeners refuse callers without the secret.
    await expect(raw('GET', 'http://127.0.0.1:2019/config/')).rejects.toThrow(/ECONNREFUSED/)
    for (const control of Object.values(controls)) expect((await raw('GET', control + '/state')).status).toBe(401)
    // Known limitation: on one host, a local process (not the browser page)
    // can still open the fixture's loopback API port.
    const local = await raw('GET', 'http://127.0.0.1:9502/inventory/health')
    record('bypass-probes', {
      forged: forgedEvents[0],
      adminReceipt: ordersReceipts[0],
      attempts,
      adminOnSite,
      localProcessToFixture: local.status,
    })
  })

  test('the authorisation scenarios separate role permits, membership and the API’s own rules, and say when the caller differs', async () => {
    const since = await demo.counts()
    const reasons: Record<string, string> = {}
    const refused = async (subject: string, title: string, reason: string) => {
      await useSlot(site, 'Customer', token(subject))
      await scenarioGroup(site, 'Authorisation')
      const region = operation(site, title)
      expect(await sendScenario(region), title).toEqual({ status: 403, asExpected: true })
      await inspect(region)
      const events = demo.events(await shownRequestId(region))
      expect(events[0], title).toMatchObject({ kind: 'request.rejected', stage: 'RBAC', subject, reason })
      await expectTrace(region, events[0].stage)
      reasons[title] = reason
    }
    await useSlot(site, 'Staff', token('staff-01'))
    await scenarioGroup(site, 'Authorisation')
    expect(await sendScenario(operation(site, 'Role with a permit'))).toEqual({ status: 200, asExpected: true })
    expect(await sendScenario(operation(site, 'Role without a permit'))).toEqual({ status: 403, asExpected: true })
    await refused('nobody-01', 'Subject with no roles', 'no_permit')
    await refused('former-01', 'Disabled subject', 'disabled_subject')
    await refused('visitor-01', 'Subject the policy does not list', 'unknown_subject')

    // The gateway admits CUSTOMER to orders.get; the Orders API's own rule
    // answers for another customer's order.
    await useSlot(site, 'Customer', token('cust-001'))
    await scenarioGroup(site, 'Authorisation')
    const ownership = operation(site, 'Another customer’s order')
    expect(await sendScenario(ownership)).toEqual({ status: 404, asExpected: true })
    await inspect(ownership)
    expect(await hops(ownership)).toEqual(['Browser sent', 'Gateway admitted', 'Orders API answered 404'])

    // The same scenario sent as another caller is reported as not expected,
    // and the page says why.
    const permitted = operation(site, 'Role with a permit')
    expect(await sendScenario(permitted)).toEqual({ status: 403, asExpected: false })
    await expect(permitted.getByText('The expectation is for staff-01, but the request was sent as cust-001.')).toBeVisible()
    await shot(site, '07-authorisation')

    const after = await demo.counts()
    const inventory = await received('inventory', since.inventory)
    expect(inventory).toHaveLength(1)
    expect(inventory[0]).toMatchObject({ operation: 'inventory.get', status: 200, subject: 'staff-01' })
    const orders = await received('orders', since.orders)
    expect(orders).toHaveLength(1)
    expect(orders[0]).toMatchObject({ operation: 'orders.get', status: 404, subject: 'cust-001', roles: 'CUSTOMER' })
    expect(after.reports).toBe(since.reports)
    record('authorisation-scenarios', { reasons, inventory: inventory[0], orders: orders[0] })
  })

  test('the rate limit admits five summary requests and refuses the rest', async () => {
    await restart()
    await useSlot(site, 'Staff', token('staff-01'))
    await scenarioGroup(site, 'Rate limit')
    const since = (await demo.receipts('reports')).length
    const burst = operation(site, 'Burst of summary requests')
    await burst.getByRole('button', { name: 'Send 7 requests' }).click()
    await expect(burst.getByRole('button', { name: 'Send 7 requests' })).toBeEnabled()
    const shown = await runAnswers(burst)
    expect(shown).toEqual(['HTTP 200', 'HTTP 200', 'HTTP 200', 'HTTP 200', 'HTTP 200', 'HTTP 429 rate_limited', 'HTTP 429 rate_limited'])
    await expect(burst.locator('.compare-observed')).toContainText('5 admitted, 2 refused by the rate limit.')
    await expect(burst.locator('.compare-observed .match')).toHaveText('As expected')
    await shot(site, '05-rate-limit')

    const receipts = await received('reports', since)
    expect(receipts).toHaveLength(5)
    expect(receipts.every((r) => r.operation === 'reports.summary' && r.status === 200)).toBe(true)
    const refused = demo.events().filter((e) => e.stage === 'RATE' && e.subject === 'staff-01')
    expect(refused).toHaveLength(2)
    expect(refused.every((e) => e.reason === 'quota_exceeded' && e.status === '429')).toBe(true)
    record('rate-limit', { shown, receipts: receipts.length, refused })
  })

  test('malformed, invalid and oversized bodies, a wrong method, an unknown path and an encoded separator are refused before any fixture', async () => {
    const since = await demo.counts()
    const results: Record<string, unknown> = {}
    const patches: string[] = []
    const onRequest = (r: { method(): string; url(): string }) => {
      if (r.method() === 'PATCH') patches.push(r.url())
    }
    const check = async (title: string, wantStatus: number, wantStage: string, control: string) => {
      const region = operation(site, title)
      expect(await observed(region), title).toEqual({ status: wantStatus, asExpected: true })
      await inspect(region)
      await expect(region.getByText(control, { exact: true }), title).toBeVisible()
      expect((await hops(region))[2], title).toMatch(/not forwarded$/)
      const events = demo.events(await shownRequestId(region))
      expect(events[0], title).toMatchObject({ kind: 'request.rejected', stage: wantStage })
      await expectTrace(region, events[0].stage)
      results[title] = { status: wantStatus, stage: events[0].stage, reason: events[0].reason }
    }

    // One click sends every validation scenario that changes nothing, in
    // order, each once.
    await useSlot(site, 'Staff', token('staff-01'))
    await scenarioGroup(site, 'Validation')
    site.on('request', onRequest)
    await site.getByRole('button', { name: 'Send 8 in a row' }).click()
    await expect(site.getByRole('button', { name: 'Send 8 in a row' })).toBeEnabled()
    site.off('request', onRequest)
    expect(patches).toHaveLength(8)
    await expect(operation(site, 'Valid body').locator('.compare-observed')).toContainText('Not sent yet.')
    await check('Malformed JSON', 400, 'BODY', 'request form or body checks')
    await check('Missing required member', 400, 'VALIDATION', 'request form or body checks')
    await check('Wrong value type', 400, 'VALIDATION', 'request form or body checks')
    await check('Value out of range', 400, 'VALIDATION', 'request form or body checks')
    await check('Undeclared member', 400, 'VALIDATION', 'request form or body checks')
    await check('Repeated member', 400, 'BODY', 'request form or body checks')
    await check('Body too large', 413, 'BODY', 'request form or body checks')
    await check('Wrong media type', 415, 'BODY', 'body checks')
    await shot(site, '06-request-validation')

    await scenarioGroup(site, 'Routing')
    await site.getByRole('button', { name: 'Send 3 in a row' }).click()
    await expect(site.getByRole('button', { name: 'Send 3 in a row' })).toBeEnabled()
    await check('Unknown path', 404, 'ROUTE', 'routing (default deny)')
    await check('Wrong method', 405, 'ROUTE', 'routing')
    await check('Encoded path separator', 400, 'CANONICAL', 'request form or body checks')
    // The gateway's Allow header is shown with the 405.
    await expect(operation(site, 'Wrong method').getByText('Methods allowed here')).toBeVisible()
    await expect(operation(site, 'Wrong method').locator('dd code').filter({ hasText: /^GET$/ })).toBeVisible()

    expect(await demo.counts()).toEqual(since)
    expect(await stock('ITM-001')).toBe(40)

    // The valid body is forwarded once, and the API applies it once.
    await scenarioGroup(site, 'Validation')
    expect(await sendScenario(operation(site, 'Valid body'))).toEqual({ status: 200, asExpected: true })
    const receipts = await received('inventory', since.inventory)
    expect(receipts).toHaveLength(1)
    expect(receipts[0]).toMatchObject({ operation: 'inventory.adjust', status: 200, effect: 'ITM-001 stock 40->39', subject: 'staff-01' })
    expect(await stock('ITM-001')).toBe(39)
    record('body-route-method-refusals', { ...results, valid: receipts[0] })
  })

  test('a validated request header is enforced: missing, wrong or Connection-nominated values never reach the fixture', async () => {
    // The administrator adds a header rule to orders.list in a provisional
    // policy; the client meets it at once.
    const document = JSON.parse(demo.file('baseline.json'))
    const list = document.operations.find((o: { id: string }) => o.id === 'orders.list')
    list.headers = {
      type: 'object',
      additionalProperties: false,
      required: ['X-Client-Version'],
      properties: { 'X-Client-Version': { type: 'string', enum: ['2026-10'] } },
    }
    const provisional = await api.activate(await api.stage(JSON.stringify(document)), 600)
    const since = (await demo.receipts('orders')).length
    const authorization = { Authorization: `Bearer ${token('staff-01')}` }
    const cases = {
      missing: await raw('GET', siteURL + '/orders', authorization),
      wrong: await raw('GET', siteURL + '/orders', { ...authorization, 'X-Client-Version': '1999-01' }),
      nominated: await raw('GET', siteURL + '/orders', { ...authorization, 'X-Client-Version': '2026-10', Connection: 'X-Client-Version' }),
      'gateway header nominated': await raw('GET', siteURL + '/orders', { ...authorization, Connection: 'X-Gateway-Subject' }),
      valid: await raw('GET', siteURL + '/orders', { ...authorization, 'X-Client-Version': '2026-10' }),
    }
    expect(Object.fromEntries(Object.entries(cases).map(([k, v]) => [k, v.status]))).toEqual({
      missing: 400,
      wrong: 400,
      nominated: 400,
      'gateway header nominated': 400,
      valid: 200,
    })
    const reasons: Record<string, string> = {}
    for (const name of ['missing', 'wrong', 'nominated', 'gateway header nominated'] as const) {
      const event = demo.events(JSON.parse(cases[name].body).request_id)[0]
      expect(event.kind, name).toBe('request.rejected')
      reasons[name] = `${event.stage} ${event.reason ?? ''}`.trim()
    }
    expect(reasons).toEqual({
      missing: 'VALIDATION header_mismatch',
      wrong: 'VALIDATION header_mismatch',
      nominated: 'VALIDATION connection_nominated',
      'gateway header nominated': 'INGRESS',
    })
    const receipts = await received('orders', since)
    expect(receipts).toHaveLength(1)
    expect(receipts[0]).toMatchObject({ operation: 'orders.list', status: 200, subject: 'staff-01' })

    // The client sends no such header, so its request is refused the same
    // way, and it says so.
    const refused = await listOrders('staff-01')
    expect(refused.status).toBe(400)
    await expect(refused.region.getByText('The Gateway refused the request as invalid.', { exact: true })).toBeVisible()
    expect(refused.receipts).toHaveLength(0)
    expect(await received('orders', since + 1)).toHaveLength(0)

    // The guided header scenario sends the same request without and with
    // the header, and lists both answers.
    await scenarioGroup(site, 'Policy changes')
    const guidedHeader = operation(site, 'Required request header')
    await guidedHeader.getByRole('checkbox', { name: 'Send the X-Client-Version header' }).uncheck()
    await guidedHeader.getByRole('button', { name: 'Send', exact: true }).click()
    await expect.poll(() => runAnswers(guidedHeader)).toEqual(['HTTP 400 bad_request'])
    await inspect(guidedHeader)
    expect(await hops(guidedHeader)).toEqual(['Browser sent', 'Gateway refused 400', 'Orders API not forwarded'])
    await expectTrace(guidedHeader, 'VALIDATION')
    await guidedHeader.getByRole('checkbox', { name: 'Send the X-Client-Version header' }).check()
    await guidedHeader.getByLabel('Header value').fill('2026-10')
    await guidedHeader.getByRole('button', { name: 'Send', exact: true }).click()
    await expect.poll(() => runAnswers(guidedHeader)).toEqual(['HTTP 400 bad_request', 'HTTP 200'])
    const guidedReceipts = await received('orders', since + 1)
    expect(guidedReceipts).toHaveLength(1)
    expect(guidedReceipts[0]).toMatchObject({ operation: 'orders.list', status: 200, subject: 'staff-01' })
    await shot(site, '08-header-rule')

    await api.resolve('rollback')
    expect((await listOrders('staff-01')).status).toBe(200)
    record('header-validation', {
      provisional,
      statuses: Object.fromEntries(Object.entries(cases).map(([k, v]) => [k, v.status])),
      reasons,
    })
  })

  test('markup in data is shown as text and never runs', async () => {
    const markup = '<img src=x onerror=alert(1)>'
    const lookup = await lookupOrder('staff-01', markup)
    await expect(site.getByLabel('Order number', { exact: true })).toHaveValue(markup)
    const sent = await Promise.all(siteRequests)
    expect(sent.some((request) => request.url.includes('/orders/%3Cimg'))).toBe(true)
    expect(await site.locator('img').count()).toBe(0)
    expect(scriptDialogs).toBe(0)
    record('markup-as-text', { lookupStatus: lookup.status, scriptDialogs })
  })

  test('policy changes made by the administrator take effect in the client at once: provisional, rollback, override and expiry', async () => {
    // A provisional permit lets CUSTOMER read the summary; a rollback ends it.
    expect((await summary('cust-001')).status).toBe(403)
    const provisional = await api.activate(await api.stage(withPermit({ role: 'CUSTOMER', operation: 'reports.summary' })), 600)
    const during = await api.status()
    expect(during).toMatchObject({ provisional, revision: provisional })
    const allowed = await summary('cust-001')
    expect(allowed.status).toBe(200)
    expect(allowed.receipts[0]).toMatchObject({ subject: 'cust-001', roles: 'CUSTOMER', generation: during.generation })
    await api.resolve('rollback')
    const rolledBack = await summary('cust-001')
    expect(rolledBack.status).toBe(403)
    expect(rolledBack.receipts).toHaveLength(0)

    // A containment blocks STAFF from adjusting stock until it is revoked.
    const stockBefore = await stock('ITM-002')
    const containment = await api.createOverride({
      type: 'containment',
      control: 'permit',
      operation: 'inventory.adjust',
      role: 'STAFF',
      expires_in_seconds: 300,
    })
    const contained = await adjust('staff-01', '-1')
    expect(contained.status).toBe(403)
    expect(contained.receipts).toHaveLength(0)
    expect(await stock('ITM-002')).toBe(stockBefore)
    await api.revokeOverride(containment)
    const lifted = await adjust('staff-01', '-1')
    expect(lifted.status).toBe(200)
    expect(lifted.receipts).toHaveLength(1)

    // An exception lets CUSTOMER read sales until the gateway expires it,
    // shown by the guided scenario sent before, during and after it.
    const salesSince = (await demo.receipts('reports')).length
    await useSlot(site, 'Staff', token('cust-001'))
    await scenarioGroup(site, 'Policy changes')
    const guided = operation(site, 'Temporary exception')
    const sendGuided = async (count: number) => {
      await guided.getByRole('button', { name: 'Send', exact: true }).click()
      await expect(guided.locator('table.runs tbody tr')).toHaveCount(count)
      await expect(guided.getByRole('button', { name: 'Send', exact: true })).toBeEnabled()
    }
    await sendGuided(1)
    const exception = await api.createOverride({
      type: 'exception',
      control: 'permit',
      operation: 'reports.sales',
      role: 'CUSTOMER',
      expires_in_seconds: 4,
    })
    await sendGuided(2)
    const granted = { receipts: await received('reports', salesSince) }
    expect(granted.receipts).toHaveLength(1)
    expect(granted.receipts[0].generation).toContain('+o')
    await expect.poll(async () => (await api.override(exception)).status, { timeout: 20_000 }).toBe('expired')
    await sendGuided(3)
    expect(await runAnswers(guided)).toEqual(['HTTP 403 forbidden', 'HTTP 200', 'HTTP 403 forbidden'])
    expect(await received('reports', salesSince)).toHaveLength(1)
    await shot(site, '09-policy-change')
    record('policy-changes', {
      provisional,
      during,
      allowed: allowed.receipts[0],
      containment,
      lifted: lifted.receipts[0],
      exception,
      granted: granted.receipts[0],
    })
  })

  test('a write whose answer is lost is reported as unknown, sent once and never resent, and late answers are dropped', async () => {
    const before = await stock('ITM-002')
    await useSlot(site, 'Staff', token('staff-01'))
    await shopPage(site, 'Stock')
    const since = (await demo.receipts('inventory')).length
    const patches: string[] = []
    const onRequest = (r: { method(): string; url(): string }) => {
      if (r.method() === 'PATCH') patches.push(r.url())
    }
    site.on('request', onRequest)
    let release!: () => void
    const held = new Promise<void>((done) => (release = done))
    await site.route('**/inventory/items/ITM-002', async (route) => {
      if (route.request().method() !== 'PATCH') return route.continue()
      await held
      await route.fetch()
      await route.abort('connectionreset')
    })
    const region = stockRow()
    await region.getByLabel('Change', { exact: true }).fill('1')
    await region.getByRole('button', { name: 'Remove', exact: true }).click()
    await expect(region.getByRole('button', { name: 'Remove', exact: true })).toBeDisabled()
    await expect(region.getByText('Updating stock…')).toBeVisible()
    release()
    await expect(
      region.getByText('No answer. Your stock change may or may not have been applied: check before trying again.'),
    ).toBeVisible()
    expect(await shopStatus(region)).toBe(0)
    await shot(site, '07-unknown-outcome')
    await site.unroute('**/inventory/items/ITM-002')
    await site.waitForTimeout(2000)
    site.off('request', onRequest)
    expect(patches).toHaveLength(1)
    const receipts = await received('inventory', since)
    expect(receipts).toHaveLength(1)
    expect(await stock('ITM-002')).toBe(before - 1)
    await expect(await product()).toContainText(`In stock: ${before - 1}`)
    // The inspector also preserves the technical no-answer path.
    await scenarioGroup(site, 'Authorisation')
    await site.route('**/inventory/items/ITM-001', (route) => route.abort('connectionreset'))
    const unanswered = operation(site, 'Role with a permit')
    expect((await sendScenario(unanswered)).status).toBe(0)
    await inspect(unanswered)
    expect(await hops(unanswered)).toEqual(['Browser sent', 'Gateway no answer', 'Inventory API unknown'])
    await site.unroute('**/inventory/items/ITM-001')
    // Preload both slots before holding the read. The actual switch below
    // uses only the select, so navigation cannot cancel it on our behalf.
    await useSlot(site, 'Manager', token('mgr-01'))
    await useSlot(site, 'Customer', token('cust-001'))
    await shopPage(site, 'My orders')
    let releaseRead!: () => void
    const heldRead = new Promise<void>((done) => (releaseRead = done))
    let lateDone!: () => void
    const late = new Promise<void>((done) => (lateDone = done))
    await site.route('**/orders/ORD-1001', async (route) => {
      await heldRead
      try {
        await route.fulfill({ response: await route.fetch() })
      } catch {
        /* cancelled by slot change */
      }
      lateDone()
    })
    await site.getByLabel('Order number').fill('ORD-1001')
    await site.getByRole('button', { name: 'Find order', exact: true }).click()
    await expect(site.getByText('Reading from the shop…', { exact: true })).toBeVisible()
    await site.getByLabel('Signed in as', { exact: true }).selectOption('Manager')
    releaseRead()
    await late
    await expect(site.getByLabel('Signed in as', { exact: true })).toHaveValue('Manager')
    await expect(site.getByRole('heading', { name: 'All orders', exact: true })).toBeVisible()
    await expect(site.getByLabel('Order number', { exact: true })).toHaveCount(0)
    await site.getByLabel('Signed in as', { exact: true }).selectOption('Customer')
    await expect(site.getByRole('table')).toHaveCount(0)
    await site.unroute('**/orders/ORD-1001')
    record('unknown-and-late', { patchesSent: patches.length, receipts: receipts.length, stock: before - 1 })
  })

  test('an unavailable fixture and an unreachable gateway are reported without claiming the API never received the request', async () => {
    process.kill(demo.pids.reports)
    await expect.poll(() => demo.log()).toContain('fixture reports stopped')
    await useSlot(site, 'Manager', token('mgr-01'))
    await shopPage(site, 'Reports')
    const reportRegion = site.getByRole('region', { name: 'Monthly sales', exact: true })
    const down = { region: reportRegion, status: await shopStatus(reportRegion) }
    expect(down.status).toBe(502)
    await expect(down.region.getByText('The shop could not complete this right now. Try again later.')).toBeVisible()
    await expect(down.region.getByText(/not forwarded|went no further/)).toHaveCount(0)
    const outage = demo.events(await shownRequestId(down.region))
    expect(outage.at(-1)).toMatchObject({ kind: 'request.forwarded', status: '502', operation: 'reports.sales' })
    await shot(site, '08-upstream-failure')
    await scenarioGroup(site, 'Upstream')
    const unavailable = operation(site, 'API unavailable')
    expect(await sendScenario(unavailable)).toEqual({ status: 502, asExpected: true })
    await inspect(unavailable)
    expect(await hops(unavailable)).toEqual(['Browser sent', 'Gateway error 502', 'Reports API not stated by the answer'])
    await expect(unavailable.getByText(/not forwarded|went no further/)).toHaveCount(0)
    await expectCleared(unavailable)
    await expect(
      unavailable.locator('.trace:not(.trace-compact)').getByText('The answer does not say whether the API received it.', { exact: false }),
    ).toBeVisible()
    await useSlot(site, 'Staff', token('staff-01'))
    await demo.stop()
    await shopPage(site, 'Stock')
    const region = stockRow()
    expect(await shopStatus(region)).toBe(0)
    await expect(region.getByText('No answer from the shop.')).toBeVisible()
    await region.getByLabel('Change', { exact: true }).fill('1')
    await region.getByRole('button', { name: 'Remove', exact: true }).click()
    await expect(
      region.getByText('No answer. Your stock change may or may not have been applied: check before trying again.'),
    ).toBeVisible()
    await restart()
    record('failures', { outage: outage.at(-1) })
  })

  test('a gateway that cannot record security events refuses everything, and the client says so', async () => {
    await restart({ fault: 'audit-partial' })
    const refused = await listOrders('staff-01')
    expect(refused.status).toBe(503)
    expect(refused.receipts).toHaveLength(0)
    await expect(refused.region.getByText('The shop could not complete this right now. Try again later.')).toBeVisible()
    const again = await lookupOrder('cust-001', 'ORD-1001')
    expect(again.status).toBe(503)
    expect(again.receipts).toHaveLength(0)
    expect(await api.status()).toMatchObject({ audit_failed: true, admission: 'audit_failed' })
    await restart()
    const admitted = await listOrders('staff-01')
    expect(admitted.status).toBe(200)
    expect(admitted.receipts).toHaveLength(1)
    record('audit-failure', { refused: [refused.status, again.status], recovered: admitted.receipts[0] })
  })

  test('the controlled reset restores the seed and a fixed scenario replays identically', async () => {
    const baseline = JSON.parse(demo.file('baseline.json'))
    const resets: Record<string, unknown>[] = []

    const resetDemonstration = async () => {
      const steps: string[] = []
      for (const o of (await api.get('/admin/overrides')).body.overrides as { id: number }[]) {
        await api.revokeOverride(o.id)
        steps.push(`revoked override ${o.id}`)
      }
      if ((await api.get('/admin/provisional')).status === 200) {
        await api.resolve('rollback')
        steps.push('rolled back the provisional policy')
      }
      if (canonical((await api.get('/admin/policy')).body.document) !== canonical(baseline)) {
        await api.activate(await api.stage(demo.file('baseline.json')), 600)
        await api.resolve('confirm')
        steps.push('re-established the baseline')
      }
      await restart()
      const output = demo.reset()
      expect(output.trim().split('\n')).toHaveLength(3)
      expect(
        output
          .trim()
          .split('\n')
          .every((line) => line.endsWith('seed restored')),
      ).toBe(true)
      for (const [service, seed] of Object.entries(seeds)) expect((await demo.state(service)).sha256).toBe(seed)
      for (const service of Object.keys(seeds)) expect(await demo.receipts(service)).toHaveLength(0)
      const now = await api.status()
      expect(now).toMatchObject({ state: 'normal', overrides: [] })
      expect(now.provisional).toBeUndefined()
      expect(canonical((await api.get('/admin/policy')).body.document)).toBe(canonical(baseline))
      // Reloading the client forgets its tokens; new ones are issued.
      await site.reload()
      await site.getByRole('banner').getByRole('button', { name: 'Manage sign-ins', exact: true }).click()
      for (const slot of ['Customer', 'Staff', 'Manager'])
        await expect(site.getByRole('dialog').getByLabel(`${slot} token`, { exact: true })).toHaveValue('')
      await site.getByRole('dialog').getByRole('button', { name: 'Done', exact: true }).click()
      for (const subject of Object.keys(tokens)) delete tokens[subject]
      resets.push({ steps, status: now })
    }

    const scenario = async () => {
      const outcomes: Record<string, number> = {}
      outcomes['staff-01 lists orders'] = (await listOrders('staff-01')).status
      outcomes['cust-001 gets ORD-1001'] = (await lookupOrder('cust-001', 'ORD-1001')).status
      outcomes['cust-001 adjusts stock'] = (await adjust('cust-001', '-1')).status
      outcomes['staff-01 adjusts stock'] = (await adjust('staff-01', '-2')).status
      const contained = await api.createOverride({
        type: 'containment',
        control: 'permit',
        operation: 'inventory.adjust',
        role: 'STAFF',
        expires_in_seconds: 300,
      })
      outcomes['staff-01 adjusts while contained'] = (await adjust('staff-01', '-1')).status
      await api.revokeOverride(contained)
      outcomes['staff-01 adjusts after revocation'] = (await adjust('staff-01', '-1')).status
      outcomes['mgr-01 reads sales'] = (await sales('mgr-01')).status
      await site.getByLabel('Signed in as', { exact: true }).selectOption('')
      await scenarioGroup(site, 'Authentication')
      outcomes['no token, public health'] = (await sendScenario(operation(site, 'Public operation without a token'))).status
      await useSlot(site, 'Manager', '')
      await shopPage(site, 'Reports')
      outcomes['no token, sales'] = await shopStatus(site.getByRole('region', { name: 'Monthly sales', exact: true }))
      const hashes: Record<string, string> = {}
      for (const service of Object.keys(seeds)) hashes[service] = (await demo.state(service)).sha256
      return { outcomes, stock: await stock('ITM-002'), hashes, receipts: await demo.counts() }
    }

    // Leave the demonstration in a changed state: a confirmed change, an
    // active override and a provisional policy, which the reset must undo.
    await api.activate(await api.stage(withPermit({ role: 'CUSTOMER', operation: 'reports.summary' })), 600)
    await api.resolve('confirm')
    await api.createOverride({
      type: 'exception',
      control: 'permit',
      operation: 'reports.sales',
      role: 'CUSTOMER',
      expires_in_seconds: 600,
    })
    await api.activate(await api.stage(withPermit({ role: 'CUSTOMER', operation: 'inventory.get' })), 600)
    expect((await adjust('staff-01', '-5')).status).toBe(200)

    const runs = []
    for (let run = 0; run < 2; run++) {
      await resetDemonstration()
      runs.push(await scenario())
    }
    expect(resets[0].steps).toEqual([
      expect.stringMatching(/^revoked override \d+$/),
      'rolled back the provisional policy',
      're-established the baseline',
    ])
    expect(runs[0].stock).toBe(12)
    expect(runs[1]).toEqual(runs[0])
    record('reset-and-replay', { resets, runs })
  })

  test('the client passes the automated accessibility rules, works by keyboard and fits narrow screens', async () => {
    const groups = ['Authentication', 'Authorisation', 'Rate limit', 'Validation', 'Routing', 'Upstream', 'Policy changes']
    const results: Record<string, string[]> = {}
    const scan = async (name: string) => {
      const report = await new AxeBuilder({ page: site })
        .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22a', 'wcag22aa'])
        .analyze()
      results[name] = report.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`)
      expect(results[name], name).toEqual([])
    }
    await site.reload()
    for (const subject of Object.keys(tokens)) delete tokens[subject]

    // Fill a sign-in slot, close the dialog and navigate with the keyboard.
    const manage = site.getByRole('banner').getByRole('button', { name: 'Manage sign-ins', exact: true })
    for (let i = 0; i < 25 && !(await manage.evaluate((el) => el === document.activeElement)); i++) await site.keyboard.press('Tab')
    await expect(manage).toBeFocused()
    expect(await manage.evaluate((el) => getComputedStyle(el).outlineStyle)).not.toBe('none')
    await site.keyboard.press('Enter')
    const tokenField = site.getByRole('dialog').getByLabel('Staff token', { exact: true })
    await tokenField.focus()
    await site.keyboard.insertText(token('staff-01'))
    await site.getByRole('dialog').getByRole('button', { name: 'Done', exact: true }).focus()
    await site.keyboard.press('Enter')
    await expect(manage).toBeFocused()
    await site.getByLabel('Signed in as', { exact: true }).focus()
    await site.keyboard.press('Home')
    await site.keyboard.press('ArrowDown')
    await expect(site.getByLabel('Signed in as', { exact: true })).toHaveValue('Staff')
    await site.getByRole('navigation', { name: 'Client pages' }).getByRole('link', { name: 'Orders', exact: true }).focus()
    await site.keyboard.press('Enter')
    await expect(site.getByRole('heading', { level: 1, name: 'All orders' })).toBeFocused()
    await expect(site.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
    expect(await shopStatus(site.locator('main'))).toBe(200)
    const refresh = site.getByRole('button', { name: 'Refresh orders', exact: true })
    await refresh.focus()
    await site.keyboard.press('Enter')
    await expect(site.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
    expect(await shopStatus(site.locator('main'))).toBe(200)
    await shopPage(site, 'Gateway inspector')
    const inspectorURL = site.url()
    await site.getByRole('link', { name: 'Skip to content', exact: true }).focus()
    await site.keyboard.press('Enter')
    await expect(site.locator('main')).toBeFocused()
    expect(site.url()).toBe(inspectorURL)
    await site.getByRole('navigation', { name: 'Scenario groups' }).getByRole('link', { name: 'Routing', exact: true }).focus()
    await site.keyboard.press('Enter')
    await expect(site.getByRole('heading', { level: 2, name: 'Routing' })).toBeFocused()
    for (const page of ['Orders', 'Stock', 'Reports'] as const) {
      await shopPage(site, page)
      await scan(page)
    }
    for (const group of groups) {
      await scenarioGroup(site, group)
      await scan(`Scenarios: ${group}`)
    }
    // With answers shown and opened: refusals, an admission and a run table.
    await scenarioGroup(site, 'Authentication')
    await site.getByRole('button', { name: 'Send 5 in a row' }).click()
    await expect(site.getByRole('button', { name: 'Send 5 in a row' })).toBeEnabled()
    await inspect(operation(site, 'No token'))
    await scan('Authentication with answers')
    await scenarioGroup(site, 'Policy changes')
    await operation(site, 'Temporary exception').getByRole('button', { name: 'Send', exact: true }).click()
    await expect(operation(site, 'Temporary exception').locator('table.runs tbody tr')).toHaveCount(1)
    await scan('Policy changes with answers')

    // No page is wider than the window at phone, tablet and desktop widths.
    const overflow: Record<string, number> = {}
    for (const size of [
      { width: 375, height: 812 },
      { width: 768, height: 1024 },
      { width: 1440, height: 900 },
    ]) {
      await site.setViewportSize(size)
      for (const page of ['Orders', 'Stock', 'Reports', ...groups]) {
        if (groups.includes(page)) await scenarioGroup(site, page)
        else await shopPage(site, page as 'Orders' | 'Stock' | 'Reports')
        if (groups.includes(page)) {
          const first = site.locator('section.scenario').first()
          await first.getByRole('button', { name: /^Send/ }).click()
          await expect(first.getByRole('button', { name: /^Send/ })).toBeEnabled()
          if (await first.locator(':scope > details.inspect').count()) await inspect(first)
        } else {
          const refresh = site.getByRole('button', {
            name: page === 'Orders' ? 'Refresh orders' : page === 'Reports' ? 'Refresh reports' : 'Refresh stock for The Widget',
            exact: true,
          })
          await refresh.click()
          await expect(site.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
        }
        const { extra, offenders } = await site.evaluate(() => {
          const offenders = [...document.querySelectorAll('body *')]
            .filter((el) => !el.parentElement?.closest('pre') && el.getBoundingClientRect().right > window.innerWidth + 0.5)
            .slice(0, 5)
            .map((el) => `${el.tagName.toLowerCase()}.${el.className} ${Math.round(el.getBoundingClientRect().right)}px`)
          return { extra: document.documentElement.scrollWidth - window.innerWidth, offenders }
        })
        overflow[`${size.width} ${page}`] = extra
        expect(extra, `${size.width}px ${page}: ${offenders.join('; ')}`).toBeLessThanOrEqual(0)
        await shot(site, `09-${size.width}-${page.toLowerCase().replace(/ /g, '-')}`)
      }
      if (size.width === 375) await scan('Scenarios at 375px')
    }
    record('accessibility', { results, overflow })
  })

  test('no secret appears in browser storage, URLs, console output, built files, logs or evidence, and no content policy violation occurred', async () => {
    const privateKey = demo.file('issuer-key.pem').split('\n')[1]
    const secrets = [password, ...handedOut, demo.file('fixture.secret').trim(), privateKey]
    const leaks: string[] = []
    const check = (where: string, text: string) => {
      for (const secret of secrets) if (text.includes(secret)) leaks.push(where)
      if (/PRIVATE KEY/.test(text)) leaks.push(`${where}: private key marker`)
    }

    expect(await site.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0)
    for (const details of await site.locator('details').all()) await details.evaluate((d) => ((d as HTMLDetailsElement).open = true))
    check('page text', await site.locator('body').innerText())
    expect(await siteContext.cookies()).toHaveLength(0)
    const requests = await Promise.all(siteRequests)
    for (const r of requests) check(`request ${r.url}`, r.url)
    check('console', consoleLines.join('\n'))
    expect(requests.filter((r) => !r.url.startsWith(siteURL + '/'))).toEqual([])
    expect(requests.filter((r) => new URL(r.url).pathname.startsWith('/admin')).map((r) => r.url)).toEqual(pageProbes)

    const walk = (dir: string): string[] =>
      readdirSync(dir).flatMap((name) => (statSync(join(dir, name)).isDirectory() ? walk(join(dir, name)) : [join(dir, name)]))
    const built = [...walk(resolvePath('dist')), ...walk(join(demo.web, 'demo'))]
    for (const file of built) check(file, readFileSync(file, 'utf8'))
    for (const name of ['audit.log', 'gateway.log', 'orders.log', 'inventory.log', 'reports.log', 'gateway.json'])
      check(name, demo.file(name))
    if (process.env.E2E_EVIDENCE) check('evidence file', readFileSync(process.env.E2E_EVIDENCE, 'utf8'))

    expect(leaks).toEqual([])
    // The two direct calls the bypass test made are the only ones the
    // content policy blocked.
    const violations = consoleLines.filter((line) => /Content Security Policy|Refused to (load|execute|apply|connect)/.test(line))
    expect(violations.every((line) => line.includes('127.0.0.1:9503') || line.includes('127.0.0.1:9440'))).toBe(true)
    expect(violations.length).toBeGreaterThanOrEqual(2)
    const consoleMessages = [...new Set(consoleLines.map((line) => line.replace(/\d+/g, 'N')))]
    record('secret-scan', {
      secretsChecked: secrets.length,
      builtFiles: built.length,
      requests: requests.length,
      consoleMessages,
      scriptDialogs,
    })
    expect(scriptDialogs).toBe(0)
  })
})
