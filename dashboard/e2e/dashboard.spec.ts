import { spawnSync } from 'node:child_process'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { createServer } from 'node:http'
import { dirname, join, resolve as resolvePath } from 'node:path'
import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Browser, type BrowserContext, type Locator, type Page } from '@playwright/test'
import { adminURL, build, Demo, gatewayIdentity, raw, ShortSessions, siteURL, webRoot } from './env'
import { activate, createOverride, dialogButton, enrol, expectLayoutOnlyStorage, go, nav, record, resolve, revisions, revokeOverride, shot, signIn, stage, trial } from './pages'

// One demonstration deployment, driven in order as an operator and its
// administrators would drive it. Every step checks what the dashboard shows
// against independent evidence: the Admin API read directly, the gateway's
// data plane called with test tokens, the fixtures' own receipts and state,
// and the gateway's security events.

const password = 'correct horse battery staple 42'
const uiPolicy =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

let demo: Demo
let enrolmentSecret: string
let adminContext: BrowserContext
let admin: Page
const tokens: Record<string, string> = {}
// Every request the dashboard's pages made, every console line and whether a
// script dialog ever opened.
const adminRequests: string[] = []
const consoleLines: string[] = []
let scriptDialogs = 0
// Every token and one-time secret handed out, for the final secret scan.
const handedOut: string[] = []

function token(subject: string): string {
  if (!tokens[subject]) {
    tokens[subject] = demo.token(subject)
    handedOut.push(tokens[subject])
  }
  return tokens[subject]
}

function watch(context: BrowserContext) {
  context.on('request', (r) => adminRequests.push(r.url()))
  context.on('console', (m) => consoleLines.push(m.text()))
  context.on('page', (page) =>
    page.on('dialog', (dialog) => {
      scriptDialogs++
      dialog.dismiss()
    }),
  )
}

async function newContext(browser: Browser): Promise<BrowserContext> {
  const context = await browser.newContext()
  watch(context)
  return context
}

// adminCall reads or changes state through the Admin API directly, with the
// dashboard context's session, as an independent check of what it shows.
async function adminCall(method: string, path: string, body?: unknown, headers: Record<string, string> = {}) {
  const session = await adminContext.request.get(adminURL + '/admin/session')
  const csrf = session.ok() ? ((await session.json()) as { csrf_token: string }).csrf_token : ''
  const response = await adminContext.request.fetch(adminURL + path, {
    method,
    headers: method === 'GET' ? headers : { Origin: adminURL, 'X-CSRF-Token': csrf, ...headers },
    data: body,
    failOnStatusCode: false,
  })
  const text = await response.text()
  return { status: response.status(), etag: response.headers()['etag'], body: text ? JSON.parse(text) : undefined }
}

interface StatusBody {
  state: string
  admission: string
  audit_failed: boolean
  confirmed?: number
  provisional?: number
  revision?: number
  generation?: string
  overrides?: number[]
}

async function status(): Promise<StatusBody> {
  const reply = await adminCall('GET', '/admin/status')
  expect(reply.status).toBe(200)
  return reply.body as StatusBody
}

// listAll reads every page of an Admin API list.
async function listAll<T>(path: string): Promise<T[]> {
  const items: T[] = []
  let after: string | undefined
  do {
    const reply = await adminCall('GET', `${path}?limit=100${after ? `&after=${encodeURIComponent(after)}` : ''}`)
    expect(reply.status, path).toBe(200)
    items.push(...(reply.body.items as T[]))
    after = reply.body.next
  } while (after)
  return items
}

async function stock(item: string): Promise<number> {
  const state = await demo.state('inventory')
  const items = (state.data as { items: { id: string; stock: number }[] }).items
  return items.find((i) => i.id === item)!.stock
}

// call sends one request through the gateway's data plane as the subject, as
// an ordinary API client would, and returns the status together with the
// receipts the named fixture recorded for it.
async function call(service: string, method: string, path: string, subject: string, body?: unknown) {
  const since = (await demo.receipts(service)).length
  const headers: Record<string, string> = { Authorization: `Bearer ${token(subject)}` }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const reply = await raw(method, siteURL + path, headers, body === undefined ? undefined : JSON.stringify(body))
  return { status: reply.status, receipts: (await demo.receipts(service)).slice(since) }
}

const summary = (subject: string) => call('reports', 'GET', '/reports/summary', subject)
const sales = (subject: string) => call('reports', 'GET', '/reports/sales', subject)
const adjust = (subject: string, delta: number) => call('inventory', 'PATCH', '/inventory/items/ITM-002', subject, { delta })

// propose stages, from Policy revisions, the confirmed policy with one extra
// permit, and returns the change's id.
async function propose(page: Page, permit: { role: string; operation: string }): Promise<number> {
  await revisions(page)
  await page.getByRole('button', { name: 'Start a change from this' }).click()
  const editor = page.getByLabel('Complete policy document (JSON)')
  await expect(editor).not.toHaveValue('')
  const document = JSON.parse(await editor.inputValue())
  document.permits.push(permit)
  return stage(page, { text: JSON.stringify(document, null, 2) })
}

// signedInPage opens the dashboard in a new browser context and signs in.
async function signedInPage(browser: Browser, name: string, secret = password) {
  const context = await newContext(browser)
  const page = await context.newPage()
  await page.goto(adminURL + '/')
  await signIn(page, name, secret)
  return { context, page }
}

// sessionCookie returns the dashboard context's session cookie as a header,
// for requests that must carry it with headers a browser would not send.
async function sessionCookie(context = adminContext): Promise<string> {
  const cookie = (await context.cookies(adminURL)).find((c) => c.name === '__Host-gateway-admin')!
  return `${cookie.name}=${cookie.value}`
}

// auditEvents reads the gateway's audit file. After an interrupted write the
// file holds one incomplete line, which the gateway terminates and names in an
// audit.recovered event at its next start; that line is skipped here.
function auditEvents(): Record<string, unknown>[] {
  const events: Record<string, unknown>[] = []
  for (const line of demo.file('audit.log').split('\n')) {
    if (line.trim() === '') continue
    try {
      events.push(JSON.parse(line) as Record<string, unknown>)
    } catch {
      // the incomplete record
    }
  }
  return events
}

function fact(page: Page, label: string) {
  return page.getByRole('main').getByText(label, { exact: true }).locator('xpath=following-sibling::dd[1]')
}

// strip is the Overview's status strip: policy in force, change on trial,
// temporary rules.
const strip = (page: Page) => page.getByRole('region', { name: 'Policy status' })

// subjectsView opens the version 3 Subjects view, which has no navigation
// item: policy schema version 4 removes subjects.
async function subjectsView(page: Page) {
  await page.evaluate(() => {
    window.location.hash = '#/subjects'
  })
  await expect(page.getByRole('heading', { level: 1, name: 'Subjects', exact: true })).toBeVisible()
  await expect(page.getByText(/^Reading/)).toHaveCount(0)
}

// fontsInUse lists the page's registered faces and whether its body text and
// its code are set in the interface faces.
function fontsInUse(page: Page) {
  return page.evaluate(() => {
    const first = (element: Element | null) => (element ? getComputedStyle(element).fontFamily.split(',')[0].replace(/["']/g, '').trim() : '')
    return {
      faces: [...document.fonts].map((face) => `${face.family.replace(/["']/g, '')}:${face.status}`),
      text: first(document.body) === 'Hanken Grotesk' && document.fonts.check('16px "Hanken Grotesk"'),
      code: first(document.querySelector('code')) === 'Geist Mono' && document.fonts.check('16px "Geist Mono"'),
    }
  })
}

// rowFor finds the table row whose first cell, its identifier, is exactly id.
function rowFor(scope: Locator, id: number) {
  return scope.locator('tbody tr').filter({ has: scope.page().locator('td:first-child', { hasText: new RegExp(`^${id}$`) }) })
}

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    enrolmentSecret = demo.setup()
    handedOut.push(enrolmentSecret)
    await demo.start()
    adminContext = await newContext(browser)
    admin = await adminContext.newPage()
  })

  test.afterAll(async () => {
    await adminContext?.close()
    await demo?.stop()
  })

  test('cold start serves the dashboard and its assets from the admin listener only, under the content policy', async () => {
    const identity = gatewayIdentity()
    record('cold-start', { gateway: identity, pids: Object.keys(demo.pids), web: demo.web })
    expect(Object.keys(demo.pids).sort()).toEqual(['gateway', 'inventory', 'orders', 'reports'])

    await admin.goto(adminURL + '/')
    await expect(admin.getByRole('heading', { level: 1, name: 'Sign in' })).toBeVisible()
    // The state shown is the gateway's own: a new installation admits nothing.
    await expect(admin.getByText('Gateway state: bootstrap_locked')).toBeVisible()
    // The two typefaces load under the content policy, from the page's own
    // script, and the page's text and code are set in them.
    expect(await fontsInUse(admin)).toEqual({ faces: ['Hanken Grotesk:loaded', 'Geist Mono:loaded'], text: true, code: true })
    await shot(admin, '01-sign-in')

    // Every built file is served from /assets/ with a fixed type and the
    // frontends' content policy; none is reachable on the data plane.
    const index = readFileSync(resolvePath('dist', 'index.html'), 'utf8')
    const assets = [...index.matchAll(/(?:src|href)="\/assets\/([^"]+)"/g)].map((m) => m[1])
    expect(assets.map((a) => a.split('.').pop()).sort()).toEqual(['css', 'js', 'svg'])
    const types: Record<string, string> = { js: 'text/javascript; charset=utf-8', css: 'text/css; charset=utf-8', svg: 'image/svg+xml' }
    const served = []
    for (const asset of assets) {
      const reply = await raw('GET', `${adminURL}/assets/${asset}`)
      expect(reply.status, asset).toBe(200)
      expect(reply.headers['content-type'], asset).toBe(types[asset.split('.').pop()!])
      expect(reply.headers['content-security-policy'], asset).toBe(uiPolicy)
      expect(reply.body, asset).toBe(readFileSync(resolvePath('dist', 'assets', asset), 'utf8'))
      const onDataPlane = await raw('GET', `${siteURL}/assets/${asset}`)
      expect(onDataPlane.status, asset).toBe(503)
      served.push({ asset, status: reply.status, dataPlane: onDataPlane.status })
    }

    // API paths answer as the API, never with the dashboard page, and the
    // data plane serves no dashboard.
    const probes = [
      { url: adminURL + '/admin/status', status: 401 },
      { url: adminURL + '/admin/policy', status: 401 },
      { url: adminURL + '/admin/subjects', status: 401 },
      { url: adminURL + '/admin/history', status: 401 },
      { url: adminURL + '/admin/no-such-route', status: 404 },
      { url: adminURL + '/index.html', status: 404 },
      { url: adminURL + '/assets/index.html', status: 404 },
      { url: adminURL + '/assets/no-such-file.js', status: 404 },
      { url: adminURL + '/admin/', status: 404 },
      { url: siteURL + '/', status: 503 },
      { url: siteURL + '/admin/status', status: 503 },
      { url: siteURL + '/demo/../admin/index.html', status: 503 },
    ]
    const seen = []
    for (const probe of probes) {
      const reply = await raw('GET', probe.url)
      seen.push({ url: probe.url, status: reply.status, type: reply.headers['content-type'] })
      expect(reply.status, probe.url).toBe(probe.status)
      expect(String(reply.headers['content-type'] ?? ''), probe.url).not.toContain('text/html')
      expect(reply.body, probe.url).not.toContain('<!doctype html>')
    }

    // The page itself carries no data, needs no session and is never cached.
    const page = await raw('GET', adminURL + '/')
    expect(page.status).toBe(200)
    expect(page.headers['content-security-policy']).toBe(uiPolicy)
    expect(page.headers['x-frame-options']).toBe('DENY')
    expect(page.headers['referrer-policy']).toBe('no-referrer')
    expect(page.headers['x-content-type-options']).toBe('nosniff')
    expect(page.headers['cache-control']).toBe('no-store')
    expect(page.body).not.toMatch(/revision|csrf|subject/i)

    // Each interface listens only on its own loopback host.
    await expect(raw('GET', 'http://127.0.0.2:9440/')).rejects.toThrow(/ECONNREFUSED/)
    await expect(raw('GET', 'http://127.0.0.1:9480/')).rejects.toThrow(/ECONNREFUSED/)
    expect(await demo.counts()).toEqual({ orders: 0, inventory: 0, reports: 0 })
    record('api-paths', { assets: served, probes: seen })
  })

  test('wrong credentials are refused; enrolment and sign-in use the HttpOnly host-only session cookie', async () => {
    // A wrong enrolment secret and a wrong password are refused, and no
    // session cookie is set.
    await admin.getByRole('button', { name: 'First sign-in with an enrolment secret' }).click()
    await admin.getByLabel('Administrator name').fill('admin')
    await admin.getByLabel('Enrolment secret').fill('not-the-enrolment-secret')
    await admin.getByLabel('New password', { exact: true }).fill(password)
    await admin.getByLabel('Repeat the new password').fill(password)
    await admin.getByRole('button', { name: 'Enrol', exact: true }).click()
    const enrolRefusal = admin.getByRole('alert').filter({ hasText: 'Refused (HTTP 401)' })
    await expect(enrolRefusal).toContainText('unauthenticated')
    await admin.getByRole('button', { name: 'Password', exact: true }).click()
    await admin.getByLabel('Administrator name').fill('admin')
    await admin.getByLabel('Password', { exact: true }).fill('wrong password, long enough')
    await admin.getByRole('button', { name: 'Sign in', exact: true }).click()
    const refusal = admin.getByRole('alert').filter({ hasText: 'Refused (HTTP 401)' })
    await expect(refusal).toContainText('unauthenticated')
    await expect(refusal).toContainText('You are not signed in, or the credentials are wrong.')
    await shot(admin, '02-sign-in-refused')
    expect(await adminContext.cookies(adminURL)).toHaveLength(0)

    await enrol(admin, 'admin', enrolmentSecret, password)
    await signIn(admin, 'admin', password)

    const cookies = await adminContext.cookies(adminURL)
    expect(cookies).toHaveLength(1)
    expect(cookies[0]).toMatchObject({ name: '__Host-gateway-admin', httpOnly: true, secure: true, sameSite: 'Strict', path: '/' })
    expect(await admin.evaluate(() => document.cookie)).toBe('')
    await expectLayoutOnlyStorage(admin, [password, ...handedOut, cookies[0].value, String((await adminCall('GET', '/admin/session')).body.csrf_token)])
    // The data-plane host never receives the administrator's cookie.
    expect(await adminContext.cookies(siteURL)).toHaveLength(0)

    // A new installation says how to establish its first policy.
    await go(admin, 'Overview')
    const firstBase = admin.getByRole('region', { name: 'Establish the first policy' })
    await expect(firstBase).toContainText('Stage a complete policy document that denies every operation')
    await expect(firstBase.getByRole('button', { name: 'Go to Changes' })).toBeVisible()
    // The status strip and the not-admitting notice say so in the gateway's words.
    await expect(strip(admin).getByText('Not admitting: bootstrap_locked')).toBeVisible()
    await expect(strip(admin)).toContainText('None yet')
    await expect(admin.getByRole('alert').filter({ hasText: 'The gateway is not admitting requests' })).toContainText(
      'New installation: nothing is admitted until an administrator establishes the first policy (Changes).',
    )
    // Every other page carries the same warning across its top.
    await go(admin, 'Changes')
    await expect(admin.getByRole('status').filter({ hasText: 'The gateway is not admitting requests.' })).toBeVisible()
    await go(admin, 'Overview')
    await shot(admin, '03-overview-bootstrap')

    // The open view is kept across a reload, and the session resumes.
    await revisions(admin)
    expect(new URL(admin.url()).hash).toBe('#/policy')
    await expect(admin.getByText('No confirmed policy is in force.')).toBeVisible()
    await admin.reload()
    await expect(admin.getByText('Signed in as admin')).toBeVisible()
    await expect(admin.getByRole('heading', { level: 1, name: 'Policy revisions' })).toBeVisible()
    await shot(admin, '03-policy-empty')
    record('session-cookie', { cookies: cookies.map((c) => ({ name: c.name, httpOnly: c.httpOnly, secure: c.secure, sameSite: c.sameSite, path: c.path })) })
  })

  test('the first policy denies everything, then the baseline is activated and confirmed', async () => {
    const denyAll = await stage(admin, { file: demo.path('deny-all.json') })
    await expect(admin.getByRole('main').getByText('yes', { exact: true }).first()).toBeVisible()
    await admin.getByRole('button', { name: 'Establish as the first policy' }).click()
    await dialogButton(admin, 'Establish').click()
    await expect(admin.getByText(/^Established: revision 1 is the confirmed policy\.$/)).toBeVisible()
    expect(await status()).toMatchObject({ state: 'normal', confirmed: 1 })
    await go(admin, 'Overview')
    await expect(admin.getByRole('region', { name: 'Establish the first policy' })).toHaveCount(0)
    await expect(strip(admin)).toContainText('Revision 1')
    await expect(strip(admin).getByText('Admitting', { exact: true })).toBeVisible()
    // The APIs view shows every operation closed by the deny-all policy.
    await go(admin, 'APIs')
    const closed = await admin.getByRole('main').getByText('Closed', { exact: true }).count()
    expect(closed).toBe(((await adminCall('GET', '/admin/policy')).body.document.operations as unknown[]).length)

    const baseline = await stage(admin, { file: demo.path('baseline.json') })
    const listChange = admin.getByRole('listitem').filter({ hasText: 'operations: changed orders.list' })
    await expect(listChange.getByRole('listitem')).toHaveText(['access: "deny" → "allow"'])
    await expect(admin.getByText('permits: added STAFF → orders.list')).toBeVisible()
    await shot(admin, '04-candidate-review')
    await admin.getByLabel('Trial length (seconds)').fill('600')
    await admin.getByRole('main').getByRole('button', { name: 'Put on trial', exact: true }).click()
    await expect(admin.getByRole('dialog')).toContainText('Unless someone confirms it within 10 min 0 s, the gateway rolls back to the current confirmed policy, revision 1.')
    await shot(admin, '05-activate-dialog')
    await dialogButton(admin, 'Put on trial').click()
    const done = admin.getByText(/^Revision \d+ is on trial and in force/)
    await expect(done).toBeVisible()
    const provisional = Number((await done.textContent())!.match(/Revision (\d+)/)![1])
    expect(await status()).toMatchObject({ confirmed: 1, provisional, revision: provisional })

    // While it waits, the overview shows both bases and the countdown, and
    // the navigation flags the trial.
    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText(`Revision ${provisional}`)
    await expect(strip(admin)).toContainText('On trial; confirmed revision 1 is the fallback')
    await expect(strip(admin).getByText('Rolls back in')).toBeVisible()
    await expect(strip(admin).getByText(/^\d{2}:\d{2}$/)).toBeVisible()
    await expect(nav(admin, 'Changes')).toContainText('On trial')
    await shot(admin, '06-overview-provisional')
    await go(admin, 'Changes')
    await expect(trial(admin)).toBeVisible()
    await shot(admin, '07-provisional')
    await resolve(admin, 'confirm')
    const after = await status()
    expect(after).toMatchObject({ state: 'normal', admission: 'ready', confirmed: provisional, revision: provisional, overrides: [] })
    expect(after.provisional).toBeUndefined()
    record('genesis-and-baseline', { denyAll, baseline, provisional, status: after })
  })

  test('the overview, policy revisions and subjects views show the controller state as the Admin API reports it, and only traffic figures refresh by themselves', async () => {
    await go(admin, 'Overview')
    const now = await status()
    const main = admin.getByRole('main')
    await expect(strip(admin).getByText('Admitting', { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText(`Revision ${now.confirmed}`)
    await expect(strip(admin).getByText(now.generation!, { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText('None active')
    await expect(strip(admin).getByText('None', { exact: true })).toBeVisible()
    // The policy timeline is the newest administrative history.
    const timeline = main.getByRole('region', { name: 'Policy timeline' })
    await expect(timeline.getByRole('listitem').first()).toContainText(`Revision ${now.confirmed}`)
    await expect(timeline.getByRole('listitem').first()).toContainText('Confirmed')

    // The policy revisions view shows the document the server holds.
    const confirmed = (await adminCall('GET', '/admin/policy')).body as {
      id: number
      document: {
        upstreams: { id: string; dial: string }[]
        operations: { id: string; method: string; target: string; access: string; auth: string; body?: unknown; rate?: { count: number; window_seconds: number } }[]
        roles: { id: string }[]
        permits: { role: string; operation: string }[]
        subjects: { id: string; status: string; roles: string[] }[]
      }
    }
    const doc = confirmed.document
    await shot(admin, '08-overview')

    // The APIs view lists every operation of each API with its limit, from
    // the same document.
    await go(admin, 'APIs')
    for (const o of doc.operations) {
      const row = main.getByRole('row').filter({ has: admin.getByRole('cell', { name: o.id, exact: true }) })
      await expect(row).toContainText(`${o.method} ${o.target}`)
      if (o.rate) await expect(row).toContainText(`${o.rate.count} per ${o.rate.window_seconds} s, per account`)
    }

    await revisions(admin)
    // The page's explanation sits behind its info button.
    await admin.getByRole('button', { name: 'About this page' }).click()
    await expect(main.getByText(`Revision ${confirmed.id},`)).toBeVisible()
    for (const u of confirmed.document.upstreams) await expect(main.getByRole('row').filter({ hasText: u.id })).toContainText(u.dial)
    for (const o of confirmed.document.operations) {
      await expect(main.getByRole('row').filter({ has: admin.getByRole('cell', { name: o.id, exact: true }) })).toContainText(`${o.method} ${o.target}`)
    }
    // The permit matrix has a column per role and, for every operation that
    // needs a token, exactly the permits the document holds.
    const matrix = main.getByRole('region', { name: 'Role permits' })
    for (const r of doc.roles) await expect(matrix.getByRole('columnheader', { name: new RegExp(`^${r.id}\\b`) })).toBeVisible()
    for (const o of doc.operations) {
      const row = matrix.getByRole('row').filter({ has: admin.getByRole('rowheader', { name: o.id, exact: true }) })
      if (o.access !== 'allow') {
        await expect(row).toContainText('closed to everyone by its access flag')
        continue
      }
      if (o.auth === 'none') {
        await expect(row).toContainText('public: no token or role needed')
        continue
      }
      const cells = row.getByRole('cell')
      await expect(cells).toHaveText(doc.roles.map((r) => (doc.permits.some((p) => p.role === r.id && p.operation === o.id) ? /permitted/ : /no permit/)))
    }
    // The request rules state each body field the schema declares.
    const adjustRules = main.getByRole('region', { name: 'Request rules of inventory.adjust' })
    await expect(adjustRules.getByRole('row', { name: /^delta/ })).toHaveText(/delta\s*body\s*integer\s*required\s*-100 to 100/)
    const createRules = main.getByRole('region', { name: 'Request rules of orders.create' })
    await expect(createRules.getByRole('row')).toHaveCount(4)
    await expect(createRules.getByRole('row', { name: /^customer/ })).toContainText('1 to 64 characters')
    await expect(main.getByRole('region', { name: 'Request rules of orders.list' })).toHaveCount(0)
    await shot(admin, '09-policy')
    await subjectsView(admin)
    for (const s of await listAll<{ id: string; status: string; roles: string[] }>('/admin/subjects')) {
      const row = main.getByRole('row').filter({ has: admin.getByRole('cell', { name: s.id, exact: true }) })
      await expect(row).toContainText(s.status)
      for (const role of s.roles) await expect(row).toContainText(role)
      // The operations its roles are permitted in the confirmed document.
      const permitted = [...new Set(doc.permits.filter((p) => s.roles.includes(p.role)).map((p) => p.operation))]
      for (const operation of permitted) {
        if (s.status === 'active') await expect(row.getByText(operation, { exact: true })).toBeVisible()
      }
      if (s.status !== 'active') await expect(row).toContainText(`none while ${s.status}`)
    }
    await expect(main.getByText(/is in force, so what the gateway enforces now can differ/)).toHaveCount(0)
    await shot(admin, '10-subjects')

    // Left open and idle, a policy view sends nothing.
    await go(admin, 'Changes')
    const before = adminRequests.length
    await admin.waitForTimeout(12_000)
    expect(adminRequests.length - before).toBe(0)
    // The Overview rereads only its traffic figures by itself.
    await go(admin, 'Overview')
    const start = adminRequests.length
    await admin.waitForTimeout(12_000)
    const idle = adminRequests.slice(start).map((url) => new URL(url).pathname)
    expect(idle.length).toBeGreaterThan(0)
    expect(idle.filter((path) => path !== '/admin/traffic' && path !== '/admin/events')).toEqual([])
    record('overview', { status: now, idlePolicyRequests: adminRequests.length - before, overviewIdle: [...new Set(idle)], seconds: 12 })
  })

  test('markup in policy documents is shown as text and never runs', async () => {
    const document = JSON.parse(demo.file('baseline.json'))
    const create = document.operations.find((o: { id: string }) => o.id === 'orders.create')
    create.body.schema.properties.item.enum = ['<img src=x onerror=alert(2)>', 'ITM-001', 'ITM-002', 'ITM-003']
    const id = await stage(admin, { text: JSON.stringify(document, null, 2) })
    await admin.getByText('Full document').click()
    await expect(admin.locator('pre').filter({ hasText: '<img src=x onerror=alert(2)>' })).toBeVisible()
    await expect(admin.getByText('operations: changed orders.create')).toBeVisible()
    expect(await admin.locator('img').count()).toBe(0)
    await admin.getByRole('button', { name: 'Withdraw', exact: true }).click()
    await dialogButton(admin, 'Withdraw change').click()
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
    await expect(admin.getByText('permits: added CUSTOMER → reports.summary')).toBeVisible()
    const provisional = await activate(admin, 600)
    const during = await status()
    expect(during).toMatchObject({ confirmed: before.confirmed, provisional, revision: provisional })
    expect(during.generation).toMatch(new RegExp(`^r${provisional}-`))

    await go(admin, 'Changes')
    const view = (await adminCall('GET', '/admin/provisional')).body as { changes: { target: string }[]; activation: { rollback_target: number } }
    expect(view.activation.rollback_target).toBe(before.confirmed)
    expect(view.changes.length).toBeGreaterThan(0)
    await expect(trial(admin).getByRole('heading', { name: `Revision ${provisional} is in force but not confirmed` })).toBeVisible()
    await expect(trial(admin).getByRole('heading', { name: `What changed from revision ${before.confirmed}` })).toBeVisible()
    await expect(trial(admin).getByRole('button', { name: `Roll back to revision ${before.confirmed}` })).toBeVisible()
    await expect(fact(admin, 'Generation served')).toHaveText(during.generation!)
    for (const change of view.changes) await expect(trial(admin).getByRole('listitem').filter({ hasText: change.target })).toBeVisible()
    await expect(trial(admin).getByRole('listitem').filter({ hasText: 'CUSTOMER:reports.summary' })).toHaveText(/^permit\s*CUSTOMER:reports.summary$/)
    await expect(trial(admin).getByText(/^\d{2}:\d{2}$/)).toBeVisible()

    const allowed = await summary('cust-001')
    expect(allowed.status).toBe(200)
    expect(allowed.receipts).toHaveLength(1)
    expect(allowed.receipts[0]).toMatchObject({ subject: 'cust-001', roles: 'CUSTOMER', generation: during.generation })

    await admin.getByRole('main').getByRole('button', { name: `Roll back to revision ${before.confirmed}` }).click()
    await expect(admin.getByRole('dialog')).toContainText(`Revision ${before.confirmed} is enforced again from the next request`)
    await shot(admin, '11-rollback-dialog')
    await dialogButton(admin, `Roll back to revision ${before.confirmed}`).click()
    await expect(admin.getByText(`Rolled back: revision ${before.confirmed} is in force again.`)).toBeVisible()
    const after = await status()
    expect(after).toMatchObject({ confirmed: before.confirmed, revision: before.confirmed, generation: before.generation })
    expect(after.provisional).toBeUndefined()
    const again = await summary('cust-001')
    expect(again.status).toBe(403)
    expect(again.receipts).toHaveLength(0)
    record('cpa-rollback', { before, during, changes: view.changes, receipt: allowed.receipts[0], after })
  })

  test('a confirmed change stays in force, and the baseline is restored the same way', async () => {
    const before = await status()
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const provisional = await activate(admin, 600)
    await resolve(admin, 'confirm')
    const confirmed = await status()
    expect(confirmed).toMatchObject({ confirmed: provisional, revision: provisional })
    expect(confirmed.provisional).toBeUndefined()
    const allowed = await summary('cust-001')
    expect(allowed.status).toBe(200)
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
    await admin.getByRole('button', { name: 'Inspect revision 1', exact: true }).click()
    const detail = admin.getByRole('region', { name: 'Revision 1' })
    await expect(detail.getByRole('heading', { name: 'Revision 1', exact: true })).toBeFocused()
    await expect(fact(admin, 'Origin')).toContainText('first policy (genesis)')
    const listChange = detail.getByRole('listitem').filter({ hasText: 'operations: changed orders.list' })
    await expect(listChange.getByRole('listitem')).toHaveText(['access: "allow" → "deny"'])
    await expect(detail.getByText('subjects: removed staff-01')).toBeVisible()
    await shot(admin, '10b-revision')
    await detail.getByRole('button', { name: 'Start a change from this revision' }).click()
    await expect(admin.getByRole('heading', { level: 1, name: 'Changes' })).toBeVisible()
    const editor = admin.getByLabel('Complete policy document (JSON)')
    expect(JSON.parse(await editor.inputValue())).toEqual(genesis.document)
    // Nothing was sent: the gateway's state and its candidates are unchanged.
    expect(await status()).toEqual(before)
    expect(await listAll('/admin/candidates')).toHaveLength(candidatesBefore)

    // Starting again from the confirmed policy asks before replacing the text.
    await admin.getByRole('button', { name: /^Start from revision \d+$/ }).click()
    await expect(admin.getByRole('dialog')).toContainText('The text in the editor is replaced by confirmed revision')
    await dialogButton(admin, 'Replace it').click()
    expect(JSON.parse(await editor.inputValue())).toEqual((await adminCall('GET', '/admin/policy')).body.document)

    // A changed rate limit is listed member by member in the review.
    const changed = JSON.parse(await editor.inputValue())
    changed.operations.find((o: { id: string }) => o.id === 'reports.summary').rate.count = 10
    await editor.fill(JSON.stringify(changed, null, 2))
    await admin.getByRole('button', { name: 'Stage and check' }).click()
    const staged = admin.getByText(/^The gateway checked it: staged as change \d+/)
    await expect(staged).toBeVisible()
    const id = Number((await staged.textContent())!.match(/change (\d+)/)![1])
    await admin.getByRole('region', { name: 'Stage a change' }).getByRole('button', { name: `Review change ${id}` }).click()
    const summaryChange = admin.getByRole('listitem').filter({ hasText: 'operations: changed reports.summary' })
    await expect(summaryChange.getByRole('listitem')).toHaveText(['rate.count: 5 → 10'])
    await admin.getByRole('button', { name: 'Withdraw', exact: true }).click()
    await dialogButton(admin, 'Withdraw change').click()
    await expect(admin.getByText(`Withdrawn: change ${id} can no longer be put on trial.`)).toBeVisible()
    expect(await status()).toEqual(before)
    record('revision-inspection', { candidate: id, status: before })
  })

  test('an unconfirmed change rolls back by itself at its deadline', async () => {
    const before = await status()
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const provisional = await activate(admin, 3)
    expect((await summary('cust-001')).status).toBe(200)

    await go(admin, 'Changes')
    // The countdown only prompts one reread; the gateway's own timeout is what
    // the page then shows.
    await expect(trial(admin)).toHaveCount(0, { timeout: 20_000 })
    const history = admin.getByRole('region', { name: 'Past trials' })
    await expect(rowFor(history, provisional)).toContainText('Rolled back at deadline')
    await expect(nav(admin, 'Changes')).not.toContainText('On trial')
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

  test('concurrent sessions cannot overwrite each other and a lost answer is resolved by resending', async ({ browser }) => {
    const other = await signedInPage(browser, 'admin')

    // One session confirms; the other's rollback, based on what it read
    // earlier, is refused and shown as refused.
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const first = await activate(admin, 600)
    await go(other.page, 'Changes')
    const target = (await status()).confirmed
    await expect(other.page.getByRole('button', { name: `Roll back to revision ${target}` })).toBeVisible()
    await resolve(admin, 'confirm')
    await other.page.getByRole('main').getByRole('button', { name: `Roll back to revision ${target}` }).click()
    await dialogButton(other.page, `Roll back to revision ${target}`).click()
    const refusal = other.page.getByRole('alert').filter({ hasText: /Refused \(HTTP 4\d\d\)/ })
    await expect(refusal).toBeVisible()
    const refusalText = await refusal.textContent()
    await expect(other.page.getByText(/^Rolled back:/)).toHaveCount(0)
    await shot(other.page, '13-stale-refusal')
    expect(await status()).toMatchObject({ confirmed: first, revision: first })

    // A confirmation whose answer is lost: the gateway applies it, the page
    // says the outcome is unknown, and resending the identical request returns
    // the recorded receipt without a second transition.
    await stage(admin, { file: demo.path('baseline.json') })
    const second = await activate(admin, 600)
    let lostReceipt: { transition_id?: string } = {}
    await admin.route('**/admin/provisional/confirm', async (route) => {
      const response = await route.fetch()
      lostReceipt = await response.json()
      await route.abort('connectionreset')
    })
    await go(admin, 'Changes')
    await admin.getByRole('main').getByRole('button', { name: `Confirm revision ${second}` }).click()
    await dialogButton(admin, `Confirm revision ${second}`).click()
    await expect(admin.getByText('Outcome unknown.')).toBeVisible()
    await expect(admin.getByText(/^Confirmed:/)).toHaveCount(0)
    await shot(admin, '14-outcome-unknown')
    expect(await status()).toMatchObject({ confirmed: second })
    await admin.unroute('**/admin/provisional/confirm')
    await admin.getByRole('button', { name: 'Send the identical request again' }).click()
    await expect(admin.getByText(`Confirmed: revision ${second} is now the permanent policy.`)).toBeVisible()
    // Changes also shows the earlier staging and trial receipts; this is the
    // confirmation's own.
    const receipt = admin.getByRole('main').locator('.notice', { hasText: `Confirmed: revision ${second} is now the permanent policy.` })
    const shownTransition = await receipt.getByText('Transition', { exact: true }).locator('xpath=following-sibling::dd[1]').textContent()
    expect(shownTransition).toBe(lostReceipt.transition_id)
    const confirms = (await listAll<{ kind: string; target: string }>('/admin/history')).filter(
      (h) => h.kind === 'cpa.confirm' && h.target === `revision ${second}`,
    )
    expect(confirms).toHaveLength(1)
    await other.context.close()
    record('concurrency-and-lost-answer', { refusal: refusalText, lostReceipt, shownTransition, confirmTransitions: confirms.length })
  })

  test('a temporary containment blocks STAFF without changing the confirmed base, and revocation lifts it', async () => {
    const before = await status()
    const id = await createOverride(admin, { action: 'Containment: stop a role calling an operation', operation: 'inventory.adjust', role: 'STAFF', seconds: 300 })
    const during = await status()
    expect(during).toMatchObject({ confirmed: before.confirmed, revision: before.confirmed, overrides: [id] })
    expect(during.generation).toContain('+o')
    await shot(admin, '15-overrides-active')

    const row = rowFor(admin.getByRole('main').getByRole('region', { name: 'In force' }), id)
    await expect(row).toContainText('Containment')
    await expect(row).toContainText('STAFF may not call inventory.adjust')

    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText('1 active')
    await expect(strip(admin)).toContainText('STAFF may not call inventory.adjust')
    await expect(strip(admin).getByText(during.generation!, { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText(`Revision ${before.confirmed}`)
    await expect(nav(admin, 'Temporary rules')).toContainText('1')
    await shot(admin, '16-overview-override')
    await subjectsView(admin)
    await expect(
      admin.getByText(
        `A temporary override is in force, so what the gateway enforces now can differ from the permitted operations below, which are those of confirmed revision ${before.confirmed}.`,
      ),
    ).toBeVisible()

    const stockBefore = await stock('ITM-002')
    const contained = await adjust('staff-01', -1)
    expect(contained.status).toBe(403)
    expect(contained.receipts).toHaveLength(0)
    expect(await stock('ITM-002')).toBe(stockBefore)

    // A second override on the same rule is refused as a conflict.
    await go(admin, 'Temporary rules')
    await admin.getByLabel('Action', { exact: true }).selectOption({ label: 'Containment: stop a role calling an operation' })
    await admin.getByLabel('Operation', { exact: true }).selectOption('inventory.adjust')
    await admin.getByLabel('Role', { exact: true }).selectOption('STAFF')
    await admin.getByRole('button', { name: 'Check with the gateway' }).click()
    await expect(admin.getByRole('alert').filter({ hasText: 'OVERRIDE_CONFLICT' })).toBeVisible()
    expect((await status()).overrides).toEqual([id])

    await revokeOverride(admin, id)
    const after = await status()
    expect(after).toMatchObject({ confirmed: before.confirmed, overrides: [] })
    const lifted = await adjust('staff-01', -1)
    expect(lifted.status).toBe(200)
    expect(lifted.receipts).toHaveLength(1)
    expect(lifted.receipts[0].generation).toBe(after.generation)
    expect(await stock('ITM-002')).toBe(stockBefore - 1)
    record('override-containment', { id, during, contained: contained.status, after, lifted: lifted.receipts[0] })
  })

  test('a temporary exception lets CUSTOMER read sales until the gateway expires it', async () => {
    expect((await sales('cust-001')).status).toBe(403)
    const id = await createOverride(admin, { action: 'Exception: let a role call an operation', operation: 'reports.sales', role: 'CUSTOMER', seconds: 4 })
    const allowed = await sales('cust-001')
    expect(allowed.status).toBe(200)
    expect(allowed.receipts[0].generation).toContain('+o')

    await go(admin, 'Temporary rules')
    await expect(admin.getByText('No temporary rule is in force. The policy applies exactly as confirmed.')).toBeVisible({ timeout: 20_000 })
    const history = admin.getByRole('region', { name: 'Past rules' })
    await expect(rowFor(history, id)).toContainText('expired')
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
    await go(admin, 'Temporary rules')
    await expect(admin.getByText('A change is on trial: the gateway refuses new temporary rules until it is confirmed or rolled back.')).toBeVisible()
    // The incident quick actions are unavailable while a change is on trial.
    for (const action of await admin.getByRole('group', { name: 'Incident quick actions' }).getByRole('button').all()) await expect(action).toBeDisabled()
    await admin.getByLabel('Action', { exact: true }).selectOption({ label: 'Containment: block an operation for everyone' })
    await admin.getByLabel('Operation', { exact: true }).selectOption('orders.list')
    await expect(admin.getByText('This rule: orders.list blocked for everyone, for 5 min 0 s.')).toBeVisible()
    await admin.getByRole('button', { name: 'Check with the gateway' }).click()
    await expect(admin.getByRole('alert').filter({ hasText: 'PROVISIONAL_ACTIVE' })).toBeVisible()
    await shot(admin, '18-override-refused')
    expect((await status()).overrides).toEqual([])
    await resolve(admin, 'rollback')
    record('override-during-provisional', { status: await status() })
  })

  test('an invalid candidate is refused with the field named and changes nothing', async () => {
    const before = await status()
    const candidates = (await listAll('/admin/candidates')).length
    const results: Record<string, string> = {}

    // A document that is not JSON is stopped by the page before sending.
    await go(admin, 'Changes')
    await admin.getByLabel('Complete policy document (JSON)').fill('{"schema_version": 3,')
    await expect(admin.getByText('This is not valid JSON.')).toBeVisible()
    await admin.getByRole('button', { name: 'Stage and check' }).click()

    const upstream = JSON.parse(demo.file('baseline.json'))
    upstream.upstreams[0].dial = '10.0.0.5:80'
    const missing = JSON.parse(demo.file('baseline.json'))
    delete missing.operations
    const unsupported = JSON.parse(demo.file('baseline.json'))
    unsupported.operations[0].method = 'TRACE'
    for (const [name, text] of [
      ['non-loopback upstream', JSON.stringify(upstream)],
      ['missing operations', JSON.stringify(missing)],
      ['unsupported method', JSON.stringify(unsupported)],
    ]) {
      await go(admin, 'Changes')
      await admin.getByLabel('Complete policy document (JSON)').fill(text)
      await admin.getByRole('button', { name: 'Stage and check' }).click()
      const refusal = admin.getByRole('region', { name: 'Stage a change' }).getByRole('alert')
      await expect(refusal).toContainText('Refused (HTTP 400)')
      results[name] = (await refusal.innerText()) ?? ''
      if (name === 'non-loopback upstream') await shot(admin, '19-candidate-refused')
    }
    expect(results['non-loopback upstream']).toContain('INVALID_UPSTREAM')
    expect(results['non-loopback upstream']).toContain('An upstream must be a literal loopback address')
    expect(results['non-loopback upstream']).toContain('Field: upstreams[0].dial')
    expect(results['unsupported method']).toContain('Field: operations[0].method')
    expect(await listAll('/admin/candidates')).toHaveLength(candidates)
    expect(await status()).toEqual(before)
    record('invalid-candidates', results)
  })

  test('subject membership changes publish a revision the next request uses, and stale edits are refused', async ({ browser }) => {
    expect((await adjust('nobody-01', -1)).status).toBe(403)

    await subjectsView(admin)
    const main = admin.getByRole('main')
    const publish = async (page: Page) => {
      await page.getByRole('button', { name: 'Review and publish' }).click()
      await dialogButton(page, 'Publish').click()
    }
    // published waits for a receipt naming a revision newer than `previous`
    // and returns it; it must be the confirmed revision the server reports.
    const published = async (page: Page, previous: number) => {
      const text = page.getByText(/^Membership change published: from the next request the gateway uses revision \d+\.$/)
      let revision = 0
      await expect
        .poll(async () => {
          const shown = (await text.count()) > 0 ? await text.textContent() : ''
          revision = Number(shown?.match(/revision (\d+)/)?.[1] ?? 0)
          return revision > previous
        })
        .toBe(true)
      expect((await status()).confirmed).toBe(revision)
      return revision
    }

    const start = (await status()).confirmed!
    await main.getByRole('button', { name: 'Change nobody-01' }).click()
    await main.getByRole('checkbox', { name: 'STAFF' }).check()
    await shot(admin, '20-subject-editor')
    await admin.getByRole('button', { name: 'Review and publish' }).click()
    await expect(admin.getByRole('dialog')).toContainText('none → STAFF')
    await shot(admin, '21-publish-dialog')
    await dialogButton(admin, 'Publish').click()
    const granted = await published(admin, start)
    const afterGrant = await status()
    expect(afterGrant).toMatchObject({ confirmed: granted, revision: granted })
    await expect(main.getByRole('row').filter({ hasText: 'nobody-01' })).toContainText('STAFF')
    const allowed = await adjust('nobody-01', -1)
    expect(allowed.status).toBe(200)
    expect(allowed.receipts[0]).toMatchObject({ subject: 'nobody-01', roles: 'STAFF', generation: afterGrant.generation })

    // A second session reads the list, then the first session changes it.
    const other = await signedInPage(browser, 'admin')
    await subjectsView(other.page)
    const otherMain = other.page.getByRole('main')
    await expect(otherMain.getByRole('row').filter({ hasText: 'cust-002' })).toBeVisible()

    await main.getByRole('checkbox', { name: 'STAFF' }).uncheck()
    await publish(admin)
    const removed = await published(admin, granted)

    await otherMain.getByRole('button', { name: 'Change cust-002' }).click()
    await otherMain.getByLabel('Status').selectOption('disabled')
    await publish(other.page)
    const stale = otherMain.getByRole('alert').filter({ hasText: /Refused \(HTTP 4\d\d\)/ })
    await expect(stale).toBeVisible()
    const staleText = await stale.textContent()
    expect((await status()).confirmed).toBe(removed)
    expect((await adminCall('GET', '/admin/subjects/cust-002')).body.status).toBe('active')

    // After a refresh the same change is based on the current list.
    await other.page.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(otherMain.getByRole('row').filter({ hasText: 'nobody-01' })).toContainText('none')
    await publish(other.page)
    const disabled = await published(other.page, removed)
    expect((await adminCall('GET', '/admin/subjects/cust-002')).body.status).toBe('disabled')
    await otherMain.getByLabel('Status').selectOption('active')
    await publish(other.page)
    await published(other.page, disabled)
    expect((await adminCall('GET', '/admin/subjects/cust-002')).body.status).toBe('active')
    await other.context.close()

    const revoked = await adjust('nobody-01', -1)
    expect(revoked.status).toBe(403)
    expect(revoked.receipts).toHaveLength(0)
    record('subjects', { granted, receipt: allowed.receipts[0], removed, stale: staleText, final: await status() })
  })

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

    await go(admin, 'History')
    const table = admin.getByRole('region', { name: 'Administrative history' })
    const rows = table.locator('tbody tr')
    await expect(rows).toHaveCount(100)
    await expect(admin.getByText(/^Showing 100 entries\. The gateway lists the oldest first and more exist/)).toBeVisible()
    await admin.getByRole('button', { name: 'Load all' }).click()
    await expect(rows).toHaveCount(entries.length)
    await expect(admin.getByText(`Showing ${entries.length} entries, the whole history.`)).toBeVisible()
    await expect(admin.getByRole('button', { name: 'Load all' })).toHaveCount(0)
    const kinds = [...new Set(entries.map((e) => e.kind))]
    for (const kind of kinds) await expect(table.getByText(kind, { exact: true }).first()).toBeVisible()
    await shot(admin, '22-history')

    // The filters work on the entries read, and the order can be reversed.
    await admin.getByLabel('Area').selectOption('overrides')
    const overrides = entries.filter((e) => e.kind.startsWith('tpo.'))
    expect(overrides.length).toBeGreaterThan(0)
    await expect(rows).toHaveCount(overrides.length)
    await expect(admin.getByText(`Showing ${overrides.length} of the ${entries.length} entries read, the whole history.`)).toBeVisible()
    await admin.getByLabel('Area').selectOption('all')
    await admin.getByLabel('Outcome').selectOption('rejected')
    const rejected = entries.filter((e) => e.outcome === 'rejected')
    if (rejected.length > 0) await expect(rows).toHaveCount(rejected.length)
    else await expect(admin.getByText('No entry read so far matches these filters.')).toBeVisible()
    await admin.getByLabel('Outcome').selectOption('all')
    await admin.getByLabel('Newest first').check()
    await expect(rows.first().locator('td').first()).toHaveText(String(Math.max(...entries.map((e) => e.seq))))
    await admin.getByLabel('Area').selectOption('policy')
    await shot(admin, '22b-history-filtered')
    await admin.getByLabel('Area').selectOption('all')
    await admin.getByLabel('Newest first').uncheck()
    await expect(rows.first().locator('td').first()).toHaveText(String(entries[0].seq))

    await table.getByRole('button', { name: 'Details of entry 1', exact: true }).click()
    const entry = admin.getByRole('region', { name: /^Transition / })
    await expect(entry).toContainText(entries[0].transition_id)
    await expect(entry.getByText('Kind', { exact: true }).locator('xpath=following-sibling::dd[1]')).toHaveText(entries[0].kind)
    await shot(admin, '23-history-entry')
    const shown = await admin.locator('body').innerText()
    for (const secret of [password, ...handedOut, demo.file('fixture.secret').trim()]) expect(shown).not.toContain(secret)
    record('history', { entries: entries.length, kinds, overrides: overrides.length, rejected: rejected.length })
  })

  test('the main screens pass the automated accessibility rules, work by keyboard and fit narrow screens', async ({ browser }) => {
    const results: Record<string, string[]> = {}
    const scan = async (page: Page, name: string) => {
      const report = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22a', 'wcag22aa']).analyze()
      results[name] = report.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`)
      expect(results[name], name).toEqual([])
    }
    const tabTo = async (page: Page, target: ReturnType<Page['getByLabel']>) => {
      for (let i = 0; i < 20; i++) {
        await page.keyboard.press('Tab')
        if (await target.evaluate((el) => el === document.activeElement)) return
      }
      throw new Error('keyboard focus never reached the field')
    }

    const context = await newContext(browser)
    const page = await context.newPage()
    await page.goto(adminURL + '/')
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    await scan(page, 'sign-in')

    // Field errors are text, linked to their field.
    await page.getByRole('button', { name: 'First sign-in with an enrolment secret' }).click()
    const newPassword = page.getByLabel('New password', { exact: true })
    await newPassword.fill('short')
    await expect(page.getByText('Use 15 to 256 bytes.')).toBeVisible()
    expect(await newPassword.getAttribute('aria-describedby')).toContain('-error')
    await scan(page, 'first sign-in with an error')
    await shot(page, '24-enrol-error')
    await page.getByRole('button', { name: 'Password', exact: true }).click()

    // Sign in with the keyboard only; focus is visible on each control.
    const name = page.getByLabel('Administrator name')
    await tabTo(page, name)
    expect(await name.evaluate((el) => getComputedStyle(el).outlineStyle)).not.toBe('none')
    await page.keyboard.type('admin')
    await page.keyboard.press('Tab')
    await page.keyboard.type(password)
    await page.keyboard.press('Enter')
    await expect(page.getByText('Signed in as admin')).toBeVisible()

    // Moving between views by keyboard puts focus on the new view's heading.
    await nav(page, 'Changes').focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('heading', { level: 1, name: 'Changes' })).toBeFocused()

    // Every view in the navigation. Views whose Admin API endpoints this
    // gateway does not serve show its refusal, which must be accessible too.
    const views = ['Overview', 'Live traffic', 'Request lookup', 'APIs', 'Policy', 'Changes', 'Temporary rules', 'Accounts', 'History', 'Policy as code', 'Team', 'My account']
    for (const view of views) {
      await go(page, view)
      await scan(page, view)
    }
    // The page explanation behind the info button.
    await go(page, 'Changes')
    await page.getByRole('button', { name: 'About this page' }).click()
    await expect(page.getByText(/^A change is a complete proposed policy\./)).toBeVisible()
    await scan(page, 'page explanation')
    // The review panels: an inspected revision, and the history filtered.
    await revisions(page)
    await scan(page, 'Policy revisions')
    await page.getByRole('button', { name: 'Inspect revision 1', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Revision 1', exact: true })).toBeFocused()
    await scan(page, 'revision detail')
    await subjectsView(page)
    await scan(page, 'Subjects')
    await go(page, 'History')
    await page.getByLabel('Area').selectOption('policy')
    await page.getByLabel('Newest first').check()
    await scan(page, 'history filtered')
    await go(page, 'My account')
    await shot(page, '25-account')

    // A confirmation dialog takes focus and Escape cancels it without acting.
    const before = await status()
    await subjectsView(page)
    await page.getByRole('button', { name: 'Remove cust-002' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await scan(page, 'confirmation dialog')
    await shot(page, '26-remove-dialog')
    expect(await page.evaluate(() => document.activeElement?.textContent)).toBe('Cancel')
    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog')).toHaveCount(0)
    expect(await status()).toEqual(before)

    // No view is wider than the window at phone, tablet and desktop widths;
    // wide tables scroll inside their own region. On a narrow screen the
    // views are behind a menu button.
    const overflow: Record<string, number> = {}
    for (const size of [
      { width: 375, height: 812 },
      { width: 768, height: 1024 },
      { width: 1440, height: 900 },
    ]) {
      await page.setViewportSize(size)
      const narrow = size.width <= 960
      for (const view of views) {
        if (narrow) {
          const menu = page.getByRole('button', { name: 'Menu' })
          await expect(menu).toHaveAttribute('aria-expanded', 'false')
          await expect(nav(page, view)).toBeHidden()
          await menu.click()
          await expect(menu).toHaveAttribute('aria-expanded', 'true')
        }
        await go(page, view)
        if (narrow) await expect(page.getByRole('button', { name: 'Menu' })).toHaveAttribute('aria-expanded', 'false')
        const { extra, offenders } = await page.evaluate(() => {
          // Elements that reach past the window, outside the regions that
          // scroll on their own.
          const offenders = [...document.querySelectorAll('body *')]
            .filter((el) => !el.parentElement?.closest('.table-scroll, pre') && el.getBoundingClientRect().right > window.innerWidth + 0.5)
            .slice(0, 5)
            .map((el) => `${el.tagName.toLowerCase()}.${el.className} ${Math.round(el.getBoundingClientRect().right)}px`)
          return { extra: document.documentElement.scrollWidth - window.innerWidth, offenders }
        })
        overflow[`${size.width} ${view}`] = extra
        expect(extra, `${size.width}px ${view}: ${offenders.join('; ')}`).toBeLessThanOrEqual(0)
        await shot(page, `27-${size.width}-${view.toLowerCase().replace(/ /g, '-')}`)
      }
      if (size.width === 375) {
        await scan(page, 'My account at 375px')
        await page.getByRole('button', { name: 'Menu' }).click()
        await shot(page, '28-375-menu-open')
        await scan(page, 'menu open at 375px')
        await page.getByRole('button', { name: 'Menu' }).click()
      }
    }
    await context.close()
    record('accessibility', { results, overflow })
  })

  test('other origins, missing CSRF tokens, wrong hosts and API tokens cannot use the Admin API', async () => {
    const before = await status()
    const cookie = await sessionCookie()
    const csrf = ((await adminCall('GET', '/admin/session')).body as { csrf_token: string }).csrf_token
    const etag = (await adminCall('GET', '/admin/subjects')).etag!
    const change = JSON.stringify({ status: 'disabled', roles: [] })
    const put = (headers: Record<string, string>) =>
      raw('PUT', adminURL + '/admin/subjects/cust-002', { 'Content-Type': 'application/json', 'If-Match': etag, 'Idempotency-Key': crypto.randomUUID(), ...headers }, change)

    const cases = {
      'website origin': await put({ Cookie: cookie, Origin: siteURL, 'X-CSRF-Token': csrf }),
      'no origin': await put({ Cookie: cookie, 'X-CSRF-Token': csrf }),
      'no CSRF token': await put({ Cookie: cookie, Origin: adminURL }),
      'wrong CSRF token': await put({ Cookie: cookie, Origin: adminURL, 'X-CSRF-Token': 'x'.repeat(csrf.length) }),
      'wrong host': await put({ Cookie: cookie, Origin: adminURL, 'X-CSRF-Token': csrf, Host: 'localhost:9440' }),
      'API token, no session': await put({ Authorization: `Bearer ${token('mgr-01')}`, Origin: adminURL }),
    }
    expect(Object.fromEntries(Object.entries(cases).map(([k, v]) => [k, v.status]))).toEqual({
      'website origin': 403,
      'no origin': 403,
      'no CSRF token': 403,
      'wrong CSRF token': 403,
      'wrong host': 400,
      'API token, no session': 401,
    })
    expect((await raw('GET', adminURL + '/admin/status', { Authorization: `Bearer ${token('mgr-01')}` })).status).toBe(401)
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
    expect((await adminCall('GET', '/admin/subjects/cust-002')).body.status).toBe('active')
    const refused = auditEvents().filter((e) => (e.kind === 'admin.rejected' || e.kind === 'admin.denied') && typeof e.path === 'string')
    const reasons = (path: string) => refused.filter((e) => e.path === path).map((e) => e.reason)
    expect(reasons('/admin/subjects/cust-002')).toEqual(expect.arrayContaining(['bad_origin', 'csrf', 'bad_host', 'no_session']))
    // The session cookie is SameSite=Strict, so the other site's requests
    // arrived without it. Refusals are written to the audit file shortly after
    // the answer.
    const fromAttacker = (path: string) =>
      auditEvents()
        .filter((e) => e.path === path && e.kind !== 'admin.read' && Date.parse(String(e.time)) >= attackStarted)
        .map((e) => `${e.kind} ${e.reason}`)
    await expect.poll(() => fromAttacker('/admin/provisional/rollback')).toEqual(['admin.rejected no_session'])
    await expect.poll(() => fromAttacker('/admin/status')).toEqual(['admin.rejected no_session'])
    record('admin-api-refusals', { statuses: Object.fromEntries(Object.entries(cases).map(([k, v]) => [k, v.status])), read, reasons: reasons('/admin/subjects/cust-002') })
  })

  test('a wrong current password keeps the session; a password change ends every session of the administrator', async ({ browser }) => {
    const other = await signedInPage(browser, 'admin')
    const otherCookie = await sessionCookie(other.context)
    const changed = 'a different correct horse battery staple'

    await admin.getByRole('navigation', { name: 'Administration' }).getByRole('link', { name: 'My account' }).click()
    await admin.getByLabel('Current password').fill('not the current password at all')
    await admin.getByLabel('New password', { exact: true }).fill(changed)
    await admin.getByLabel('Repeat the new password').fill(changed)
    await admin.getByRole('button', { name: 'Change password' }).click()
    // The gateway refuses the change (401) but the session goes on: the page
    // stays signed in and says what was wrong.
    await expect(admin.getByText('The current password is not correct.')).toBeVisible()
    await expect(admin.getByText('Signed in as admin')).toBeVisible()
    await shot(admin, '29-password-refused')
    expect((await raw('GET', adminURL + '/admin/session', { Cookie: await sessionCookie() })).status).toBe(200)

    await admin.getByLabel('Current password').fill(password)
    await admin.getByLabel('New password', { exact: true }).fill(changed)
    await admin.getByLabel('Repeat the new password').fill(changed)
    await admin.getByRole('button', { name: 'Change password' }).click()
    await expect(admin.getByText('Password changed. Every session of this administrator has ended; sign in with the new password.')).toBeVisible()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    // The other session ended too, at the gateway and in its page.
    expect((await raw('GET', adminURL + '/admin/session', { Cookie: otherCookie })).status).toBe(401)
    await other.page.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(other.page.getByText('Your session has ended. Sign in again.')).toBeVisible()
    await other.context.close()

    // The old password no longer works; the new one does. It is then set back.
    await admin.getByLabel('Administrator name').fill('admin')
    await admin.getByLabel('Password', { exact: true }).fill(password)
    await admin.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(admin.getByRole('alert').filter({ hasText: 'Refused (HTTP 401)' })).toBeVisible()
    await signIn(admin, 'admin', changed)
    await admin.getByRole('navigation', { name: 'Administration' }).getByRole('link', { name: 'My account' }).click()
    await admin.getByLabel('Current password').fill(changed)
    await admin.getByLabel('New password', { exact: true }).fill(password)
    await admin.getByLabel('Repeat the new password').fill(password)
    await admin.getByRole('button', { name: 'Change password' }).click()
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
    await second.goto(adminURL + '/#/subjects')
    await expect(second.getByText('Signed in as admin')).toBeVisible()
    await expect(second.getByRole('heading', { level: 1, name: 'Subjects' })).toBeVisible()

    await admin.getByRole('button', { name: 'Sign out' }).click()
    await expect(admin.getByText('Signed out.')).toBeVisible()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    expect((await raw('GET', adminURL + '/admin/status', { Cookie: cookie })).status).toBe(401)

    // The other tab keeps what it last read until it asks the gateway again;
    // the next read is refused and the tab drops every privileged view.
    await second.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(second.getByText('Your session has ended. Sign in again.')).toBeVisible()
    await expect(second.getByText('Signed in as admin')).toHaveCount(0)
    await expect(second.getByRole('navigation', { name: 'Administration' })).toHaveCount(0)
    await expect(second.getByRole('cell', { name: 'cust-002' })).toHaveCount(0)
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
    const expiring = await createOverride(admin, { action: 'Exception: let a role call an operation', operation: 'reports.sales', role: 'CUSTOMER', seconds: 5 })
    const expiresAt = Date.parse((await adminCall('GET', `/admin/overrides/${expiring}`)).body.expires_at)
    await go(admin, 'Overview')
    await demo.stop()
    await admin.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(admin.getByText('No answer from the gateway: the server could not be reached.').first()).toBeVisible()
    await expect(admin.getByText('Signed in as admin')).toBeVisible()
    await shot(admin, '30-unreachable')
    while (Date.now() < expiresAt + 1000) await admin.waitForTimeout(250)
    await demo.start()
    // Restarting ended every session.
    await admin.reload()
    await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    await expect(admin.getByText('Gateway state: normal')).toBeVisible()
    await signIn(admin, 'admin', password)
    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText(`Revision ${confirmed}`)
    expect(await status()).toMatchObject({ state: 'normal', confirmed, overrides: [] })
    await expect(strip(admin)).toContainText('None active')
    expect((await adminCall('GET', `/admin/overrides/${expiring}`)).body.status).toBe('expired')
    expect((await sales('cust-001')).status).toBe(403)
    expect((await sales('mgr-01')).status).toBe(200)

    const viewerSecret = await demo.enrol('viewer', 'read')
    handedOut.push(viewerSecret)
    const viewerContext = await newContext(browser)
    const viewer = await viewerContext.newPage()
    await viewer.goto(adminURL + '/')
    await enrol(viewer, 'viewer', viewerSecret, password)
    await signIn(viewer, 'viewer', password)
    await expect(nav(viewer, 'History')).toHaveCount(0)
    await go(viewer, 'Changes')
    await expect(viewer.getByRole('heading', { name: 'Staged changes' })).toBeVisible()
    await expect(viewer.getByRole('region', { name: 'Stage a change' })).toHaveCount(0)
    await subjectsView(viewer)
    await expect(viewer.getByRole('row').filter({ hasText: 'cust-002' })).toBeVisible()
    await expect(viewer.getByRole('button', { name: 'Add a subject' })).toHaveCount(0)
    await expect(viewer.getByRole('button', { name: /^Change / })).toHaveCount(0)
    await revisions(viewer)
    await expect(viewer.getByRole('button', { name: 'Start a change from this' })).toHaveCount(0)
    // No block form, quick action or key reload without the permissions.
    await go(viewer, 'Accounts')
    await expect(viewer.getByRole('heading', { name: 'Block an account' })).toHaveCount(0)
    await go(viewer, 'Team')
    await expect(viewer.getByRole('button', { name: 'Reload keys from settings' })).toHaveCount(0)
    await go(viewer, 'Temporary rules')
    await expect(viewer.getByRole('heading', { name: 'In force' })).toBeVisible()
    await expect(viewer.getByRole('heading', { name: 'Add a temporary rule' })).toHaveCount(0)
    await expect(viewer.getByRole('group', { name: 'Incident quick actions' })).toHaveCount(0)
    await expect(viewer.getByRole('heading', { name: 'Past rules' })).toHaveCount(0)
    await shot(viewer, '31-viewer-overrides')

    // Hiding the buttons is only a convenience: the server refuses the change.
    const session = (await (await viewerContext.request.get(adminURL + '/admin/session')).json()) as { csrf_token: string }
    const list = await viewerContext.request.get(adminURL + '/admin/subjects')
    const attempt = await viewerContext.request.put(adminURL + '/admin/subjects/cust-002', {
      headers: { Origin: adminURL, 'X-CSRF-Token': session.csrf_token, 'If-Match': list.headers()['etag'], 'Idempotency-Key': crypto.randomUUID() },
      data: { status: 'disabled', roles: [] },
      failOnStatusCode: false,
    })
    expect(attempt.status()).toBe(403)
    expect((await viewerContext.request.get(adminURL + '/admin/history', { failOnStatusCode: false })).status()).toBe(403)
    await viewerContext.close()

    await admin.reload()
    await signIn(admin, 'admin', password)
    expect(await status()).toMatchObject({ confirmed })
    expect((await adminCall('GET', '/admin/subjects/cust-002')).body.status).toBe('active')
    const denied = auditEvents()
      .filter((e) => e.kind === 'admin.denied' && e.actor === 'viewer')
      .map((e) => `${e.method} ${e.path} ${e.reason}`)
    expect(denied).toEqual(expect.arrayContaining(['PUT /admin/subjects/cust-002 permission', 'GET /admin/history permission']))
    record('outage-restart-viewer', { confirmed, viewerDenied: denied })
  })

  test('a failing security event log is shown as such, nothing is admitted, and a restart recovers', async () => {
    const failure = 'The security event log cannot be written, so every protected request is refused.'
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
    await expect(main.getByText('Admitting', { exact: true })).toBeVisible()
    await expect(admin.getByText(failure)).toHaveCount(0)

    const refused = await call('orders', 'GET', '/orders', 'staff-01')
    expect(refused.status).toBe(503)
    expect(refused.receipts).toHaveLength(0)
    const again = await call('orders', 'GET', '/orders/ORD-1001', 'cust-001')
    expect(again.status).toBe(503)
    expect(again.receipts).toHaveLength(0)

    // The dashboard shows the failure from the server's status, after a read.
    expect(await status()).toMatchObject({ audit_failed: true, admission: 'audit_failed' })
    await admin.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(main.getByRole('alert').filter({ hasText: failure })).toBeVisible()
    await expect(main.getByText('Not admitting: audit_failed', { exact: true })).toBeVisible()
    await shot(admin, '32-audit-failed')
    // Every other page says so across its top.
    await go(admin, 'Changes')
    await expect(admin.getByRole('status').filter({ hasText: 'The gateway is not admitting requests.' })).toContainText(failure)
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
    const admitted = await call('orders', 'GET', '/orders', 'staff-01')
    expect(admitted.status).toBe(200)
    expect(admitted.receipts).toHaveLength(1)
    await expect.poll(() => auditEvents().filter((e) => e.kind === 'audit.recovered' && Date.parse(String(e.time)) >= startedAt).length).toBe(1)
    record('audit-failure', { refused: [refused.status, again.status], recovered: admitted.receipts[0] })
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
    await expect(admin.getByText('Gateway state: recovery_required')).toBeVisible()
    // The earlier administrator no longer gets in.
    await admin.getByLabel('Administrator name').fill('admin')
    await admin.getByLabel('Password', { exact: true }).fill(password)
    await admin.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(admin.getByRole('alert').filter({ hasText: 'Refused (HTTP 401)' })).toBeVisible()
    await enrol(admin, 'recovery', secret, password)
    await signIn(admin, 'recovery', password)

    // The overview says what state this is and how to leave it, and shows no
    // policy in force.
    await go(admin, 'Overview')
    const main = admin.getByRole('main')
    await expect(main.getByText('Not admitting: recovery_required', { exact: true })).toBeVisible()
    await expect(main.getByText('Restored from a backup: nothing is admitted until an administrator establishes a recovery base (Changes).')).toBeVisible()
    const panel = admin.getByRole('region', { name: 'Establish a recovery base' })
    await expect(panel).toBeVisible()
    await expect(strip(admin)).toContainText('None yet')
    await shot(admin, '32b-overview-recovery')
    expect(await status()).toMatchObject({ state: 'recovery_required', admission: 'recovery_required' })
    const locked = await call('orders', 'GET', '/orders', 'staff-01')
    expect(locked.status).toBe(503)
    expect(locked.receipts).toHaveLength(0)

    // The recovery base is a confirmed policy that denies everything.
    await panel.getByRole('button', { name: 'Go to Changes' }).click()
    await expect(admin.getByRole('heading', { level: 1, name: 'Changes' })).toBeVisible()
    await stage(admin, { file: demo.path('deny-all.json') })
    await admin.getByRole('button', { name: 'Establish as the recovery base' }).click()
    await expect(admin.getByRole('dialog')).toContainText('becomes the confirmed recovery base')
    await dialogButton(admin, 'Establish').click()
    const established = admin.getByText(/^Established: revision \d+ is the confirmed policy\.$/)
    await expect(established).toBeVisible()
    const after = await status()
    expect(after).toMatchObject({ state: 'normal', admission: 'ready' })
    await go(admin, 'Overview')
    await expect(admin.getByRole('region', { name: 'Establish a recovery base' })).toHaveCount(0)
    await expect(strip(admin)).toContainText(`Revision ${after.confirmed}`)
    const denied = await call('orders', 'GET', '/orders', 'staff-01')
    expect(denied.status).toBe(403)
    expect(denied.receipts).toHaveLength(0)

    // The history names both steps.
    await go(admin, 'History')
    const loadAll = admin.getByRole('button', { name: 'Load all' })
    if (await loadAll.isVisible()) await loadAll.click()
    await expect(admin.getByRole('region', { name: 'Administrative history' }).getByText('Restored from a backup by the operator')).toBeVisible()
    await expect(admin.getByRole('region', { name: 'Administrative history' }).getByText('Recovery base established')).toBeVisible()
    record('recovery-base', { before, locked: locked.status, after, denied: denied.status })
  })

  test('an idle session and a session past its lifetime end, and the dashboard says so', async ({ browser }) => {
    const sessions = new ShortSessions(demo.bin, demo, 4, 12)
    const secret = sessions.install()
    handedOut.push(secret)
    await sessions.start()
    try {
      const context = await newContext(browser)
      const page = await context.newPage()
      await page.goto(ShortSessions.url + '/')
      await enrol(page, 'admin', secret, password)
      await signIn(page, 'admin', password)
      await expect(page.getByText('New installation: nothing is admitted until an administrator establishes the first policy (Changes).')).toBeVisible()

      // Idle: no request for longer than the idle timeout ends the session.
      const idleStart = Date.now()
      await page.waitForTimeout(sessions.idleSeconds * 1000 + 1500)
      await page.getByRole('button', { name: 'Refresh', exact: true }).click()
      await expect(page.getByText('Your session has ended. Sign in again.')).toBeVisible()
      await expect(page.getByRole('navigation', { name: 'Administration' })).toHaveCount(0)
      const idleEnded = Date.now() - idleStart
      await shot(page, '33-session-ended')

      // Lifetime: a session kept busy still ends at its absolute deadline.
      await signIn(page, 'admin', password)
      const signedInAt = Date.now()
      const cookie = await sessionCookie(context)
      let alive = true
      while (alive) {
        await page.waitForTimeout(1000)
        alive = (await raw('GET', ShortSessions.url + '/admin/session', { Cookie: cookie })).status === 200
        expect(Date.now() - signedInAt, 'the session outlived its lifetime').toBeLessThan((sessions.lifetimeSeconds + 4) * 1000)
      }
      const lifetimeEnded = Date.now() - signedInAt
      expect(lifetimeEnded).toBeGreaterThanOrEqual(sessions.lifetimeSeconds * 1000 - 1000)
      await page.getByRole('button', { name: 'Refresh', exact: true }).click()
      await expect(page.getByText('Your session has ended. Sign in again.')).toBeVisible()
      await context.close()
      record('session-expiry', { idleSeconds: sessions.idleSeconds, idleEndedMs: idleEnded, lifetimeSeconds: sessions.lifetimeSeconds, lifetimeEndedMs: lifetimeEnded })
    } finally {
      await sessions.stop()
    }
  })

  test('no secret appears in browser storage, URLs, console output, built files, logs or evidence, and no content policy violation occurred', async () => {
    const privateKey = demo.file('issuer-key.pem').split('\n')[1]
    const secrets = [password, ...handedOut, demo.file('fixture.secret').trim(), privateKey]
    const leaks: string[] = []
    const check = (where: string, text: string) => {
      for (const secret of secrets) if (text.includes(secret)) leaks.push(where)
      if (/PRIVATE KEY/.test(text)) leaks.push(`${where}: private key marker`)
    }

    await expectLayoutOnlyStorage(admin, secrets)
    for (const url of adminRequests) check(`request ${url}`, url)
    check('console', consoleLines.join('\n'))

    const walk = (dir: string): string[] =>
      readdirSync(dir).flatMap((name) => (statSync(join(dir, name)).isDirectory() ? walk(join(dir, name)) : [join(dir, name)]))
    const built = [...walk(resolvePath('dist')), ...walk(join(demo.web, 'admin'))]
    for (const file of built) check(file, readFileSync(file, 'utf8'))
    for (const name of ['audit.log', 'gateway.log', 'orders.log', 'inventory.log', 'reports.log', 'gateway.json']) check(name, demo.file(name))
    if (process.env.E2E_EVIDENCE) check('evidence file', readFileSync(process.env.E2E_EVIDENCE, 'utf8'))

    expect(leaks).toEqual([])
    const violations = consoleLines.filter((line) => /Content Security Policy|Refused to (load|execute|apply|connect)/.test(line))
    expect(violations).toEqual([])
    const consoleMessages = [...new Set(consoleLines.map((line) => line.replace(/\d+/g, 'N')))]
    record('secret-scan', { secretsChecked: secrets.length, builtFiles: built.length, requests: adminRequests.length, consoleMessages, scriptDialogs })
    expect(scriptDialogs).toBe(0)
  })
})
