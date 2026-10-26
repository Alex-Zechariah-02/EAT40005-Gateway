import { resolve } from 'node:path'
import { createServer, type ViteDevServer } from 'vite'
import { expect, test, type BrowserContext } from '@playwright/test'
import { adminURL, build, Demo, webRoot } from './env'
import { enrol, record, shot, signIn, stage } from './pages'

// Development observations do not establish production content-policy or
// Admin API acceptance. This uses the actual Vite configuration and Gateway.
test.describe.serial('Dashboard development server observation', () => {
  let demo: Demo
  let vite: ViteDevServer
  let context: BrowserContext
  const password = 'controlled development observation 42'

  test.afterAll(async () => {
    await context?.close()
    await vite?.close()
    await demo?.stop()
  })

  test('localhost sign-in keeps the Secure cookie and staging reaches the native Gateway', async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    const secret = demo.setup()
    await demo.start()
    const bootstrapContext = await browser.newContext()
    const bootstrap = await bootstrapContext.newPage()
    await bootstrap.goto(adminURL + '/')
    await enrol(bootstrap, 'admin', secret, password)
    await bootstrapContext.close()

    vite = await createServer({ configFile: resolve('vite.config.ts') })
    await vite.listen()
    expect(vite.config.server.host).toBe('localhost')
    expect(vite.config.server.port).toBe(5173)
    expect(vite.config.server.strictPort).toBe(true)

    context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    const page = await context.newPage()
    await page.goto('http://localhost:5173/')
    await signIn(page, 'admin', password)
    const cookie = (await context.cookies('http://localhost:5173')).find((entry) => entry.name === '__Host-gateway-admin')
    expect(cookie).toMatchObject({ domain: 'localhost', secure: true, httpOnly: true, sameSite: 'Strict', path: '/' })
    expect(await context.cookies(adminURL)).toEqual([])
    const candidate = await stage(page, { file: demo.path('deny-all.json') })
    const session = await context.request.get('http://localhost:5173/admin/session')
    expect(session.status()).toBe(200)
    const stored = await context.request.get(`http://localhost:5173/admin/candidates/${candidate}`)
    expect(stored.status()).toBe(200)
    expect((await stored.json()).id).toBe(candidate)
    await shot(page, 'dashboard-dev-staged-1440')
    record('dashboard-development-only', {
      signIn: true,
      candidate,
      secureCookie: true,
      isolatedHost: true,
      productionAcceptance: false,
    })
  })
})
