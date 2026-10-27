import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, type Locator, type Page } from '@playwright/test'

// UI steps shared by the tests. They use only what a person sees and
// operates: roles, labels and visible text.

function escape(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

// nav finds a view's link in the sidebar. A link may carry a flag after its
// name: "On trial", or a count of temporary rules.
export function nav(page: Page, name: string): Locator {
  return page
    .getByRole('navigation', { name: 'Administration' })
    .getByRole('link', { name: new RegExp(`^${escape(name)}(?: (?:On trial|\\d+(?: active)?))?$`) })
}

export async function go(page: Page, name: string) {
  await nav(page, name).click()
  await expect(page.getByRole('heading', { level: 1, name, exact: true })).toBeVisible()
  await expect(page.getByText(/^Reading/)).toHaveCount(0)
}

// revisions opens Policy revisions, which the Policy page links to.
export async function revisions(page: Page) {
  await go(page, 'Policy')
  await page.getByRole('link', { name: 'Revisions and tables' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Policy revisions', exact: true })).toBeVisible()
  await expect(page.getByText(/^Reading/)).toHaveCount(0)
}

export function dialogButton(page: Page, name: string | RegExp): Locator {
  return page.getByRole('dialog').getByRole('button', typeof name === 'string' ? { name, exact: true } : { name })
}

export async function enrol(page: Page, name: string, secret: string, password: string) {
  await page.getByRole('button', { name: 'First sign-in with an enrolment secret' }).click()
  await page.getByLabel('Administrator name').fill(name)
  await page.getByLabel('Enrolment secret').fill(secret)
  await page.getByLabel('New password', { exact: true }).fill(password)
  await page.getByLabel('Repeat the new password').fill(password)
  await page.getByRole('button', { name: 'Enrol', exact: true }).click()
  await expect(page.getByText('Enrolled. Sign in with the new password.')).toBeVisible()
  await page.getByRole('button', { name: 'Go to sign-in' }).click()
}

export async function signIn(page: Page, name: string, password: string) {
  await page.getByLabel('Administrator name').fill(name)
  await page.getByLabel('Password', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByText(`Signed in as ${name}`)).toBeAttached()
}

// stage submits a policy document on the Changes view and returns the new
// change's id, with the change open for review.
export async function stage(page: Page, document: { text?: string; file?: string }): Promise<number> {
  await go(page, 'Changes')
  if (document.file) await page.getByLabel('Load a policy document from a file (optional)').setInputFiles(document.file)
  if (document.text) await page.getByLabel('Complete policy document (JSON)').fill(document.text)
  await page.getByRole('button', { name: 'Stage and check' }).click()
  const staged = page.getByText(/^The gateway checked it: staged as change \d+/)
  await expect(staged).toBeVisible()
  const id = Number((await staged.textContent())!.match(/change (\d+)/)![1])
  await page
    .getByRole('region', { name: 'Stage a change' })
    .getByRole('button', { name: `Review change ${id}` })
    .click()
  await expect(page.getByRole('heading', { name: `Change ${id}`, exact: true })).toBeVisible()
  return id
}

// activate puts the change open for review on trial and returns the
// provisional revision from the receipt.
export async function activate(page: Page, seconds: number): Promise<number> {
  await page.getByLabel('Trial length (seconds)').fill(String(seconds))
  await page.getByRole('main').getByRole('button', { name: 'Put on trial', exact: true }).click()
  await dialogButton(page, 'Put on trial').click()
  const done = page.getByText(/^Revision \d+ is on trial and in force/)
  await expect(done).toBeVisible()
  return Number((await done.textContent())!.match(/Revision (\d+)/)![1])
}

// trial is the change on trial at the top of the Changes view.
export function trial(page: Page): Locator {
  return page.getByRole('region', { name: /is in force but not confirmed$/ })
}

// confirmTrial opens the confirmation of the change on trial and sends it.
// The dialog first reads the gateway again; when it finds a reason to look
// twice (no request decided yet, an old reading, a change made elsewhere)
// sending needs the review box ticked, which these flows do knowingly. The
// guard itself is tested on its own.
export async function confirmTrial(page: Page): Promise<void> {
  await trial(page)
    .getByRole('button', { name: /^Confirm revision \d+$/ })
    .click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByText(/^Read again at /)).toBeVisible()
  const reviewed = dialog.getByLabel('I have reviewed the current facts above')
  if (await reviewed.count()) await reviewed.check()
  await dialogButton(page, /^Confirm revision \d+$/).click()
}

export async function resolve(page: Page, action: 'confirm' | 'rollback'): Promise<void> {
  await go(page, 'Changes')
  if (action === 'confirm') {
    await confirmTrial(page)
    await expect(page.getByText(/^Confirmed: revision \d+ is now the permanent policy\.$/)).toBeVisible()
  } else {
    await trial(page)
      .getByRole('button', { name: /^Roll back to revision \d+$/ })
      .click()
    await dialogButton(page, /^Roll back to revision \d+$/).click()
    await expect(page.getByText(/^Rolled back: revision \d+ is in force again\.$/)).toBeVisible()
  }
}

export interface OverrideInput {
  action: string
  operation: string
  role?: string
  count?: number
  seconds: number
}

// createOverride adds a temporary rule after the gateway's check and returns
// its id.
export async function createOverride(page: Page, o: OverrideInput): Promise<number> {
  await go(page, 'Temporary rules')
  await page.getByLabel('Action', { exact: true }).selectOption({ label: o.action })
  await page.getByLabel('Operation', { exact: true }).selectOption(o.operation)
  if (o.role) await page.getByLabel('Role', { exact: true }).selectOption(o.role)
  if (o.count !== undefined) await page.getByLabel('Temporary request count per window').fill(String(o.count))
  await page.getByLabel('Lifetime (seconds)').fill(String(o.seconds))
  await page.getByRole('button', { name: 'Check with the gateway' }).click()
  await expect(page.getByText('The gateway would accept it now. Nothing has been created.')).toBeVisible()
  await page.getByRole('main').getByRole('button', { name: 'Add rule', exact: true }).click()
  await dialogButton(page, 'Add rule').click()
  const done = page.getByText(/^Rule \d+ is in force on/)
  await expect(done).toBeVisible()
  return Number((await done.textContent())!.match(/Rule (\d+)/)![1])
}

export async function revokeOverride(page: Page, id: number) {
  await go(page, 'Temporary rules')
  await page
    .getByRole('main')
    .getByRole('button', { name: `Revoke rule ${id}` })
    .click()
  await dialogButton(page, `Revoke rule ${id}`).click()
  await expect(page.getByText(`Rule ${id} revoked (revoked)`)).toBeVisible()
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
// numbers only) and, when the administrator chose one, the appearance. Report
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
