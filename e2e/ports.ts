// Shared by development configuration and real-backend browser helpers only.
export function offsetPort(base: number): number {
  const value = process.env.EAT_PORT_OFFSET ?? ''
  if (value && !/^\d+$/.test(value)) throw new Error('EAT_PORT_OFFSET must be a nonnegative decimal integer')
  const port = base + Number(value)
  if (!Number.isSafeInteger(port) || port > 65535) throw new Error('EAT_PORT_OFFSET exceeds the available port range')
  return port
}

export function httpOrigin(host: string, base: number): string {
  return `http://${host}:${offsetPort(base)}`
}

export const adminURL = httpOrigin('127.0.0.1', 9440)
export const siteURL = httpOrigin('127.0.0.2', 9480)
export const controls: Record<string, string> = Object.fromEntries(
  ['inventory', 'orders', 'reports', 'support', 'accounts'].map((name, i) => [name, httpOrigin('127.0.0.1', 9601 + i)]),
)

export function developmentPort(defaultPort: number): number {
  const value = process.env.E2E_DEV_PORT
  if (value === undefined) return defaultPort
  const port = Number(value)
  if (!/^\d+$/.test(value) || !Number.isInteger(port) || port < 1 || port > 65535) throw new Error('E2E_DEV_PORT must be a valid port')
  return port
}
