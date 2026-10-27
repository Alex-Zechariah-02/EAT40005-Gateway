import { spawn, spawnSync, type ChildProcess } from 'node:child_process'
import { cpSync, existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { request as httpRequest } from 'node:http'
import { join, resolve } from 'node:path'
import { buildApplication, temporaryDir, httpOrigin, offsetPort } from '../../e2e/backend'
import { Demo as SharedDemo, enrolmentSecret } from '../../e2e/demo'
export type { Receipt } from '../../e2e/demo'
export { adminURL, build, controls, gatewayIdentity, siteURL } from '../../e2e/backend'

export const applicationDist = resolve(process.env.E2E_APP_DIST ?? 'dist')

// The end-to-end tests drive the real programs as an operator would: the
// gateway repository's demo tool sets up and runs the gateway, the Fetch
// services and Fetch Accounts, issues test tokens and resets the services. The
// tests read the services' own receipts and the gateway's audit events as evidence
// independent of what the browser shows.

const exe = process.platform === 'win32' ? '.exe' : ''

// webRoot lays out the frontend directory the demo tool needs, with this
// repository's build as admin/ and a client build as demo/. The client build
// is WEBSITE_DIST when given, or else a fresh build of the repository's
// client folder.
export function webRoot(): string {
  const dashboard = applicationDist
  if (!existsSync(join(dashboard, 'index.html'))) throw new Error('build the dashboard first (npm run build)')
  const root = temporaryDir('dashboard-e2e-web-')
  cpSync(dashboard, join(root, 'admin'), { recursive: true })
  cpSync(process.env.WEBSITE_DIST ?? buildApplication('client'), join(root, 'demo'), { recursive: true })
  return root
}

export class Demo extends SharedDemo {
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
}

// ShortSessions is a second, separately configured native deployment whose
// administrator sessions end after a few seconds: idle after idleSeconds and
// at the latest after lifetimeSeconds. The demonstration's configuration
// keeps the gateway defaults (15 minutes and 8 hours), which a browser test
// cannot wait for. It reuses the demonstration's token key, serves this
// repository's build and listens on its own addresses.
export class ShortSessions {
  static readonly url = httpOrigin('127.0.0.1', 9441)
  readonly dir = temporaryDir('dashboard-e2e-sessions-')
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
        admin_listen: `127.0.0.1:${offsetPort(9441)}`,
        // Beside the demonstration's own metrics listener (9450).
        metrics_listen: `127.0.0.1:${offsetPort(9451)}`,
        admin_ui: demoConfig.gateway.admin_ui,
        admin_idle_timeout: `${this.idleSeconds}s`,
        admin_session_lifetime: `${this.lifetimeSeconds}s`,
        jwt: demoConfig.gateway.jwt,
      },
      data: {
        listen: `127.0.0.2:${offsetPort(9481)}`,
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
