import { spawn, spawnSync, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { cpSync, existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { request as httpRequest } from 'node:http'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

// Folders this test process creates under the system's temporary folder,
// removed when it ends. One that a program still holds open is left to the
// system's own temporary-file cleanup rather than failing the run.
const temporary: string[] = []
process.on('exit', () => {
  for (const dir of temporary) {
    try {
      rmSync(dir, { recursive: true, force: true })
    } catch {
      // Still in use; see above.
    }
  }
})
function temporaryDir(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix))
  temporary.push(dir)
  return dir
}

// The end-to-end tests drive the real programs as an operator would: the
// repository's demo tool sets up and runs the gateway and the services behind
// it, issues test tokens and reads the gateway's events. The tests read the
// services' own receipts and the gateway's records as evidence independent of
// what the browser shows.

export const adminURL = 'http://127.0.0.1:9440'
export const siteURL = 'http://127.0.0.2:9480'
// The Fetch services' and Fetch Accounts' control listeners (receipts, state,
// reset).
export const controls: Record<string, string> = {
  inventory: 'http://127.0.0.1:9601',
  orders: 'http://127.0.0.1:9602',
  reports: 'http://127.0.0.1:9603',
  support: 'http://127.0.0.1:9604',
  accounts: 'http://127.0.0.1:9605',
}

// The data plane's content policy for the app's page and every asset.
export const uiPolicy =
  "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// The app's page routes; the gateway serves index.html for each.
export const pageRoutes = [
  '/',
  '/food',
  '/mart',
  '/store/MER-001',
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

const exe = process.platform === 'win32' ? '.exe' : ''

// The tests run from this application's folder; the repository root holds
// the gateway programs and the other application.
function repository(): string {
  return resolve('..')
}

function run(command: string, args: string[], cwd: string) {
  const result = spawnSync(command, args, { cwd, encoding: 'utf8' })
  if (result.status !== 0) throw new Error(`${command} ${args.join(' ')}: ${result.stderr || result.stdout}`)
  return result.stdout
}

// shell runs one fixed command line through the shell, which npm's scripts
// need on Windows.
function shell(commandLine: string, cwd: string) {
  const result = spawnSync(commandLine, { cwd, encoding: 'utf8', shell: true })
  if (result.status !== 0) throw new Error(`${commandLine}: ${result.stderr || result.stdout}`)
}

// gatewayIdentity names the gateway source the programs are built from: the
// last commit, exported clean, so uncommitted work in the checkout
// never reaches the programs under test.
export function gatewayIdentity(): { commit: string } {
  if (!builtCommit) throw new Error('build() has not run')
  return { commit: builtCommit }
}

// builtCommit is the commit the programs were last built from, resolved once
// so the evidence names exactly what was tested even if a commit lands
// during the run.
let builtCommit = ''

// exportHead writes the given commit's files to a new temporary folder with
// git archive and returns it.
function exportHead(commit: string): string {
  const source = mkdtempSync(join(tmpdir(), 'client-e2e-src-'))
  run('git', ['archive', '--format=tar', '-o', join(source, 'head.tar'), commit], repository())
  // A relative archive name: GNU tar reads "C:\..." as a remote host.
  run('tar', ['-xf', 'head.tar'], source)
  return source
}

// build compiles the gateway programs from the last commit into a directory
// outside the repository. The fault-injection gateway runs only inside these
// tests, to show how the app reports a gateway that cannot record events; it
// is never deployed.
export function build(): string {
  const commit = run('git', ['rev-parse', 'HEAD'], repository()).trim()
  const source = exportHead(commit)
  const bin = temporaryDir('client-e2e-bin-')
  const programs = [
    { name: 'gateway', args: ['./cmd/gateway'] },
    { name: 'services', args: ['./cmd/services'] },
    { name: 'accounts', args: ['./cmd/accounts'] },
    { name: 'demo', args: ['./cmd/demo'] },
    { name: 'gateway-fault', args: ['-tags', 'faultinject', './cmd/gateway'] },
  ]
  for (const program of programs) run('go', ['build', '-o', join(bin, program.name + exe), ...program.args], source)
  rmSync(source, { recursive: true, force: true })
  builtCommit = commit
  return bin
}

// webRoot lays out the frontend directory the demo tool needs, with this
// application's build as demo/ and a dashboard build as admin/. The dashboard
// build is DASHBOARD_DIST when given, or else a fresh build of the
// repository's dashboard folder.
export function webRoot(): string {
  const client = resolve('dist')
  if (!existsSync(join(client, 'index.html'))) throw new Error('build the app first (npm run build)')
  const root = temporaryDir('client-e2e-web-')
  cpSync(client, join(root, 'demo'), { recursive: true })
  cpSync(process.env.DASHBOARD_DIST ?? buildApplication('dashboard'), join(root, 'admin'), { recursive: true })
  return root
}

// buildApplication builds the named application folder of the repository and
// returns its build directory.
function buildApplication(folder: string): string {
  const source = join(repository(), folder)
  if (!existsSync(join(source, 'node_modules'))) shell('npm ci --prefer-offline --no-audit --no-fund', source)
  shell('npm run build', source)
  return join(source, 'dist')
}

export interface Receipt {
  seq: number
  request_id: string
  generation: string
  method: string
  target: string
  operation: string
  status: number
  effect?: string
  subject?: string
  roles?: string
}

export class Demo {
  readonly dir: string
  private process: ChildProcessWithoutNullStreams | null = null
  private output = ''
  pids: Record<string, number> = {}

  constructor(
    readonly bin: string,
    readonly web: string,
  ) {
    const parent = temporaryDir('client-e2e-')
    this.dir = join(parent, 'demo')
  }

  private tool(...args: string[]) {
    return spawnSync(join(this.bin, 'demo' + exe), args, { encoding: 'utf8' })
  }

  // setup creates the demonstration directory and returns the first
  // administrator's enrolment secret, which is never printed by the tests.
  setup(): string {
    const result = this.tool('setup', '-dir', this.dir, '-web', this.web, '-gateway', join(this.bin, 'gateway' + exe))
    if (result.status !== 0) throw new Error(`setup failed: ${result.stderr}`)
    return enrolmentSecret(result.stdout)
  }

  // start runs the demonstration with the normal gateway, or with the
  // fault-injection gateway and one named fault, which the run command's
  // processes inherit through the environment.
  async start(options: { fault?: string } = {}): Promise<void> {
    const gateway = options.fault ? 'gateway-fault' : 'gateway'
    const env = { ...process.env }
    delete env.GATEWAY_FAULT
    if (options.fault) env.GATEWAY_FAULT = options.fault
    const child = spawn(
      join(this.bin, 'demo' + exe),
      ['run', '-dir', this.dir, '-gateway', join(this.bin, gateway + exe), '-services', join(this.bin, 'services' + exe), '-accounts', join(this.bin, 'accounts' + exe), '-stdin'],
      { stdio: 'pipe', env },
    )
    this.process = child
    this.output = ''
    child.stdout.setEncoding('utf8')
    child.stderr.setEncoding('utf8')
    child.stdout.on('data', (chunk: string) => (this.output += chunk))
    child.stderr.on('data', (chunk: string) => (this.output += chunk))
    const deadline = Date.now() + 60_000
    while (!this.output.includes('gateway   process')) {
      if (child.exitCode !== null) throw new Error(`demo run exited: ${this.output}`)
      if (Date.now() > deadline) throw new Error(`demo run not ready: ${this.output}`)
      await new Promise((r) => setTimeout(r, 100))
    }
    this.pids = {}
    for (const match of this.output.matchAll(/^\s+(\w+)\s+process (\d+)$/gm)) {
      this.pids[match[1]] = Number(match[2])
    }
  }

  // stop closes the run command's input, which stops it and every process
  // it started.
  async stop(): Promise<void> {
    const child = this.process
    if (!child) return
    this.process = null
    const exited = new Promise((r) => child.once('exit', r))
    child.stdin.end()
    await exited
  }

  log(): string {
    return this.output
  }

  token(subject: string, ttl = '15m'): string {
    const result = this.tool('token', '-dir', this.dir, '-subject', subject, '-ttl', ttl)
    if (result.status !== 0) throw new Error(`token failed: ${result.stderr}`)
    return result.stdout.trim()
  }

  // events returns the gateway's request events, each as a field map.
  events(requestId?: string): Record<string, string>[] {
    const args = ['events', '-dir', this.dir, ...(requestId ? ['-request', requestId] : ['-last', '1000'])]
    const result = this.tool(...args)
    if (result.status !== 0) throw new Error(`events failed: ${result.stderr}`)
    return result.stdout
      .split('\n')
      .filter((line) => line.trim() !== '')
      .map((line) => Object.fromEntries(line.split(' ').map((field) => field.split(/=(.*)/s).slice(0, 2) as [string, string])))
  }

  file(name: string): string {
    return readFileSync(join(this.dir, name), 'utf8')
  }

  path(name: string): string {
    return join(this.dir, name)
  }

  private async control<T>(service: string, method: string, path: string): Promise<T> {
    const response = await fetch(controls[service] + path, {
      method,
      headers: { Authorization: `Bearer ${this.file('service.secret').trim()}` },
    })
    if (!response.ok) throw new Error(`service ${service} ${path}: ${response.status}`)
    return (await response.json()) as T
  }

  async receipts(service: string): Promise<Receipt[]> {
    return (await this.control<{ receipts: Receipt[] }>(service, 'GET', '/receipts')).receipts
  }

  async state(service: string): Promise<{ service: string; sha256: string; data: unknown }> {
    return this.control(service, 'GET', '/state')
  }

  async counts(): Promise<Record<string, number>> {
    const counts: Record<string, number> = {}
    for (const service of Object.keys(controls)) counts[service] = (await this.receipts(service)).length
    return counts
  }
}

function enrolmentSecret(output: string): string {
  const line = output.split('\n').find((l) => l.startsWith('enrolment secret for '))
  const secret = line?.slice(line.lastIndexOf(': ') + 2).trim()
  if (!secret) throw new Error('no enrolment secret in the command output')
  return secret
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
