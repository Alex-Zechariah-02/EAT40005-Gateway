import { spawnSync } from 'node:child_process'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { createServer } from 'node:http'
import { dirname, join, resolve as resolvePath } from 'node:path'
import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Browser, type BrowserContext, type Locator, type Page } from '@playwright/test'
import { adminURL, applicationDist, build, Demo, gatewayIdentity, raw, ShortSessions, siteURL, webRoot, type Receipt } from './env'
import { activate, confirmTrial, createOverride, dialogButton, enrol, expectLayoutOnlyStorage, go, moreDetail, nav, record, resolve, revisions, revokeOverride, shot, signIn, stage, trial, trialStep, versionsPart } from './pages'

// One demonstration deployment, driven in order as an operator and its
// administrators would drive it. Every step checks what the dashboard shows
// against independent evidence: the Admin API read directly, the gateway's
// data plane called with test tokens, the Fetch services' own receipts and
// state, and the gateway's security events.

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
  const items = (state.data as { items: { item: { id: string }; stock: number }[] }).items
  return items.find((i) => i.item.id === item)!.stock
}

// call sends one request through the gateway's data plane as the subject, as
// an ordinary API client would, and returns the status together with the
// receipts the named service recorded for it.
async function call(service: string, method: string, path: string, subject: string, body?: unknown) {
  const since = (await demo.receipts(service)).length
  const headers: Record<string, string> = { Authorization: `Bearer ${token(subject)}` }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const reply = await raw(method, siteURL + path, headers, body === undefined ? undefined : JSON.stringify(body))
  return { status: reply.status, receipts: (await demo.receipts(service)).slice(since) }
}

// The Fetch baseline lets only OPERATIONS read the platform reports, and
// OPERATIONS (ops-01) adjust any shop's stock; customers may do neither.
// admitted checks that a granted request went through: the gateway forwarded
// it once, and the service, which leaves role decisions to the gateway,
// answered it.
function admitted(reply: { status: number; receipts: Receipt[] }) {
  expect(reply.status).toBe(200)
  expect(reply.receipts).toHaveLength(1)
  expect(reply.receipts[0].status).toBe(200)
}

const summary = (subject: string) => call('reports', 'GET', '/reports/summary', subject)
const sales = (subject: string) => call('reports', 'GET', '/reports/sales', subject)
const adjust = (subject: string, delta: number) => call('inventory', 'PATCH', '/inventory/stock/ITM-501', subject, { delta })

// propose stages, from Versions, the confirmed policy with one extra
// permit, and returns the change's id.
async function propose(page: Page, permit: { role: string; operation: string }): Promise<number> {
  await revisions(page)
  await page.getByRole('button', { name: 'Start a Change from This' }).click()
  const editor = page.getByLabel('Complete Policy Document (JSON)')
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

// strip is the Overview's status strip: active policy, change on trial,
// temporary rules.
const strip = (page: Page) => page.getByRole('region', { name: 'Policy Status' })

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
    expect(Object.keys(demo.pids).sort()).toEqual(['accounts', 'gateway', 'inventory', 'orders', 'reports', 'support'])

    await admin.goto(adminURL + '/')
    await expect(admin.getByRole('heading', { level: 1, name: 'Sign in' })).toBeVisible()
    // The state shown is the gateway's own: a new installation admits nothing.
    await expect(admin.locator('.entry-state')).toContainText('Gateway: New installation, waiting for its first policy')
    await expect(admin.locator('.entry-state code')).toHaveText('bootstrap_locked')
    // The two typefaces load under the content policy, from the page's own
    // script, and the page's text and code are set in them.
    expect(await fontsInUse(admin)).toEqual({ faces: ['Hanken Grotesk:loaded', 'Geist Mono:loaded'], text: true, code: true })
    await shot(admin, '01-sign-in')

    // Every built file is served from /assets/ with a fixed type and the
    // frontends' content policy; none is reachable on the data plane, whose
    // /assets/ holds only the Fetch app's files.
    const index = readFileSync(resolvePath(applicationDist, 'index.html'), 'utf8')
    const assets = [...index.matchAll(/(?:src|href)="\/assets\/([^"]+)"/g)].map((m) => m[1])
    expect(assets.map((a) => a.split('.').pop()).sort()).toEqual(['css', 'js', 'svg'])
    const types: Record<string, string> = { js: 'text/javascript; charset=utf-8', css: 'text/css; charset=utf-8', svg: 'image/svg+xml' }
    const served = []
    for (const asset of assets) {
      const reply = await raw('GET', `${adminURL}/assets/${asset}`)
      expect(reply.status, asset).toBe(200)
      expect(reply.headers['content-type'], asset).toBe(types[asset.split('.').pop()!])
      expect(reply.headers['content-security-policy'], asset).toBe(uiPolicy)
      expect(reply.body, asset).toBe(readFileSync(resolvePath(applicationDist, 'assets', asset), 'utf8'))
      const onDataPlane = await raw('GET', `${siteURL}/assets/${asset}`)
      expect(onDataPlane.status, asset).toBe(404)
      expect(onDataPlane.body, asset).not.toBe(reply.body)
      served.push({ asset, status: reply.status, dataPlane: onDataPlane.status })
    }

    // API paths answer as the API, never with the dashboard page, and the
    // data plane serves no dashboard.
    const probes = [
      { url: adminURL + '/admin/status', status: 401 },
      { url: adminURL + '/admin/policy', status: 401 },
      { url: adminURL + '/admin/blocked-accounts', status: 401 },
      { url: adminURL + '/admin/events', status: 401 },
      { url: adminURL + '/admin/history', status: 401 },
      { url: adminURL + '/admin/subjects', status: 404 },
      { url: adminURL + '/admin/no-such-route', status: 404 },
      { url: adminURL + '/index.html', status: 404 },
      { url: adminURL + '/assets/index.html', status: 404 },
      { url: adminURL + '/assets/no-such-file.js', status: 404 },
      { url: adminURL + '/admin/', status: 404 },
      { url: siteURL + '/admin/status', status: 503 },
      { url: siteURL + '/admin/', status: 503 },
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
    // The data plane's page is the Fetch app, never the dashboard.
    const fetchPage = await raw('GET', siteURL + '/')
    expect(fetchPage.status).toBe(200)
    expect(fetchPage.body).not.toBe(index)
    expect(fetchPage.body).not.toContain(assets[0])

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
    await expect(raw('GET', adminURL.replace('127.0.0.1', '127.0.0.2') + '/')).rejects.toThrow(/ECONNREFUSED/)
    await expect(raw('GET', siteURL.replace('127.0.0.2', '127.0.0.1') + '/')).rejects.toThrow(/ECONNREFUSED/)
    expect(await demo.counts()).toEqual({ inventory: 0, orders: 0, reports: 0, support: 0, accounts: 0 })
    record('api-paths', { assets: served, probes: seen })
  })

  test('wrong credentials are refused; enrolment and sign-in use the HttpOnly host-only session cookie', async () => {
    // A wrong enrolment secret and a wrong password are refused, and no
    // session cookie is set.
    await admin.getByRole('button', { name: 'First Sign-In with an Enrolment Secret' }).click()
    await admin.getByLabel('Administrator Name').fill('admin')
    await admin.getByLabel('Enrolment Secret').fill('not-the-enrolment-secret')
    await admin.getByLabel('New Password', { exact: true }).fill(password)
    await admin.getByLabel('Repeat the New Password').fill(password)
    await admin.getByRole('button', { name: 'Enrol', exact: true }).click()
    const enrolRefusal = admin.getByRole('alert').filter({ has: admin.locator('.more-details dd code', { hasText: /^401$/ }) })
    await expect(enrolRefusal).toHaveClass(/tone-refused/)
    await expect(moreDetail(enrolRefusal, 'Error code')).toHaveText('unauthenticated')
    await admin.getByRole('button', { name: 'Password', exact: true }).click()
    await admin.getByLabel('Administrator Name').fill('admin')
    await admin.getByLabel('Password', { exact: true }).fill('wrong password, long enough')
    await admin.getByRole('button', { name: 'Sign in', exact: true }).click()
    const refusal = admin.getByRole('alert').filter({ has: admin.locator('.more-details dd code', { hasText: /^401$/ }) })
    await expect(refusal).toHaveClass(/tone-refused/)
    await expect(moreDetail(refusal, 'Error code')).toHaveText('unauthenticated')
    await expect(refusal).toContainText('Not signed in')
    await expect(refusal).toContainText('Your session has ended, or the name or password is wrong. Sign in again.')
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
    await expect(firstBase).toContainText('Save a draft change: a complete policy that denies every endpoint')
    await expect(firstBase.getByRole('button', { name: 'Go to Changes' })).toBeVisible()
    // The status strip and the not-accepting notice say so, with the
    // gateway's own admission value under More details.
    await expect(strip(admin).getByText('Not Accepting Requests', { exact: true })).toBeVisible()
    await expect(moreDetail(admin.getByRole('alert').filter({ hasText: 'The gateway is not accepting requests' }), 'Admission value')).toHaveText('bootstrap_locked')
    await expect(strip(admin)).toContainText('None yet')
    await expect(admin.getByRole('alert').filter({ hasText: 'The gateway is not accepting requests' })).toContainText(
      'New installation: every request is refused until an administrator establishes the first policy (Changes).',
    )
    // Every other page carries the same warning across its top.
    await go(admin, 'Changes')
    await expect(admin.getByRole('status').filter({ hasText: 'The gateway is not accepting requests.' })).toBeVisible()
    await go(admin, 'Overview')
    await shot(admin, '03-overview-bootstrap')

    // The open view is kept across a reload, and the session resumes.
    await revisions(admin)
    expect(new URL(admin.url()).hash).toBe('#/policy')
    await expect(admin.getByText('No policy is active yet.')).toBeVisible()
    await admin.reload()
    await expect(admin.getByText('Signed in as admin')).toBeAttached()
    await expect(admin.getByRole('heading', { level: 1, name: 'Versions' })).toBeVisible()
    await shot(admin, '03-policy-empty')
    record('session-cookie', { cookies: cookies.map((c) => ({ name: c.name, httpOnly: c.httpOnly, secure: c.secure, sameSite: c.sameSite, path: c.path })) })
  })

  test('the first policy denies everything, then the baseline is activated and confirmed', async () => {
    const denyAll = await stage(admin, { file: demo.path('deny-all.json') })
    await expect(admin.getByRole('main').getByText('yes', { exact: true }).first()).toBeVisible()
    await admin.getByRole('button', { name: 'Establish as the First Policy' }).click()
    // Establishing a policy that denies everything is confirmed by typing.
    await expect(dialogButton(admin, 'Establish')).toBeDisabled()
    await admin.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
    await dialogButton(admin, 'Establish').click()
    await expect(admin.getByText(/^Established: version 1 is the active policy\.$/)).toBeVisible()
    expect(await status()).toMatchObject({ state: 'normal', confirmed: 1 })
    await go(admin, 'Overview')
    await expect(admin.getByRole('region', { name: 'Establish the first policy' })).toHaveCount(0)
    await expect(strip(admin)).toContainText('Version 1')
    await expect(strip(admin).getByText('Accepting Requests', { exact: true })).toBeVisible()
    // The APIs view shows every operation closed by the deny-all policy.
    await go(admin, 'APIs')
    const closed = await admin.getByRole('main').getByText('Closed', { exact: true }).count()
    expect(closed).toBe(((await adminCall('GET', '/admin/policy')).body.document.operations as unknown[]).length)

    const baseline = await stage(admin, { file: demo.path('baseline.json') })
    const listChange = admin.getByRole('listitem').filter({ hasText: 'endpoints: changed orders.all' })
    await expect(listChange.getByRole('listitem')).toHaveText(['access: "deny" → "allow"'])
    await expect(admin.getByText('permissions: added SUPPORT → orders.all')).toBeVisible()
    await shot(admin, '04-candidate-review')
    await trialStep(admin)
    await admin.getByLabel('Trial Length (Seconds)').fill('600')
    await admin.getByRole('main').getByRole('button', { name: 'Put on Trial', exact: true }).click()
    await expect(admin.getByRole('dialog')).toContainText('Unless someone keeps it within 10 min, the gateway undoes it and returns to the active version now, version 1.')
    await shot(admin, '05-activate-dialog')
    await dialogButton(admin, 'Put on Trial').click()
    const done = admin.getByText(/^Version \d+ is on trial and active/)
    await expect(done).toBeVisible()
    const provisional = Number((await done.textContent())!.match(/Version (\d+)/)![1])
    expect(await status()).toMatchObject({ confirmed: 1, provisional, revision: provisional })

    // While it waits, the overview shows both bases and the countdown, and
    // the navigation flags the trial.
    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText(`Version ${provisional}`)
    await expect(strip(admin)).toContainText('On trial; version 1 returns if it is undone')
    await expect(strip(admin).getByText('Undone in')).toBeVisible()
    await expect(strip(admin).getByText(/^\d{2}:\d{2}$/)).toBeVisible()
    await expect(nav(admin, 'Changes')).toContainText('On Trial')
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

  test('the overview, policy revisions, accounts and team views show the controller state as the Admin API reports it, and only traffic figures and the status refresh by themselves', async () => {
    await go(admin, 'Overview')
    const now = await status()
    const main = admin.getByRole('main')
    await expect(strip(admin).getByText('Accepting Requests', { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText(`Version ${now.confirmed}`)
    await expect(strip(admin).getByText(now.generation!, { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText('None active')
    await expect(strip(admin).getByText('None', { exact: true })).toBeVisible()
    // The policy timeline is the newest administrative history.
    const timeline = main.getByRole('region', { name: 'Policy Timeline' })
    await expect(timeline.getByRole('listitem').first()).toContainText(`Version ${now.confirmed}`)
    await expect(timeline.getByRole('listitem').first()).toContainText('Kept')

    // The Getting started guide ticks each step from what the gateway reports:
    // the baseline trial was kept, so all three are done. Hidden, it stays
    // hidden on the Overview until My account brings it back.
    const guide = main.locator('details.getting-started')
    await expect(guide.locator('summary')).toHaveText('Getting Started · 3 of 3 done')
    await guide.locator('summary').click()
    await expect(guide.getByRole('listitem')).toHaveCount(3)
    await expect(guide.getByText('To Do', { exact: true })).toHaveCount(0)
    await shot(admin, '08a-getting-started')
    await guide.getByRole('button', { name: 'Hide This Guide' }).click()
    await expect(guide).toHaveCount(0)
    await go(admin, 'Changes')
    await go(admin, 'Overview')
    await expect(timeline).toBeVisible()
    await expect(guide).toHaveCount(0)
    await go(admin, 'My Account')
    await main.getByRole('button', { name: 'Show the Getting Started Guide' }).click()
    await expect(guide.locator('summary')).toHaveText('Getting Started · 3 of 3 done')

    // The policy revisions view shows the document the server holds.
    const confirmed = (await adminCall('GET', '/admin/policy')).body as {
      id: number
      document: {
        upstreams: { id: string; dial: string }[]
        operations: { id: string; method: string; target: string; access: string; auth: string; body?: unknown; rate?: { count: number; window_seconds: number; key: string } }[]
        roles: { id: string }[]
        permits: { role: string; operation: string }[]
      }
    }
    const doc = confirmed.document
    await shot(admin, '08-overview')

    // The APIs view lists every operation of each API with its limit, from
    // the same document.
    await go(admin, 'APIs')
    for (const o of doc.operations) {
      const row = main.getByRole('row').filter({ has: admin.getByRole('link', { name: o.id, exact: true }) })
      await expect(row).toContainText(`${o.method} ${o.target}`)
      const span: Record<number, string> = { 1: 's', 60: 'min', 3600: 'h' }
      if (o.rate) await expect(row).toContainText(`${o.rate.count}/${span[o.rate.window_seconds] ?? `${o.rate.window_seconds} s`}`)
    }

    await revisions(admin)
    await expect(main.getByText(`Version ${confirmed.id},`)).toBeVisible()
    await versionsPart(admin, 'Service Addresses')
    for (const u of confirmed.document.upstreams) await expect(main.getByRole('row').filter({ hasText: u.id })).toContainText(u.dial)
    // The Endpoints tab lists each endpoint once; the role tables list them per role.
    await versionsPart(admin, 'Endpoints')
    const endpoints = main.getByRole('region', { name: 'Endpoints', exact: true })
    for (const o of confirmed.document.operations) {
      await expect(endpoints.getByRole('row').filter({ has: admin.getByRole('cell', { name: o.id, exact: true }) })).toContainText(`${o.method} ${o.target}`)
    }
    // The permit matrix has a column per role and, for every operation that
    // needs a token, exactly the permits the document holds.
    await versionsPart(admin, 'Roles')
    const matrix = main.getByRole('region', { name: 'Role Permissions' })
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
      await expect(cells).toHaveText(doc.roles.map((r) => (doc.permits.some((p) => p.role === r.id && p.operation === o.id) ? /permitted/ : /no permission/)))
    }
    // The request rules state each body field the schema declares, including
    // nested objects and each element of an array.
    await versionsPart(admin, 'Endpoints')
    const adjustRules = main.getByRole('region', { name: 'Request Rules of inventory.stock.adjust' })
    await expect(adjustRules.getByRole('row', { name: /^delta/ })).toHaveText(/delta\s*body\s*integer\s*required\s*-100 to 100/)
    const createRules = main.getByRole('region', { name: 'Request Rules of orders.create' })
    await expect(createRules.getByRole('row')).toHaveCount(14)
    await expect(createRules.getByRole('row', { name: /^items\s/ })).toContainText('1 to 20 items')
    await expect(createRules.getByRole('row', { name: /^items\[\]\s/ })).toContainText('every element')
    await expect(createRules.getByRole('row', { name: /^items\[\]\.quantity/ })).toContainText('1 to 50')
    await expect(createRules.getByRole('row', { name: /^address\.postcode/ })).toContainText('5 to 5 characters')
    await expect(main.getByRole('region', { name: 'Request Rules of orders.all' })).toHaveCount(0)
    await shot(admin, '09-policy')

    // The APIs page states the same rules in words, with the exact schema one
    // click away; it describes the policy and checks nothing itself.
    await go(admin, 'APIs')
    const ordersApi = main.getByRole('region', { name: 'Orders', exact: true })
    await ordersApi.getByText('Request Rules', { exact: true }).click()
    const apiRules = ordersApi.getByRole('region', { name: 'Request Rules of orders.create' })
    await expect(apiRules.getByRole('row')).toHaveCount(14)
    await expect(apiRules.getByRole('row', { name: /^items\s/ })).toContainText('1 to 20 items')
    await expect(ordersApi.locator('.more-details dt', { hasText: 'Body Schema (JSON)' }).first()).toBeAttached()

    // Accounts: nothing is blocked yet, and the accounts seen in the latest
    // requests carry the roles their verified tokens held.
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([])
    expect((await summary('ops-01')).status).toBe(200)
    await go(admin, 'Accounts')
    // One table lists every known account; none carries a Blocked badge yet.
    await expect(main.getByRole('row').filter({ has: admin.getByText('Blocked', { exact: true }) })).toHaveCount(0)
    await expect(main.getByRole('row').filter({ hasText: 'ops-01' })).toContainText('OPERATIONS')
    await shot(admin, '10-accounts')

    // Team lists the administrators and signing keys exactly as the Admin
    // API reports them.
    await go(admin, 'Team')
    for (const a of (await adminCall('GET', '/admin/administrators')).body.items as { name: string; status: string }[]) {
      await expect(main.getByRole('row').filter({ has: admin.getByRole('cell', { name: a.name, exact: true }) })).toContainText(a.status === 'active' ? 'Active' : a.status)
    }
    const keys = (await adminCall('GET', '/admin/keys')).body.items as { kid: string; fingerprint: string }[]
    expect(keys.length).toBeGreaterThan(0)
    for (const k of keys) await expect(main.getByRole('row').filter({ hasText: k.kid })).toContainText(k.fingerprint)
    await shot(admin, '10-team')

    // Every visible view rereads the gateway's status each second, so what
    // it shows is never more than a second old. A policy view reads nothing
    // else by itself.
    const idleReads = async (view: string) => {
      await go(admin, view)
      await admin.waitForTimeout(2_000)
      const start = adminRequests.length
      await admin.waitForTimeout(6_000)
      return adminRequests.slice(start).map((url) => new URL(url).pathname)
    }
    const changesIdle = await idleReads('Changes')
    expect(changesIdle.filter((path) => path !== '/admin/status')).toEqual([])
    expect(changesIdle.length).toBeGreaterThanOrEqual(4)
    expect(changesIdle.length).toBeLessThanOrEqual(8)
    // The Overview also rereads its traffic figures and recent refusals each
    // second, and nothing more.
    const idle = await idleReads('Overview')
    expect(idle.filter((path) => path !== '/admin/traffic' && path !== '/admin/events' && path !== '/admin/status')).toEqual([])
    for (const path of ['/admin/traffic', '/admin/events', '/admin/status']) {
      expect(idle.filter((p) => p === path).length, path).toBeGreaterThanOrEqual(4)
      expect(idle.filter((p) => p === path).length, path).toBeLessThanOrEqual(8)
    }
    record('overview', { status: now, changesIdle: changesIdle.length, overviewIdle: idle.length, seconds: 6 })
  })

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

  test('concurrent sessions cannot overwrite each other and a lost answer is resolved by resending', async ({ browser }) => {
    const other = await signedInPage(browser, 'admin')

    // One session confirms; the other's rollback, based on what it read
    // earlier, is refused and shown as refused. The other session's status
    // reads fail meanwhile (as on a dropped connection), so its page still
    // shows the trial when its rollback reaches the gateway.
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const first = await activate(admin, 600)
    await go(other.page, 'Changes')
    const target = (await status()).confirmed
    await expect(other.page.getByRole('button', { name: 'Undo Change', exact: true })).toBeVisible()
    await other.page.route('**/admin/status', (route) => route.abort())
    await resolve(admin, 'confirm')
    await other.page.getByRole('main').getByRole('button', { name: 'Undo Change', exact: true }).click()
    await expect(other.page.getByRole('dialog').getByRole('heading', { name: `Undo the change and return to version ${target}?` })).toBeVisible()
    await dialogButton(other.page, 'Undo Change').click()
    const refusal = other.page.getByRole('alert').filter({ has: other.page.locator('.more-details dd code', { hasText: /^4\d\d$/ }) })
    await expect(refusal).toBeVisible()
    await expect(refusal).toHaveClass(/tone-refused/)
    const refusalText = await refusal.textContent()
    await expect(other.page.getByText(/^Undone:/)).toHaveCount(0)
    await shot(other.page, '13-stale-refusal')
    expect(await status()).toMatchObject({ confirmed: first, revision: first })
    await other.page.unroute('**/admin/status')

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
    await confirmTrial(admin)
    await expect(admin.getByText('No answer from the gateway: outcome unknown')).toBeVisible()
    await expect(admin.getByText(/^Kept:/)).toHaveCount(0)
    await shot(admin, '14-outcome-unknown')
    expect(await status()).toMatchObject({ confirmed: second })
    // Without sending anything again, the page looks the change up by its
    // exact transition and finds it applied.
    const unresolved = admin.getByRole('region', { name: /^(A change has an unknown outcome|Outcome found)$/ })
    await expect(unresolved).toContainText(`Keep version ${second}`)
    await expect(unresolved.getByText(/^Applied: the gateway recorded it at /)).toBeVisible()
    await admin.unroute('**/admin/provisional/confirm')
    await admin.getByRole('button', { name: 'Send the Identical Request Again', exact: true }).click()
    await expect(admin.getByText(`Kept: version ${second} is now the active policy.`)).toBeVisible()
    // Changes also shows the earlier staging and trial receipts; this is the
    // confirmation's own.
    const receipt = admin.getByRole('main').locator('.notice', { hasText: `Kept: version ${second} is now the active policy.` })
    const shownTransition = await moreDetail(receipt, 'Transition').textContent()
    expect(shownTransition).toBe(lostReceipt.transition_id)
    const confirms = (await listAll<{ kind: string; target: string }>('/admin/history')).filter(
      (h) => h.kind === 'cpa.confirm' && h.target === `revision ${second}`,
    )
    expect(confirms).toHaveLength(1)
    await other.context.close()
    record('concurrency-and-lost-answer', { refusal: refusalText, lostReceipt, shownTransition, confirmTransitions: confirms.length })
  })

  test('Versions states what each role can do under the active policy, with temporary rules beside it', async () => {
    const policy = (await adminCall('GET', '/admin/policy')).body as { id: number; document: { roles: { id: string }[]; permits: { role: string; operation: string }[] } }
    await go(admin, 'Versions')
    const main = admin.getByRole('main')
    await expect(main.getByText(new RegExp(`^Version ${policy.id},`))).toBeVisible()
    // One tab per role; the chosen role's endpoints are listed in its panel.
    const panel = main.getByRole('tabpanel', { name: 'Roles', exact: true }).getByRole('tabpanel')
    for (const role of policy.document.roles) {
      const permitted = policy.document.permits.filter((p) => p.role === role.id).map((p) => p.operation)
      await main.getByRole('tab', { name: new RegExp(`^${role.id} ${permitted.length}$`) }).click()
      await expect(panel).toContainText(permitted.length === 0 ? `${role.id} has no permissions.` : `${role.id} can call ${permitted.length} ${permitted.length === 1 ? 'endpoint' : 'endpoints'}.`)
      for (const operation of permitted) await expect(panel.getByText(operation, { exact: true })).toBeVisible()
    }
    // The filter narrows the endpoints shown and says how many are left; it reads nothing new.
    await main.getByRole('tab', { name: /^OPERATIONS \d+$/ }).click()
    const total = policy.document.permits.filter((p) => p.role === 'OPERATIONS').length
    const filter = main.getByRole('searchbox', { name: 'Filter endpoints' })
    await filter.fill('inventory.stock.adjust')
    await expect(main.getByText(`1 of ${total} endpoints`, { exact: true })).toBeVisible()
    await expect(panel.getByText('reports.summary', { exact: true })).toHaveCount(0)
    await filter.fill('')
    await expect(main.getByText(`${total} endpoints`, { exact: true })).toBeVisible()
    // A temporary rule is listed beside the role it changes; the permission
    // in the policy stays listed, because the policy itself is unchanged.
    const id = await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 120 })
    await go(admin, 'Versions')
    await main.getByRole('tab', { name: /^OPERATIONS \d+$/ }).click()
    await expect(panel).toContainText(`Rule ${id}: OPERATIONS may not call inventory.stock.adjust, until`)
    await expect(panel.getByText('inventory.stock.adjust', { exact: true })).toBeVisible()
    await shot(admin, '47-roles')
    await revokeOverride(admin, id)
    await go(admin, 'Versions')
    await main.getByRole('tab', { name: /^OPERATIONS \d+$/ }).click()
    await expect(panel).not.toContainText(`Rule ${id}:`)
  })

  test('the temporary rule dialog ends on a review of every choice, adds the rule from it and is listed by the filter', async () => {
    await go(admin, 'Temporary Rules')
    const main = admin.getByRole('main')
    await main.getByRole('button', { name: 'Add a Temporary Rule', exact: true }).click()
    const wizard = admin.getByRole('dialog', { name: 'Add a Temporary Rule' })
    await wizard.getByRole('radio', { name: 'Restrict Temporarily: Stop a Role Calling an Endpoint', exact: true }).check()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByLabel('Endpoint', { exact: true }).selectOption('inventory.stock.adjust')
    await wizard.getByLabel('Role', { exact: true }).selectOption('OPERATIONS')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByLabel('Lifetime (Seconds)').fill('0')
    await expect(wizard.getByRole('button', { name: 'Next', exact: true })).toBeDisabled()
    await wizard.getByLabel('Lifetime (Seconds)').fill('120')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await expect(wizard).toContainText('OPERATIONS may not call inventory.stock.adjust, for 2 min.')
    // The review lists every choice, not only the summary.
    const facts = wizard.locator('.review-facts')
    await expect(facts).toContainText('Restrict Temporarily: Stop a Role Calling an Endpoint')
    await expect(facts).toContainText('inventory.stock.adjust')
    await expect(facts).toContainText('OPERATIONS')
    await expect(facts).toContainText('2 min from the next request')
    await wizard.getByRole('button', { name: 'Check with the Gateway' }).click()
    await expect(wizard.getByText('The gateway would accept it now. Nothing has been created.')).toBeVisible()
    await shot(admin, '48-rule-wizard-review')
    // The review is the confirmation: Add rule on it sends the rule.
    await wizard.getByRole('button', { name: 'Add Rule', exact: true }).click()
    await expect(wizard).toHaveCount(0)
    const done = admin.getByText(/^Rule \d+ is active until/)
    await expect(done).toBeVisible()
    const id = Number((await done.textContent())!.match(/Rule (\d+)/)![1])
    expect((await status()).overrides).toContain(id)
    const filter = main.getByRole('searchbox', { name: 'Filter active temporary rules' })
    await filter.fill(`rule ${id}`)
    await expect(main.getByText('1 of 1 active rule', { exact: true })).toBeVisible()
    await filter.fill('no-such-endpoint')
    await expect(main.getByText('No active rule matches the filter.')).toBeVisible()
    await filter.fill('')
    await revokeOverride(admin, id)
  })

  test('a temporary containment stops OPERATIONS adjusting stock without changing the confirmed base, and revocation lifts it', async () => {
    const before = await status()
    const id = await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 300 })
    const during = await status()
    expect(during).toMatchObject({ confirmed: before.confirmed, revision: before.confirmed, overrides: [id] })
    expect(during.generation).toContain('+o')
    // A notification names the rule from the status the gateway reports,
    // without moving focus. It closes by itself after a few seconds unless
    // pointed at, and can be dismissed.
    const started = admin.getByRole('status').filter({ hasText: `Temporary Rule ${id} is active.` })
    await expect(started).toBeVisible({ timeout: 15_000 })
    await started.locator('p', { hasText: `Temporary rule ${id} is active.` }).hover()
    await admin.getByRole('button', { name: `Dismiss: Temporary rule ${id} is active.` }).click()
    await expect(started).toHaveCount(0)
    // Screenshots finish CSS animations, which would end the notice's
    // countdown, so the page is captured once it is dismissed.
    await shot(admin, '15-overrides-active')

    const row = rowFor(admin.getByRole('main').getByRole('region', { name: 'Active Rules' }), id)
    await expect(row).toContainText('Restrict Temporarily')
    await expect(row).toContainText('OPERATIONS may not call inventory.stock.adjust')

    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText('1 active')
    await expect(strip(admin)).toContainText('OPERATIONS may not call inventory.stock.adjust')
    await expect(strip(admin).getByText(during.generation!, { exact: true })).toBeVisible()
    await expect(strip(admin)).toContainText(`Version ${before.confirmed}`)
    await expect(nav(admin, 'Temporary Rules')).toContainText('1')
    await shot(admin, '16-overview-override')

    const stockBefore = await stock('ITM-501')
    const contained = await adjust('ops-01', -1)
    expect(contained.status).toBe(403)
    expect(contained.receipts).toHaveLength(0)
    expect(await stock('ITM-501')).toBe(stockBefore)

    // A second override on the same rule is refused as a conflict.
    await go(admin, 'Temporary Rules')
    await admin.getByRole('main').getByRole('button', { name: 'Add a Temporary Rule', exact: true }).click()
    const wizard = admin.getByRole('dialog', { name: 'Add a Temporary Rule' })
    await wizard.getByRole('radio', { name: 'Restrict Temporarily: Stop a Role Calling an Endpoint', exact: true }).check()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByLabel('Endpoint', { exact: true }).selectOption('inventory.stock.adjust')
    await wizard.getByLabel('Role', { exact: true }).selectOption('OPERATIONS')
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByRole('button', { name: 'Next', exact: true }).click()
    await wizard.getByRole('button', { name: 'Check with the Gateway' }).click()
    await expect(wizard.getByRole('alert').filter({ hasText: 'OVERRIDE_CONFLICT' })).toBeVisible()
    await wizard.getByRole('button', { name: 'Cancel', exact: true }).click()
    expect((await status()).overrides).toEqual([id])

    await revokeOverride(admin, id)
    const after = await status()
    expect(after).toMatchObject({ confirmed: before.confirmed, overrides: [] })
    await expect(admin.getByRole('status').filter({ hasText: `Temporary Rule ${id} ended.` })).toBeVisible({ timeout: 15_000 })
    const lifted = await adjust('ops-01', -1)
    expect(lifted.status).toBe(200)
    expect(lifted.receipts).toHaveLength(1)
    expect(lifted.receipts[0].generation).toBe(after.generation)
    expect(await stock('ITM-501')).toBe(stockBefore - 1)
    record('override-containment', { id, during, contained: contained.status, after, lifted: lifted.receipts[0] })
  })

  test('a temporary exception lets CUSTOMER read sales until the gateway expires it', async () => {
    const beforeGrant = await sales('cust-001')
    expect(beforeGrant.status).toBe(403)
    expect(beforeGrant.receipts).toHaveLength(0)
    const id = await createOverride(admin, { action: 'Allow Temporarily: Let a Role Call an Endpoint', operation: 'reports.sales', role: 'CUSTOMER', seconds: 4 })
    const allowed = await sales('cust-001')
    admitted(allowed)
    expect(allowed.receipts[0].generation).toContain('+o')

    await go(admin, 'Temporary Rules')
    await expect(admin.getByText('No temporary rules are active.')).toBeVisible({ timeout: 20_000 })
    const history = admin.getByRole('region', { name: 'Past Rules' })
    await expect(rowFor(history, id)).toContainText('Ended')
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
    await go(admin, 'Temporary Rules')
    await expect(admin.getByText('A change is on trial: the gateway refuses new temporary rules until it is kept or undone.')).toBeVisible()
    // The page offers no way to add one while a change is on trial, and the
    // gateway itself refuses the same check.
    await expect(admin.getByRole('main').getByRole('button', { name: 'Add a Temporary Rule', exact: true })).toBeDisabled()
    const policy = await adminCall('GET', '/admin/overrides')
    const refused = await adminCall(
      'POST',
      '/admin/overrides/validate',
      { type: 'containment', control: 'operation', operation: 'orders.all', expires_in_seconds: 300 },
      { 'If-Match': policy.etag ?? '' },
    )
    expect(JSON.stringify(refused.body).toUpperCase()).toContain('PROVISIONAL_ACTIVE')
    await shot(admin, '18-override-refused')
    expect((await status()).overrides).toEqual([])
    await resolve(admin, 'rollback')
    record('override-during-provisional', { status: await status() })
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

  test('blocking an account refuses its next request at once, a policy rollback cannot lift it, and unblocking restores it', async ({ browser }) => {
    const before = await status()
    expect((await adjust('ops-01', -1)).status).toBe(200)

    // The Recently seen shortcut fills in the account; the block needs a reason.
    await go(admin, 'Accounts')
    const main = admin.getByRole('main')
    const blockedList = main.getByRole('region', { name: 'Account List', exact: true })
    await main.getByRole('button', { name: 'Block… ops-01' }).click()
    await expect(admin.getByLabel('Account ID')).toHaveValue('ops-01')
    await expect(admin.getByRole('button', { name: 'Block Now' })).toBeDisabled()
    await admin.getByLabel('Reason').fill('Reported a stolen phone')
    await admin.getByRole('button', { name: 'Block Now' }).click()
    await expect(admin.getByText(/^ops-01 is blocked: its requests are refused from now on$/)).toBeVisible()
    const row = blockedList.getByRole('row').filter({ hasText: 'ops-01' })
    await expect(row).toContainText('Reported a stolen phone')
    await expect(row).toContainText('by admin')
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([
      expect.objectContaining({ subject: 'ops-01', reason: 'Reported a stolen phone', blocked_by: 'admin' }),
    ])
    await shot(admin, '20-account-blocked')

    // A block is not a policy change: the active policy is unchanged, and
    // the next request is refused before it reaches any service.
    expect(await status()).toEqual(before)
    const refused = await adjust('ops-01', -1)
    expect(refused.status).toBe(403)
    expect(refused.receipts).toHaveLength(0)
    const [event] = (await adminCall('GET', '/admin/events?subject=ops-01&limit=1')).body.items
    expect(event).toMatchObject({ decision: 'refused', status: 403, stage: 'blocked', code: 'account_blocked', roles: ['OPERATIONS'] })

    // The refusal's Ref finds the request and the check that decided it.
    await admin.getByRole('searchbox', { name: 'Search', exact: true }).fill(event.request_id)
    await admin.getByRole('searchbox', { name: 'Search', exact: true }).press('Enter')
    await expect(admin.getByRole('heading', { level: 1, name: 'Request Lookup' })).toBeVisible()
    const checks = main.getByRole('list', { name: "The gateway's checks, in order" })
    await expect(checks.getByRole('listitem').filter({ hasText: 'Blocked List' })).toContainText('refused here')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Sign-In' })).toContainText('passed')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Role' })).toContainText('not reached')
    await shot(admin, '21-lookup-blocked')

    // The same header search finds an account's requests without putting the
    // account into the address, opens a draft change by its number, and says
    // plainly that a short Ref is not enough.
    const search = admin.getByRole('searchbox', { name: 'Search', exact: true })
    await go(admin, 'Overview')
    await search.fill('ops-01')
    await search.press('Enter')
    await expect(admin.getByRole('heading', { level: 1, name: 'Request Lookup' })).toBeVisible()
    await expect(main.getByLabel('Account', { exact: true })).toHaveValue('ops-01')
    expect(admin.url()).not.toContain('ops-01')
    await go(admin, 'Overview')
    await search.fill(`${event.request_id.slice(0, 4)}-${event.request_id.slice(4, 8)}`)
    await search.press('Enter')
    await expect(admin.getByText(/^That is a short Ref\./)).toBeVisible()
    await search.fill('change 1')
    await search.press('Enter')
    await expect(admin.getByRole('heading', { name: 'Change 1', exact: true })).toBeVisible()

    // A request decided before routing (at cold start, when the gateway
    // admitted nothing) never shows the checks it did not meet as passed.
    const early = (await listAll<{ request_id: string; operation: string | null; code: string | null; stage: string }>('/admin/events')).find(
      (e) => e.operation === null && e.code === 'gateway_unavailable',
    )!
    expect(early.stage).toBe('record')
    // Request lookup has its own search form in place of the header box; the
    // account searched above is cleared so only the Ref is asked for.
    await go(admin, 'Request Lookup')
    await main.getByLabel('Account', { exact: true }).fill('')
    await main.getByLabel('Reference (Ref)').fill(early.request_id)
    await main.getByRole('button', { name: 'Find Requests' }).click()
    await expect(checks.getByRole('listitem').filter({ hasText: 'Route' })).toContainText('not checked')
    await expect(checks.getByRole('listitem').filter({ hasText: 'Record' })).toContainText('refused here')
    await expect(main.getByText('Decided before endpoint matching; earlier checks did not run.')).toBeVisible()

    // A trial and its rollback leave the block in place.
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    await activate(admin, 600)
    const duringTrial = await adjust('ops-01', -1)
    expect(duringTrial.status).toBe(403)
    expect(duringTrial.receipts).toHaveLength(0)
    await resolve(admin, 'rollback')
    const rolledBack = await adjust('ops-01', -1)
    expect(rolledBack.status).toBe(403)
    expect(rolledBack.receipts).toHaveLength(0)

    // A second session reads the list; the first unblocks, after a
    // confirmation; the second session's unblock of the same account is then
    // refused, and its Refresh shows the current list.
    const other = await signedInPage(browser, 'admin')
    await go(other.page, 'Accounts')
    const otherMain = other.page.getByRole('main')
    await expect(otherMain.getByRole('row').filter({ hasText: 'ops-01' }).getByText('Blocked', { exact: true })).toBeVisible()
    await go(admin, 'Accounts')
    await blockedList.getByRole('button', { name: 'Unblock ops-01' }).click()
    await expect(admin.getByRole('dialog')).toContainText('ops-01 can use Fetch again at once')
    await dialogButton(admin, 'Unblock').click()
    await expect(admin.getByText(/^ops-01 is unblocked and can use Fetch again$/)).toBeVisible()
    await expect(main.getByRole('row').filter({ has: admin.getByText('Blocked', { exact: true }) })).toHaveCount(0)
    await otherMain.getByRole('button', { name: 'Unblock ops-01' }).click()
    await dialogButton(other.page, 'Unblock').click()
    const stale = otherMain.getByRole('alert').filter({ has: other.page.locator('.more-details dd code', { hasText: /^404$/ }) })
    await expect(stale).toBeVisible()
    await expect(stale).toHaveClass(/tone-refused/)
    const staleText = await stale.textContent()
    await other.page.getByRole('button', { name: 'Refresh', exact: true }).click()
    await expect(otherMain.getByRole('row').filter({ has: other.page.getByText('Blocked', { exact: true }) })).toHaveCount(0)
    await other.context.close()

    const restored = await adjust('ops-01', -1)
    expect(restored.status).toBe(200)
    expect(restored.receipts).toHaveLength(1)
    expect(restored.receipts[0]).toMatchObject({ subject: 'ops-01', roles: 'OPERATIONS' })

    // A block whose answer is lost: the gateway applies it, the page says the
    // outcome is unknown and sends nothing more; the explicit identical resend
    // returns the recorded receipt with exactly one transition.
    let lostReceipt: { subject?: string } = {}
    await admin.route('**/admin/blocked-accounts/cust-002', async (route) => {
      const response = await route.fetch()
      lostReceipt = await response.json()
      await route.abort('connectionreset')
    })
    await admin.getByRole('main').getByRole('button', { name: 'Block an Account' }).click()
    await admin.getByLabel('Account ID').fill('cust-002')
    await admin.getByLabel('Reason').fill('Chargeback under review')
    await admin.getByRole('button', { name: 'Block Now' }).click()
    await expect(admin.getByText('No answer from the gateway: outcome unknown')).toBeVisible()
    await expect(admin.getByText(/^cust-002 is blocked/)).toHaveCount(0)
    await shot(admin, '22-block-outcome-unknown')
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([expect.objectContaining({ subject: 'cust-002' })])
    await admin.unroute('**/admin/blocked-accounts/cust-002')
    await admin.getByRole('button', { name: 'Send the Identical Request Again' }).click()
    await expect(admin.getByText(/^cust-002 is blocked: its requests are refused from now on$/)).toBeVisible()
    expect(lostReceipt.subject).toBe('cust-002')
    const blocks = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'account.block' && h.target === 'cust-002')
    expect(blocks).toHaveLength(1)
    await blockedList.getByRole('button', { name: 'Unblock cust-002' }).click()
    await dialogButton(admin, 'Unblock').click()
    await expect(admin.getByText(/^cust-002 is unblocked and can use Fetch again$/)).toBeVisible()

    // History names each change in words.
    await go(admin, 'History')
    await expect(main.getByText('Account Blocked').first()).toBeVisible()
    await expect(main.getByText('Account Unblocked').first()).toBeVisible()
    record('blocked-accounts', { event, refused: refused.status, afterRollback: rolledBack.status, stale: staleText, restored: restored.receipts[0], blockTransitions: blocks.length })
  })

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
    await expect(row(allowedRef)).toContainText('Forwarded')

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
    await expect(row(refusedEvent.request_id)).toContainText('No Permission')
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
    await page.getByRole('button', { name: 'First Sign-In with an Enrolment Secret' }).click()
    const newPassword = page.getByLabel('New Password', { exact: true })
    await newPassword.fill('short')
    await expect(page.getByText('Use 15 to 256 bytes.')).toBeVisible()
    expect(await newPassword.getAttribute('aria-describedby')).toContain('-error')
    await scan(page, 'first sign-in with an error')
    await shot(page, '24-enrol-error')
    await page.getByRole('button', { name: 'Password', exact: true }).click()

    // Sign in with the keyboard only; focus is visible on each control.
    const name = page.getByLabel('Administrator Name')
    await tabTo(page, name)
    expect(await name.evaluate((el) => getComputedStyle(el).outlineStyle)).not.toBe('none')
    await page.keyboard.type('admin')
    await page.keyboard.press('Tab')
    await page.keyboard.type(password)
    await page.keyboard.press('Enter')
    await expect(page.getByText('Signed in as admin')).toBeAttached()

    // Moving between views by keyboard puts focus on the new view's heading.
    await nav(page, 'Changes').focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('heading', { level: 1, name: 'Changes' })).toBeFocused()

    // Every view in the navigation.
    const views = ['Overview', 'Live Traffic', 'Request Lookup', 'APIs', 'Policy', 'Versions', 'Changes', 'Temporary Rules', 'Accounts', 'History', 'Team', 'My Account']
    for (const view of views) {
      await go(page, view)
      await scan(page, view)
    }
    // Each page says in one line what it is for, under its title.
    await go(page, 'Changes')
    await expect(page.getByText('Review draft changes, preview their impact and activate one through a timed trial.')).toBeVisible()
    // The review panels: an inspected revision, and the history filtered.
    await revisions(page)
    await scan(page, 'Versions')
    await versionsPart(page, 'Version History')
    await page.getByRole('button', { name: 'Inspect version 1', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Version 1', exact: true })).toBeFocused()
    await scan(page, 'revision detail')
    await go(page, 'History')
    await page.getByLabel('Area').selectOption('policy')
    await page.getByLabel('Newest First').check()
    await scan(page, 'history filtered')
    await go(page, 'My Account')
    await shot(page, '25-account')

    // A confirmation dialog takes focus and Escape cancels it without acting.
    const before = await status()
    expect((await adminCall('PUT', '/admin/blocked-accounts/cust-002', { reason: 'Keyboard check' }, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
    await go(page, 'Accounts')
    await scan(page, 'Accounts with a blocked account')
    await page.getByRole('button', { name: 'Unblock cust-002' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await scan(page, 'confirmation dialog')
    await shot(page, '26-unblock-dialog')
    expect(await page.evaluate(() => document.activeElement?.textContent)).toBe('Cancel')
    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog')).toHaveCount(0)
    expect(await status()).toEqual(before)
    expect((await adminCall('GET', '/admin/blocked-accounts')).body.items).toEqual([expect.objectContaining({ subject: 'cust-002' })])
    expect((await adminCall('DELETE', '/admin/blocked-accounts/cust-002', undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)

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
        await scan(page, 'My Account at 375px')
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

  test('a confirmation reads the gateway again first, needs a deliberate step when evidence is missing or the trial changed, and never sends by itself', async ({ browser }) => {
    const confirms: string[] = []
    const onRequest = (request: { url: () => string; method: () => string }) => {
      if (new URL(request.url()).pathname === '/admin/provisional/confirm') confirms.push(request.method())
    }
    admin.on('request', onRequest)
    const other = await signedInPage(browser, 'admin')
    try {
      await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
      // A trial under two minutes gets advice; the gateway would accept it.
      await trialStep(admin)
      await admin.getByLabel('Trial Length (Seconds)').fill('60')
      await expect(admin.getByText(/^Under 2 minutes leaves little time/).first()).toBeVisible()
      const revision = await activate(admin, 600)
      await expect(admin.getByText(/^Under 2 minutes leaves little time/)).toHaveCount(0)
      const trialCard = trial(admin)
      await expect(trialCard).toContainText('No request has been decided since the trial started')

      // No request yet: the dialog says so and holds the confirmation back
      // until the administrator says they have reviewed the facts.
      await trialCard.getByRole('button', { name: 'Keep Change', exact: true }).click()
      await expect(admin.getByRole('dialog').getByRole('heading', { name: `Keep version ${revision}?` })).toBeVisible()
      const dialog = admin.getByRole('dialog')
      await expect(dialog.getByText(/^Read again at /)).toBeVisible()
      await expect(dialog).toContainText('No request has been decided since the trial started')
      await expect(dialogButton(admin, 'Keep Change')).toBeDisabled()
      await dialog.getByLabel('I have reviewed the current facts above').check()
      await expect(dialogButton(admin, 'Keep Change')).toBeEnabled()
      await shot(admin, '40-confirm-needs-review')
      await dialogButton(admin, 'Cancel').click()

      // Real requests during the trial, counted from its start.
      admitted(await sales('ops-01'))
      admitted(await sales('ops-01'))
      const refused = await sales('cust-001')
      expect(refused.status).toBe(403)
      expect(refused.receipts).toHaveLength(0)
      await expect(trialCard).toContainText('3 decided · 2 allowed · 1 refused')

      // With current figures and nothing changed, no extra step is needed.
      await trialCard.getByRole('button', { name: 'Keep Change', exact: true }).click()
      await expect(admin.getByRole('dialog').getByRole('heading', { name: `Keep version ${revision}?` })).toBeVisible()
      await expect(dialog.getByText(/^Read again at /)).toBeVisible()
      await expect(dialog.getByLabel('I have reviewed the current facts above')).toHaveCount(0)
      await expect(dialogButton(admin, 'Keep Change')).toBeEnabled()

      // Another administrator rolls the trial back while the dialog is open:
      // this page sees it within seconds and the confirmation becomes
      // unavailable. Nothing is sent.
      await resolve(other.page, 'rollback')
      const rolledBackAt = Date.now()
      await expect(dialog).toContainText('No change is on trial any more', { timeout: 10_000 })
      const noticed = Date.now() - rolledBackAt
      expect(noticed).toBeLessThan(6_000)
      await expect(dialogButton(admin, 'Keep Change')).toBeDisabled()
      await shot(admin, '41-confirm-trial-ended')
      await dialogButton(admin, 'Cancel').click()
      expect(confirms).toEqual([])
      expect((await status()).provisional).toBeUndefined()
      const confirmed = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'cpa.confirm' && h.target === `revision ${revision}`)
      expect(confirmed).toHaveLength(0)
      record('confirm-guard', { revision, noticedMs: noticed, confirmsSent: confirms.length })
    } finally {
      admin.off('request', onRequest)
      await other.context.close()
    }
  })

  test('a change whose answer is lost stays listed on every page and after signing in again, and is resolved from its exact history record without being sent again', async () => {
    const subject = 'cust-004'
    await go(admin, 'Accounts')
    // The gateway acts on the block but its answer is lost, and the
    // history look-up is held back too, so the outcome stays unknown.
    await admin.route('**/admin/blocked-accounts/*', async (route) => {
      if (route.request().method() !== 'PUT') return route.continue()
      await route.fetch()
      await route.abort('connectionreset')
    })
    await admin.route('**/admin/history/admin*', (route) => route.abort('connectionreset'))
    try {
      await admin.getByRole('main').getByRole('button', { name: 'Block an Account' }).click()
      await admin.getByLabel('Account ID').fill(subject)
      await admin.getByLabel('Reason').fill('lost answer test')
      await admin.getByRole('button', { name: 'Block Now' }).click()
      await expect(admin.getByText('No answer from the gateway: outcome unknown')).toBeVisible()
      const unresolved = admin.getByRole('region', { name: /^(A change has an unknown outcome|Outcome found)$/ })
      await expect(unresolved).toContainText(`Block ${subject}`)
      await expect(unresolved).toContainText('It could not be looked up')
      await admin.unroute('**/admin/blocked-accounts/*')

      // Still listed on another view.
      await go(admin, 'Overview')
      await expect(unresolved).toContainText(`Block ${subject}`)
      await shot(admin, '42-unknown-outcome-listed')

      // The session ends elsewhere; signing in again as the same
      // administrator still lists it.
      const tab = await adminContext.newPage()
      await tab.goto(adminURL + '/')
      await expect(tab.getByText('Signed in as admin')).toBeAttached()
      await tab.getByRole('button', { name: 'Sign Out' }).click()
      await expect(tab.getByRole('heading', { name: 'Sign in' })).toBeVisible()
      await tab.close()
      // The Overview's next status read, a second later, finds the session ended.
      await expect(admin.getByText(/^Your session has ended\. Sign in again\./)).toBeVisible({ timeout: 20_000 })
      await signIn(admin, 'admin', password)
      await expect(unresolved).toContainText(`Block ${subject}`)

      // Look-ups work again: the exact transition shows it applied, once.
      await admin.unroute('**/admin/history/admin*')
      await unresolved.getByRole('button', { name: /^Check Its Outcome/ }).click()
      await expect(unresolved.getByText(/^Applied: the gateway recorded it at /)).toBeVisible()
      const blocks = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'account.block' && h.target === subject)
      expect(blocks).toHaveLength(1)
      await unresolved.getByRole('button', { name: /^Dismiss/ }).click()
      await expect(unresolved).toHaveCount(0)
      record('unknown-outcome-register', { subject, blockTransitions: blocks.length })
    } finally {
      await admin.unroute('**/admin/blocked-accounts/*')
      await admin.unroute('**/admin/history/admin*')
    }
    expect((await adminCall('DELETE', `/admin/blocked-accounts/${subject}`, undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
  })

  test('signing out while a change waits for its answer forgets it: a late lost or definite answer neither lists it again nor touches a later change', async () => {
    const lostSubject = 'cust-006'
    const lateSubject = 'cust-007'
    const laterSubject = 'cust-008'
    const unresolved = admin.getByRole('region', { name: /^(A change has an unknown outcome|Outcome found)$/ })
    // hold lets the gateway act on a block, then keeps its answer back until
    // released, and then loses it or delivers it.
    async function hold(subject: string, answer: 'lost' | 'definite') {
      let release!: () => void
      const gate = new Promise<void>((resolve) => (release = resolve))
      let finished!: () => void
      const done = new Promise<void>((resolve) => (finished = resolve))
      await admin.route(`**/admin/blocked-accounts/${subject}`, async (route) => {
        if (route.request().method() !== 'PUT') return route.continue()
        const response = await route.fetch()
        await gate
        if (answer === 'lost') await route.abort('connectionreset')
        else await route.fulfill({ response })
        finished()
      })
      return { release, done }
    }
    async function block(subject: string) {
      await go(admin, 'Accounts')
      await admin.getByRole('main').getByRole('button', { name: 'Block an Account' }).click()
      await admin.getByLabel('Account ID').fill(subject)
      await admin.getByLabel('Reason').fill('sign-out race test')
      await admin.getByRole('button', { name: 'Block Now' }).click()
    }
    async function signOutHere() {
      await admin.getByRole('button', { name: 'Sign Out' }).click()
      await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    }
    // Late continuations run within moments of the answer; this gives them time.
    const settleTime = 1_000

    const lost = await hold(lostSubject, 'lost')
    const late = await hold(lateSubject, 'definite')
    try {
      await block(lostSubject)
      await expect(admin.getByText("Sent; waiting for the gateway's answer…")).toBeVisible()
      await signOutHere()
      lost.release()
      await lost.done
      await admin.waitForTimeout(settleTime)
      await signIn(admin, 'admin', password)
      await go(admin, 'Accounts')
      await expect(unresolved).toHaveCount(0)
      // Forgetting it here did not undo it: the gateway applied the block once.
      const lostBlocks = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'account.block' && h.target === lostSubject)
      expect(lostBlocks).toHaveLength(1)

      // A definite answer arriving after sign-out, while a later change by the
      // same administrator is unresolved, leaves that later change as it was.
      await block(lateSubject)
      await expect(admin.getByText("Sent; waiting for the gateway's answer…")).toBeVisible()
      await signOutHere()
      await signIn(admin, 'admin', password)
      await admin.route(`**/admin/blocked-accounts/${laterSubject}`, async (route) => {
        if (route.request().method() !== 'PUT') return route.continue()
        await route.fetch()
        await route.abort('connectionreset')
      })
      await admin.route('**/admin/history/admin*', (route) => route.abort('connectionreset'))
      await block(laterSubject)
      await expect(unresolved).toContainText(`Block ${laterSubject}`)
      await expect(unresolved).toContainText('It could not be looked up')
      late.release()
      await late.done
      await admin.waitForTimeout(settleTime)
      await expect(unresolved).toContainText(`Block ${laterSubject}`)
      await expect(unresolved).toContainText('It could not be looked up')
      await expect(unresolved).not.toContainText(lateSubject)
      await expect(unresolved.getByRole('heading')).toHaveText('A change has an unknown outcome')
      await unresolved.getByRole('button', { name: /^Stop Tracking/ }).click()
      await expect(unresolved).toHaveCount(0)
      record('sign-out-forgets-late-answers', { lostBlocks: lostBlocks.length })
    } finally {
      lost.release()
      late.release()
      await admin.unroute(`**/admin/blocked-accounts/${lostSubject}`)
      await admin.unroute(`**/admin/blocked-accounts/${lateSubject}`)
      await admin.unroute(`**/admin/blocked-accounts/${laterSubject}`)
      await admin.unroute('**/admin/history/admin*')
    }
    for (const subject of [lostSubject, lateSubject, laterSubject]) {
      expect((await adminCall('DELETE', `/admin/blocked-accounts/${subject}`, undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
    }
  })

  test('keyboard alone puts a change on trial and rolls it back and revokes a rule, while timers move neither focus nor the screen reader', async () => {
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    await trialStep(admin)
    await admin.getByLabel('Trial Length (Seconds)').focus()
    await admin.keyboard.press('Control+A')
    await admin.keyboard.type('600')
    await admin.keyboard.press('Enter')
    await expect(dialogButton(admin, 'Cancel')).toBeFocused()
    // Escape cancels and changes nothing.
    await admin.keyboard.press('Escape')
    await expect(admin.getByRole('dialog')).toHaveCount(0)
    expect((await status()).provisional).toBeUndefined()
    await trialStep(admin)
    await admin.getByLabel('Trial Length (Seconds)').focus()
    await admin.keyboard.press('Enter')
    await admin.keyboard.press('Tab')
    await expect(dialogButton(admin, 'Put on Trial')).toBeFocused()
    await admin.keyboard.press('Enter')
    await expect(admin.getByText(/^Version \d+ is on trial and active/)).toBeVisible()

    // With the trial active the page rereads and counts down every second;
    // focus stays put and nothing is announced.
    const confirmButton = trial(admin).getByRole('button', { name: 'Keep Change', exact: true })
    await confirmButton.focus()
    await admin.evaluate(() => {
      const page = window as unknown as { announced: string[] }
      page.announced = []
      new MutationObserver((records) => {
        for (const change of records) {
          const node = change.target instanceof Element ? change.target : change.target.parentElement
          const region = node?.closest('[aria-live]:not([aria-live="off"]), [role="status"], [role="alert"], [role="log"]')
          if (region) page.announced.push(region.textContent ?? '')
        }
      }).observe(document.body, { subtree: true, childList: true, characterData: true })
    })
    await admin.waitForTimeout(7_000)
    await expect(confirmButton).toBeFocused()
    const announced = await admin.evaluate(() => (window as unknown as { announced: string[] }).announced)
    expect(announced).toEqual([])

    const rollback = trial(admin).getByRole('button', { name: 'Undo Change', exact: true })
    await rollback.focus()
    await admin.keyboard.press('Enter')
    await expect(dialogButton(admin, 'Cancel')).toBeFocused()
    await admin.keyboard.press('Tab')
    await admin.keyboard.press('Enter')
    await expect(admin.getByText(/^Undone: version \d+ is active again\.$/)).toBeVisible()

    const id = await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 300 })
    await admin.getByRole('main').getByRole('button', { name: `Revoke rule ${id}` }).focus()
    await admin.keyboard.press('Enter')
    await expect(dialogButton(admin, 'Cancel')).toBeFocused()
    await admin.keyboard.press('Tab')
    await admin.keyboard.press('Enter')
    await expect(admin.getByText(`Rule ${id} revoked. It no longer applies.`)).toBeVisible()
    expect(await status()).toMatchObject({ overrides: [] })
    record('keyboard-urgent-actions', { announcedWhileWaiting: announced.length, override: id })
  })

  test('background reads pause after ten minutes without input, resume at the next input, and go on while a temporary rule is active', async () => {
    const page = await adminContext.newPage()
    try {
      await page.clock.install()
      await page.goto(adminURL + '/#/overview')
      await expect(page.getByRole('heading', { level: 1, name: 'Overview', exact: true })).toBeVisible()
      const reads: string[] = []
      page.on('request', (r) => reads.push(new URL(r.url()).pathname))
      const readsDuring = async (ms: number) => {
        const from = reads.length
        await page.clock.runFor(ms)
        await page.waitForTimeout(300)
        return reads.slice(from)
      }
      const notice = page.getByText(/^Auto-refresh paused after 10 minutes of inactivity/)
      expect((await readsDuring(30_000)).length).toBeGreaterThan(0)
      await readsDuring(10 * 60_000)
      expect(await readsDuring(60_000)).toEqual([])
      await expect(notice).toBeVisible()
      await shot(page, '43-updates-paused')

      // Any input resumes them.
      await page.mouse.move(40, 40)
      expect(await readsDuring(11_000)).toContain('/admin/status')
      await expect(notice).toHaveCount(0)

      // While a temporary rule is active they go on without input.
      const id = await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 600 })
      await readsDuring(11_000)
      await readsDuring(11 * 60_000)
      const during = await readsDuring(60_000)
      expect(during).toContain('/admin/status')
      await expect(notice).toHaveCount(0)
      await revokeOverride(admin, id)
      record('inactivity-pause', { readsWhileRuleActive: during.length })
    } finally {
      await page.close()
    }
  })

  test('request lookup keeps its search while moving between views without putting the account in the address, and a miss says what was searched', async () => {
    await go(admin, 'Request Lookup')
    await admin.getByLabel('Account').fill('cust-001')
    await admin.getByRole('button', { name: 'Find Requests' }).click()
    await expect(admin.getByText(/^Reading/)).toHaveCount(0)
    await expect(admin.getByText('No request matches.')).toHaveCount(0)
    await go(admin, 'Overview')
    await go(admin, 'Request Lookup')
    await expect(admin.getByLabel('Account')).toHaveValue('cust-001')
    await expect(admin.getByText('No request matches.')).toHaveCount(0)
    expect(admin.url()).not.toContain('cust-001')
    await admin.getByLabel('Account').fill('')
    await admin.getByLabel('Reference (Ref)').fill('0'.repeat(32))
    await admin.getByRole('button', { name: 'Find Requests' }).click()
    await expect(admin.getByText(/Only the latest 10,000 requests since the gateway started are searched/)).toBeVisible()
  })

  test('an address that names no view, an inherited name such as constructor included, opens the Overview', async () => {
    for (const name of ['constructor', '__proto__', 'toString', 'no-such-view']) {
      await go(admin, 'Request Lookup')
      await expect(admin).toHaveTitle('Request Lookup – Fetch Gateway')
      await admin.evaluate((fragment) => (window.location.hash = fragment), `#/${name}`)
      await expect(admin, name).toHaveTitle('Overview – Fetch Gateway')
      await expect(admin.locator('#page-title'), name).toBeVisible()
    }
  })

  test('returning to a hidden tab rereads the gateway at once, even on a page that does not poll', async () => {
    await go(admin, 'Changes')
    await admin.waitForTimeout(1500)
    const setVisibility = (state: 'hidden' | 'visible') =>
      admin.evaluate((value) => {
        Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value })
        document.dispatchEvent(new Event('visibilitychange'))
      }, state)
    try {
      const start = adminRequests.length
      await setVisibility('hidden')
      await admin.waitForTimeout(2000)
      expect(adminRequests.slice(start)).toEqual([])
      const shownAt = Date.now()
      await setVisibility('visible')
      await expect.poll(() => adminRequests.slice(start).map((url) => new URL(url).pathname)).toContain('/admin/status')
      const reread = Date.now() - shownAt
      expect(reread).toBeLessThan(2000)
      record('tab-return', { rereadMs: reread })
    } finally {
      await admin.evaluate(() => delete (document as unknown as Record<string, unknown>).visibilityState)
    }
  })

  test('a session ending during a trial says the gateway still enforces its deadline, and a trial that outlives the session is flagged', async () => {
    const confirmed = (await status()).confirmed
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const revision = await activate(admin, 86_400)
    // The session's lifetime (8 hours here) ends long before this deadline.
    await expect(admin.getByText(new RegExp(`^Your session ends at .+, before version ${revision}'s trial deadline\\.$`))).toBeVisible()

    // The session ends elsewhere; this page finds out at its next read
    // (every 3 s during a trial) and says what continues without it.
    const tab = await adminContext.newPage()
    await tab.goto(adminURL + '/')
    await expect(tab.getByText('Signed in as admin')).toBeAttached()
    await tab.getByRole('button', { name: 'Sign Out' }).click()
    await expect(tab.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    await tab.close()
    await expect(
      admin.getByText(new RegExp(`Version ${revision} was on trial when this page last read the gateway: unless someone keeps it, the gateway undoes it and returns to version ${confirmed} at `)),
    ).toBeVisible({ timeout: 15_000 })
    await shot(admin, '45-session-ended-during-trial')
    // Signed in again, the trial is still on: the session's end changed
    // nothing at the gateway.
    await signIn(admin, 'admin', password)
    expect((await status()).provisional).toBe(revision)
    await resolve(admin, 'rollback')
  })

  test('a status answer saying the stored policy could not be read is shown as not known, never as no policy', async () => {
    // A display check of the contract field only: the gateway's own answer
    // is proven by its Go test, because a failing store read cannot be
    // caused safely in this deployment. The real status answer is fetched
    // and given the form the gateway sends when that read fails.
    await admin.route('**/admin/status', async (route) => {
      const response = await route.fetch()
      const body = (await response.json()) as Record<string, unknown>
      delete body.confirmed
      delete body.provisional
      delete body.deadline
      body.stored_authority = 'unreadable'
      await route.fulfill({ response, json: body })
    })
    try {
      await go(admin, 'Overview')
      await expect(strip(admin)).toContainText('Not known')
      await expect(strip(admin)).not.toContainText('None yet')
      await expect(admin.getByText('The gateway could not read its stored policy.', { exact: true })).toBeVisible()
      await shot(admin, '46-stored-policy-unreadable')
    } finally {
      await admin.unroute('**/admin/status')
    }
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
      await expect(page.getByText('New installation: every request is refused until an administrator establishes the first policy (Changes).')).toBeVisible()

      // Idle: no request for longer than the idle timeout ends the session.
      const idleStart = Date.now()
      await page.waitForTimeout(sessions.idleSeconds * 1000 + 1500)
      const ended = page.getByText('Your session has ended. Sign in again.')
      if (!(await ended.isVisible())) await page.getByRole('button', { name: 'Refresh', exact: true }).click()
      await expect(ended).toBeVisible()
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
      // The page rereads the status each second, so it finds the ended
      // session by itself.
      await expect(page.getByText('Your session has ended. Sign in again.')).toBeVisible({ timeout: 5_000 })
      await context.close()
      record('session-expiry', { idleSeconds: sessions.idleSeconds, idleEndedMs: idleEnded, lifetimeSeconds: sessions.lifetimeSeconds, lifetimeEndedMs: lifetimeEnded })
    } finally {
      await sessions.stop()
    }
  })

  test('no secret appears in browser storage, URLs, console output, built files, logs or evidence, and no content policy violation occurred', async () => {
    const privateKey = demo.file('issuer-key.pem').split('\n')[1]
    const secrets = [password, ...handedOut, demo.file('service.secret').trim(), privateKey]
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
    const built = [...walk(applicationDist), ...walk(join(demo.web, 'admin'))]
    for (const file of built) check(file, readFileSync(file, 'utf8'))
    for (const name of ['audit.log', 'gateway.log', 'orders.log', 'inventory.log', 'reports.log', 'support.log', 'accounts.log', 'gateway.json'])
      check(name, demo.file(name))
    if (process.env.E2E_EVIDENCE) check('evidence file', readFileSync(process.env.E2E_EVIDENCE, 'utf8'))

    expect(leaks).toEqual([])
    const violations = consoleLines.filter((line) => /Content Security Policy|Refused to (load|execute|apply|connect)/.test(line))
    expect(violations).toEqual([])
    const consoleMessages = [...new Set(consoleLines.map((line) => line.replace(/\d+/g, 'N')))]
    record('secret-scan', { secretsChecked: secrets.length, builtFiles: built.length, requests: adminRequests.length, consoleMessages, scriptDialogs })
    expect(scriptDialogs).toBe(0)
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
