import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Browser, type BrowserContext, type Locator, type Page } from '@playwright/test'
import { adminURL, applicationDist, deployment, raw, siteURL, type Demo, type Receipt } from './env'
import { activate, dialogButton, enrol, expectLayoutOnlyStorage, resolve, revisions, signIn, stage } from './pages'

// The dashboard suite's shared deployment and steps. Each spec file opens one
// demonstration deployment in beforeAll and drives it as an operator and its
// administrators would. Every step checks what the dashboard shows against
// independent evidence: the Admin API read directly, the gateway's data plane
// called with test tokens, the Fetch services' own receipts and state, and the
// gateway's security events.

export const password = 'correct horse battery staple 42'
export const uiPolicy =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

export let demo: Demo
export let enrolmentSecret: string
export let adminContext: BrowserContext
export let admin: Page
let tokens: Record<string, string> = {}
// Every request the dashboard's pages made, every console line and whether a
// script dialog ever opened.
export let adminRequests: string[] = []
let consoleLines: string[] = []
export let scriptDialogs = 0
// Every token and one-time secret handed out, for the final secret scan.
export let handedOut: string[] = []

export function token(subject: string): string {
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

export async function newContext(browser: Browser): Promise<BrowserContext> {
  const context = await browser.newContext()
  watch(context)
  return context
}

// adminCall reads or changes state through the Admin API directly, with the
// dashboard context's session, as an independent check of what it shows.
export async function adminCall(method: string, path: string, body?: unknown, headers: Record<string, string> = {}) {
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

export async function status(): Promise<StatusBody> {
  const reply = await adminCall('GET', '/admin/status')
  expect(reply.status).toBe(200)
  return reply.body as StatusBody
}

// listAll reads every page of an Admin API list.
export async function listAll<T>(path: string): Promise<T[]> {
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

export async function stock(item: string): Promise<number> {
  const state = await demo.state('inventory')
  const items = (state.data as { items: { item: { id: string }; stock: number }[] }).items
  return items.find((i) => i.item.id === item)!.stock
}

// call sends one request through the gateway's data plane as the subject, as
// an ordinary API client would, and returns the status together with the
// receipts the named service recorded for it.
export async function call(service: string, method: string, path: string, subject: string, body?: unknown) {
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
export function admitted(reply: { status: number; receipts: Receipt[] }) {
  expect(reply.status).toBe(200)
  expect(reply.receipts).toHaveLength(1)
  expect(reply.receipts[0].status).toBe(200)
}

export const summary = (subject: string) => call('reports', 'GET', '/reports/summary', subject)
export const sales = (subject: string) => call('reports', 'GET', '/reports/sales', subject)
export const adjust = (subject: string, delta: number) => call('inventory', 'PATCH', '/inventory/stock/ITM-501', subject, { delta })

// propose stages, from Versions, the confirmed policy with one extra
// permit, and returns the change's id.
export async function propose(page: Page, permit: { role: string; operation: string }): Promise<number> {
  await revisions(page)
  await page.getByRole('button', { name: 'Start a Change from This' }).click()
  const editor = page.getByLabel('Complete Policy Document (JSON)')
  await expect(editor).not.toHaveValue('')
  const document = JSON.parse(await editor.inputValue())
  document.permits.push(permit)
  return stage(page, { text: JSON.stringify(document, null, 2) })
}

// signedInPage opens the dashboard in a new browser context and signs in.
export async function signedInPage(browser: Browser, name: string, secret = password) {
  const context = await newContext(browser)
  const page = await context.newPage()
  await page.goto(adminURL + '/')
  await signIn(page, name, secret)
  return { context, page }
}

// sessionCookie returns the dashboard context's session cookie as a header,
// for requests that must carry it with headers a browser would not send.
export async function sessionCookie(context = adminContext): Promise<string> {
  const cookie = (await context.cookies(adminURL)).find((c) => c.name === '__Host-gateway-admin')!
  return `${cookie.name}=${cookie.value}`
}

// auditEvents reads the gateway's audit file. After an interrupted write the
// file holds one incomplete line, which the gateway terminates and names in an
// audit.recovered event at its next start; that line is skipped here.
export function auditEvents(): Record<string, unknown>[] {
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

export function fact(page: Page, label: string) {
  return page.getByRole('main').getByText(label, { exact: true }).locator('xpath=following-sibling::dd[1]')
}

// strip is the Overview's status strip: active policy, change on trial,
// temporary rules.
export const strip = (page: Page) => page.getByRole('region', { name: 'Policy Status' })

// fontsInUse lists the page's registered faces and whether its body text and
// its code are set in the interface faces.
export function fontsInUse(page: Page) {
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
export function rowFor(scope: Locator, id: number) {
  return scope.locator('tbody tr').filter({ has: scope.page().locator('td:first-child', { hasText: new RegExp(`^${id}$`) }) })
}

// open starts a fresh deployment and an unsigned dashboard tab, and forgets
// the previous file's tokens and evidence.
export async function open(browser: Browser) {
  tokens = {}
  adminRequests = []
  consoleLines = []
  scriptDialogs = 0
  handedOut = []
  demo = deployment()
  enrolmentSecret = demo.setup()
  handedOut.push(enrolmentSecret)
  await demo.start()
  adminContext = await newContext(browser)
  admin = await adminContext.newPage()
}

// enrolled enrols the first administrator and signs in, as at a new installation.
export async function enrolled() {
  await admin.goto(adminURL + '/')
  await enrol(admin, 'admin', enrolmentSecret, password)
  await signIn(admin, 'admin', password)
}

// establish makes the deny-all policy version 1, then puts the Fetch
// baseline on trial and keeps it.
export async function establish(page: Page, deployment: Demo) {
  await stage(page, { file: deployment.path('deny-all.json') })
  await page.getByRole('button', { name: 'Establish as the First Policy' }).click()
  await page.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
  await dialogButton(page, 'Establish').click()
  await expect(page.getByText('Established: version 1 is the active policy.')).toBeVisible()
  await stage(page, { file: deployment.path('baseline.json') })
  await activate(page, 600)
  await resolve(page, 'confirm')
}

// ready signs the administrator in with the baseline as the kept policy.
export async function ready() {
  await enrolled()
  await establish(admin, demo)
}

// expectNoSecrets checks that no secret reached browser storage, URLs,
// console output, built files, logs or evidence, that no content policy
// violation occurred and that no script dialog opened.
export async function expectNoSecrets() {
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
  expect(scriptDialogs).toBe(0)
  return { secrets: secrets.length, built: built.length, consoleMessages }
}

// finish runs the scan on this file's deployment, then stops it.
export async function finish() {
  try {
    await expectNoSecrets()
  } finally {
    await adminContext?.close()
    await demo?.stop()
  }
}
