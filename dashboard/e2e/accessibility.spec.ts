import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { adminURL } from './env'
import { createOverride, dialogButton, go, nav, record, revisions, shot, trial, trialStep, versionsPart } from './pages'
import { admin, adminCall, finish, newContext, open, password, propose, ready, status } from './setup'

// Automated accessibility rules, keyboard use and narrow screens.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('the main screens pass the automated accessibility rules, work by keyboard and fit narrow screens', async ({ browser }) => {
    const results: Record<string, string[]> = {}
    const scan = async (page: Page, name: string) => {
      // Contrast is judged on the settled screen: running fades and colour transitions would be measured half-way.
      await page.evaluate(() =>
        Promise.all(
          document
            .getAnimations()
            .filter((animation) => animation.effect?.getComputedTiming().endTime !== Infinity)
            .map((animation) => animation.finished.catch(() => undefined)),
        ),
      )
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
    const views = ['Overview', 'Live Traffic', 'APIs', 'Policy', 'Versions', 'Changes', 'Temporary Rules', 'Accounts', 'History', 'Team', 'My Account']
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
})
