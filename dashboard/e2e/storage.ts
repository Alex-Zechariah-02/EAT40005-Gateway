import { expect, type Page } from '@playwright/test'

// The browser storage check shared by the tests: approved keys and values only, no secret.

// The dashboard's only browser storage: the Designer's box positions (finite
// numbers only) and, when the administrator chose them, the appearance, the
// time zone (a zone name only) and the icon-strip sidebar ("rail" only).
// Report locations of leaks without putting a credential into an assertion
// failure or the test output.
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
    if (key === 'gateway.sidebar') {
      if (text !== 'rail') problems.push('invalid sidebar value')
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
