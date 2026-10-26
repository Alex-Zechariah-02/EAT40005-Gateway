import { spawn, spawnSync, type ChildProcess, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
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

// gatewayIdentity names the gateway source the programs are built from: the
// last commit, exported clean, so uncommitted work in the checkout
// never reaches the programs under test.
export function gatewayIdentity(): { commit: string } {
  return { commit: run('git', ['rev-parse', 'HEAD'], repository()).trim() }
}

// exportHead writes the last commit's files to a new temporary folder with
// git archive and returns it.
function exportHead(): string {
  const source = mkdtempSync(join(tmpdir(), 'dashboard-e2e-src-'))
  const archive = join(source, 'head.tar')
  run('git', ['archive', '--format=tar', '-o', archive, 'HEAD'], repository())
  // A relative archive name: GNU tar reads "C:\..." as a remote host.
  run('tar', ['-xf', 'head.tar'], source)
  return source
}

// build compiles the gateway programs from the last commit into a directory
// outside the repository. The fault-injection gateway runs only inside these
// tests, to show how the dashboard reports a failing security event log; it
// is never deployed.
export function build(): string {
  const source = exportHead()
  const bin = mkdtempSync(join(tmpdir(), 'dashboard-e2e-bin-'))
  const programs = [
    { name: 'gateway', args: ['./cmd/gateway'] },
    { name: 'fixture', args: ['./cmd/fixture'] },
    { name: 'demo', args: ['./cmd/demo'] },
    { name: 'gateway-fault', args: ['-tags', 'faultinject', './cmd/gateway'] },
  ]
  for (const program of programs) run('go', ['build', '-o', join(bin, program.name + exe), ...program.args], source)
  return bin
}

// webRoot lays out the frontend directory the demo tool needs, with this
// repository's build as admin/ and a client build as demo/. The client build
// is WEBSITE_DIST when given, or else a fresh build of the repository's
// client folder.
export function webRoot(): string {
  const dashboard = resolve('dist')
  if (!existsSync(join(dashboard, 'index.html'))) throw new Error('build the dashboard first (npm run build)')
  const root = mkdtempSync(join(tmpdir(), 'dashboard-e2e-web-'))
  cpSync(dashboard, join(root, 'admin'), { recursive: true })
  cpSync(process.env.WEBSITE_DIST ?? buildApplication('client'), join(root, 'demo'), { recursive: true })
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
    const parent = mkdtempSync(join(tmpdir(), 'dashboard-e2e-'))
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

// ShortSessions is a second, separately configured native deployment whose
// administrator sessions end after a few seconds: idle after idleSeconds and
// at the latest after lifetimeSeconds. The demonstration's configuration
// keeps the gateway defaults (15 minutes and 8 hours), which a browser test
// cannot wait for. It reuses the demonstration's token key, serves this
// repository's build and listens on its own addresses.
export class ShortSessions {
  static readonly url = 'http://127.0.0.1:9441'
  readonly dir = mkdtempSync(join(tmpdir(), 'dashboard-e2e-sessions-'))
  private child: ChildProcess | null = null

  constructor(
    private readonly bin: string,
    private readonly demo: Demo,
    readonly idleSeconds: number,
    readonly lifetimeSeconds: number,
  ) {}

  private config(): string {
    return join(this.dir, 'gateway.json')
  }

  // install writes the configuration, creates the database and returns the
  // first administrator's enrolment secret.
  install(): string {
    const demoConfig = JSON.parse(this.demo.file('gateway.json')) as { gateway: { admin_ui: string; jwt: unknown } }
    mkdirSync(this.dir, { recursive: true })
    const config = {
      version: 1,
      gateway: {
        database: join(this.dir, 'state.db'),
        audit_log: join(this.dir, 'audit.log'),
        admin_listen: '127.0.0.1:9441',
        admin_ui: demoConfig.gateway.admin_ui,
        admin_idle_timeout: `${this.idleSeconds}s`,
        admin_session_lifetime: `${this.lifetimeSeconds}s`,
        jwt: demoConfig.gateway.jwt,
      },
      data: {
        listen: '127.0.0.2:9481',
        max_body_bytes: 16384,
        read_header_timeout: '5s',
        read_timeout: '10s',
        write_timeout: '30s',
      },
      upstream: { dial_timeout: '2s', response_header_timeout: '5s', read_idle_timeout: '5s' },
    }
    writeFileSync(this.config(), JSON.stringify(config, null, 2))
    const result = spawnSync(join(this.bin, 'gateway' + exe), ['install', '--config', this.config(), '--admin', 'admin'], { encoding: 'utf8' })
    if (result.status !== 0) throw new Error(`install failed: ${result.stderr}`)
    return enrolmentSecret(result.stdout)
  }

  async start(): Promise<void> {
    this.child = spawn(join(this.bin, 'gateway' + exe), ['run', '--config', this.config()], { stdio: 'ignore' })
    const deadline = Date.now() + 30_000
    for (;;) {
      try {
        if ((await raw('GET', ShortSessions.url + '/admin/health')).status === 200) return
      } catch {
        // not listening yet
      }
      if (this.child.exitCode !== null) throw new Error('the short-session gateway exited')
      if (Date.now() > deadline) throw new Error('the short-session gateway did not start')
      await new Promise((r) => setTimeout(r, 100))
    }
  }

  // stop ends the process. This deployment holds nothing beyond its own
  // test database, so it is not stopped gracefully.
  async stop(): Promise<void> {
    const child = this.child
    if (!child || child.exitCode !== null) return
    this.child = null
    const exited = new Promise((r) => child.once('exit', r))
    child.kill()
    await exited
  }
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
