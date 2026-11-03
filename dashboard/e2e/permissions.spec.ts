import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { adminURL, build, Demo, webRoot } from './env'
import { enrol, go, signIn } from './pages'

// Administrator permissions read the same on every page: one set of words,
// with the exact permission name beside or behind it.

const password = 'correct horse battery staple 42'
const words = ['View Status', 'View History', 'Block Accounts', 'Change Policy']

let demo: Demo
let context: BrowserContext
let page: Page

test.describe.serial('Permission words', () => {
  test.beforeAll(async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    const enrolment = demo.setup()
    await demo.start()
    context = await browser.newContext({ viewport: { width: 1280, height: 860 } })
    page = await context.newPage()
    await page.goto(adminURL + '/')
    await enrol(page, 'admin', enrolment, password)
    await signIn(page, 'admin', password)
  })

  test.afterAll(async () => {
    await context?.close()
    await demo?.stop()
  })

  test('My Account and Team name each permission with the same words', async () => {
    await go(page, 'My Account')
    const list = page.locator('.permission-list li')
    await expect(list).toHaveText([
      `${words[0]} read`,
      `${words[1]} read_history`,
      `${words[2]} manage_subjects`,
      `${words[3]} manage_policy`,
    ])
    await go(page, 'Team')
    const row = page.getByRole('row').filter({ has: page.getByText('admin', { exact: true }) })
    await expect(row.locator('.chip')).toHaveText(words)
    await expect(row.locator('.chip').first()).toHaveAttribute('title', 'read')
  })
})
