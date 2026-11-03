import { expect, type Page } from '@playwright/test'
import { raw, siteURL } from './env'
import { adminCall, token } from './setup'

// Steps shared by the request drawer's browser tests.

export interface RequestRecord {
  request_id: string
  time: string
  method: string
  path: string
  operation: string | null
  subject: string | null
  client_address: string
  decision: string
  status: number
  generation: string
}

// full is a time in the full form: 24-hour, day first, with its zone.
export const full = /^\d{1,2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4}, \d{2}:\d{2}:\d{2} UTC[+-]\d{2}:\d{2}$/

// send makes one data-plane request, signed in as subject when given, and
// returns the gateway's own record of it.
export async function send(path: string, subject?: string): Promise<RequestRecord> {
  const reply = await raw('GET', siteURL + path, subject ? { Authorization: `Bearer ${token(subject)}` } : {})
  const ref = String(reply.headers['x-request-id'])
  const [item] = (await adminCall('GET', `/admin/events?ref=${ref}`)).body.items as RequestRecord[]
  expect(item.status).toBe(reply.status)
  return item
}

export const drawer = (page: Page) => page.getByRole('dialog')

// drawerFact is the value the drawer shows beside one label.
export function drawerFact(page: Page, label: string) {
  return drawer(page).locator('dt', { hasText: new RegExp(`^${label}$`) }).locator('xpath=following-sibling::dd[1]')
}

// openLink is a request row's link to its drawer.
export function openLink(page: Page, ref: string) {
  return page.getByRole('main').getByRole('link', { name: `Open request ${ref.slice(0, 8)}`, exact: true })
}
