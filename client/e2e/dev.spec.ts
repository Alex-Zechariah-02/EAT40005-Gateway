import { resolve } from 'node:path'
import { createServer, type ViteDevServer } from 'vite'
import { expect, test, type BrowserContext } from '@playwright/test'
import { AdminApi, build, Demo, webRoot } from './env'
import { record, shot, useSlot } from './pages'

// Development observation only. Production acceptance runs on the native
// Gateway with its unchanged content policy in the other suites.
test.describe.serial('Client development server observation', () => {
  let demo: Demo
  let vite: ViteDevServer
  let context: BrowserContext
  test.afterAll(async () => {
    await context?.close()
    await vite?.close()
    await demo?.stop()
  })

  test('localhost shop reads real stock through the development proxy', async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    const secret = demo.setup()
    await demo.start()
    const api = new AdminApi()
    const password = 'controlled client development password 42'
    await api.enrol('admin', secret, password)
    await api.signIn('admin', password)
    await api.genesis(await api.stage(demo.file('deny-all.json')))
    await api.activate(await api.stage(demo.file('baseline.json')), 300)
    await api.resolve('confirm')
    vite = await createServer({ configFile: resolve('vite.config.ts') })
    await vite.listen()
    expect(vite.config.server.host).toBe('localhost')
    expect(vite.config.server.port).toBe(5174)
    expect(vite.config.server.strictPort).toBe(true)
    context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    const page = await context.newPage()
    await page.goto('http://localhost:5174/demo/')
    await useSlot(page, 'Staff', demo.token('staff-01'))
    const before = (await demo.receipts('inventory')).length
    await page.evaluate(() => {
      location.hash = '#/product/ITM-001'
    })
    await expect(page.getByText('In stock: 40', { exact: true })).toBeVisible()
    const receipts = (await demo.receipts('inventory')).slice(before)
    expect(receipts).toHaveLength(1)
    expect(receipts[0]).toMatchObject({ operation: 'inventory.get', status: 200, subject: 'staff-01' })
    expect(page.url()).toBe('http://localhost:5174/demo/#/product/ITM-001')
    await shot(page, 'client-dev-stock-1440')
    record('client-development-only', { proxyRead: receipts[0], port: 5174, productionAcceptance: false })
  })
})
