import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, resolve as resolvePath } from 'node:path'
import { expect, test, type BrowserContext, type Page } from '@playwright/test'
import { adminURL, applicationDist, build, controls, Demo, gatewayIdentity, pageRoutes, raw, siteURL, uiPolicy, webRoot } from './env'
import { accessible, record, shot, storage } from './pages'

// The Fetch app as the gateway's data plane serves it, checked before any
// policy exists. The gateway admits no API request yet, so everything here
// holds whatever services and policy later run behind it: how the app is
// served, what the browser may load and keep, and how a refusal is shown.

const assetTypes: Record<string, string> = { js: 'text/javascript', css: 'text/css', svg: 'image/svg+xml', webp: 'image/webp' }

let demo: Demo
let context: BrowserContext
let site: Page
const requests: string[] = []
const consoleLines: string[] = []
const violations: string[] = []
let scriptDialogs = 0

// files lists every file of a directory tree.
function files(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    return statSync(path).isDirectory() ? files(path) : [path]
  })
}

// open loads a page route and waits until every read it started has an answer.
async function open(page: Page, route: string) {
  await page.goto(siteURL + route, { waitUntil: 'networkidle' })
  await expect(page.locator('main')).toBeVisible()
  await expect(page.locator('[aria-busy="true"]')).toHaveCount(0)
}

test.describe.serial('Fetch app on the gateway data plane', () => {
  test.beforeAll(async ({ browser }) => {
    demo = new Demo(build(), webRoot())
    demo.setup()
    await demo.start()
    context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    await context.exposeBinding('reportViolation', (_source, text: string) => {
      violations.push(text)
    })
    await context.addInitScript(() => {
      document.addEventListener('securitypolicyviolation', (event) => {
        void (window as unknown as { reportViolation: (text: string) => Promise<void> }).reportViolation(`${event.effectiveDirective} ${event.blockedURI}`)
      })
    })
    context.on('request', (r) => requests.push(r.url()))
    context.on('console', (m) => consoleLines.push(m.text()))
    context.on('page', (page) =>
      page.on('dialog', (dialog) => {
        scriptDialogs++
        void dialog.dismiss()
      }),
    )
    site = await context.newPage()
  })

  test.afterAll(async () => {
    await context?.close()
    await demo?.stop()
  })

  test('the gateway serves the page at / and at each page route, and the flat assets, under the content policy', async () => {
    record('cold-start', { gateway: gatewayIdentity(), pids: Object.keys(demo.pids), web: demo.web })
    expect(Object.keys(demo.pids)).toContain('gateway')
    const index = readFileSync(resolvePath(applicationDist, 'index.html'), 'utf8')

    // Every page route answers the same page, with the security headers and
    // the gateway's own reference.
    for (const route of pageRoutes) {
      const reply = await raw('GET', siteURL + route + '?from=link')
      expect(reply.status, route).toBe(200)
      expect(reply.body, route).toBe(index)
      expect(reply.headers['content-type'], route).toBe('text/html; charset=utf-8')
      expect(reply.headers['content-security-policy'], route).toBe(uiPolicy)
      expect(reply.headers['x-frame-options'], route).toBe('DENY')
      expect(reply.headers['referrer-policy'], route).toBe('no-referrer')
      expect(reply.headers['x-content-type-options'], route).toBe('nosniff')
      expect(reply.headers['cache-control'], route).toBe('no-store')
      expect(reply.headers['x-request-id'], route).toMatch(/^[0-9a-f]{32}$/)
      const head = await raw('HEAD', siteURL + route)
      expect(head.status, `HEAD ${route}`).toBe(200)
      expect(head.body, `HEAD ${route}`).toBe('')
    }

    // Every built file is served byte for byte from /assets/ with its fixed
    // type; the build holds no other kind of file and nothing in a folder.
    const assets = readdirSync(resolvePath(applicationDist, 'assets'))
    expect([...new Set(assets.map((a) => a.split('.').pop()!))].sort()).toEqual(['css', 'js', 'svg', 'webp'])
    for (const asset of assets) {
      const reply = await raw('GET', `${siteURL}/assets/${asset}`)
      expect(reply.status, asset).toBe(200)
      expect(reply.headers['content-type'], asset).toBe(assetTypes[asset.split('.').pop()!])
      expect(reply.headers['content-security-policy'], asset).toBe(uiPolicy)
      expect(reply.headers['x-content-type-options'], asset).toBe('nosniff')
      expect(reply.bytes.equals(readFileSync(resolvePath(applicationDist, 'assets', asset))), asset).toBe(true)
      expect((await raw('GET', `${adminURL}/assets/${asset}`)).status, `${asset} on the admin listener`).toBe(404)
    }

    // Every other path, spelling and method is not the website's: it goes to
    // enforcement, which admits nothing yet, and never answers with the page.
    const probes = [
      { method: 'GET', path: '/assets/no-such-file.js', status: 404 },
      { method: 'GET', path: '/index.html', status: 503 },
      { method: 'GET', path: '/food/', status: 503 },
      { method: 'GET', path: '/store', status: 503 },
      { method: 'GET', path: '/store/MER-001/menu', status: 503 },
      { method: 'GET', path: '/%66ood', status: 503 },
      { method: 'GET', path: '/assets/picture.png', status: 503 },
      { method: 'GET', path: '/assets/../index.html', status: 503 },
      { method: 'GET', path: '/assets/sub/app.js', status: 503 },
      { method: 'GET', path: '/demo/', status: 503 },
      { method: 'POST', path: '/', status: 503 },
      { method: 'POST', path: '/checkout', status: 503 },
      { method: 'GET', path: '/orders', status: 503 },
      { method: 'GET', path: '/admin/status', status: 503 },
    ]
    const seen = []
    for (const probe of probes) {
      const reply = await raw(probe.method, siteURL + probe.path)
      seen.push({ ...probe, observed: reply.status })
      expect(reply.status, `${probe.method} ${probe.path}`).toBe(probe.status)
      expect(String(reply.headers['content-type'] ?? ''), probe.path).not.toContain('text/html')
      expect(reply.body, probe.path).not.toContain('<!doctype html>')
      expect(reply.headers['x-request-id'], probe.path).toMatch(/^[0-9a-f]{32}$/)
    }
    // The admin listener serves the dashboard, never this app.
    expect((await raw('GET', adminURL + '/food')).status).toBe(404)
    expect((await raw('GET', adminURL + '/')).body).not.toBe(index)

    // Each listener answers only on its own loopback host, and nothing has
    // reached a service.
    await expect(raw('GET', siteURL.replace('127.0.0.2', '127.0.0.1') + '/')).rejects.toThrow(/ECONNREFUSED/)
    await expect(raw('GET', adminURL.replace('127.0.0.1', '127.0.0.2') + '/')).rejects.toThrow(/ECONNREFUSED/)
    for (const count of Object.values(await demo.counts())) expect(count).toBe(0)
    record('website-paths', { routes: pageRoutes.length, assets: assets.length, probes: seen })
  })

  test('the app loads its typeface from its own script, and a gateway that admits nothing is shown with its Ref, never as an empty shop', async () => {
    const answers = new Map<string, { status: number; service: string | undefined }>()
    site.on('response', (response) => {
      const id = response.headers()['x-request-id']
      if (id && !new URL(response.url()).pathname.startsWith('/assets/')) answers.set(id, { status: response.status(), service: response.headers()['x-service'] })
    })
    await open(site, '/')
    await expect(site.getByRole('heading', { level: 1, name: 'What can we fetch for you?' })).toBeVisible()
    expect(
      await site.evaluate(() => ({
        faces: [...document.fonts].map((face) => `${face.family.replace(/["']/g, '')}:${face.status}`),
        text: getComputedStyle(document.body).fontFamily.split(',')[0].replace(/["']/g, '').trim() === 'Plus Jakarta Sans' && document.fonts.check('16px "Plus Jakarta Sans"'),
      })),
    ).toEqual({ faces: ['Plus Jakarta Sans:loaded'], text: true })

    // Each section says in plain words that Fetch can't take requests, that
    // the gateway answered, and the reference of the very answer it got.
    const problems = site.getByRole('alert').filter({ hasText: 'Fetch can’t take requests right now.' })
    await expect(problems).toHaveCount(2)
    const shown: string[] = []
    for (const problem of await problems.all()) {
      await expect(problem).toContainText('Answered by the Fetch gateway · 503')
      const ref = ((await problem.getByTitle(/^Full reference /).getAttribute('title')) ?? '').replace(/^Full reference /, '')
      expect(answers.get(ref), ref).toEqual({ status: 503, service: undefined })
      // The page shows the reference's first eight characters, grouped.
      await expect(problem.getByText(`Ref ${ref.slice(0, 4).toUpperCase()}-${ref.slice(4, 8).toUpperCase()}`, { exact: true })).toBeVisible()
      shown.push(ref)
    }
    await shot(site, '01-home-not-admitting')

    // Try again asks again and shows the new answer's reference.
    await problems.first().getByRole('button', { name: 'Try again' }).click()
    await expect(problems.first().getByTitle(/^Full reference /)).not.toHaveAttribute('title', `Full reference ${shown[0]}`)

    // Signing in reaches the gateway too, and its refusal carries a Ref.
    await open(site, '/signin')
    await site.getByLabel('Email').fill('customer1@fetch.test')
    await site.getByLabel('Password', { exact: true }).fill('FetchDemo2026!')
    await site.getByRole('button', { name: 'Sign in', exact: true }).click()
    const refusal = site.getByRole('alert').filter({ hasText: 'Answered by the Fetch gateway · 503' })
    await expect(refusal).toBeVisible()
    const ref = ((await refusal.getByTitle(/^Full reference /).getAttribute('title')) ?? '').replace(/^Full reference /, '')
    expect(answers.get(ref)?.status).toBe(503)
    await shot(site, '02-signin-not-admitting')
    for (const count of Object.values(await demo.counts())) expect(count).toBe(0)
    record('refusals-with-ref', { home: shown, signIn: ref })
  })

  test('every request stays on the app origin, scripts cannot reach a service or the Admin API, and the tab keeps only its delivery address', async () => {
    const start = requests.length
    for (const route of pageRoutes) await open(site, route)

    // A delivery address is kept in this tab only.
    await open(site, '/')
    await site.getByRole('button', { name: 'Add a delivery address' }).click()
    await site.getByLabel('Street address').fill('12 Jalan Telawi 3')
    await site.getByLabel('Postcode').fill('59100')
    await site.getByRole('button', { name: 'Save address' }).click()
    await expect(site.getByRole('button', { name: /12 Jalan Telawi 3/ })).toBeVisible()
    expect(await storage(site)).toEqual({ session: ['fetch.address'], local: [], cookie: '' })
    expect(await context.cookies(siteURL)).toEqual([])
    // Another tab starts with nothing.
    const other = await context.newPage()
    await open(other, '/')
    expect(await storage(other)).toEqual({ session: [], local: [], cookie: '' })
    await expect(other.getByRole('button', { name: 'Add a delivery address' })).toBeVisible()
    await other.close()

    // Every request the app made went to its own origin, and none to /admin.
    const made = requests.slice(start)
    expect(made.filter((url) => !url.startsWith(siteURL + '/'))).toEqual([])
    expect(made.filter((url) => new URL(url).pathname.startsWith('/admin'))).toEqual([])
    expect(violations).toEqual([])

    // The content policy stops the page's scripts from reaching a service or
    // the Admin API directly; /admin on the app's origin is the gateway's
    // enforcement, not the Admin API.
    const probes = await site.evaluate(
      async ([service, admin]) => {
        const attempt = (url: string) =>
          fetch(url).then(
            async (r) => `answered ${r.status} ${r.headers.get('content-type')}`,
            () => 'blocked',
          )
        return { service: await attempt(service), admin: await attempt(admin), sameOrigin: await attempt('/admin/status') }
      },
      [controls.inventory + '/receipts', adminURL + '/admin/status'],
    )
    expect(probes).toEqual({ service: 'blocked', admin: 'blocked', sameOrigin: 'answered 503 application/json' })
    await expect.poll(() => violations.length).toBe(2)
    expect(violations.every((v) => v.startsWith('connect-src http://127.0.0.1:'))).toBe(true)
    violations.length = 0
    expect(scriptDialogs).toBe(0)
    record('origin-and-storage', { requests: made.length, probes })
  })

  test('the built files hold no secret, development host or stand-in data, and nothing secret reaches the page or console', async () => {
    const secret = demo.file('service.secret').trim()
    const forbidden = ['PRIVATE KEY', 'localhost', ':5173', ':5174', ':5175', 'fetch.dev.fail', 'Coconut rice with slow-cooked beef rendang', secret]
    const found: string[] = []
    for (const path of files(applicationDist)) {
      const text = readFileSync(path, 'latin1')
      for (const word of forbidden) if (text.includes(word)) found.push(`${word === secret ? 'service secret' : word} in ${path}`)
    }
    expect(found).toEqual([])
    expect(consoleLines.filter((line) => line.includes(secret) || /eyJ[\w-]+\.[\w-]+\./.test(line))).toEqual([])
    expect(requests.filter((url) => url.includes(secret))).toEqual([])
    await open(site, '/')
    expect(await site.content()).not.toContain(secret)
    record('no-secrets', { scanned: files(applicationDist).length, found })
  })

  test('malformed saved basket, address and session data cannot break the tab', async () => {
    const cases = [
      { key: 'fetch.basket', value: { merchant: { id: 'MER-001', name: 'Shop' }, lines: [null] } },
      { key: 'fetch.address', value: { line1: 'Saved street', postcode: 59100, city: {}, state: 'Selangor' } },
      { key: 'fetch.session', value: { token: 'invalid-saved-token', expiresAt: Date.now() + 600_000, user: {} } },
    ]
    for (const saved of cases) {
      const page = await context.newPage()
      const errors: string[] = []
      page.on('pageerror', (error) => errors.push(error.message))
      try {
        await page.addInitScript(({ key, value }) => sessionStorage.setItem(key, JSON.stringify(value)), saved)
        await page.goto(siteURL + '/cart', { waitUntil: 'networkidle' })
        await expect.soft(page.getByRole('main'), saved.key).toBeVisible()
        await expect.soft(page.getByRole('link', { name: 'Sign in', exact: true }).first(), saved.key).toBeVisible()
        if (saved.key === 'fetch.address') await expect.soft(page.getByRole('button', { name: 'Add a delivery address' })).toBeVisible()
        expect.soft(errors, saved.key).toEqual([])
      } finally {
        await page.close()
      }
    }
    // A page address with a malformed escape, such as a sign-in link's next
    // page, names no page: the tab says so and keeps working.
    const page = await context.newPage()
    const errors: string[] = []
    page.on('pageerror', (error) => errors.push(error.message))
    try {
      await page.goto(siteURL + '/', { waitUntil: 'networkidle' })
      await page.evaluate(() => {
        history.pushState(null, '', '/store/%')
        dispatchEvent(new PopStateEvent('popstate'))
      })
      await expect(page.getByText('This page isn’t on Fetch')).toBeVisible()
      await expect(page.getByRole('main')).toBeVisible()
      expect(errors).toEqual([])
    } finally {
      await page.close()
    }
    for (const count of Object.values(await demo.counts())) expect(count).toBe(0)
    record('malformed-saved-data', { keys: [...cases.map((saved) => saved.key), 'malformed-address'] })
  })

  test('the reachable pages pass the accessibility rules at desktop, tablet and phone widths, and work by keyboard', async () => {
    const results: Record<string, number> = {}
    for (const width of [1440, 768, 375]) {
      await site.setViewportSize({ width, height: 900 })
      for (const route of ['/', '/food', '/mart', '/signin', '/help', '/cart', '/lab']) {
        await open(site, route)
        await accessible(site, `${route} at ${width}px`)
        results[`${width} ${route}`] = 0
        await shot(site, `03-${width}-${route === '/' ? 'home' : route.slice(1)}`)
      }
    }
    await site.setViewportSize({ width: 1440, height: 900 })

    // The site reads like a working product: no demonstration badge or
    // wording anywhere, and the security lab is a plain footer link.
    await open(site, '/')
    await expect(site.getByRole('banner').getByText('Demo', { exact: true })).toHaveCount(0)
    await expect(site.locator('body')).not.toContainText('demonstration')
    await site.getByRole('contentinfo').getByRole('link', { name: 'Security lab' }).click()
    await expect(site).toHaveURL(/\/lab$/)

    // From the top of the page, the keyboard reaches the search box and Sign
    // in, each with a visible focus ring, drawn on the control or on the
    // field that wraps it.
    await open(site, '/')
    await site.keyboard.press('Tab')
    const seen: string[] = []
    for (let i = 0; i < 8; i++) {
      seen.push(
        await site.evaluate(() => {
          const el = document.activeElement as HTMLElement
          // A ring is the control's own focus outline, or an outline on its
          // direct wrapper, which a wrapper draws only while focus is within.
          // A shadow does not count: some controls carry one at rest.
          const ring = getComputedStyle(el).outlineStyle !== 'none' || (el.parentElement !== null && getComputedStyle(el.parentElement).outlineStyle !== 'none')
          const name = el.getAttribute('aria-label') || el.textContent?.trim() || (el as HTMLInputElement).labels?.[0]?.textContent?.trim() || el.tagName
          return `${name}${ring ? '' : ' (no focus ring)'}`
        }),
      )
      await site.keyboard.press('Tab')
    }
    expect(seen.filter((s) => s.endsWith('(no focus ring)'))).toEqual([])
    expect(seen.some((s) => /Sign in/.test(s))).toBe(true)

    // Text at twice its size still fits.
    await site.evaluate(() => {
      document.documentElement.style.fontSize = '200%'
    })
    for (const width of [1440, 375]) {
      await site.setViewportSize({ width, height: 900 })
      expect(await site.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `200% text at ${width}px`).toBe(true)
    }
    await site.evaluate(() => {
      document.documentElement.style.fontSize = ''
    })
    await site.setViewportSize({ width: 1440, height: 900 })
    expect(violations).toEqual([])
    record('accessibility', { results, keyboard: seen })
  })
})
