import { readdirSync, readFileSync } from 'node:fs'
import { resolve as resolvePath } from 'node:path'
import { expect, test } from '@playwright/test'
import { adminURL, applicationDist, controls, gatewayIdentity, pageRoutes, raw, siteURL, uiPolicy } from './env'
import { record, storage } from './pages'
import { consoleLines, context, demo, files, open, requests, scriptDialogs, site, useSite, violations } from './site'

// How the gateway's data plane serves the Fetch app before any policy
// exists: its routes and assets, what the page may reach and keep, and that
// the built files hold nothing secret.
const assetTypes: Record<string, string> = { js: 'text/javascript', css: 'text/css', svg: 'image/svg+xml', webp: 'image/webp' }

test.describe.serial('How the gateway serves the Fetch app', () => {
  useSite()

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
    expect(scriptDialogs()).toBe(0)
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
})
