import { readFileSync } from 'node:fs'
import { resolve as resolvePath } from 'node:path'
import { expect, test } from '@playwright/test'
import { adminURL, applicationDist, gatewayIdentity, raw, siteURL } from './env'
import { dialogButton, enrol, expectLayoutOnlyStorage, go, moreDetail, nav, record, resolve, revisions, shot, signIn, stage, trial, trialStep } from './pages'
import { admin, adminCall, adminContext, demo, enrolled, enrolmentSecret, finish, fontsInUse, handedOut, open, password, status, strip, uiPolicy } from './setup'

// Cold start, sign-in and the first policy on a new installation.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await admin.goto(adminURL + '/')
  })

  test.afterAll(finish)

  test('cold start serves the dashboard and its assets from the admin listener only, under the content policy', async () => {
    const identity = gatewayIdentity()
    record('cold-start', { gateway: identity, pids: Object.keys(demo.pids), web: demo.web })
    expect(Object.keys(demo.pids).sort()).toEqual(['accounts', 'gateway', 'inventory', 'orders', 'reports', 'support'])

    await admin.goto(adminURL + '/')
    await expect(admin.getByRole('heading', { level: 1, name: 'Sign in' })).toBeVisible()
    // The state shown is the gateway's own: a new installation admits nothing.
    await expect(admin.locator('.entry-state')).toContainText('Gateway: New installation, waiting for its first policy')
    await expect(admin.locator('.entry-state code')).toHaveText('bootstrap_locked')
    // The two typefaces load under the content policy, from the page's own
    // script, and the page's text and code are set in them.
    expect(await fontsInUse(admin)).toEqual({ faces: ['Hanken Grotesk:loaded', 'Geist Mono:loaded'], text: true, code: true })
    await shot(admin, '01-sign-in')

    // Every built file is served from /assets/ with a fixed type and the
    // frontends' content policy; none is reachable on the data plane, whose
    // /assets/ holds only the Fetch app's files.
    const index = readFileSync(resolvePath(applicationDist, 'index.html'), 'utf8')
    const assets = [...index.matchAll(/(?:src|href)="\/assets\/([^"]+)"/g)].map((m) => m[1])
    expect(assets.map((a) => a.split('.').pop()).sort()).toEqual(['css', 'js', 'svg'])
    const types: Record<string, string> = { js: 'text/javascript; charset=utf-8', css: 'text/css; charset=utf-8', svg: 'image/svg+xml' }
    const served = []
    for (const asset of assets) {
      const reply = await raw('GET', `${adminURL}/assets/${asset}`)
      expect(reply.status, asset).toBe(200)
      expect(reply.headers['content-type'], asset).toBe(types[asset.split('.').pop()!])
      expect(reply.headers['content-security-policy'], asset).toBe(uiPolicy)
      expect(reply.body, asset).toBe(readFileSync(resolvePath(applicationDist, 'assets', asset), 'utf8'))
      const onDataPlane = await raw('GET', `${siteURL}/assets/${asset}`)
      expect(onDataPlane.status, asset).toBe(404)
      expect(onDataPlane.body, asset).not.toBe(reply.body)
      served.push({ asset, status: reply.status, dataPlane: onDataPlane.status })
    }

    // API paths answer as the API, never with the dashboard page, and the
    // data plane serves no dashboard.
    const probes = [
      { url: adminURL + '/admin/status', status: 401 },
      { url: adminURL + '/admin/policy', status: 401 },
      { url: adminURL + '/admin/blocked-accounts', status: 401 },
      { url: adminURL + '/admin/events', status: 401 },
      { url: adminURL + '/admin/history', status: 401 },
      { url: adminURL + '/admin/subjects', status: 404 },
      { url: adminURL + '/admin/no-such-route', status: 404 },
      { url: adminURL + '/index.html', status: 404 },
      { url: adminURL + '/assets/index.html', status: 404 },
      { url: adminURL + '/assets/no-such-file.js', status: 404 },
      { url: adminURL + '/admin/', status: 404 },
      { url: siteURL + '/admin/status', status: 503 },
      { url: siteURL + '/admin/', status: 503 },
      { url: siteURL + '/demo/../admin/index.html', status: 503 },
    ]
    const seen = []
    for (const probe of probes) {
      const reply = await raw('GET', probe.url)
      seen.push({ url: probe.url, status: reply.status, type: reply.headers['content-type'] })
      expect(reply.status, probe.url).toBe(probe.status)
      expect(String(reply.headers['content-type'] ?? ''), probe.url).not.toContain('text/html')
      expect(reply.body, probe.url).not.toContain('<!doctype html>')
    }
    // The data plane's page is the Fetch app, never the dashboard.
    const fetchPage = await raw('GET', siteURL + '/')
    expect(fetchPage.status).toBe(200)
    expect(fetchPage.body).not.toBe(index)
    expect(fetchPage.body).not.toContain(assets[0])

    // The page itself carries no data, needs no session and is never cached.
    const page = await raw('GET', adminURL + '/')
    expect(page.status).toBe(200)
    expect(page.headers['content-security-policy']).toBe(uiPolicy)
    expect(page.headers['x-frame-options']).toBe('DENY')
    expect(page.headers['referrer-policy']).toBe('no-referrer')
    expect(page.headers['x-content-type-options']).toBe('nosniff')
    expect(page.headers['cache-control']).toBe('no-store')
    expect(page.body).not.toMatch(/revision|csrf|subject/i)

    // Each interface listens only on its own loopback host.
    await expect(raw('GET', adminURL.replace('127.0.0.1', '127.0.0.2') + '/')).rejects.toThrow(/ECONNREFUSED/)
    await expect(raw('GET', siteURL.replace('127.0.0.2', '127.0.0.1') + '/')).rejects.toThrow(/ECONNREFUSED/)
    expect(await demo.counts()).toEqual({ inventory: 0, orders: 0, reports: 0, support: 0, accounts: 0 })
    record('api-paths', { assets: served, probes: seen })
  })

  test('wrong credentials are refused; enrolment and sign-in use the HttpOnly host-only session cookie', async () => {
    // A wrong enrolment secret and a wrong password are refused, and no
    // session cookie is set.
    await admin.getByRole('button', { name: 'First Sign-In with an Enrolment Secret' }).click()
    await admin.getByLabel('Administrator Name').fill('admin')
    await admin.getByLabel('Enrolment Secret').fill('not-the-enrolment-secret')
    await admin.getByLabel('New Password', { exact: true }).fill(password)
    await admin.getByLabel('Repeat the New Password').fill(password)
    await admin.getByRole('button', { name: 'Enrol', exact: true }).click()
    const enrolRefusal = admin.getByRole('alert').filter({ has: admin.locator('.more-details dd code', { hasText: /^401$/ }) })
    await expect(enrolRefusal).toHaveClass(/tone-refused/)
    await expect(moreDetail(enrolRefusal, 'Error code')).toHaveText('unauthenticated')
    await admin.getByRole('button', { name: 'Password', exact: true }).click()
    await admin.getByLabel('Administrator Name').fill('admin')
    await admin.getByLabel('Password', { exact: true }).fill('wrong password, long enough')
    await admin.getByRole('button', { name: 'Sign in', exact: true }).click()
    const refusal = admin.getByRole('alert').filter({ has: admin.locator('.more-details dd code', { hasText: /^401$/ }) })
    await expect(refusal).toHaveClass(/tone-refused/)
    await expect(moreDetail(refusal, 'Error code')).toHaveText('unauthenticated')
    await expect(refusal).toContainText('Not signed in')
    await expect(refusal).toContainText('Your session has ended, or the name or password is wrong. Sign in again.')
    await shot(admin, '02-sign-in-refused')
    expect(await adminContext.cookies(adminURL)).toHaveLength(0)

    await enrol(admin, 'admin', enrolmentSecret, password)
    await signIn(admin, 'admin', password)

    const cookies = await adminContext.cookies(adminURL)
    expect(cookies).toHaveLength(1)
    expect(cookies[0]).toMatchObject({ name: '__Host-gateway-admin', httpOnly: true, secure: true, sameSite: 'Strict', path: '/' })
    expect(await admin.evaluate(() => document.cookie)).toBe('')
    await expectLayoutOnlyStorage(admin, [password, ...handedOut, cookies[0].value, String((await adminCall('GET', '/admin/session')).body.csrf_token)])
    // The data-plane host never receives the administrator's cookie.
    expect(await adminContext.cookies(siteURL)).toHaveLength(0)

    // A new installation says how to establish its first policy.
    await go(admin, 'Overview')
    const firstBase = admin.getByRole('region', { name: 'Establish the first policy' })
    await expect(firstBase).toContainText('Save a draft change: a complete policy that denies every endpoint')
    await expect(firstBase.getByRole('button', { name: 'Go to Changes' })).toBeVisible()
    // The status strip and the not-accepting notice say so, with the
    // gateway's own admission value under More details.
    await expect(strip(admin).getByText('Not Accepting Requests', { exact: true })).toBeVisible()
    await expect(moreDetail(admin.getByRole('alert').filter({ hasText: 'The gateway is not accepting requests' }), 'Admission value')).toHaveText('bootstrap_locked')
    await expect(strip(admin)).toContainText('None yet')
    await expect(admin.getByRole('alert').filter({ hasText: 'The gateway is not accepting requests' })).toContainText(
      'New installation: every request is refused until an administrator establishes the first policy (Changes).',
    )
    // Every other page carries the same warning across its top.
    await go(admin, 'Changes')
    await expect(admin.getByRole('status').filter({ hasText: 'The gateway is not accepting requests.' })).toBeVisible()
    await go(admin, 'Overview')
    await shot(admin, '03-overview-bootstrap')

    // The open view is kept across a reload, and the session resumes.
    await revisions(admin)
    expect(new URL(admin.url()).hash).toBe('#/versions')
    await expect(admin.getByText('No policy is active yet.')).toBeVisible()
    await admin.reload()
    await expect(admin.getByText('Signed in as admin')).toBeAttached()
    await expect(admin.getByRole('heading', { level: 1, name: 'Versions' })).toBeVisible()
    await shot(admin, '03-policy-empty')
    record('session-cookie', { cookies: cookies.map((c) => ({ name: c.name, httpOnly: c.httpOnly, secure: c.secure, sameSite: c.sameSite, path: c.path })) })
  })
})

test.describe.serial('dashboard: first policy', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await enrolled()
  })

  test.afterAll(finish)

  test('the first policy denies everything, then the baseline is activated and confirmed', async () => {
    const denyAll = await stage(admin, { file: demo.path('deny-all.json') })
    await expect(admin.getByRole('main').getByText('yes', { exact: true }).first()).toBeVisible()
    await admin.getByRole('button', { name: 'Establish as the First Policy' }).click()
    // Establishing a policy that denies everything is confirmed by typing.
    await expect(dialogButton(admin, 'Establish')).toBeDisabled()
    await admin.getByRole('dialog').getByLabel('Type deny all to confirm').fill('deny all')
    await dialogButton(admin, 'Establish').click()
    await expect(admin.getByText(/^Established: version 1 is the active policy\.$/)).toBeVisible()
    expect(await status()).toMatchObject({ state: 'normal', confirmed: 1 })
    await go(admin, 'Overview')
    await expect(admin.getByRole('region', { name: 'Establish the first policy' })).toHaveCount(0)
    await expect(strip(admin)).toContainText('Version 1')
    await expect(strip(admin).getByText('Accepting Requests', { exact: true })).toBeVisible()
    // The APIs view shows every operation closed by the deny-all policy.
    await go(admin, 'APIs')
    const closed = await admin.getByRole('main').getByText('Closed', { exact: true }).count()
    expect(closed).toBe(((await adminCall('GET', '/admin/policy')).body.document.operations as unknown[]).length)

    const baseline = await stage(admin, { file: demo.path('baseline.json') })
    const listChange = admin.getByRole('listitem').filter({ hasText: 'endpoints: changed orders.all' })
    await expect(listChange.getByRole('listitem')).toHaveText(['access: "deny" → "allow"'])
    await expect(admin.getByText('permissions: added SUPPORT → orders.all')).toBeVisible()
    await shot(admin, '04-candidate-review')
    await trialStep(admin)
    await admin.getByLabel('Trial Length (Seconds)').fill('600')
    await admin.getByRole('main').getByRole('button', { name: 'Put on Trial', exact: true }).click()
    await expect(admin.getByRole('dialog')).toContainText('Unless someone keeps it within 10 min, the gateway undoes it and returns to the active version now, version 1.')
    await shot(admin, '05-activate-dialog')
    await dialogButton(admin, 'Put on Trial').click()
    const done = admin.getByText(/^Version \d+ is on trial and active/)
    await expect(done).toBeVisible()
    const provisional = Number((await done.textContent())!.match(/Version (\d+)/)![1])
    expect(await status()).toMatchObject({ confirmed: 1, provisional, revision: provisional })

    // While it waits, the overview shows both bases and the countdown, and
    // the navigation flags the trial.
    await go(admin, 'Overview')
    await expect(strip(admin)).toContainText(`Version ${provisional}`)
    await expect(strip(admin)).toContainText('On trial; version 1 returns if it is undone')
    await expect(strip(admin).getByText('Undone in')).toBeVisible()
    await expect(strip(admin).getByText(/^\d{2}:\d{2}$/)).toBeVisible()
    await expect(nav(admin, 'Changes')).toContainText('On Trial')
    await shot(admin, '06-overview-provisional')
    await go(admin, 'Changes')
    await expect(trial(admin)).toBeVisible()
    await shot(admin, '07-provisional')
    await resolve(admin, 'confirm')
    const after = await status()
    expect(after).toMatchObject({ state: 'normal', admission: 'ready', confirmed: provisional, revision: provisional, overrides: [] })
    expect(after.provisional).toBeUndefined()
    record('genesis-and-baseline', { denyAll, baseline, provisional, status: after })
  })
})
