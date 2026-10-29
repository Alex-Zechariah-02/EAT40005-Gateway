import { cpSync, existsSync } from 'node:fs'
import { request as httpRequest } from 'node:http'
import { join, resolve } from 'node:path'
import { adminURL, buildApplication, temporaryDir } from '../../e2e/backend'
import { Demo } from '../../e2e/demo'
export { Demo, type Receipt } from '../../e2e/demo'
export { adminURL, build, controls, gatewayIdentity, siteURL } from '../../e2e/backend'

export const applicationDist = resolve(process.env.E2E_APP_DIST ?? 'dist')

// The end-to-end tests drive the real programs as an operator would: the
// repository's demo tool sets up and runs the gateway and the services behind
// it, issues test tokens and reads the gateway's events. The tests read the
// services' own receipts and the gateway's records as evidence independent of
// what the browser shows.

// The data plane's content policy for the app's page and every asset.
export const uiPolicy =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// The app's page routes; the gateway serves index.html for each.
export const pageRoutes = [
  '/',
  '/food',
  '/mart',
  '/store/MER-001',
  '/store/warung-nasi-lemak',
  '/item/ITM-001',
  '/cart',
  '/checkout',
  '/my-orders',
  '/my-orders/ORD-1001',
  '/help',
  '/help/TKT-1001',
  '/signin',
  '/merchant',
  '/rider',
  '/support',
  '/ops',
  '/lab',
]

// webRoot lays out the frontend directory the demo tool needs, with this
// application's build as demo/ and a dashboard build as admin/. The dashboard
// build is DASHBOARD_DIST when given, or else a fresh build of the
// repository's dashboard folder.
export function webRoot(): string {
  const client = applicationDist
  if (!existsSync(join(client, 'index.html'))) throw new Error('build the app first (npm run build)')
  const root = temporaryDir('client-e2e-web-')
  cpSync(client, join(root, 'demo'), { recursive: true })
  cpSync(process.env.DASHBOARD_DIST ?? buildApplication('dashboard'), join(root, 'admin'), { recursive: true })
  return root
}

// raw sends one HTTP request exactly as given, including a Host header, which
// fetch does not allow. It returns the body as text and as the exact bytes.
export function raw(
  method: string,
  url: string,
  headers: Record<string, string> = {},
  body?: string,
): Promise<{ status: number; headers: Record<string, string | string[] | undefined>; body: string; bytes: Buffer }> {
  return new Promise((done, fail) => {
    const req = httpRequest(url, { method, headers }, (res) => {
      const chunks: Buffer[] = []
      res.on('data', (chunk: Buffer) => chunks.push(chunk))
      res.on('end', () => {
        const bytes = Buffer.concat(chunks)
        done({ status: res.statusCode ?? 0, headers: res.headers, body: bytes.toString('utf8'), bytes })
      })
    })
    req.on('error', fail)
    if (body !== undefined) req.write(body)
    req.end()
  })
}

// AdminApi is controlled test tooling that acts as an administrator through
// the gateway's Admin API, so the app's tests can establish and change the
// policy the app meets. It speaks the API's protocol as a browser would: the
// session cookie, the admin Origin and the session's CSRF token on every
// change. The app itself never calls the Admin API.
export class AdminApi {
  private cookie = ''
  private csrf = ''

  private async exchange(method: string, path: string, body?: string, headers: Record<string, string> = {}) {
    const all: Record<string, string> = { Origin: adminURL, ...headers }
    if (this.cookie) all.Cookie = this.cookie
    if (this.csrf && method !== 'GET') all['X-CSRF-Token'] = this.csrf
    if (body !== undefined) all['Content-Type'] = 'application/json'
    const reply = await raw(method, adminURL + path, all, body)
    const etag = reply.headers['etag']
    return { status: reply.status, etag: typeof etag === 'string' ? etag : undefined, body: reply.body ? JSON.parse(reply.body) : undefined }
  }

  private async expect(method: string, path: string, wanted: number, body?: string, headers: Record<string, string> = {}) {
    const reply = await this.exchange(method, path, body, headers)
    if (reply.status !== wanted) throw new Error(`${method} ${path}: ${reply.status} ${JSON.stringify(reply.body)}`)
    return reply
  }

  async enrol(name: string, secret: string, password: string) {
    await this.expect('POST', '/admin/enrol', 201, JSON.stringify({ name, secret, password }))
  }

  async signIn(name: string, password: string) {
    const reply = await raw('POST', adminURL + '/admin/session', { Origin: adminURL, 'Content-Type': 'application/json' }, JSON.stringify({ name, password }))
    if (reply.status !== 201) throw new Error(`sign-in: ${reply.status}`)
    const cookie = [reply.headers['set-cookie'] ?? []].flat().find((c) => c.startsWith('__Host-gateway-admin='))
    if (!cookie) throw new Error('sign-in set no session cookie')
    this.cookie = cookie.split(';')[0]
    this.csrf = (JSON.parse(reply.body) as { csrf_token: string }).csrf_token
  }

  get(path: string) {
    return this.exchange('GET', path)
  }

  async status(): Promise<{ state: string; admission: string; confirmed?: number; provisional?: number; revision?: number; generation?: string; overrides?: number[] }> {
    return (await this.expect('GET', '/admin/status', 200)).body
  }

  // stage stores a complete policy document as a candidate against the
  // confirmed revision and returns it.
  async stage(policy: string): Promise<{ id: number; checksum: string }> {
    const confirmed = await this.exchange('GET', '/admin/policy')
    const ifMatch = confirmed.status === 200 ? confirmed.etag! : '"r0"'
    const reply = await this.expect('POST', '/admin/candidates', 201, `{"policy":${policy}}`, { 'If-Match': ifMatch, 'Idempotency-Key': crypto.randomUUID() })
    return reply.body.candidate
  }

  async genesis(candidate: { id: number; checksum: string }) {
    await this.expect('POST', '/admin/genesis', 200, JSON.stringify({ candidate: candidate.id, checksum: candidate.checksum }))
  }

  // activate makes a staged candidate the provisional base and returns its
  // revision.
  async activate(candidate: { id: number; checksum: string }, seconds: number): Promise<number> {
    const confirmed = await this.expect('GET', '/admin/policy', 200)
    const reply = await this.expect(
      'POST',
      '/admin/provisional',
      200,
      JSON.stringify({ candidate: candidate.id, checksum: candidate.checksum, confirm_within_seconds: seconds }),
      { 'If-Match': confirmed.etag!, 'Idempotency-Key': crypto.randomUUID() },
    )
    return reply.body.provisional
  }

  async resolve(action: 'confirm' | 'rollback') {
    const current = await this.expect('GET', '/admin/provisional', 200)
    await this.expect('POST', `/admin/provisional/${action}`, 200, JSON.stringify({ revision: current.body.activation.revision }), {
      'If-Match': current.etag!,
      'Idempotency-Key': crypto.randomUUID(),
    })
  }

  // block puts an account on the gateway's blocked list; unblock takes it off.
  async block(subject: string, reason: string) {
    await this.expect('PUT', `/admin/blocked-accounts/${encodeURIComponent(subject)}`, 200, JSON.stringify({ reason }), { 'Idempotency-Key': crypto.randomUUID() })
  }

  async unblock(subject: string) {
    await this.expect('DELETE', `/admin/blocked-accounts/${encodeURIComponent(subject)}`, 200, undefined, { 'Idempotency-Key': crypto.randomUUID() })
  }

  // baseline establishes the demonstration's confirmed policy: deny-all as the
  // first policy, then the baseline through a confirmed trial.
  async baseline(demo: Demo) {
    await this.genesis(await this.stage(demo.file('deny-all.json')))
    await this.activate(await this.stage(demo.file('baseline.json')), 600)
    await this.resolve('confirm')
  }
}
