import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Locator } from '@playwright/test'
import { applyJson, baseline, discard, page, open, ready, finish } from './designer'
import { dialogButton, go, shot } from './pages'

// Connect an API: where it starts, keyboard use and the accessibility scan.

const flow = () => page.getByRole('dialog', { name: 'Connect an API' })
const heading = () => flow().locator('h3[tabindex="-1"]')
const toolbar = () => page.locator('.designer-bar').getByRole('button', { name: 'Connect an API', exact: true })

// tabTo presses Tab until the target has keyboard focus.
async function tabTo(target: Locator) {
  for (let i = 0; i < 40; i++) {
    await page.keyboard.press('Tab')
    if (await target.evaluate((element) => element === document.activeElement)) return
  }
  throw new Error('keyboard focus never reached the target')
}

async function settled() {
  await page.evaluate(() =>
    Promise.all(
      document
        .getAnimations()
        .filter((animation) => animation.effect?.getComputedTiming().endTime !== Infinity)
        .map((animation) => animation.finished.catch(() => undefined)),
    ),
  )
}

test.describe.serial('Connect an API: entry points, keyboard and accessibility', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('the Policy toolbar and the empty canvas open the flow, and closing it untouched asks nothing', async () => {
    await toolbar().click()
    await expect(flow()).toBeVisible()
    await expect(heading()).toHaveText('Step 1 of 7: Service Address')
    await expect(heading()).toBeFocused()
    await expect(flow().getByRole('list').first().getByRole('listitem')).toHaveText([
      '1 Service Address',
      '2 API',
      '3 Endpoints',
      '4 Roles',
      '5 Rate',
      '6 Validate Request Bodies',
      '7 Review',
    ])
    await flow().getByRole('button', { name: 'Close', exact: true }).click()
    await expect(flow()).toHaveCount(0)
    await expect(dialogButton(page, 'Discard Input')).toHaveCount(0)
    await expect(toolbar()).toBeFocused()

    // A draft with no API yet shows the way to connect one on the canvas itself.
    await applyJson({ ...baseline, apis: [], operations: [], permits: [] })
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    const canvas = page.locator('.designer-canvas-area')
    await expect(canvas.getByText('This draft has no APIs yet.')).toBeVisible()
    await shot(page, 'connect-empty-canvas')
    await canvas.getByRole('button', { name: 'Connect an API', exact: true }).click()
    await expect(heading()).toHaveText('Step 1 of 7: Service Address')
    await page.keyboard.press('Escape')
    await expect(flow()).toHaveCount(0)
    await discard()
  })

  test('the APIs page and the Getting Started guide open the Policy page with the flow open', async () => {
    await go(page, 'APIs')
    await page.getByRole('button', { name: 'Connect an API', exact: true }).click()
    await expect(page).toHaveURL(/#\/policy$/)
    await expect(heading()).toHaveText('Step 1 of 7: Service Address')
    await flow().getByRole('button', { name: 'Close', exact: true }).click()
    await expect(flow()).toHaveCount(0)

    // Every step is done here, so the guide has hidden itself; My Account shows it again.
    await go(page, 'Overview')
    const guide = page.locator('details.getting-started')
    await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible()
    await go(page, 'My Account')
    await page.getByRole('main').getByRole('button', { name: 'Show the Getting Started Guide' }).click()
    await guide.locator('summary').click()
    await guide.getByRole('button', { name: 'Connect an API', exact: true }).click()
    await expect(page).toHaveURL(/#\/policy$/)
    await expect(heading()).toHaveText('Step 1 of 7: Service Address')
    await flow().getByRole('button', { name: 'Close', exact: true }).click()
    // The request is used once: the Policy page opened again shows no flow.
    await go(page, 'Overview')
    await go(page, 'Policy')
    await expect(page.getByRole('tab', { name: 'Canvas', exact: true })).toBeVisible()
    await expect(flow()).toHaveCount(0)
  })

  test('keyboard only: every step is completed, Tab stays inside, and Escape asks before discarding and returns focus', async () => {
    await page.getByRole('heading', { name: 'Policy', level: 1 }).focus()
    await tabTo(toolbar())
    await page.keyboard.press('Enter')
    await expect(heading()).toBeFocused()
    const next = flow().getByRole('button', { name: 'Next', exact: true })
    const close = flow().getByRole('button', { name: 'Close', exact: true })
    // Tab and Shift+Tab wrap between the first and last controls.
    await tabTo(next)
    await page.keyboard.press('Tab')
    await expect(close).toBeFocused()
    await page.keyboard.press('Shift+Tab')
    await expect(next).toBeFocused()

    const fill = async (label: string, text: string) => {
      await tabTo(flow().getByLabel(label, { exact: true }))
      await page.keyboard.type(text)
    }
    await fill('Service Address ID', 'kb-svc')
    await fill('Address (Host and Port)', '127.0.0.1:9691')
    await tabTo(next)
    await page.keyboard.press('Enter')
    await expect(heading()).toHaveText('Step 2 of 7: API')
    await expect(heading()).toBeFocused()
    await fill('API ID', 'keyboard')
    await tabTo(next)
    await page.keyboard.press('Enter')
    await expect(heading()).toHaveText('Step 3 of 7: Endpoints')
    await fill('Path', '/keyboard/items')
    await tabTo(next)
    await page.keyboard.press('Enter')
    await expect(heading()).toHaveText('Step 4 of 7: Roles')
    await tabTo(flow().getByLabel('OPERATIONS', { exact: true }))
    await page.keyboard.press('Space')
    await expect(flow().getByLabel('OPERATIONS', { exact: true })).toBeChecked()
    await tabTo(next)
    await page.keyboard.press('Enter')
    await expect(heading()).toHaveText('Step 5 of 7: Rate')
    await tabTo(next)
    await page.keyboard.press('Enter')
    await expect(heading()).toHaveText('Step 6 of 7: Validate Request Bodies')
    await expect(flow().getByText('None of these endpoints takes a request body (POST, PUT or PATCH), so there is nothing to check.')).toBeVisible()
    await tabTo(next)
    await page.keyboard.press('Enter')
    await expect(heading()).toHaveText('Step 7 of 7: Review')
    await expect(flow().locator('.change-set li')).toHaveText([
      'Service address kb-svc added',
      'API keyboard added',
      'Endpoint keyboard.items added',
      'OPERATIONS can now call keyboard.items',
    ])

    // Escape asks first; Cancel (focused) keeps the input, Discard Input closes.
    await page.keyboard.press('Escape')
    const ask = page.getByRole('dialog', { name: 'Discard This Input?' })
    await expect(ask.getByRole('button', { name: 'Cancel', exact: true })).toBeFocused()
    await page.keyboard.press('Enter')
    await expect(ask).toHaveCount(0)
    await expect(heading()).toHaveText('Step 7 of 7: Review')
    await page.keyboard.press('Escape')
    await tabTo(ask.getByRole('button', { name: 'Discard Input', exact: true }))
    await page.keyboard.press('Enter')
    await expect(flow()).toHaveCount(0)
    await expect(toolbar()).toBeFocused()
    await expect(page.locator('.designer-draft-state')).toHaveText(/^No changes to version \d+$/)
  })

  test('the flow passes the accessibility scan at 1440, 768 and 375 px without sideways scrolling', async () => {
    const results: Record<string, string[]> = {}
    const scan = async (name: string) => {
      for (const width of [1440, 768, 375]) {
        await page.setViewportSize({ width, height: 900 })
        await settled()
        const report = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22a', 'wcag22aa']).analyze()
        results[`${name}-${width}`] = report.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`)
        expect(results[`${name}-${width}`], `${name} at ${width}`).toEqual([])
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${name} page at ${width}`).toBe(true)
        expect(await flow().evaluate((dialog) => dialog.scrollWidth <= dialog.clientWidth), `${name} drawer at ${width}`).toBe(true)
        await shot(page, `connect-${name}-${width}`)
      }
      await page.setViewportSize({ width: 1440, height: 900 })
    }
    const next = flow().getByRole('button', { name: 'Next', exact: true })
    await toolbar().click()
    await next.click()
    await scan('errors')
    await flow().getByLabel('Service Address', { exact: true }).selectOption('reports-svc')
    await next.click()
    await flow().getByLabel('API ID', { exact: true }).fill('scanned')
    await next.click()
    await flow().getByRole('group', { name: 'Endpoint 1', exact: true }).getByLabel('Path', { exact: true }).fill('/scanned/items')
    await flow().getByRole('button', { name: 'Add Another Endpoint', exact: true }).click()
    await flow().getByRole('group', { name: 'Endpoint 2', exact: true }).getByLabel('Method', { exact: true }).selectOption('PUT')
    await flow().getByRole('group', { name: 'Endpoint 2', exact: true }).getByLabel('Path', { exact: true }).fill('/scanned/items/*')
    await flow().getByRole('group', { name: 'Endpoint 2', exact: true }).getByLabel('Endpoint ID', { exact: true }).fill('scanned.update')
    await scan('endpoints')
    await next.click()
    await next.click()
    await next.click()
    await flow().getByLabel("Check This Endpoint's Request Body", { exact: true }).check()
    await scan('bodies')
    await next.click()
    await scan('review')
    await flow().getByRole('button', { name: 'Cancel', exact: true }).click()
    await dialogButton(page, 'Discard Input').click()
    await expect(flow()).toHaveCount(0)
  })
})
