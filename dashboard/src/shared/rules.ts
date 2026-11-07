// The policy's per-API and per-endpoint rules that the Dashboard describes
// in words: conditions, forwarding, browser access, response headers and
// client address lists. The gateway alone applies them.

// When lists the host and header conditions that select an endpoint among
// those sharing its method and path. Conditions never grant access.
export interface When {
  hosts?: string[]
  headers?: { name: string; value: string }[]
}

// whenRule describes an endpoint's conditions in words, or null when it has
// none. The gateway alone decides which endpoint a request selects.
export function whenRule(when: When | undefined): string | null {
  const hosts = when?.hosts?.length ? `host ${when.hosts.join(' or ')}` : ''
  const headers = (when?.headers ?? []).map((h) => `header ${h.name}: ${h.value}`)
  const text = [hosts, ...headers].filter(Boolean).join(' and ')
  return text ? `Only for ${text}` : null
}

// Forward is an API's path rewrite: the service receives the request path
// without strip_prefix, after add_prefix. Decisions use the request path.
export interface Forward {
  strip_prefix?: string
  add_prefix?: string
}

// forwardRule describes a forward rule in words, or null when the path is
// forwarded unchanged. The gateway alone applies the rule.
export function forwardRule(forward: Forward | undefined): string | null {
  const parts = [forward?.strip_prefix ? `removes ${forward.strip_prefix}` : '', forward?.add_prefix ? `adds ${forward.add_prefix} in front` : '']
  const text = parts.filter(Boolean).join(', then ')
  return text ? text[0].toUpperCase() + text.slice(1) : null
}

// Cors is an API's browser access: the gateway answers browsers'
// cross-origin preflights and adds the allowed origin to its answers.
export interface Cors {
  origins: string[]
  methods: string[]
  headers?: string[]
  expose?: string[]
  credentials?: boolean
  max_age_seconds?: number
}

// ResponseHeaders adds security headers the service did not set, and
// removes the named service headers.
export interface ResponseHeaders {
  hsts?: boolean
  nosniff?: boolean
  frame?: 'deny' | 'sameorigin'
  referrer?: 'no-referrer' | 'strict-origin-when-cross-origin'
  remove?: string[]
}

export interface ApiEntry {
  id: string
  upstream: string
  forward?: Forward
  cors?: Cors
  response_headers?: ResponseHeaders
  allow_addresses?: string[]
}

// corsRule describes an API's browser access in words, or null without one.
export function corsRule(cors: Cors | undefined): string | null {
  if (!cors) return null
  const from = cors.origins.includes('*') ? 'any origin' : cors.origins.join(', ')
  return `From ${from} for ${cors.methods.join(', ')}${cors.credentials ? ', with cookies' : ''}`
}

// addressRule describes an API's allow list in words, or null without one.
export function addressRule(ranges: string[] | undefined): string | null {
  return ranges?.length ? `Only from ${ranges.join(', ')}` : null
}
