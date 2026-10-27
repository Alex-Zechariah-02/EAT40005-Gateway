import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { appendFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
export { adminURL, controls, httpOrigin, offsetPort, siteURL } from './ports'

const repository = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const executable = process.platform === 'win32' ? '.exe' : ''
const temporary: string[] = []

process.on('exit', () => {
  for (const dir of temporary) {
    try {
      rmSync(dir, { recursive: true, force: true })
    } catch {
      // A child may still hold a temporary file; preserve it for inspection.
    }
  }
})

export function temporaryDir(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix))
  temporary.push(dir)
  return dir
}

function run(command: string, args: string[], cwd = repository): string {
  const result = spawnSync(command, args, { cwd, encoding: 'utf8', env: { ...process.env, GIT_OPTIONAL_LOCKS: '0' } })
  if (result.status !== 0) throw new Error(`${command} ${args.join(' ')}: ${result.stderr || result.stdout || result.error}`)
  return result.stdout
}

function digest(data: string | Buffer): string {
  return createHash('sha256').update(data).digest('hex')
}

interface SourceFile {
  path: string
  sha256: string
}

interface BackendIdentity {
  commit: string
  source: 'head' | 'worktree'
  source_sha256: string
  diff_sha256: string
  files: SourceFile[]
}

let builtIdentity: BackendIdentity | undefined

export function gatewayIdentity(): BackendIdentity {
  if (!builtIdentity) throw new Error('build() has not run')
  return builtIdentity
}

// Both modes compile an isolated immutable snapshot. The default retains the
// clean committed-source workflow; worktree mode explicitly includes local
// backend changes and records every input file, never just a commit label.
export function build(): string {
  const mode = process.env.E2E_BACKEND_SOURCE ?? 'head'
  if (mode !== 'head' && mode !== 'worktree') throw new Error('E2E_BACKEND_SOURCE must be head or worktree')
  const commit = run('git', ['--no-optional-locks', 'rev-parse', 'HEAD']).trim()
  const source = temporaryDir('gateway-e2e-src-')
  const bin = temporaryDir('gateway-e2e-bin-')
  const inputs = ['cmd', 'internal', 'go.mod', 'go.sum']
  const diffArgs = ['--no-optional-locks', 'diff', '--no-ext-diff', '--binary', commit, '--', ...inputs]
  const diff = mode === 'worktree' ? run('git', diffArgs) : ''
  const selectedPaths = () => (mode === 'head'
    ? run('git', ['--no-optional-locks', 'ls-tree', '-rz', '--name-only', commit, '--', ...inputs])
    : run('git', ['--no-optional-locks', 'ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', ...inputs]))
    .split('\0').filter((path) => path !== '' && (mode === 'head' || existsSync(join(repository, path)))).sort()
  const paths = selectedPaths()
  if (mode === 'head') {
    run('git', ['--no-optional-locks', 'archive', '--format=tar', '-o', join(source, 'head.tar'), commit, '--', ...inputs])
    run('tar', ['-xf', 'head.tar'], source)
  }
  const files = paths.map((path) => {
    const data = readFileSync(join(mode === 'head' ? source : repository, path))
    if (mode === 'worktree') {
      const target = join(source, path)
      mkdirSync(dirname(target), { recursive: true })
      writeFileSync(target, data)
    }
    return { path, sha256: digest(data) }
  })
  if (mode === 'worktree' && (
    diff !== run('git', diffArgs) ||
    JSON.stringify(paths) !== JSON.stringify(selectedPaths()) ||
    files.some((file) => digest(readFileSync(join(repository, file.path))) !== file.sha256)
  )) throw new Error('Backend source changed during the snapshot; build was refused')
  if (commit !== run('git', ['--no-optional-locks', 'rev-parse', 'HEAD']).trim()) throw new Error('HEAD changed during the snapshot')
  const identity: BackendIdentity = { commit, source: mode, source_sha256: digest(JSON.stringify(files)), diff_sha256: digest(diff), files }
  const programs = [
    { name: 'gateway', args: ['./cmd/gateway'] },
    { name: 'services', args: ['./cmd/services'] },
    { name: 'accounts', args: ['./cmd/accounts'] },
    { name: 'demo', args: ['./cmd/demo'] },
    { name: 'gateway-fault', args: ['-tags', 'faultinject', './cmd/gateway'] },
  ]
  try {
    for (const program of programs) run('go', ['build', '-trimpath', '-o', join(bin, program.name + executable), ...program.args], source)
    writeFileSync(join(bin, 'source.json'), JSON.stringify(identity, null, 2))
    if (process.env.E2E_EVIDENCE) appendFileSync(process.env.E2E_EVIDENCE, JSON.stringify({ kind: 'backend-build', ...identity }) + '\n')
    builtIdentity = identity
    return bin
  } finally {
    rmSync(source, { recursive: true, force: true })
  }
}

export function buildApplication(folder: string): string {
  const source = join(repository, folder)
  const shell = (command: string) => {
    const result = spawnSync(command, { cwd: source, encoding: 'utf8', shell: true })
    if (result.status !== 0) throw new Error(`${command}: ${result.stderr || result.stdout || result.error}`)
  }
  if (!existsSync(join(source, 'node_modules'))) shell('npm ci --prefer-offline --no-audit --no-fund')
  shell('npm run build')
  return join(source, 'dist')
}
