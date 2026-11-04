import { expect, type Browser, type BrowserContext, type Page } from '@playwright/test'
import type { Candidate, Policy, Revision } from '../src/shared/api'
import { adminURL, deployment, type Demo } from './env'
import { activate, dialogButton, enrol, go, resolve, shot, signIn, stage } from './pages'
import { expectLayoutOnlyStorage } from './storage'
import { establish } from './setup'

// The Policy Designer suite's shared deployment and steps: each spec file
// opens its own native Gateway and signs in, and the steps below drive the
// Designer as an administrator would.

export const password = 'designer controlled local password 42'
export let demo: Demo
export let context: BrowserContext
export let page: Page
export let baseline: Policy
export let enrolment: string
export let policyViolations: string[] = []
export let browserMessages: string[] = []
export let requests: string[] = []

export async function api(method: string, path: string, data?: unknown, headers: Record<string, string> = {}, session = context) {
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

export async function currentPolicy(): Promise<Revision & { document: Policy }> {
  const reply = await api('GET', '/admin/policy')
  expect(reply.status).toBe(200)
  return reply.body
}

export async function candidates(session = context): Promise<Candidate[]> {
  const reply = await api('GET', '/admin/candidates?limit=100', undefined, {}, session)
  expect(reply.status).toBe(200)
  return reply.body.items
}

export async function capture(name: string, target = page) {
  const original = target.viewportSize()!
  for (const width of [1440, 768, 375]) {
    await target.setViewportSize({ width, height: 900 })
    await shot(target, `${name}-${width}`)
    expect(await target.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${name} width ${width}`).toBe(true)
  }
  await target.setViewportSize(original)
}

export async function designer() {
  await go(page, 'Policy')
  await expect(page.getByRole('tab', { name: 'Canvas', exact: true })).toBeVisible()
  await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
  await expect(node('role:CUSTOMER')).toBeVisible()
}

export async function discard() {
  const button = page.getByRole('button', { name: 'Discard', exact: true })
  if (await button.isEnabled()) {
    await button.click()
    await dialogButton(page, 'Discard Draft').click()
  }
}

export async function applyJson(document: Policy) {
  await page.getByRole('tab', { name: 'JSON', exact: true }).click()
  await page.getByLabel('Complete Policy Document (JSON)', { exact: true }).fill(JSON.stringify(document, null, 2))
  await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
  await expect(page.locator('#designer-json-document-error')).toHaveCount(0)
}

export async function stagedDraft(): Promise<number> {
  await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
  await dialogButton(page, 'Save Draft Change').click()
  const result = page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is active yet\.$/)
  await expect(result).toBeVisible()
  return Number((await result.textContent())!.match(/change (\d+)/)![1])
}

export async function restoreBaseline() {
  await stage(page, { text: JSON.stringify(baseline) })
  await activate(page, 300)
  await resolve(page, 'confirm')
  await designer()
  await discard()
}

// formItem opens one box's editor from the Form view's list.
export async function formItem(name: string) {
  const list = page.getByRole('navigation', { name: 'Policy objects' })
  const group = ['Service address', 'Endpoint', 'Role', 'API'].find((kind) => name.startsWith(kind + ' '))!
  const toggle = list.getByRole('button', { name: new RegExp(`^${group === 'Service address' ? 'Service Addresses' : group + 's'}\\b`) })
  if ((await toggle.getAttribute('aria-expanded')) === 'false') await toggle.click()
  // An endpoint's name is its kind, its route and its ID: "Endpoint GET /x x.id".
  const endpoint = name.match(/^Endpoint (\S+)$/)
  const id = endpoint?.[1].replace(/\./g, '\\.')
  const item = id ? list.getByRole('button', { name: new RegExp(`^Endpoint [A-Z]+ /\\S* ${id}$`) }) : list.getByRole('button', { name, exact: true })
  await item.click()
}

// node is a box's row on the canvas; an endpoint's row shows once its API's
// group is open.
export function node(id: string, target = page) {
  return target.locator(`.designer-row[data-box="${id}"]`)
}

export async function expandAll(target = page) {
  await target.getByRole('button', { name: 'Expand All', exact: true }).click()
}

export function selected(target = page) {
  return target.getByRole('complementary', { name: 'Selected policy box' })
}

// open starts a fresh deployment and a signed-in Designer tab that reports
// content policy violations, console lines and requests.
export async function open(browser: Browser) {
  policyViolations = []
  browserMessages = []
  requests = []
  demo = deployment()
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
}

// ready keeps the Fetch baseline as the active policy and opens the canvas.
export async function ready() {
  await establish(page, demo)
  await designer()
}

// expectPolicyKept checks that no content policy violation was reported and
// that only the layout preference reached browser storage.
export async function expectPolicyKept() {
  expect(policyViolations).toEqual([])
  expect(
    browserMessages.filter((line) => /Content Security Policy|Refused to (load|execute|apply|connect)|violates.*directive/i.test(line)),
  ).toEqual([])
  await expectLayoutOnlyStorage(page, [password, enrolment])
}

// finish runs the content policy check on this file's deployment, then stops it.
export async function finish() {
  try {
    await expectPolicyKept()
  } finally {
    await context?.close()
    await demo?.stop()
  }
}
