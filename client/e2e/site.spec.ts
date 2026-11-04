import { expect, test } from '@playwright/test'
import { siteURL } from './env'
import { accessible, record, shot } from './pages'
import { context, demo, open, site, useSite, violations } from './site'

// The Fetch app in the browser before any policy exists: a gateway that
// admits nothing is shown with its Ref, malformed saved data cannot break the
// tab, and the reachable pages pass the accessibility rules.

test.describe.serial('Fetch app on the gateway data plane', () => {
  useSite()

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
