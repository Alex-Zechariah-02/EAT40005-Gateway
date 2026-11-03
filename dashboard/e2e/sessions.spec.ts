import { expect, test } from '@playwright/test'
import { adminURL, raw, ShortSessions } from './env'
import { createOverride, enrol, go, record, revokeOverride, shot, signIn } from './pages'
import { admin, adminCall, adminContext, demo, finish, handedOut, listAll, newContext, open, password, ready, sessionCookie } from './setup'

// Signing out with answers pending, the inactivity pause and short sessions.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('signing out while a change waits for its answer forgets it: a late lost or definite answer neither lists it again nor touches a later change', async () => {
    const lostSubject = 'cust-006'
    const lateSubject = 'cust-007'
    const laterSubject = 'cust-008'
    const unresolved = admin.getByRole('region', { name: /^(A change has an unknown outcome|Outcome found)$/ })
    // hold lets the gateway act on a block, then keeps its answer back until
    // released, and then loses it or delivers it.
    async function hold(subject: string, answer: 'lost' | 'definite') {
      let release!: () => void
      const gate = new Promise<void>((resolve) => (release = resolve))
      let finished!: () => void
      const done = new Promise<void>((resolve) => (finished = resolve))
      await admin.route(`**/admin/blocked-accounts/${subject}`, async (route) => {
        if (route.request().method() !== 'PUT') return route.continue()
        const response = await route.fetch()
        await gate
        if (answer === 'lost') await route.abort('connectionreset')
        else await route.fulfill({ response })
        finished()
      })
      return { release, done }
    }
    async function block(subject: string) {
      await go(admin, 'Accounts')
      await admin.getByRole('main').getByRole('button', { name: 'Block an Account' }).click()
      await admin.getByLabel('Account ID').fill(subject)
      await admin.getByLabel('Reason').fill('sign-out race test')
      await admin.getByRole('button', { name: 'Block Now' }).click()
    }
    async function signOutHere() {
      await admin.getByRole('button', { name: 'Sign Out' }).click()
      await expect(admin.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    }
    // Late continuations run within moments of the answer; this gives them time.
    const settleTime = 1_000

    const lost = await hold(lostSubject, 'lost')
    const late = await hold(lateSubject, 'definite')
    try {
      await block(lostSubject)
      await expect(admin.getByText("Sent; waiting for the gateway's answer…")).toBeVisible()
      await signOutHere()
      lost.release()
      await lost.done
      await admin.waitForTimeout(settleTime)
      await signIn(admin, 'admin', password)
      await go(admin, 'Accounts')
      await expect(unresolved).toHaveCount(0)
      // Forgetting it here did not undo it: the gateway applied the block once.
      const lostBlocks = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'account.block' && h.target === lostSubject)
      expect(lostBlocks).toHaveLength(1)

      // A definite answer arriving after sign-out, while a later change by the
      // same administrator is unresolved, leaves that later change as it was.
      await block(lateSubject)
      await expect(admin.getByText("Sent; waiting for the gateway's answer…")).toBeVisible()
      await signOutHere()
      await signIn(admin, 'admin', password)
      await admin.route(`**/admin/blocked-accounts/${laterSubject}`, async (route) => {
        if (route.request().method() !== 'PUT') return route.continue()
        await route.fetch()
        await route.abort('connectionreset')
      })
      await admin.route('**/admin/history/admin*', (route) => route.abort('connectionreset'))
      await block(laterSubject)
      await expect(unresolved).toContainText(`Block ${laterSubject}`)
      await expect(unresolved).toContainText('It could not be looked up')
      late.release()
      await late.done
      await admin.waitForTimeout(settleTime)
      await expect(unresolved).toContainText(`Block ${laterSubject}`)
      await expect(unresolved).toContainText('It could not be looked up')
      await expect(unresolved).not.toContainText(lateSubject)
      await expect(unresolved.getByRole('heading')).toHaveText('A change has an unknown outcome')
      await unresolved.getByRole('button', { name: /^Stop Tracking/ }).click()
      await expect(unresolved).toHaveCount(0)
      record('sign-out-forgets-late-answers', { lostBlocks: lostBlocks.length })
    } finally {
      lost.release()
      late.release()
      await admin.unroute(`**/admin/blocked-accounts/${lostSubject}`)
      await admin.unroute(`**/admin/blocked-accounts/${lateSubject}`)
      await admin.unroute(`**/admin/blocked-accounts/${laterSubject}`)
      await admin.unroute('**/admin/history/admin*')
    }
    for (const subject of [lostSubject, lateSubject, laterSubject]) {
      expect((await adminCall('DELETE', `/admin/blocked-accounts/${subject}`, undefined, { 'Idempotency-Key': crypto.randomUUID() })).status).toBe(200)
    }
  })

  test('background reads pause after ten minutes without input, resume at the next input, and go on while a temporary rule is active', async () => {
    // The fake clock runs about 24 minutes of one-second reads against the
    // real gateway, about 1,400 read rounds, which needs more than the
    // default budget.
    test.setTimeout(480_000)
    const page = await adminContext.newPage()
    try {
      await page.clock.install()
      await page.goto(adminURL + '/#/overview')
      await expect(page.getByRole('heading', { level: 1, name: 'Overview', exact: true })).toBeVisible()
      const reads: string[] = []
      page.on('request', (r) => reads.push(new URL(r.url()).pathname))
      const readsDuring = async (ms: number) => {
        const from = reads.length
        await page.clock.runFor(ms)
        await page.waitForTimeout(300)
        return reads.slice(from)
      }
      const notice = page.getByText(/^Auto-refresh paused after 10 minutes of inactivity/)
      expect((await readsDuring(30_000)).length).toBeGreaterThan(0)
      await readsDuring(10 * 60_000)
      expect(await readsDuring(60_000)).toEqual([])
      await expect(notice).toBeVisible()
      await shot(page, '43-updates-paused')

      // Any input resumes them.
      await page.mouse.move(40, 40)
      expect(await readsDuring(11_000)).toContain('/admin/status')
      await expect(notice).toHaveCount(0)

      // While a temporary rule is active they go on without input.
      const id = await createOverride(admin, { action: 'Restrict Temporarily: Stop a Role Calling an Endpoint', operation: 'inventory.stock.adjust', role: 'OPERATIONS', seconds: 600 })
      await readsDuring(11_000)
      await readsDuring(11 * 60_000)
      const during = await readsDuring(60_000)
      expect(during).toContain('/admin/status')
      await expect(notice).toHaveCount(0)
      await revokeOverride(admin, id)
      record('inactivity-pause', { readsWhileRuleActive: during.length })
    } finally {
      await page.close()
    }
  })

  test('an idle session and a session past its lifetime end, and the dashboard says so', async ({ browser }) => {
    const sessions = new ShortSessions(demo.bin, demo, 4, 12)
    const secret = sessions.install()
    handedOut.push(secret)
    await sessions.start()
    try {
      const context = await newContext(browser)
      const page = await context.newPage()
      await page.goto(ShortSessions.url + '/')
      await enrol(page, 'admin', secret, password)
      await signIn(page, 'admin', password)
      await expect(page.getByText('New installation: every request is refused until an administrator establishes the first policy (Changes).')).toBeVisible()

      // Idle: no request for longer than the idle timeout ends the session.
      const idleStart = Date.now()
      await page.waitForTimeout(sessions.idleSeconds * 1000 + 1500)
      const ended = page.getByText('Your session has ended. Sign in again.')
      if (!(await ended.isVisible())) await page.getByRole('button', { name: 'Refresh', exact: true }).click()
      await expect(ended).toBeVisible()
      await expect(page.getByRole('navigation', { name: 'Administration' })).toHaveCount(0)
      const idleEnded = Date.now() - idleStart
      await shot(page, '33-session-ended')

      // Lifetime: a session kept busy still ends at its absolute deadline.
      await signIn(page, 'admin', password)
      const signedInAt = Date.now()
      const cookie = await sessionCookie(context)
      let alive = true
      while (alive) {
        await page.waitForTimeout(1000)
        alive = (await raw('GET', ShortSessions.url + '/admin/session', { Cookie: cookie })).status === 200
        expect(Date.now() - signedInAt, 'the session outlived its lifetime').toBeLessThan((sessions.lifetimeSeconds + 4) * 1000)
      }
      const lifetimeEnded = Date.now() - signedInAt
      expect(lifetimeEnded).toBeGreaterThanOrEqual(sessions.lifetimeSeconds * 1000 - 1000)
      // The page rereads the status each second, so it finds the ended
      // session by itself.
      await expect(page.getByText('Your session has ended. Sign in again.')).toBeVisible({ timeout: 5_000 })
      await context.close()
      record('session-expiry', { idleSeconds: sessions.idleSeconds, idleEndedMs: idleEnded, lifetimeSeconds: sessions.lifetimeSeconds, lifetimeEndedMs: lifetimeEnded })
    } finally {
      await sessions.stop()
    }
  })
})
