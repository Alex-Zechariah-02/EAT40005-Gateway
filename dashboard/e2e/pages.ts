import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Locator, type Page } from '@playwright/test'

// UI steps shared by the tests. They use only what a person sees and
// operates: roles, labels and visible text.

function escape(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

// moreDetail is the exact value a notice shows under More details for one
// label, such as 'Error code', 'HTTP status' or 'Field'. The value is in the
// page even while More details is collapsed.
export function moreDetail(notice: Locator, label: string): Locator {
  return notice.locator('.more-details dt', { hasText: new RegExp(`^${escape(label)}$`) }).locator('xpath=following-sibling::dd[1]/code')
}

// nav finds a view's link in the sidebar. A link may carry a flag after its
// name: "On Trial", or a count of temporary rules.
export function nav(page: Page, name: string): Locator {
  return page
    .getByRole('navigation', { name: 'Administration' })
    .getByRole('link', { name: new RegExp(`^${escape(name)}(?: (?:On Trial|\\d+(?: active)?))?$`) })
}

export async function go(page: Page, name: string) {
  await nav(page, name).click()
  await expect(page.getByRole('heading', { level: 1, name, exact: true })).toBeVisible()
  await expect(page.getByText(/^Reading/)).toHaveCount(0)
}

// revisions opens Versions: the active policy in detail and every version.
export async function revisions(page: Page) {
  await go(page, 'Versions')
}

// versionsPart opens one of the Versions page's tabs.
export async function versionsPart(page: Page, name: 'Roles' | 'Endpoints' | 'Service Addresses' | 'Version History') {
  await page.getByRole('tablist', { name: 'Versions sections' }).getByRole('tab', { name, exact: true }).click()
}

export function dialogButton(page: Page, name: string | RegExp): Locator {
  return page.getByRole('dialog').getByRole('button', typeof name === 'string' ? { name, exact: true } : { name })
}

export async function enrol(page: Page, name: string, secret: string, password: string) {
  await page.getByRole('button', { name: 'First Sign-In with an Enrolment Secret' }).click()
  await page.getByLabel('Administrator Name').fill(name)
  await page.getByLabel('Enrolment Secret').fill(secret)
  await page.getByLabel('New Password', { exact: true }).fill(password)
  await page.getByLabel('Repeat the New Password').fill(password)
  await page.getByRole('button', { name: 'Enrol', exact: true }).click()
  await expect(page.getByText('Enrolled. Sign in with the new password.')).toBeVisible()
  await page.getByRole('button', { name: 'Go to Sign-In' }).click()
}

export async function signIn(page: Page, name: string, password: string) {
  await page.getByLabel('Administrator Name').fill(name)
  await page.getByLabel('Password', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByText(`Signed in as ${name}`)).toBeAttached()
}

// stage submits a policy document on the Changes view and returns the new
// change's id, with the change open for review.
export async function stage(page: Page, document: { text?: string; file?: string }): Promise<number> {
  await go(page, 'Changes')
  if (document.file) await page.getByLabel('Load a Policy Document from a File (Optional)').setInputFiles(document.file)
  if (document.text) await page.getByLabel('Complete Policy Document (JSON)').fill(document.text)
  await page.getByRole('button', { name: 'Save Draft and Check' }).click()
  const staged = page.getByText(/^The gateway checked it: saved as draft change \d+/)
  await expect(staged).toBeVisible()
  const id = Number((await staged.textContent())!.match(/change (\d+)/)![1])
  await page
    .getByRole('region', { name: 'Save a Draft Change' })
    .getByRole('button', { name: `Review Change ${id}` })
    .click()
  await expect(page.getByRole('heading', { name: `Change ${id}`, exact: true })).toBeVisible()
  return id
}

// trialStep moves the change open for review through its guided steps
// (review, then impact preview) to its trial step, by keyboard.
export async function trialStep(page: Page) {
  const trial = page.getByLabel('Trial Length (Seconds)')
  const next = page.getByRole('main').getByRole('button', { name: /^Next: (Preview Impact|Start Trial)$/ })
  // Wait for whichever step is showing; each press must leave its step.
  for (let step = 0; step < 2; step++) {
    await expect(next.or(trial).first()).toBeVisible()
    if (await trial.isVisible()) break
    const name = (await next.first().textContent())!.trim()
    await next.first().focus()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('main').getByRole('button', { name, exact: true })).toBeHidden()
  }
  await expect(trial).toBeVisible()
}

// activate puts the change open for review on trial and returns the
// version on trial from the receipt.
export async function activate(page: Page, seconds: number): Promise<number> {
  await trialStep(page)
  await page.getByLabel('Trial Length (Seconds)').fill(String(seconds))
  await page.getByRole('main').getByRole('button', { name: 'Put on Trial', exact: true }).click()
  await dialogButton(page, 'Put on Trial').click()
  const done = page.getByText(/^Version \d+ is on trial and active/)
  await expect(done).toBeVisible()
  return Number((await done.textContent())!.match(/Version (\d+)/)![1])
}

// trial is the change on trial at the top of the Changes view.
export function trial(page: Page): Locator {
  return page.getByRole('region', { name: /^Version \d+ is on trial$/ })
}

// confirmTrial opens the confirmation of the change on trial and sends it.
// The dialog first reads the gateway again; when it finds a reason to look
// twice (no request decided yet, an old reading, a change made elsewhere)
// sending needs the review box ticked, which these flows do knowingly. The
// guard itself is tested on its own.
export async function confirmTrial(page: Page): Promise<void> {
  await trial(page)
    .getByRole('button', { name: 'Keep Change', exact: true })
    .click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByText(/^Read again at /)).toBeVisible()
  const reviewed = dialog.getByLabel('I have reviewed the current facts above')
  if (await reviewed.count()) await reviewed.check()
  await dialogButton(page, 'Keep Change').click()
}

export async function resolve(page: Page, action: 'confirm' | 'rollback'): Promise<void> {
  await go(page, 'Changes')
  if (action === 'confirm') {
    await confirmTrial(page)
    await expect(page.getByText(/^Kept: version \d+ is now the active policy\.$/)).toBeVisible()
  } else {
    await trial(page)
      .getByRole('button', { name: 'Undo Change', exact: true })
      .click()
    await dialogButton(page, 'Undo Change').click()
    await expect(page.getByText(/^Undone: version \d+ is active again\.$/)).toBeVisible()
  }
}

export interface OverrideInput {
  action: string
  operation: string
  role?: string
  count?: number
  seconds: number
}

// createOverride adds a temporary rule through the dialog's steps, after the
// gateway's check on the review, and returns its id.
export async function createOverride(page: Page, o: OverrideInput): Promise<number> {
  await go(page, 'Temporary Rules')
  await page.getByRole('main').getByRole('button', { name: 'Add a Temporary Rule', exact: true }).click()
  const wizard = page.getByRole('dialog', { name: 'Add a Temporary Rule' })
  await wizard.getByRole('radio', { name: o.action, exact: true }).check()
  await wizard.getByRole('button', { name: 'Next', exact: true }).click()
  await wizard.getByLabel('Endpoint', { exact: true }).selectOption(o.operation)
  if (o.role) await wizard.getByLabel('Role', { exact: true }).selectOption(o.role)
  if (o.count !== undefined) await wizard.getByLabel('Temporary Request Count per Window').fill(String(o.count))
  await wizard.getByRole('button', { name: 'Next', exact: true }).click()
  await wizard.getByLabel('Lifetime (Seconds)').fill(String(o.seconds))
  await wizard.getByRole('button', { name: 'Next', exact: true }).click()
  await wizard.getByRole('button', { name: 'Check with the Gateway' }).click()
  await expect(wizard.getByText('The gateway would accept it now. Nothing has been created.')).toBeVisible()
  await wizard.getByRole('button', { name: 'Add Rule', exact: true }).click()
  await expect(wizard).toHaveCount(0)
  const done = page.getByText(/^Rule \d+ added\. It ends at /)
  await expect(done).toBeVisible()
  return Number((await done.textContent())!.match(/Rule (\d+)/)![1])
}

export async function revokeOverride(page: Page, id: number) {
  await go(page, 'Temporary Rules')
  await page
    .getByRole('main')
    .getByRole('button', { name: `Revoke rule ${id}` })
    .click()
  await dialogButton(page, `Revoke rule ${id}`).click()
  await expect(page.getByText(`Rule ${id} revoked. It no longer applies.`)).toBeVisible()
}

// record appends one evidence entry when E2E_EVIDENCE names a file. Entries
// hold statuses, receipts and event fields, never tokens or secrets.
export function record(id: string, data: Record<string, unknown>) {
  const file = process.env.E2E_EVIDENCE
  if (!file) return
  appendFileSync(file, JSON.stringify({ id, time: new Date().toISOString(), ...data }) + '\n')
}

// shot saves a full-page screenshot for visual review when E2E_SHOTS names a
// directory. Screens show what the browser rendered, never a secret.
export async function shot(page: Page, name: string) {
  const dir = process.env.E2E_SHOTS
  if (!dir) return
  mkdirSync(dir, { recursive: true })
  // From the top, so the sticky sidebar and header are drawn where they
  // belong, and with animations finished.
  await page.evaluate(() => window.scrollTo(0, 0))
  const size = await page.evaluate(() => ({ width: innerWidth, height: document.documentElement.scrollHeight, step: innerHeight }))
  if (size.height > 16_000) {
    // Chrome repeats bitmap bands above its compositor limit. Capture actual
    // viewports instead; the manifest also permits lossless external stitching.
    const parts: { file: string; y: number }[] = []
    for (let top = 0; top < size.height; top += size.step) {
      const y = await page.evaluate(async (offset) => {
        window.scrollTo(0, offset)
        await new Promise(requestAnimationFrame)
        await new Promise(requestAnimationFrame)
        return scrollY
      }, top)
      const file = `${name}--${String(y).padStart(6, '0')}.png`
      await page.screenshot({ path: join(dir, file), fullPage: false, animations: 'disabled' })
      parts.push({ file, y })
    }
    writeFileSync(join(dir, `${name}.capture.json`), JSON.stringify({ method: 'viewport-segments', ...size, parts }, null, 2))
    await page.evaluate(() => window.scrollTo(0, 0))
    return
  }
  await page.screenshot({ path: join(dir, `${name}.png`), fullPage: true, animations: 'disabled' })
}

// The dashboard's only browser storage: the Designer's box positions (finite
// numbers only) and, when the administrator chose them, the appearance and
// the time zone (a zone name only). Report
// locations of leaks without putting
// a credential into an assertion failure or the test output.
export async function expectLayoutOnlyStorage(page: Page, secrets: string[] = []) {
  const stored = await page.evaluate(() => ({
    sessionCount: sessionStorage.length,
    entries: Object.entries(localStorage),
  }))
  const problems: string[] = []
  if (stored.sessionCount !== 0) problems.push('sessionStorage is not empty')
  for (const [key, text] of stored.entries) {
    if (secrets.some((secret) => secret && text.includes(secret))) problems.push('secret in storage')
    if (key === 'gateway.theme') {
      if (text !== 'light' && text !== 'dark') problems.push('invalid appearance value')
      continue
    }
    if (key === 'gateway.timezone') {
      try {
        if (text.length > 64) throw new RangeError('long')
        new Intl.DateTimeFormat('en-US', { timeZone: text })
      } catch {
        problems.push('invalid time zone value')
      }
      continue
    }
    if (key !== 'eat40005.policy-designer.layout.v1') {
      problems.push('unexpected localStorage key')
      continue
    }
    try {
      const value = JSON.parse(text)
      if (
        Object.keys(value).sort().join(',') !== 'positions,version' ||
        value.version !== 1 ||
        !value.positions ||
        Array.isArray(value.positions) ||
        typeof value.positions !== 'object'
      ) {
        problems.push('invalid layout envelope')
        continue
      }
      for (const [box, point] of Object.entries(value.positions)) {
        if (!/^(role|operation|api|upstream):/.test(box)) problems.push('invalid box key')
        if (!point || typeof point !== 'object' || Array.isArray(point) || Object.keys(point).sort().join(',') !== 'x,y') {
          problems.push('invalid point shape')
          continue
        }
        const position = point as { x: unknown; y: unknown }
        if (
          typeof position.x !== 'number' ||
          typeof position.y !== 'number' ||
          !Number.isFinite(position.x) ||
          !Number.isFinite(position.y)
        )
          problems.push('non-finite position')
      }
    } catch {
      problems.push('invalid layout JSON')
    }
  }
  expect(problems).toEqual([])
}
