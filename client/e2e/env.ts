import { spawn, spawnSync, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { cpSync, existsSync, mkdtempSync, readFileSync } from 'node:fs'
import { request as httpRequest } from 'node:http'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

// The end-to-end tests drive the real programs as an operator would: the
// gateway repository's demo tool sets up and runs the gateway and its three
// fixture APIs, issues test tokens and resets the fixtures. The tests read the
// fixtures' own receipts and the gateway's audit events as evidence
// independent of what the browser shows.

export const adminURL = 'http://127.0.0.1:9440'
export const siteURL = 'http://127.0.0.2:9480'
export const controls: Record<string, string> = {
  orders: 'http://127.0.0.1:9601',
  inventory: 'http://127.0.0.1:9602',
  reports: 'http://127.0.0.1:9603',
}

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

// gatewayIdentity names the gateway source the programs are built from: its
// commit and whether the worktree held changes.
export function gatewayIdentity(): { commit: string; clean: boolean } {
  const commit = run('git', ['rev-parse', 'HEAD'], repository()).trim()
  const clean = run('git', ['status', '--porcelain', '--untracked-files=no'], repository()).trim() === ''
  return { commit, clean }
}

// build compiles the gateway programs into a directory outside both
// repositories. The fault-injection gateway runs only inside these tests, to
// show how the client reports a gateway that cannot record events; it is
// never deployed.
export function build(): string {
  const bin = mkdtempSync(join(tmpdir(), 'client-e2e-bin-'))
  const programs = [
    { name: 'gateway', args: ['./cmd/gateway'] },
    { name: 'fixture', args: ['./cmd/fixture'] },
    { name: 'demo', args: ['./cmd/demo'] },
    { name: 'gateway-fault', args: ['-tags', 'faultinject', './cmd/gateway'] },
  ]
  for (const program of programs) run('go', ['build', '-o', join(bin, program.name + exe), ...program.args], repository())
  return bin
}

// webRoot lays out the frontend directory the demo tool needs, with this
// repository's build as demo/ and a dashboard build as admin/. The dashboard
// build is DASHBOARD_DIST when given, or else a fresh build of the
// repository's dashboard folder.
export function webRoot(): string {
  const client = resolve('dist')
  if (!existsSync(join(client, 'index.html'))) throw new Error('build the client first (npm run build)')
  const root = mkdtempSync(join(tmpdir(), 'client-e2e-web-'))
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
    const parent = mkdtempSync(join(tmpdir(), 'client-e2e-'))
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
      ['run', '-dir', this.dir, '-gateway', join(this.bin, gateway + exe), '-fixture', join(this.bin, 'fixture' + exe), '-stdin'],
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

  // enrol adds an administrator with the operator command, which needs the
  // gateway stopped, and returns the one-time secret.
  async enrol(name: string, permissions: string): Promise<string> {
    await this.stop()
    const result = spawnSync(
      join(this.bin, 'gateway' + exe),
      ['enrol', '--config', join(this.dir, 'gateway.json'), '--admin', name, '--permissions', permissions],
      { encoding: 'utf8' },
    )
    if (result.status !== 0) throw new Error(`enrol failed: ${result.stderr}`)
    await this.start()
    return enrolmentSecret(result.stdout)
  }

  token(subject: string, ttl = '15m'): string {
    const result = this.tool('token', '-dir', this.dir, '-subject', subject, '-ttl', ttl)
    if (result.status !== 0) throw new Error(`token failed: ${result.stderr}`)
    return result.stdout.trim()
  }

  reset(): string {
    const result = this.tool('reset', '-dir', this.dir)
    if (result.status !== 0) throw new Error(`reset failed: ${result.stdout}${result.stderr}`)
    return result.stdout
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
      headers: { Authorization: `Bearer ${this.file('fixture.secret').trim()}` },
    })
    if (!response.ok) throw new Error(`fixture ${service} ${path}: ${response.status}`)
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
// fetch does not allow.
export function raw(
  method: string,
  url: string,
  headers: Record<string, string> = {},
  body?: string,
): Promise<{ status: number; headers: Record<string, string | string[] | undefined>; body: string }> {
  return new Promise((done, fail) => {
    const req = httpRequest(url, { method, headers }, (res) => {
      let text = ''
      res.setEncoding('utf8')
      res.on('data', (chunk: string) => (text += chunk))
      res.on('end', () => done({ status: res.statusCode ?? 0, headers: res.headers, body: text }))
    })
    req.on('error', fail)
    if (body !== undefined) req.write(body)
    req.end()
  })
}

// AdminApi is controlled test tooling that acts as an administrator through
// the gateway's Admin API, so the client tests can establish and change the
// policy the client meets. It speaks the API's protocol as a browser would:
// the session cookie, the admin Origin and the session's CSRF token on every
// change. The client itself never calls the Admin API.
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

  async createOverride(override: Record<string, unknown>): Promise<number> {
    const current = await this.expect('GET', '/admin/overrides', 200)
    const reply = await this.expect('POST', '/admin/overrides', 201, JSON.stringify(override), {
      'If-Match': current.etag!,
      'Idempotency-Key': crypto.randomUUID(),
    })
    return reply.body.override
  }

  async revokeOverride(id: number) {
    await this.expect('DELETE', `/admin/overrides/${id}`, 200, undefined, { 'Idempotency-Key': crypto.randomUUID() })
  }

  async override(id: number): Promise<{ status: string; expires_at: string }> {
    return (await this.expect('GET', `/admin/overrides/${id}`, 200)).body
  }
}
