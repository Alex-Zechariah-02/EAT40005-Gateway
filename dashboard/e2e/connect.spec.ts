import { expect, test, type Locator } from '@playwright/test'
import type { Policy } from '../src/shared/api'
import { raw, siteURL } from './env'
import { activate, dialogButton, moreDetail, resolve, shot } from './pages'
import { api, candidates, currentPolicy, demo, designer, discard, expandAll, formItem, node, page, open, ready, finish } from './designer'

// Connect an API: a stepped drawer that only edits the draft, then the usual
// save, the gateway's own check, a trial and keeping.

const flow = () => page.getByRole('dialog', { name: 'Connect an API' })
const next = () => flow().getByRole('button', { name: 'Next', exact: true })
const row = (n: number) => flow().getByRole('group', { name: `Endpoint ${n}`, exact: true })
const schema = '{"type": "object", "properties": {"note": {"type": "string", "maxLength": 200}}, "required": ["note"], "additionalProperties": false}'

async function step(title: string, plain: string, technical: RegExp) {
  await expect(flow().locator('h3[tabindex="-1"]')).toHaveText(new RegExp(`^Step \\d of 8: ${title}$`))
  await expect(flow().locator('.wizard-plain')).toHaveText(plain)
  await expect(flow().locator('.wizard-technical')).toHaveText(technical)
}

// fieldError checks that a field is marked invalid and names what to fix beside it.
async function fieldError(field: Locator, text: string) {
  await expect(field).toHaveAttribute('aria-invalid', 'true')
  const described = (await field.getAttribute('aria-describedby')) ?? ''
  const error = page.locator(described.split(' ').map((id) => `[id="${id}"].field-error`).join(', '))
  await expect(error).toHaveText(text)
}

async function draftJson(): Promise<Policy> {
  await page.getByRole('tab', { name: 'JSON', exact: true }).click()
  const text = await page.getByLabel('Complete Policy Document (JSON)', { exact: true }).inputValue()
  await page.getByRole('tab', { name: 'Canvas', exact: true }).click()
  return JSON.parse(text)
}

// connect walks the flow for the analytics API on the existing reports service.
async function connect(body: string | null) {
  await page.getByRole('button', { name: 'Connect an API', exact: true }).click()
  await flow().getByLabel('Service Address', { exact: true }).selectOption('reports-svc')
  await next().click()
  await flow().getByLabel('API ID', { exact: true }).fill('analytics')
  await next().click()
  await row(1).getByLabel('Path', { exact: true }).fill('/reports/daily')
  await flow().getByRole('button', { name: 'Add Another Endpoint', exact: true }).click()
  await row(2).getByLabel('Method', { exact: true }).selectOption('POST')
  await row(2).getByLabel('Path', { exact: true }).fill('/reports/notes')
  await next().click()
  for (const id of ['analytics.daily', 'analytics.notes'])
    await flow().getByRole('group', { name: new RegExp(`^${id.replace('.', '\\.')}`) }).getByLabel('OPERATIONS', { exact: true }).check()
  await next().click()
  await flow().getByRole('group', { name: /^analytics\.notes/ }).getByLabel('Requests', { exact: true }).fill('20')
  await next().click()
  await next().click() // Browser Access: none
  if (body === null) await flow().getByRole('button', { name: 'Skip This Step', exact: true }).click()
  else {
    const notes = flow().getByRole('group', { name: /^analytics\.notes/ })
    await notes.getByLabel("Check This Endpoint's Request Body", { exact: true }).check()
    await notes.getByLabel('JSON Schema', { exact: true }).fill(body)
    await next().click()
  }
  await step('Review', 'Exactly what the flow adds to the draft, and nothing else.', /^Adds 0 to upstreams\[\], 1 to apis\[\], 2 to operations\[\] and 2 to permits\[\]\.$/)
}

async function send(method: string, path: string, subject: string, body?: unknown) {
  const since = (await demo.receipts('reports')).length
  const headers: Record<string, string> = { Authorization: `Bearer ${demo.token(subject)}` }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const reply = await raw(method, siteURL + path, headers, body === undefined ? undefined : JSON.stringify(body))
  return { status: reply.status, receipts: (await demo.receipts('reports')).slice(since) }
}

test.describe.serial('Connect an API on the native Gateway', () => {
  test.beforeAll(async ({ browser }) => {
    await open(browser)
    await ready()
  })

  test.afterAll(finish)

  test('each step says what it decides and what it writes, and names input errors beside the field', async () => {
    await page.getByRole('button', { name: 'Connect an API', exact: true }).click()
    await expect(flow()).toBeVisible()
    await step('Service Address', "Where the gateway forwards this API's requests: the address of the service behind it.", /^Writes upstreams\[\]: \{"id": "", "dial": ""\}$/)
    await next().click()
    await fieldError(flow().getByLabel('Service Address ID', { exact: true }), 'Enter an ID.')
    await fieldError(flow().getByLabel('Address (Host and Port)', { exact: true }), 'Enter the address as host:port, for example 127.0.0.1:9600.')
    await expect(flow().getByLabel('Service Address ID', { exact: true })).toBeFocused()
    await flow().getByLabel('Service Address ID', { exact: true }).fill('reports-svc')
    await fieldError(flow().getByLabel('Service Address ID', { exact: true }), 'This draft already has a service address with this ID.')
    await flow().getByLabel('Service Address ID', { exact: true }).fill('.billing')
    await fieldError(flow().getByLabel('Service Address ID', { exact: true }), 'Use letters, digits, dots, hyphens and underscores, starting with a letter or digit.')
    await flow().getByLabel('Service Address ID', { exact: true }).fill('billing-svc')
    await flow().getByLabel('Address (Host and Port)', { exact: true }).fill('127.0.0.1:9690')
    await step('Service Address', "Where the gateway forwards this API's requests: the address of the service behind it.", /^Writes upstreams\[\]: \{"id": "billing-svc", "dial": "127\.0\.0\.1:9690"\}$/)
    await next().click()

    await step('API', 'A name that groups the service’s endpoints on the Policy and APIs pages.', /^Writes apis\[\]: \{"id": "", "upstream": "billing-svc"\}$/)
    await flow().getByLabel('API ID', { exact: true }).fill('orders')
    await next().click()
    await fieldError(flow().getByLabel('API ID', { exact: true }), 'This draft already has an API with this ID.')
    await flow().getByLabel('API ID', { exact: true }).fill('billing')
    await next().click()

    await step('Endpoints', 'The requests this API answers: each one’s method and path, and whether callers must sign in.', /^Writes operations\[\]: one entry per endpoint, with "access": "allow" and "auth": "required" or "none"\.$/)
    await row(1).getByLabel('Path', { exact: true }).fill('billing/invoices')
    await next().click()
    await fieldError(row(1).getByLabel('Path', { exact: true }), 'Enter a path starting with /.')
    // The suggested ID follows the path's last fixed part, never a wildcard.
    await row(1).getByLabel('Path', { exact: true }).fill('/billing/invoices/*')
    await expect(row(1).getByLabel('Endpoint ID', { exact: true })).toHaveValue('billing.invoices')
    await row(1).getByLabel('Path', { exact: true }).fill('/billing/invoices')
    await expect(row(1).getByLabel('Endpoint ID', { exact: true })).toHaveValue('billing.invoices')
    await flow().getByRole('button', { name: 'Add Another Endpoint', exact: true }).click()
    await row(2).getByLabel('Path', { exact: true }).fill('/billing/invoices')
    await row(2).getByLabel('Method', { exact: true }).selectOption('POST')
    await next().click()
    await fieldError(row(2).getByLabel('Endpoint ID', { exact: true }), 'Another endpoint in this flow has this ID.')
    await row(2).getByLabel('Endpoint ID', { exact: true }).fill('orders.create')
    await fieldError(row(2).getByLabel('Endpoint ID', { exact: true }), 'This draft already has an endpoint with this ID.')
    await row(2).getByLabel('Endpoint ID', { exact: true }).fill('billing.create')
    await row(2).getByLabel('Who May Call It', { exact: true }).selectOption('none')
    await next().click()

    await step('Roles', 'Which roles may call each endpoint that needs a sign-in. A role comes from the caller’s sign-in token.', /^Writes permits\[\]: \{"role", "operation"\}, one entry per role ticked\.$/)
    await expect(flow().getByText('billing.create is public: anyone may call it, so it takes no permission.')).toBeVisible()
    await next().click()

    await step('Rate', 'How many requests one account, or one client address for a public endpoint, may send in a time window.', /^Writes operations\[\]\.rate: \{"algorithm": "sliding-window", "count", "window_seconds", "key"\}\.$/)
    await flow().getByRole('group', { name: /^billing\.invoices/ }).getByLabel('Requests', { exact: true }).fill('0')
    await next().click()
    await fieldError(flow().getByRole('group', { name: /^billing\.invoices/ }).getByLabel('Requests', { exact: true }), 'Enter a whole number of at least 1.')
    await flow().getByRole('group', { name: /^billing\.invoices/ }).getByLabel('Requests', { exact: true }).fill('30')
    await next().click()

    await step('Browser Access', 'Optional: web pages on other sites that may call this API from a browser. Every other check still applies.', /^Writes apis\[\]\.cors: \{"origins", "methods", "headers"\}/)
    await next().click()

    await step('Validate Request Bodies', 'Optional: the gateway refuses a write request whose body does not match its schema, before the service sees it.', /^Writes operations\[\]\.body: \{"max_bytes", "schema"\}\. Only the gateway checks the schema, when the draft is saved\.$/)
    // Only write endpoints are offered: the GET endpoint has no body to check.
    await expect(flow().getByRole('group', { name: /^billing\.invoices/ })).toHaveCount(0)
    const create = flow().getByRole('group', { name: /^billing\.create/ })
    await create.getByLabel("Check This Endpoint's Request Body", { exact: true }).check()
    await create.getByLabel('JSON Schema', { exact: true }).fill('{"type": "object",')
    await next().click()
    await fieldError(create.getByLabel('JSON Schema', { exact: true }), 'Enter well-formed JSON: one object, such as {"type": "object"}.')
    await shot(page, 'connect-errors')

    // Closing with unfinished input asks first; Cancel keeps the input.
    await page.keyboard.press('Escape')
    await expect(dialogButton(page, 'Discard Input')).toBeVisible()
    await page.getByRole('dialog', { name: 'Discard This Input?' }).getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(create.getByLabel('JSON Schema', { exact: true })).toHaveValue('{"type": "object",')
    await flow().getByRole('button', { name: 'Close', exact: true }).click()
    await dialogButton(page, 'Discard Input').click()
    await expect(flow()).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Connect an API', exact: true })).toBeFocused()
    await expect(page.locator('.designer-draft-state')).toHaveText(/^No changes to version \d+$/)
  })

  test('Validate Request Bodies can be skipped, and Cancel on Review leaves the draft unchanged', async () => {
    const before = await draftJson()
    await connect(null)
    // Review lists exactly what will be added: the API, both endpoints and both permissions.
    await expect(flow().locator('.change-set li')).toHaveText([
      'API analytics added',
      'Endpoint analytics.daily added',
      'Endpoint analytics.notes added',
      'OPERATIONS can now call analytics.daily',
      'OPERATIONS can now call analytics.notes',
    ])
    await flow().getByRole('button', { name: 'Cancel', exact: true }).click()
    await dialogButton(page, 'Discard Input').click()
    expect(await draftJson()).toEqual(before)

    await connect(null)
    await flow().getByRole('button', { name: 'Add to the Draft', exact: true }).click()
    const added = await draftJson()
    expect(added.operations.find((operation) => operation.id === 'analytics.notes')!.body).toBeUndefined()
    await discard()
  })

  test('Finish changes only the draft: every view shows it, and the gateway has no new draft change, trial or version', async () => {
    const version = (await currentPolicy()).id
    const saved = (await candidates()).length
    await connect(schema)
    await shot(page, 'connect-review')
    await flow().getByRole('button', { name: 'Add to the Draft', exact: true }).click()
    await expect(flow()).toHaveCount(0)
    await expect(page.getByText(/^Applied to the draft: API analytics connected with 2 endpoints\./)).toBeVisible()

    await expandAll()
    await expect(node('api:analytics')).toBeVisible()
    await expect(node('operation:analytics.daily')).toBeVisible()
    await page.getByRole('tab', { name: 'Form', exact: true }).click()
    await formItem('Endpoint analytics.notes')
    await expect(page.getByRole('region', { name: 'Endpoint analytics.notes', exact: true }).getByLabel('Request count', { exact: true })).toHaveValue('20')
    const document = await draftJson()
    expect(document.apis).toContainEqual({ id: 'analytics', upstream: 'reports-svc' })
    expect(document.operations.find((operation) => operation.id === 'analytics.daily')).toEqual({
      id: 'analytics.daily',
      api: 'analytics',
      method: 'GET',
      target: '/reports/daily',
      access: 'allow',
      auth: 'required',
      rate: { algorithm: 'sliding-window', count: 100, window_seconds: 60, key: 'subject' },
    })
    expect(document.operations.find((operation) => operation.id === 'analytics.notes')!.body).toEqual({ max_bytes: 4096, schema: JSON.parse(schema) })
    expect(document.permits).toEqual(expect.arrayContaining([{ role: 'OPERATIONS', operation: 'analytics.daily' }, { role: 'OPERATIONS', operation: 'analytics.notes' }]))

    // Nothing reached the gateway: no draft change, no trial, the same version.
    expect((await candidates()).length).toBe(saved)
    expect((await api('GET', '/admin/provisional')).status).toBe(404)
    expect((await currentPolicy()).id).toBe(version)
    expect((await currentPolicy()).document.apis.some((item) => item.id === 'analytics')).toBe(false)
  })

  test('saving makes the gateway compile it: a refused schema names its field, and a kept one decides requests', async () => {
    // A schema whose root is not an object or array is refused by the compiler, not the browser.
    const document = await draftJson()
    const index = document.operations.findIndex((operation) => operation.id === 'analytics.notes')
    document.operations[index].body!.schema = { type: 'string' }
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    await page.getByLabel('Complete Policy Document (JSON)', { exact: true }).fill(JSON.stringify(document, null, 2))
    await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
    const saved = (await candidates()).length
    await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
    await dialogButton(page, 'Save Draft Change').click()
    const refusal = page.getByRole('alert').filter({ has: page.locator('.more-details dd code', { hasText: /^400$/ }) })
    await expect(refusal).toHaveClass(/tone-refused/)
    await expect(moreDetail(refusal, 'Field')).toHaveText(`operations[${index}].body.schema.type`)
    await expect(moreDetail(refusal, 'Error code')).toHaveText('INVALID_VALUE')
    await shot(page, 'connect-refused')
    expect((await candidates()).length).toBe(saved)

    document.operations[index].body!.schema = JSON.parse(schema)
    await page.getByRole('tab', { name: 'JSON', exact: true }).click()
    await page.getByLabel('Complete Policy Document (JSON)', { exact: true }).fill(JSON.stringify(document, null, 2))
    await page.getByRole('button', { name: 'Apply JSON', exact: true }).click()
    await page.getByRole('button', { name: 'Save Changes', exact: true }).click()
    await dialogButton(page, 'Save Draft Change').click()
    await page.getByRole('button', { name: 'Preview Its Impact and Put It on Trial', exact: true }).click()
    await activate(page, 300)
    await resolve(page, 'confirm')
    expect((await currentPolicy()).document.apis).toContainEqual({ id: 'analytics', upstream: 'reports-svc' })

    // The gateway forwards OPERATIONS to the reports service and refuses a customer before it.
    const allowed = await send('GET', '/reports/daily', 'ops-01')
    expect(allowed.receipts).toHaveLength(1)
    expect(allowed.receipts[0].target).toBe('/reports/daily')
    expect(allowed.status).toBe(allowed.receipts[0].status)
    const refused = await send('GET', '/reports/daily', 'cust-001')
    expect(refused.status).toBe(403)
    expect(refused.receipts).toHaveLength(0)
    // The body rule is the gateway's: a body outside the schema never reaches the service.
    const valid = await send('POST', '/reports/notes', 'ops-01', { note: 'daily close' })
    expect(valid.receipts).toHaveLength(1)
    const invalid = await send('POST', '/reports/notes', 'ops-01', { note: 'daily close', extra: true })
    expect(invalid.status).toBe(400)
    expect(invalid.receipts).toHaveLength(0)
    await designer()
  })
})
