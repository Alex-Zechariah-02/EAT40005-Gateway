import { expect, test, type Locator, type Page } from '@playwright/test'
import { adminURL, raw, siteURL } from './env'
import { advanced, go } from './pages'
import { admin, adminCall, finish, open, ready, token } from './setup'

// Motion: short transform, opacity and colour changes on every control, which
// follow only the operating system's reduce-motion setting. Computed styles
// are read, never screenshots.

interface Motion {
  property: string[]
  duration: number[]
  animation: string
}

// motion reads what an element (or its ::before) transitions and animates.
function motion(target: Locator, pseudo?: '::before'): Promise<Motion> {
  return target.evaluate((element, part) => {
    const style = getComputedStyle(element, part)
    const seconds = (text: string) => text.split(',').map((item) => parseFloat(item) * (item.trim().endsWith('ms') ? 0.001 : 1))
    return { property: style.transitionProperty.split(', '), duration: seconds(style.transitionDuration), animation: style.animationName }
  }, pseudo ?? null)
}

// moving lists the properties that still move: transforms, or any property
// when "all" transitions.
function moving(found: Motion): string[] {
  return found.property.filter((name, index) => (name === 'transform' || name === 'all' || name === 'translate' || name === 'scale') && found.duration[index] > 0)
}

// keyframes lists, for each animation an element has now, the properties its
// @keyframes rule changes. It reads the style sheets, so an animation that
// has already finished still counts.
function keyframes(target: Locator): Promise<string[][]> {
  return target.evaluate((element) => {
    const names = getComputedStyle(element).animationName.split(', ').filter((name) => name !== 'none')
    const rules: CSSKeyframesRule[] = []
    const walk = (list: CSSRuleList) => {
      for (const rule of list) {
        if (rule instanceof CSSKeyframesRule) rules.push(rule)
        else if (rule instanceof CSSGroupingRule) walk(rule.cssRules)
      }
    }
    for (const sheet of document.styleSheets) walk(sheet.cssRules)
    // The last rule of a name is the one the browser uses.
    return names.map((name) => {
      const rule = rules.filter((item) => item.name === name).at(-1)
      return rule ? [...new Set([...rule.cssRules].flatMap((frame) => [...(frame as CSSKeyframeRule).style]))] : []
    })
  })
}

// controls are the usual interactive elements of a page, found on Live Traffic.
function controls(page: Page) {
  const main = page.getByRole('main')
  return {
    segmented: main.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'All', exact: true }),
    button: page.locator('.sidebar-toggle'),
    link: page.locator('.sidebar-nav a').first(),
    search: page.getByRole('searchbox', { name: 'Search' }),
  }
}

let ref = ''

test.describe.serial('motion', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
    const reply = await raw('GET', siteURL + '/reports/summary', { Authorization: `Bearer ${token('ops-01')}` })
    ref = String(reply.headers['x-request-id'])
  })

  test.afterAll(finish)

  test('controls ease colour, opacity and transform in under 300 ms, never every property, and the focus ring is never delayed', async () => {
    await go(admin, 'Live Traffic')
    const found = controls(admin)
    for (const [name, control] of Object.entries(found)) {
      const style = await motion(control)
      expect(style.property, name).not.toContain('all')
      expect(style.property, name).not.toContain('outline-color')
      expect(Math.max(...style.duration), name).toBeLessThan(0.3)
      expect(Math.max(...style.duration), name).toBeGreaterThan(0)
    }
    expect((await motion(found.segmented)).property).toContain('transform')
    // The sidebar changes width at once: animating its grid would lay the
    // whole page out again on every frame.
    expect((await motion(admin.locator('.shell'))).property).not.toContain('grid-template-columns')
  })

  test('reduced motion stops all movement: segmented buttons, buttons, inputs, checkboxes, disclosures, dialogs and drawers', async () => {
    await admin.emulateMedia({ reducedMotion: 'reduce' })
    try {
      await go(admin, 'Live Traffic')
      const found = controls(admin)
      expect((await motion(found.segmented)).duration.every((value) => value === 0)).toBe(true)
      for (const [name, control] of Object.entries(found)) expect(moving(await motion(control)), name).toEqual([])
      await go(admin, 'Temporary Rules')
      await admin.getByRole('button', { name: 'Add a Temporary Rule' }).click()
      const wizard = admin.getByRole('dialog')
      await expect(wizard).toBeVisible()
      expect((await motion(wizard)).animation).toBe('none')
      const choice = wizard.locator('input[type="radio"]').first()
      expect(moving(await motion(choice))).toEqual([])
      expect(moving(await motion(choice, '::before'))).toEqual([])
      await wizard.getByRole('button', { name: 'Cancel', exact: true }).click()
      await go(admin, 'Changes')
      await advanced(admin)
      const editor = admin.getByRole('main').locator('details.advanced')
      expect(moving(await motion(editor.locator('summary'), '::before'))).toEqual([])
      expect((await keyframes(editor.locator('.advanced-body'))).flat()).not.toContain('transform')
      await admin.goto(`${adminURL}/#/live-traffic?ref=${ref}`)
      const drawer = admin.locator('dialog.drawer[open]')
      await expect(drawer).toBeVisible()
      expect((await motion(drawer)).animation).toBe('none')
      await admin.keyboard.press('Escape')
      await expect(drawer).toHaveCount(0)
    } finally {
      await admin.emulateMedia({ reducedMotion: null })
    }
  })

  test('a confirmation dialog fades in while its buttons keep their colours', async () => {
    // The confirmation the accessibility scan reads, opened with the pointer.
    expect((await adminCall('PUT', '/admin/blocked-accounts/cust-002', { reason: 'Motion check' }, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
    await go(admin, 'Accounts')
    const sampled = admin.evaluate(async () => {
      const seen = { dialog: new Set<string>(), buttons: new Set<string>() }
      const start = performance.now()
      while (performance.now() - start < 600) {
        const dialog = document.querySelector('dialog[open]')
        for (const animation of dialog?.getAnimations() ?? []) seen.dialog.add((animation as CSSAnimation).animationName)
        for (const button of dialog?.querySelectorAll('.btn') ?? []) for (const animation of button.getAnimations()) seen.buttons.add(`${button.textContent}: ${(animation as CSSTransition).transitionProperty}`)
        await new Promise((done) => requestAnimationFrame(done))
      }
      return { dialog: [...seen.dialog], buttons: [...seen.buttons] }
    })
    await admin.getByRole('button', { name: 'Unblock cust-002' }).click()
    const running = await sampled
    expect(running.dialog).toEqual(['dialog-in'])
    expect(running.buttons).toEqual([])
    await admin.getByRole('dialog').getByRole('button', { name: 'Cancel', exact: true }).click()
    expect((await adminCall('DELETE', '/admin/blocked-accounts/cust-002', undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
  })

  test('disclosures and menus enter with a short fade, and the request drawer keeps its own entrance after the Policy page loads', async () => {
    await go(admin, 'Changes')
    const editor = admin.getByRole('main').locator('details.advanced')
    if ((await editor.getAttribute('open')) !== null) await editor.locator('summary').click()
    await editor.locator('summary').click()
    expect((await keyframes(editor.locator('.advanced-body'))).flat()).toEqual(expect.arrayContaining(['opacity', 'transform']))
    // The Policy page loads the Designer's styles; the drawer must still fade in from the side.
    await go(admin, 'Policy')
    await expect(admin.locator('.designer-card').first()).toBeVisible()
    await admin.getByRole('button', { name: 'Add to Roles' }).click()
    const menu = admin.getByRole('menu')
    await expect(menu).toBeVisible()
    expect((await keyframes(menu)).flat()).toEqual(expect.arrayContaining(['opacity', 'transform']))
    await admin.keyboard.press('Escape')
    await admin.goto(`${adminURL}/#/live-traffic?ref=${ref}`)
    const drawer = admin.locator('dialog.drawer[open]')
    await expect(drawer).toBeVisible()
    expect((await keyframes(drawer)).flat()).toEqual(expect.arrayContaining(['opacity', 'transform']))
    await admin.keyboard.press('Escape')
    await expect(drawer).toHaveCount(0)
  })

  test('widening the sidebar fades its labels in, and the icon strip moves nothing under reduced motion', async () => {
    await go(admin, 'Overview')
    const label = admin.locator('.sidebar .nav-label').first()
    await admin.getByRole('button', { name: 'Collapse Sidebar' }).click()
    await admin.getByRole('button', { name: 'Expand Sidebar' }).click()
    expect((await keyframes(label)).flat()).toContain('opacity')
    await admin.emulateMedia({ reducedMotion: 'reduce' })
    try {
      await admin.getByRole('button', { name: 'Collapse Sidebar' }).click()
      await admin.getByRole('button', { name: 'Expand Sidebar' }).click()
      expect((await keyframes(label)).flat()).not.toContain('transform')
    } finally {
      await admin.emulateMedia({ reducedMotion: null })
    }
  })
})
