import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Locator, type Page } from '@playwright/test'

export async function useSlot(site: Page, slot: 'Customer' | 'Staff' | 'Manager', token: string) {
  // Set up identities on the inspector, whose reads are always deliberate.
  await site.getByRole('navigation', { name: 'Client pages' }).getByRole('link', { name: 'Gateway inspector', exact: true }).click()
  await site.getByRole('banner').getByRole('button', { name: 'Manage sign-ins', exact: true }).click()
  const dialog = site.getByRole('dialog', { name: 'Manage sign-ins' })
  await dialog.getByLabel(`${slot} token`, { exact: true }).fill(token)
  await dialog.getByRole('button', { name: 'Done', exact: true }).click()
  await site.getByLabel('Signed in as', { exact: true }).selectOption(slot)
}

export async function shopPage(site: Page, name: 'Shop' | 'My orders' | 'Orders' | 'Stock' | 'Reports' | 'Gateway inspector') {
  await site.getByRole('navigation', { name: 'Client pages' }).getByRole('link', { name, exact: true }).click()
  const heading = name === 'Shop' ? 'The workshop shelf' : name === 'Orders' ? 'All orders' : name
  await expect(site.getByRole('heading', { level: 1, name: heading, exact: true })).toBeVisible()
  await expect(site.getByText('Reading from the shop…', { exact: true })).toHaveCount(0)
}

export async function shopStatus(region: Locator): Promise<number> {
  const badge = region.locator('.shop-result-ref .badge').last()
  await expect(badge).toHaveText(/HTTP \d+|Refused by the Gateway: \d+|No answer/)
  const match = (await badge.textContent())?.match(/(?:HTTP |Gateway: )(\d+)/)
  return match ? Number(match[1]) : 0
}

export async function captureShop(site: Page, name: string) {
  const original = site.viewportSize()!
  for (const width of [1440, 768, 375]) {
    await site.setViewportSize({ width, height: 900 })
    await shot(site, `${name}-${width}`)
    for (const table of await site.locator('.table-scroll.shop-table').all()) {
      if (await table.evaluate((element) => element.scrollWidth > element.clientWidth)) {
        await table.evaluate((element) => {
          element.scrollLeft = element.scrollWidth
        })
        await shot(site, `${name}-table-end-${width}`)
        await table.evaluate((element) => {
          element.scrollLeft = 0
        })
      }
    }
    expect(await site.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${name} ${width}`).toBe(true)
  }
  await site.setViewportSize(original)
}

// UI steps shared by the tests. They use only what a person sees and
// operates: roles, labels and visible text.

// scenarioGroup opens the security scenarios and one group of them.
export async function scenarioGroup(site: Page, name: string) {
  await shopPage(site, 'Gateway inspector')
  const link = site.getByRole('navigation', { name: 'Scenario groups' }).getByRole('link', { name, exact: true })
  await link.click()
  await expect(link).toHaveAttribute('aria-current', 'page')
}

// operation finds one operation's or scenario's section by its title.
export function operation(site: Page, title: string): Locator {
  return site.getByRole('region', { name: title, exact: true })
}

// observed waits for a scenario's answer and returns the status shown under
// Observed (0: no answer) and whether the page reports it as expected.
export async function observed(region: Locator): Promise<{ status: number; asExpected: boolean }> {
  const badge = region.locator('.compare-observed .badge')
  await expect(badge).toHaveText(/HTTP \d+|no answer/)
  const match = ((await badge.textContent()) ?? '').match(/HTTP (\d+)/)
  const verdict = ((await region.locator('.compare-observed .match').textContent()) ?? '').trim()
  expect(['As expected', 'Not as expected']).toContain(verdict)
  return { status: match ? Number(match[1]) : 0, asExpected: verdict === 'As expected' }
}

// sendScenario sends one scenario's request and returns what it observed.
export async function sendScenario(region: Locator) {
  await region.getByRole('button', { name: 'Send', exact: true }).click()
  return observed(region)
}

// inspect opens a scenario's full answer: the request path, the facts, the
// request as sent and the response headers.
export async function inspect(region: Locator) {
  const details = region.locator(':scope > details.inspect')
  if (!(await details.evaluate((d) => (d as HTMLDetailsElement).open))) await details.locator(':scope > summary').click()
}

// hops reads the request path an operation shows: each hop's name and state.
export async function hops(region: Locator): Promise<string[]> {
  return (await region.locator('.path .hop').allInnerTexts()).map((text) => text.replace(/\s+/g, ' ').trim())
}

// traceStates reads the full trace an answer shows: each of the gateway's
// checks, in order, with what the answer shows about it, as its list text
// reads ("Token: refused the request").
export async function traceStates(region: Locator): Promise<{ check: string; state: string }[]> {
  const items = await region.locator('.trace:not(.trace-compact) .trace-check').allTextContents()
  return items.map((text) => {
    const [check, state] = text.split(': ')
    return { check: check.trim(), state: state.trim() }
  })
}

// runAnswers reads a run table: the answer badge of each row, in order.
export async function runAnswers(region: Locator): Promise<string[]> {
  return (await region.locator('table.runs tbody tr td:nth-child(3)').allInnerTexts()).map((text) => text.trim())
}

// record appends one evidence entry when E2E_EVIDENCE names a file. Entries
// hold statuses, receipts and event fields, never tokens or secrets.
export function record(id: string, data: Record<string, unknown>) {
  const file = process.env.E2E_EVIDENCE
  if (!file) return
  appendFileSync(file, JSON.stringify({ id, time: new Date().toISOString(), ...data }) + '\n')
}

// shot saves a full-page screenshot for visual review when E2E_SHOTS names a
// directory. Screens show what the browser rendered, never a secret: tokens
// are typed into password fields.
export async function shot(page: Page, name: string) {
  const dir = process.env.E2E_SHOTS
  if (!dir) return
  if (/^\d\d-/.test(name)) name = `client-${name}`
  mkdirSync(dir, { recursive: true })
  // From the top, so the sticky sidebar and state rail are drawn where they
  // belong, and with animations finished.
  await page.evaluate(() => window.scrollTo(0, 0))
  const size = await page.evaluate(() => ({ width: innerWidth, height: document.documentElement.scrollHeight, step: innerHeight }))
  if (size.height > 16_000) {
    // Chrome repeats bitmap bands above its compositor limit. Capture actual
    // viewports instead; the manifest also permits lossless external stitching.
    const parts: { file: string; y: number }[] = []
    for (let top = 0; top < size.height; top += size.step) {
      const y = await page.evaluate(async (offset) => {
        window.scrollTo(0, offset)
        await new Promise(requestAnimationFrame)
        await new Promise(requestAnimationFrame)
        return scrollY
      }, top)
      const file = `${name}--${String(y).padStart(6, '0')}.png`
      await page.screenshot({ path: join(dir, file), fullPage: false, animations: 'disabled' })
      parts.push({ file, y })
    }
    writeFileSync(join(dir, `${name}.capture.json`), JSON.stringify({ method: 'viewport-segments', ...size, parts }, null, 2))
    await page.evaluate(() => window.scrollTo(0, 0))
    return
  }
  await page.screenshot({ path: join(dir, `${name}.png`), fullPage: true, animations: 'disabled' })
}
