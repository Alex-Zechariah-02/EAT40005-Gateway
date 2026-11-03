import { expect, test } from '@playwright/test'
import { adminURL } from './env'
import { activate, confirmTrial, dialogButton, go, moreDetail, record, resolve, shot, signIn, stage, trial, trialStep } from './pages'
import { admin, adminContext, admitted, demo, finish, listAll, open, password, propose, ready, sales, signedInPage, status } from './setup'

// Concurrent sessions, lost answers, the keep guard and sessions ending during a trial.
// Each file starts its own gateway; setup.ts builds the state the tests read.

test.describe.serial('dashboard', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('concurrent sessions cannot overwrite each other and a lost answer is resolved by resending', async ({ browser }) => {
    const other = await signedInPage(browser, 'admin')

    // One session confirms; the other's rollback, based on what it read
    // earlier, is refused and shown as refused. The other session's status
    // reads fail meanwhile (as on a dropped connection), so its page still
    // shows the trial when its rollback reaches the gateway.
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const first = await activate(admin, 600)
    await go(other.page, 'Changes')
    const target = (await status()).confirmed
    await expect(other.page.getByRole('button', { name: 'Undo Change', exact: true })).toBeVisible()
    await other.page.route('**/admin/status', (route) => route.abort())
    await resolve(admin, 'confirm')
    await other.page.getByRole('main').getByRole('button', { name: 'Undo Change', exact: true }).click()
    await expect(other.page.getByRole('dialog').getByRole('heading', { name: `Undo the change and return to version ${target}?` })).toBeVisible()
    await dialogButton(other.page, 'Undo Change').click()
    const refusal = other.page.getByRole('alert').filter({ has: other.page.locator('.more-details dd code', { hasText: /^4\d\d$/ }) })
    await expect(refusal).toBeVisible()
    await expect(refusal).toHaveClass(/tone-refused/)
    const refusalText = await refusal.textContent()
    await expect(other.page.getByText(/^Undone:/)).toHaveCount(0)
    await shot(other.page, '13-stale-refusal')
    expect(await status()).toMatchObject({ confirmed: first, revision: first })
    await other.page.unroute('**/admin/status')

    // A confirmation whose answer is lost: the gateway applies it, the page
    // says the outcome is unknown, and resending the identical request returns
    // the recorded receipt without a second transition.
    await stage(admin, { file: demo.path('baseline.json') })
    const second = await activate(admin, 600)
    let lostReceipt: { transition_id?: string } = {}
    await admin.route('**/admin/provisional/confirm', async (route) => {
      const response = await route.fetch()
      lostReceipt = await response.json()
      await route.abort('connectionreset')
    })
    await go(admin, 'Changes')
    await confirmTrial(admin)
    await expect(admin.getByText('No answer from the gateway: outcome unknown')).toBeVisible()
    await expect(admin.getByText(/^Kept:/)).toHaveCount(0)
    await shot(admin, '14-outcome-unknown')
    expect(await status()).toMatchObject({ confirmed: second })
    // Without sending anything again, the page looks the change up by its
    // exact transition and finds it applied.
    const unresolved = admin.getByRole('region', { name: /^(A change has an unknown outcome|Outcome found)$/ })
    await expect(unresolved).toContainText(`Keep version ${second}`)
    await expect(unresolved.getByText(/^Applied: the gateway recorded it at /)).toBeVisible()
    await admin.unroute('**/admin/provisional/confirm')
    await admin.getByRole('button', { name: 'Send the Identical Request Again', exact: true }).click()
    await expect(admin.getByText(`Kept: version ${second} is now the active policy.`)).toBeVisible()
    // Changes also shows the earlier staging and trial receipts; this is the
    // confirmation's own.
    const receipt = admin.getByRole('main').locator('.notice', { hasText: `Kept: version ${second} is now the active policy.` })
    const shownTransition = await moreDetail(receipt, 'Transition').textContent()
    expect(shownTransition).toBe(lostReceipt.transition_id)
    const confirms = (await listAll<{ kind: string; target: string }>('/admin/history')).filter(
      (h) => h.kind === 'cpa.confirm' && h.target === `revision ${second}`,
    )
    expect(confirms).toHaveLength(1)
    await other.context.close()
    record('concurrency-and-lost-answer', { refusal: refusalText, lostReceipt, shownTransition, confirmTransitions: confirms.length })
  })

  test('a confirmation reads the gateway again first, needs a deliberate step when evidence is missing or the trial changed, and never sends by itself', async ({ browser }) => {
    const confirms: string[] = []
    const onRequest = (request: { url: () => string; method: () => string }) => {
      if (new URL(request.url()).pathname === '/admin/provisional/confirm') confirms.push(request.method())
    }
    admin.on('request', onRequest)
    const other = await signedInPage(browser, 'admin')
    try {
      await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
      // A trial under two minutes gets advice; the gateway would accept it.
      await trialStep(admin)
      await admin.getByLabel('Trial Length (Seconds)').fill('60')
      await expect(admin.getByText(/^Under 2 minutes leaves little time/).first()).toBeVisible()
      const revision = await activate(admin, 600)
      await expect(admin.getByText(/^Under 2 minutes leaves little time/)).toHaveCount(0)
      const trialCard = trial(admin)
      await expect(trialCard).toContainText('No request has been decided since the trial started')

      // No request yet: the dialog says so and holds the confirmation back
      // until the administrator says they have reviewed the facts.
      await trialCard.getByRole('button', { name: 'Keep Change', exact: true }).click()
      await expect(admin.getByRole('dialog').getByRole('heading', { name: `Keep version ${revision}?` })).toBeVisible()
      const dialog = admin.getByRole('dialog')
      await expect(dialog.getByText(/^Read again at /)).toBeVisible()
      await expect(dialog).toContainText('No request has been decided since the trial started')
      await expect(dialogButton(admin, 'Keep Change')).toBeDisabled()
      await dialog.getByLabel('I have reviewed the current facts above').check()
      await expect(dialogButton(admin, 'Keep Change')).toBeEnabled()
      await shot(admin, '40-confirm-needs-review')
      await dialogButton(admin, 'Cancel').click()

      // Real requests during the trial, counted from its start.
      admitted(await sales('ops-01'))
      admitted(await sales('ops-01'))
      const refused = await sales('cust-001')
      expect(refused.status).toBe(403)
      expect(refused.receipts).toHaveLength(0)
      await expect(trialCard).toContainText('3 decided · 2 allowed · 1 refused')

      // With current figures and nothing changed, no extra step is needed.
      await trialCard.getByRole('button', { name: 'Keep Change', exact: true }).click()
      await expect(admin.getByRole('dialog').getByRole('heading', { name: `Keep version ${revision}?` })).toBeVisible()
      await expect(dialog.getByText(/^Read again at /)).toBeVisible()
      await expect(dialog.getByLabel('I have reviewed the current facts above')).toHaveCount(0)
      await expect(dialogButton(admin, 'Keep Change')).toBeEnabled()

      // Another administrator rolls the trial back while the dialog is open:
      // this page sees it within seconds and the confirmation becomes
      // unavailable. Nothing is sent.
      await resolve(other.page, 'rollback')
      const rolledBackAt = Date.now()
      await expect(dialog).toContainText('No change is on trial any more', { timeout: 10_000 })
      const noticed = Date.now() - rolledBackAt
      expect(noticed).toBeLessThan(6_000)
      await expect(dialogButton(admin, 'Keep Change')).toBeDisabled()
      await shot(admin, '41-confirm-trial-ended')
      await dialogButton(admin, 'Cancel').click()
      expect(confirms).toEqual([])
      expect((await status()).provisional).toBeUndefined()
      const confirmed = (await listAll<{ kind: string; target: string }>('/admin/history')).filter((h) => h.kind === 'cpa.confirm' && h.target === `revision ${revision}`)
      expect(confirmed).toHaveLength(0)
      record('confirm-guard', { revision, noticedMs: noticed, confirmsSent: confirms.length })
    } finally {
      admin.off('request', onRequest)
      await other.context.close()
    }
  })

  test('a session ending during a trial says the gateway still enforces its deadline, and a trial that outlives the session is flagged', async () => {
    const confirmed = (await status()).confirmed
    await propose(admin, { role: 'CUSTOMER', operation: 'reports.summary' })
    const revision = await activate(admin, 86_400)
    // The session's lifetime (8 hours here) ends long before this deadline.
    await expect(admin.getByText(new RegExp(`^Your session ends at .+, before version ${revision}'s trial deadline\\.$`))).toBeVisible()

    // The session ends elsewhere; this page finds out at its next read
    // (every 3 s during a trial) and says what continues without it.
    const tab = await adminContext.newPage()
    await tab.goto(adminURL + '/')
    await expect(tab.getByText('Signed in as admin')).toBeAttached()
    await tab.getByRole('button', { name: 'Sign Out' }).click()
    await expect(tab.getByRole('heading', { name: 'Sign in' })).toBeVisible()
    await tab.close()
    await expect(
      admin.getByText(new RegExp(`Version ${revision} was on trial when this page last read the gateway: unless someone keeps it, the gateway undoes it and returns to version ${confirmed} at `)),
    ).toBeVisible({ timeout: 15_000 })
    await shot(admin, '45-session-ended-during-trial')
    // Signed in again, the trial is still on: the session's end changed
    // nothing at the gateway.
    await signIn(admin, 'admin', password)
    expect((await status()).provisional).toBe(revision)
    await resolve(admin, 'rollback')
  })
})
