import AxeBuilder from '@axe-core/playwright'
import { expect, test } from '@playwright/test'
import { adminURL } from './env'
import { activate, dialogButton, expectLayoutOnlyStorage, go, record, resolve, shot, signIn, stage } from './pages'
import { api, baseline, demo, browserMessages, capture, context, currentPolicy, designer, discard, enrolment, expandAll, expectPolicyKept, node, page, password, policyViolations, requests, selected, open, ready, finish } from './designer'

// The Policy Designer canvas: first-policy guidance, groups, accessibility and the content policy.
// Each file starts its own gateway; designer.ts holds the shared steps.

test.describe.serial('Policy Designer before the first policy', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
  })

  test.afterAll(finish)

  test('first-policy guidance and exact baseline boxes, references and fixed checks', async () => {
    await go(page, 'Policy')
    await expect(page.getByText('Establish the first policy before designing changes.')).toBeVisible()
    await expect(page.locator('.designer-canvas')).toHaveCount(0)
    await capture('designer-no-confirmed-policy')
    await stage(page, { file: demo.path('deny-all.json') })
    await page.getByRole('button', { name: 'Establish as the First Policy' }).click()
    await page.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
    await dialogButton(page, 'Establish').click()
    await expect(page.getByText('Established: version 1 is the active policy.')).toBeVisible()
    await stage(page, { file: demo.path('baseline.json') })
    await activate(page, 300)
    await resolve(page, 'confirm')
    await designer()
    const confirmed = await currentPolicy()
    expect(confirmed.document).toEqual(baseline)
    // The canvas opens with every group in view and the endpoint lists
    // closed, joined by a few connections that each stand for many.
    const canvas = (await page.locator('.designer-canvas').boundingBox())!
    const inView = async (selector: string) => {
      const box = (await page.locator(selector).boundingBox())!
      return box.x >= canvas.x && box.x + box.width <= canvas.x + canvas.width && box.y >= canvas.y && box.y + box.height <= canvas.y + canvas.height
    }
    const zoom = await page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a)
    expect(zoom).toBeGreaterThanOrEqual(0.85)
    for (const id of await page.locator('.react-flow__node').evaluateAll((nodes) => nodes.map((n) => n.getAttribute('data-id')!)))
      expect(await inView(`.react-flow__node[data-id="${id}"]`), `${id} in the opening view`).toBe(true)
    for (const api of baseline.apis)
      await expect(page.getByRole('button', { name: new RegExp(`^${api.id[0].toUpperCase()}${api.id.slice(1)} API Endpoints,`) })).toHaveAttribute('aria-expanded', 'false')
    const reached = baseline.apis.filter((api) =>
      baseline.operations.some((operation) => operation.api === api.id && baseline.permits.some((permit) => permit.operation === operation.id)),
    )
    await expect(page.locator('.react-flow__edge')).toHaveCount(reached.length + baseline.apis.length + 1)
    expect(reached.length + baseline.apis.length + 1).toBeLessThan(baseline.permits.length)
    await capture('designer-baseline-canvas')
    await expandAll()
    for (const [kind, collection] of [
      ['role', baseline.roles],
      ['operation', baseline.operations],
      ['api', baseline.apis],
      ['upstream', baseline.upstreams],
    ] as const) {
      await expect(page.locator(`.designer-row[data-box^="${kind}:"]`)).toHaveCount(collection.length)
      for (const item of collection) await expect(node(`${kind}:${item.id}`)).toBeVisible()
    }
    // A policy names roles, never accounts: role rows list nobody.
    await expect(node('role:CUSTOMER')).not.toContainText('cust-001')
    // Each endpoint's editor lists the gateway's checks in their fixed order.
    for (const operation of baseline.operations) {
      await node(`operation:${operation.id}`).click()
      // Each check reads as its name and value; the step number is drawn, not read.
      const checkLabels = (await selected().locator('.designer-check').allTextContents()).map((label) => label.replace(/^\d+/, ''))
      expect(checkLabels.map((label) => label.split(' ')[0])).toEqual(['Route', 'Access', 'Sign-In', 'Role', 'Rate', 'Body', 'Record', 'Forward'])
      expect(checkLabels[0]).toBe(`Route ${operation.method} ${operation.target}`)
      expect(checkLabels[1]).toBe(`Access ${operation.access}`)
      expect(checkLabels[2]).toBe(`Sign-In ${operation.auth === 'none' ? 'public' : 'required'}`)
      // Selecting an endpoint brings it into view at a readable size; closing
      // the form returns the canvas to the view it had before.
      await expect
        .poll(() => page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a))
        .toBeGreaterThanOrEqual(0.85)
      await selected().getByRole('button', { name: 'Close', exact: true }).click()
      await expect(selected()).toHaveCount(0)
    }
    // Selecting a role marks exactly the endpoints it may call.
    await node('role:CUSTOMER').click()
    const granted = baseline.permits.filter((permit) => permit.role === 'CUSTOMER')
    await expect(page.locator('.designer-row.is-allowed')).toHaveCount(granted.length)
    for (const permit of granted) await expect(node(`operation:${permit.operation}`)).toHaveClass(/is-allowed/)
    await page.getByRole('button', { name: 'Fit', exact: true }).click()
    await capture('designer-baseline-fit')
    await selected().getByRole('button', { name: 'Close', exact: true }).click()
    await page.getByRole('button', { name: 'Collapse All', exact: true }).click()
    await expect(node('role:CUSTOMER')).toBeHidden()
    await page.getByRole('button', { name: /^Roles,/ }).click()
    await expect(node('role:CUSTOMER')).toBeVisible()
    record('designer-baseline', {
      revision: confirmed.id,
      boxes: baseline.roles.length + baseline.operations.length + baseline.apis.length + baseline.upstreams.length,
      connections: reached.length + baseline.apis.length + 1,
      permits: baseline.permits.length,
    })
  })
})

test.describe.serial('Policy Designer on the native Gateway', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('a public endpoint takes no role permission and the document stays unchanged', async () => {
    const before = await currentPolicy()
    await node('role:CUSTOMER').click()
    await expect(selected().getByRole('checkbox', { name: /\(inventory\.categories\)$/ })).toHaveCount(0)
    await expandAll()
    await node('operation:inventory.categories').click()
    await expect(selected()).toContainText('Public endpoints do not use role permissions.')
    await expect(selected().getByRole('checkbox', { name: 'CUSTOMER', exact: true })).toHaveCount(0)
    await capture('designer-public-endpoint')
    await expect(page.getByRole('button', { name: 'Save Changes', exact: true })).toBeDisabled()
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    expect(JSON.parse(await page.getByLabel('Complete Policy Document (JSON)').inputValue())).toEqual(before.document)
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    record('designer-public-endpoint', { documentUnchanged: true })
  })

  test('groups open and close, full screen fills the window, and the canvas stores nothing in the browser', async () => {
    const inventory = page.getByRole('button', { name: /^Inventory API Endpoints,/ })
    await page.getByRole('button', { name: 'Collapse All', exact: true }).click()
    await expect(inventory).toHaveAttribute('aria-expanded', 'false')
    await expect(node('operation:inventory.stock.read')).toBeHidden()
    await inventory.click()
    await expect(inventory).toHaveAttribute('aria-expanded', 'true')
    await expect(node('operation:inventory.stock.read')).toBeVisible()
    // Opening a group brings it into view at a readable size, its heading
    // inside the canvas, even with another group already open.
    const orders = page.getByRole('button', { name: /^Orders API Endpoints,/ })
    await orders.click()
    await expect(orders).toHaveAttribute('aria-expanded', 'true')
    const focused = async () => {
      const canvas = (await page.locator('.designer-canvas').boundingBox())!
      const heading = (await orders.boundingBox())!
      const zoom = await page.locator('.react-flow__viewport').evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a)
      return zoom >= 0.85 && heading.y >= canvas.y && heading.y + heading.height <= canvas.y + canvas.height && heading.x >= canvas.x
    }
    await expect.poll(focused).toBe(true)
    await capture('designer-group-open')
    await orders.click()
    await expect(orders).toHaveAttribute('aria-expanded', 'false')
    // Fit shows every card again, so the Inventory group is in reach.
    await page.getByRole('button', { name: 'Fit', exact: true }).click()
    await inventory.click()
    await expect(node('operation:inventory.stock.read')).toBeHidden()
    await page.getByRole('button', { name: 'Full Screen', exact: true }).click()
    await expect(page.locator('.designer-workspace')).toHaveClass(/is-expanded/)
    const size = page.viewportSize()!
    const area = (await page.locator('.designer-workspace').boundingBox())!
    expect(area.width).toBeGreaterThanOrEqual(size.width - 1)
    await shot(page, 'designer-full-screen')
    await page.locator('.designer-canvas').press('Escape')
    await expect(page.locator('.designer-workspace')).not.toHaveClass(/is-expanded/)
    const cookie = (await context.cookies(adminURL)).find((entry) => entry.name === '__Host-gateway-admin')!
    const session = await api('GET', '/admin/session')
    await expectLayoutOnlyStorage(page, [password, enrolment, cookie.value, String(session.body.csrf_token)])
    expect(await page.evaluate(() => localStorage.getItem('eat40005.policy-designer.layout.v1'))).toBeNull()
    record('designer-groups', { collapsedByDefault: true, fullScreen: true, layoutStored: false })
  })

  test('every view passes axe, fits all widths and a keyboard permit change is staged', async () => {
    await page.reload()
    if (await page.getByRole('heading', { name: 'Sign in', exact: true }).isVisible()) await signIn(page, 'admin', password)
    await designer()
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      const fit = page.getByRole('button', { name: 'Fit', exact: true })
      await page.keyboard.press('Tab')
      await fit.focus()
      await shot(page, `designer-control-focus-${width}`)
      await fit.hover()
      await shot(page, `designer-control-hover-${width}`)
      await page.mouse.down()
      await shot(page, `designer-control-active-${width}`)
      await page.mouse.up()
    }
    await page.setViewportSize({ width: 1440, height: 900 })
    // Scan the canvas with its side form showing a selected box.
    await node('role:CUSTOMER').click()
    await expect(page.getByRole('heading', { name: 'Role CUSTOMER', level: 2 })).toBeVisible()
    for (const view of ['Canvas', 'Form', 'JSON']) {
      await page.getByRole('tab', { name: view, exact: true }).click()
      for (const width of [1440, 768, 375]) {
        await page.setViewportSize({ width, height: 900 })
        const scan = await new AxeBuilder({ page }).analyze()
        expect(
          scan.violations.map((violation) => ({ id: violation.id, nodes: violation.nodes.map((entry) => entry.target) })),
          `${view} ${width}`,
        ).toEqual([])
        await shot(page, `designer-axe-${view.toLowerCase()}-${width}`)
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      }
      await page.setViewportSize({ width: 1440, height: 900 })
    }
    await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
    await page.getByRole('button', { name: 'Trace', exact: true }).click()
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      const scan = await new AxeBuilder({ page }).analyze()
      expect(
        scan.violations.map((violation) => ({ id: violation.id, nodes: violation.nodes.map((entry) => entry.target) })),
        `Trace ${width}`,
      ).toEqual([])
      await shot(page, `designer-axe-trace-${width}`)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    }
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.getByRole('button', { name: 'Trace', exact: true }).click()
    await page.getByRole('tab', { name: 'Form', exact: true }).focus()
    await page.keyboard.press('Enter')
    await page.getByRole('button', { name: 'Permissions Matrix', exact: true }).focus()
    await page.keyboard.press('Enter')
    const permit = page.getByRole('checkbox', { name: 'CUSTOMER may call inventory.stock.read', exact: true })
    await permit.focus()
    await page.keyboard.press('Space')
    await expect(permit).toBeChecked()
    await page.getByRole('button', { name: 'Save Changes', exact: true }).focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('dialog')).toBeVisible()
    await page.keyboard.press('Tab')
    await page.keyboard.press('Enter')
    await expect(page.getByText(/^The gateway checked it: saved as draft change \d+\. Nothing is active yet\.$/)).toBeVisible()
    await capture('designer-keyboard-staged')
    await discard()
    await page.evaluate(() => {
      document.documentElement.style.fontSize = '200%'
    })
    for (const width of [1440, 768, 375]) {
      await page.setViewportSize({ width, height: 900 })
      expect(await page.evaluate(() => getComputedStyle(document.documentElement).fontSize)).toBe('32px')
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await shot(page, `designer-text-200-${width}`)
    }
    await page.evaluate(() => {
      document.documentElement.style.fontSize = ''
    })
    await page.setViewportSize({ width: 1440, height: 900 })
    record('designer-accessibility', { axeScans: 12, violations: 0, widths: [1440, 768, 375], keyboardStage: true, textResize: '200%' })
  })

  test('all observed Designer interactions retain the content policy and secrets remain outside storage', async () => {
    await expectPolicyKept()
    record('designer-content-policy', {
      policyEvents: policyViolations.length,
      requests: requests.length,
      browserMessages: [...new Set(browserMessages.map((message) => message.replace(/\d+/g, 'N')))],
    })
  })
})
