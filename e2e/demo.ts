import { spawn, spawnSync, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { controls, temporaryDir } from './backend'

const exe = process.platform === 'win32' ? '.exe' : ''

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
    const parent = temporaryDir('demo-e2e-')
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
      if (child.exitCode !== null || child.signalCode !== null) throw new Error(`demo run exited: ${this.output}`)
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
    if (child.exitCode !== null || child.signalCode !== null) return
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

export function enrolmentSecret(output: string): string {
  const line = output.split('\n').find((l) => l.startsWith('enrolment secret for '))
  const secret = line?.slice(line.lastIndexOf(': ') + 2).trim()
  if (!secret) throw new Error('no enrolment secret in the command output')
  return secret
}

