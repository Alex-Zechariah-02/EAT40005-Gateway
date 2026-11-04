// Shared set-up for the role specs: each file runs its own gateway, services
// and Fetch Accounts with the Fetch baseline policy confirmed, and checks every
// step against the services' own receipts or the Admin API as well as what the
// page shows.
import { expect, test, type Browser, type BrowserContext, type Page } from '@playwright/test'
import { AdminApi, build, Demo, raw, siteURL, webRoot, type Receipt } from './env'
import { demoPassword, signIn } from './pages'

export let demo: Demo
export const api = new AdminApi()
const adminPassword = 'controlled client flows password 42'
const opened: BrowserContext[] = []
// Each person signs in once and keeps their tab, as in the demonstration:
// the baseline allows ten sign-ins a minute from one address, and every
// person here signs in from this machine.
const people = new Map<string, Page>()
const violations: string[] = []
const pageErrors: string[] = []
let dialogs = 0

// scriptDialogs counts the script dialogs any tab opened in this file.
export function scriptDialogs(): number {
  return dialogs
}

// person returns this person's tab, signed in, opening a new browser context
// for someone not seen before (or for nobody, when email is null).
export async function person(browser: Browser, email: string | null): Promise<Page> {
  const known = email ? people.get(email) : undefined
  if (known) {
    await known.goto(siteURL + '/', { waitUntil: 'networkidle' })
    return known
  }
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  if (!email) opened.push(context)
  await context.exposeBinding('reportViolation', (_source, text: string) => {
    violations.push(text)
  })
  await context.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (event) => {
      void (window as unknown as { reportViolation: (text: string) => Promise<void> }).reportViolation(`${event.effectiveDirective} ${event.blockedURI}`)
    })
  })
  const page = await context.newPage()
  page.on('pageerror', (error) => pageErrors.push(error.message))
  page.on('dialog', (dialog) => {
    dialogs++
    void dialog.dismiss()
  })
  if (email) {
    await signIn(page, siteURL, email)
    people.set(email, page)
  }
  return page
}

// leave closes a person's tab; they sign in again if they come back.
export async function leave(email: string) {
  await people.get(email)?.context().close()
  people.delete(email)
}

// since returns the receipts a service recorded after the given count.
export async function since(service: string, count: number): Promise<Receipt[]> {
  return (await demo.receipts(service)).slice(count)
}

// ref reads the full reference a Ref shows in its tooltip (the page shows a
// short form, and Copy full reference copies this value).
export const ref = async (page: Page) => ((await page.getByTitle(/^Full reference [0-9a-f]{32}$/).first().getAttribute('title')) ?? '').replace(/^Full reference /, '')

// clean checks that no flow in this file met a content-policy violation, page
// error or script dialog, and that the demo password never left the sign-in form.
async function clean() {
  expect(violations, 'content-policy violations').toEqual([])
  expect(pageErrors, 'page errors').toEqual([])
  expect(dialogs, 'script dialogs').toBe(0)
  for (const service of ['orders', 'inventory', 'reports', 'support', 'accounts']) {
    const receipts = JSON.stringify(await demo.receipts(service))
    expect(receipts).not.toContain(demoPassword)
  }
  expect((await raw('GET', siteURL + '/')).body).not.toContain(demoPassword)
}

// useRoles starts this file's deployment before its tests, rests every kept
// tab between tests and, after them, runs the clean check and stops it all.
export function useRoles() {
  test.beforeAll(async () => {
    violations.length = 0
    pageErrors.length = 0
    dialogs = 0
    demo = new Demo(build(), webRoot())
    const secret = demo.setup()
    await demo.start()
    await api.enrol('admin', secret, adminPassword)
    await api.signIn('admin', adminPassword)
    await api.baseline(demo)
  })

  // Between tests every kept tab rests on a blank page, so no open board (a
  // merchant's or rider's rereads every few seconds) adds requests to the
  // next test; the tab keeps its session.
  test.afterEach(async () => {
    for (const context of opened.splice(0)) await context.close()
    for (const page of people.values()) await page.goto('about:blank')
  })

  test.afterAll(async () => {
    try {
      if (demo) await clean()
    } finally {
      for (const email of [...people.keys()]) await leave(email)
      await demo?.stop()
    }
  })
}
