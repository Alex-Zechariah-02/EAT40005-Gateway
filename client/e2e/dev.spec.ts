import { resolve } from 'node:path'
import { createServer, type ViteDevServer } from 'vite'
import { expect, test, type BrowserContext } from '@playwright/test'
import { build, Demo, webRoot } from './env'
import { record, shot } from './pages'

// Development observation only. Production acceptance runs on the native
// gateway with its unchanged content policy in the other suites.
test.describe.serial('Fetch app development server observation', () => {
  let demo: Demo
  let vite: ViteDevServer
  let context: BrowserContext
  test.afterAll(async () => {
    await context?.close()
    await vite?.close()
    await demo?.stop()
  })

  test('the development server sends API paths to the gateway, never to a service, and shows its answer', async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    demo.setup()
    await demo.start()
    vite = await createServer({ configFile: resolve('vite.config.ts') })
    await vite.listen()
    const developmentURL = `http://localhost:${vite.config.server.port}`
    expect(vite.config.server.host).toBe('localhost')
    expect(vite.config.server.port).toBe(Number(process.env.E2E_DEV_PORT ?? 5174))
    expect(vite.config.server.strictPort).toBe(true)

    context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    const page = await context.newPage()
    const answers: { path: string; status: number; ref: string | undefined }[] = []
    page.on('response', (response) => {
      const path = new URL(response.url()).pathname
      if (path.startsWith('/inventory/')) answers.push({ path, status: response.status(), ref: response.headers()['x-request-id'] })
    })
    await page.goto(developmentURL + '/', { waitUntil: 'networkidle' })
    await expect(page.getByRole('heading', { level: 1, name: 'What can we fetch for you?' })).toBeVisible()
    // A first start may reload the page once its dependencies are prepared,
    // so the reads are awaited through the reference the page shows.
    const shown = page.getByTitle(/^Full reference [0-9a-f]{32}$/).first()
    await expect(shown).toBeVisible({ timeout: 30_000 })
    const ref = (await shown.getAttribute('title'))!.replace(/^Full reference /, '')
    // Each API read was answered by the gateway, which recorded it under its
    // reference; the page shows one of them.
    expect(answers.map((a) => a.ref)).toContain(ref)
    for (const answer of answers) {
      expect(answer.ref, answer.path).toMatch(/^[0-9a-f]{32}$/)
      expect(demo.events(answer.ref).length, answer.path).toBeGreaterThan(0)
    }
    for (const count of Object.values(await demo.counts())) expect(count).toBe(0)
    await shot(page, 'client-dev-1440')
    record('client-development-only', { answers, port: vite.config.server.port, productionAcceptance: false })
  })
})
