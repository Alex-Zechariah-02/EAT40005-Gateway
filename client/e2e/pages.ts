import { appendFileSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'
import AxeBuilder from '@axe-core/playwright'
import { expect, type Locator, type Page } from '@playwright/test'

// Steps and checks shared by the tests. They use only what a person sees and
// operates: roles, labels and visible text.

// record appends one evidence entry when E2E_EVIDENCE names a file. Entries
// hold statuses, references and receipts, never tokens or secrets.
export function record(id: string, data: Record<string, unknown>) {
  const file = process.env.E2E_EVIDENCE
  if (!file) return
  appendFileSync(file, JSON.stringify({ id, time: new Date().toISOString(), ...data }) + '\n')
}

// shot saves a full-page screenshot for visual review when E2E_SHOTS names a
// directory. Screens show what the browser rendered, never a secret.
export async function shot(page: Page, name: string) {
  const dir = process.env.E2E_SHOTS
  if (!dir) return
  mkdirSync(dir, { recursive: true })
  await page.evaluate(() => window.scrollTo(0, 0))
  await page.screenshot({ path: join(dir, `${name}.png`), fullPage: true, animations: 'disabled' })
}

// accessible runs axe's WCAG 2.2 A and AA rules and checks that nothing on
// the page is wider than the window and that no id is used twice (a label
// names the first element with its id, hidden or not).
export async function accessible(page: Page, name: string) {
  const result = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(
    result.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`),
    name,
  ).toEqual([])
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth), `${name} overflow`).toBeLessThanOrEqual(0)
  const repeated = await page.evaluate(() => {
    const ids = [...document.querySelectorAll('[id]')].map((element) => element.id)
    return [...new Set(ids.filter((id, index) => ids.indexOf(id) !== index))]
  })
  expect(repeated, `${name} repeated ids`).toEqual([])
}

// The password of every demonstration account, shown on the sign-in page.
export const demoPassword = 'FetchDemo2026!'

// signIn signs in through Fetch Accounts with the sign-in form and waits for
// the person's home page.
export async function signIn(page: Page, base: string, email: string) {
  await page.goto(base + '/signin', { waitUntil: 'networkidle' })
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password', { exact: true }).fill(demoPassword)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Sign in to Fetch' })).toHaveCount(0)
}

// addToBasket opens a shop and adds one of its items with the item's own
// button on the menu.
export async function addToBasket(page: Page, base: string, merchant: string, itemName: string) {
  await page.goto(`${base}/store/${merchant}`, { waitUntil: 'networkidle' })
  await page.getByRole('button', { name: `Add ${itemName} to basket` }).click()
}

// placeOrder fills the checkout form, chooses the payment and places the
// order; it returns the new order's ID from the page it lands on.
export async function placeOrder(page: Page, base: string, payment: 'Cash on delivery' | 'Fetch wallet'): Promise<string> {
  await page.goto(base + '/checkout', { waitUntil: 'networkidle' })
  await page.getByLabel('Street address').fill('12 Jalan Telawi 3')
  await page.getByLabel('Postcode').fill('59100')
  await page.getByLabel(payment).check()
  await page.getByRole('button', { name: /^Place order · RM / }).click()
  await page.waitForURL(/\/my-orders\/ORD-\d+\?placed=1$/)
  return new URL(page.url()).pathname.split('/').pop()!
}

// orderCard is one order on a merchant's, rider's or support board.
export function orderCard(page: Page, order: string): Locator {
  return page.getByRole('article', { name: `Order #${order.replace(/^ORD-/, '')}` })
}

// storage lists what the page keeps in the browser: every sessionStorage and
// localStorage key, and the cookies script can read.
export function storage(page: Page) {
  return page.evaluate(() => ({
    session: Object.keys(sessionStorage).sort(),
    local: Object.entries(localStorage).sort(),
    cookie: document.cookie,
  }))
}
