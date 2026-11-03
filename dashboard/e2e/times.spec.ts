import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Browser, type BrowserContext, type Page } from '@playwright/test'
import { adminURL, build, Demo, webRoot } from './env'
import { dialogButton, enrol, expectLayoutOnlyStorage, go, signIn, stage } from './pages'

// Every time on screen uses one format in the zone chosen on My Account:
// 24-hour, day first, with the zone, and the same moment in UTC on hover or
// focus. The browser runs in Asia/Kuching (UTC+08:00) with a 12-hour locale,
// so a time formatted by the browser's defaults would show.

const password = 'correct horse battery staple 42'
const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const full = /^\d{1,2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4}, \d{2}:\d{2}:\d{2} UTC[+-]\d{2}:\d{2}$/
// Dense tables drop the date when it is today and name the zone once, in
// the column header.
const compact = /^(\d{1,2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4}, )?\d{2}:\d{2}:\d{2}$/
const pages = [
  'overview',
  'live-traffic',
  'request-lookup',
  'security-log',
  'access-checker',
  'limits',
  'advice',
  'apis',
  'policy',
  'versions',
  'changes',
  'temporary-rules',
  'accounts',
  'history',
  'team',
  'my-account',
]

let demo: Demo
let context: BrowserContext
let page: Page
const pageErrors: string[] = []

// shown is how an instant must read at a fixed offset from UTC, in minutes.
function shown(iso: string, offset: number): string {
  const d = new Date(Date.parse(iso) + offset * 60_000)
  const pad = (n: number) => String(n).padStart(2, '0')
  const sign = offset < 0 ? '-' : '+'
  const zone = `UTC${sign}${pad(Math.floor(Math.abs(offset) / 60))}:${pad(Math.abs(offset) % 60)}`
  return `${d.getUTCDate()} ${months[d.getUTCMonth()]} ${d.getUTCFullYear()}, ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())} ${zone}`
}

async function newPage(browser: Browser, init?: () => void): Promise<{ context: BrowserContext; page: Page }> {
  const opened = await browser.newContext({ viewport: { width: 1280, height: 860 }, timezoneId: 'Asia/Kuching', locale: 'en-US' })
  if (init) await opened.addInitScript(init)
  const tab = await opened.newPage()
  tab.on('pageerror', (error) => pageErrors.push(error.message))
  return { context: opened, page: tab }
}

async function open(target: Page, view: string) {
  await target.evaluate((hash) => (window.location.hash = hash), `#/${view}`)
  await expect(target.getByText(/^Reading/)).toHaveCount(0)
}

function zoneSelect(target: Page) {
  return target.getByLabel('Time Zone', { exact: true })
}

// sessionEnd is the My Account time whose zone the setting changes.
function sessionEnd(target: Page) {
  return target.getByRole('main').locator('dd time').first()
}

test.describe.serial('One time format and the Time Zone setting', () => {
  test.beforeAll(async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    const enrolment = demo.setup()
    await demo.start()
    ;({ context, page } = await newPage(browser))
    await page.goto(adminURL + '/')
    await enrol(page, 'admin', enrolment, password)
    await signIn(page, 'admin', password)
    // A first policy gives the Versions and History tables their rows.
    await stage(page, { file: demo.path('deny-all.json') })
    await page.getByRole('button', { name: 'Establish as the First Policy' }).click()
    await page.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
    await dialogButton(page, 'Establish').click()
    await expect(page.getByText(/^Established: version 1 is the active policy\.$/)).toBeVisible()
  })

  test.afterAll(async () => {
    await context?.close()
    await demo?.stop()
  })

  test('headers and table rows show 24-hour, day first, with the zone; no page shows AM or PM', async () => {
    await go(page, 'Overview')
    const stamp = page.locator('.read-stamp time').first()
    await expect(stamp).toHaveText(full)
    expect(await stamp.textContent()).toBe(shown((await stamp.getAttribute('datetime'))!, 480))
    await go(page, 'History')
    const row = page.getByRole('main').locator('tbody tr time').first()
    await expect(row).toHaveText(full)
    expect(await row.textContent()).toBe(shown((await row.getAttribute('datetime'))!, 480))
    for (const view of pages) {
      await open(page, view)
      expect(await page.getByRole('main').innerText(), view).not.toMatch(/\b\d{1,2}:\d{2}(:\d{2})?\s?(AM|PM|am|pm)\b/)
      for (const time of await page.getByRole('main').locator('time:not(.compact)').all()) {
        await expect(time, view).toHaveText(full)
      }
      for (const time of await page.getByRole('main').locator('time.compact').all()) {
        await expect(time, view).toHaveText(compact)
        const column = await time.evaluate((e) => {
          const cell = e.closest('td')!
          return cell.closest('table')!.querySelectorAll('thead th')[cell.cellIndex].textContent
        })
        expect(column, view).toMatch(/\(UTC[+-]\d{2}:\d{2}\)$/)
      }
    }
  })

  test('pointing at or focusing a time shows the same moment in UTC', async () => {
    await go(page, 'History')
    const time = page.getByRole('main').locator('tbody tr time').first()
    const utc = shown((await time.getAttribute('datetime'))!, 0)
    await time.hover()
    await expect(page.locator('.tooltip')).toHaveText(utc)
    await page.mouse.move(0, 0)
    await page.locator('body').click({ position: { x: 1, y: 1 } })
    await expect(page.locator('.tooltip')).toHaveCount(0)
    // Keyboard focus shows the same tip.
    for (let i = 0; i < 80 && !(await time.evaluate((e) => e === document.activeElement)); i++) await page.keyboard.press('Tab')
    await expect(time).toBeFocused()
    await expect(page.locator('.tooltip')).toHaveText(utc)
  })

  test('the Time Zone setting offers Automatic, UTC or a chosen zone and changes every time at once', async () => {
    await go(page, 'My Account')
    const select = zoneSelect(page)
    await expect(select).toHaveValue('auto')
    await expect(select.locator('option[value="auto"]')).toHaveText('Automatic: Asia/Kuching (UTC+08:00)')
    await expect(select.locator('option[value="UTC"]')).toHaveText('UTC')
    await expect(select.locator('option[value="Australia/Darwin"]')).toHaveText('Australia/Darwin (UTC+09:30)')
    const end = sessionEnd(page)
    const iso = (await end.getAttribute('datetime'))!
    await expect(end).toHaveText(shown(iso, 480))
    // No reload: a marker set on the page survives each change.
    await page.evaluate(() => ((window as unknown as { marker: number }).marker = 1))
    await select.selectOption('UTC')
    await expect(end).toHaveText(shown(iso, 0))
    await select.selectOption('Australia/Darwin')
    await expect(end).toHaveText(shown(iso, 570))
    await go(page, 'Overview')
    await expect(page.locator('.read-stamp time').first()).toHaveText(/ UTC\+09:30$/)
    await go(page, 'History')
    await expect(page.getByRole('main').locator('tbody tr time').first()).toHaveText(/ UTC\+09:30$/)
    expect(await page.evaluate(() => (window as unknown as { marker?: number }).marker)).toBe(1)
    await expectLayoutOnlyStorage(page, [password])
  })

  test('the choice survives a reload; Automatic removes it', async () => {
    await page.reload()
    await expect(page.getByText('Signed in as admin')).toBeAttached()
    expect(await page.evaluate(() => localStorage.getItem('gateway.timezone'))).toBe('Australia/Darwin')
    await go(page, 'My Account')
    await expect(zoneSelect(page)).toHaveValue('Australia/Darwin')
    const end = sessionEnd(page)
    await expect(end).toHaveText(shown((await end.getAttribute('datetime'))!, 570))
    await zoneSelect(page).selectOption('auto')
    await expect(end).toHaveText(shown((await end.getAttribute('datetime'))!, 480))
    expect(await page.evaluate(() => localStorage.getItem('gateway.timezone'))).toBeNull()
  })

  test('a bad stored value falls back to Automatic without an error', async () => {
    await page.evaluate(() => localStorage.setItem('gateway.timezone', 'Not/A_Zone'))
    await page.reload()
    await expect(page.getByText('Signed in as admin')).toBeAttached()
    await go(page, 'My Account')
    await expect(zoneSelect(page)).toHaveValue('auto')
    const end = sessionEnd(page)
    await expect(end).toHaveText(shown((await end.getAttribute('datetime'))!, 480))
    await page.evaluate(() => localStorage.removeItem('gateway.timezone'))
    expect(pageErrors).toEqual([])
  })

  test('with browser storage blocked the page works and the choice lasts for the page', async ({ browser }) => {
    const blocked = await newPage(browser, () => {
      Object.defineProperty(window, 'localStorage', {
        get() {
          throw new DOMException('Storage is blocked', 'SecurityError')
        },
      })
    })
    try {
      await blocked.page.goto(adminURL + '/')
      await signIn(blocked.page, 'admin', password)
      await go(blocked.page, 'My Account')
      await expect(zoneSelect(blocked.page)).toHaveValue('auto')
      await zoneSelect(blocked.page).selectOption('UTC')
      const end = sessionEnd(blocked.page)
      await expect(end).toHaveText(shown((await end.getAttribute('datetime'))!, 0))
      expect(pageErrors).toEqual([])
    } finally {
      await blocked.context.close()
    }
  })

  test('Security Log times are entered in the chosen zone', async () => {
    await go(page, 'My Account')
    await zoneSelect(page).selectOption('UTC')
    await go(page, 'Security Log')
    await expect(page.getByText('Times are in UTC+00:00, the zone set on My Account.')).toBeVisible()
    await page.getByLabel('From', { exact: true }).fill('2026-11-03T10:00')
    const search = page.waitForRequest((r) => r.url().includes('/admin/security-log?'))
    await page.getByRole('button', { name: 'Search', exact: true }).click()
    expect(new URL((await search).url()).searchParams.get('from')).toBe('2026-11-03T10:00:00.000Z')
    await go(page, 'My Account')
    await zoneSelect(page).selectOption('Australia/Darwin')
    await go(page, 'Security Log')
    await page.getByLabel('From', { exact: true }).fill('2026-11-03T10:00')
    const again = page.waitForRequest((r) => r.url().includes('/admin/security-log?'))
    await page.getByRole('button', { name: 'Search', exact: true }).click()
    expect(new URL((await again).url()).searchParams.get('from')).toBe('2026-11-03T00:30:00.000Z')
    await go(page, 'My Account')
    await zoneSelect(page).selectOption('auto')
  })

  test('My Account works by keyboard and passes the automated accessibility rules', async () => {
    await go(page, 'My Account')
    const select = zoneSelect(page)
    for (let i = 0; i < 60 && !(await select.evaluate((e) => e === document.activeElement)); i++) await page.keyboard.press('Tab')
    await expect(select).toBeFocused()
    await page.keyboard.press('ArrowDown')
    await expect(select).toHaveValue('UTC')
    await expect(sessionEnd(page)).toHaveText(/ UTC\+00:00$/)
    await page.keyboard.press('ArrowUp')
    await expect(select).toHaveValue('auto')
    await page.evaluate(() =>
      Promise.all(
        document
          .getAnimations()
          .filter((animation) => animation.effect?.getComputedTiming().endTime !== Infinity)
          .map((animation) => animation.finished.catch(() => undefined)),
      ),
    )
    const report = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22a', 'wcag22aa']).analyze()
    expect(report.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`)).toEqual([])
  })
})
