// Shared set-up for the site specs: the Fetch app as the gateway's data plane
// serves it, before any policy exists, in one browser context that records
// every request, console line, content-policy violation and script dialog.
import { readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { build, Demo, siteURL, webRoot } from './env'

export let demo: Demo
export let context: BrowserContext
export let site: Page
export const requests: string[] = []
export const consoleLines: string[] = []
export const violations: string[] = []
let dialogs = 0

// scriptDialogs counts the script dialogs any page opened in this file.
export function scriptDialogs(): number {
  return dialogs
}

// files lists every file of a directory tree.
export function files(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    return statSync(path).isDirectory() ? files(path) : [path]
  })
}

// open loads a page route and waits until every read it started has an answer.
export async function open(page: Page, route: string) {
  await page.goto(siteURL + route, { waitUntil: 'networkidle' })
  await expect(page.locator('main')).toBeVisible()
  await expect(page.locator('[aria-busy="true"]')).toHaveCount(0)
}

// clean checks everything this file's pages did: no secret or token in a
// request or console line, no unexpected content-policy violation, no dialog.
function clean() {
  const secret = demo.file('service.secret').trim()
  expect(consoleLines.filter((line) => line.includes(secret) || /eyJ[\w-]+\.[\w-]+\./.test(line)), 'console').toEqual([])
  expect(requests.filter((url) => url.includes(secret)), 'requests').toEqual([])
  expect(violations, 'content-policy violations').toEqual([])
  expect(dialogs, 'script dialogs').toBe(0)
}

// useSite starts this file's gateway and browser context before its tests and,
// after them, runs the clean check and closes both.
export function useSite() {
  test.beforeAll(async ({ browser }) => {
    requests.length = 0
    consoleLines.length = 0
    violations.length = 0
    dialogs = 0
    demo = new Demo(build(), webRoot())
    demo.setup()
    await demo.start()
    context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    await context.exposeBinding('reportViolation', (_source, text: string) => {
      violations.push(text)
    })
    await context.addInitScript(() => {
      document.addEventListener('securitypolicyviolation', (event) => {
        void (window as unknown as { reportViolation: (text: string) => Promise<void> }).reportViolation(`${event.effectiveDirective} ${event.blockedURI}`)
      })
    })
    context.on('request', (r) => requests.push(r.url()))
    context.on('console', (m) => consoleLines.push(m.text()))
    context.on('page', (page) =>
      page.on('dialog', (dialog) => {
        dialogs++
        void dialog.dismiss()
      }),
    )
    site = await context.newPage()
  })

  test.afterAll(async () => {
    try {
      if (demo) clean()
    } finally {
      await context?.close()
      await demo?.stop()
    }
  })
}
