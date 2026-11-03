import { expect, test, type Page } from '@playwright/test'
import { go } from './pages'
import { admin, enrolled, finish, open } from './setup'

// Administrator permissions read the same on every page: one set of words,
// with the exact permission name beside or behind it.

const words = ['View Status', 'View History', 'Block Accounts', 'Change Policy']

let page: Page

test.describe.serial('Permission words', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await enrolled()
    page = admin
  })

  test.afterAll(finish)

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
