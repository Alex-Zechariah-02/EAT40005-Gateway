import { answeredBy, outcomeOfWrite, type Answer } from './api'

// Plain words for every answer a person can meet: the problem first, then
// what to do next. Who answered (the gateway or a named service) and the
// reference go underneath, so a problem can be traced to its request.

export interface Problem {
  title: string
  detail: string | null
  // by names who answered: "gateway", a service name, or null when nothing did.
  by: string | null
  ref: string | null
  status: number | null
  code: string | null
  // unknown is true when a change may or may not have been applied.
  unknown: boolean
}

// A write describes itself so an unknown outcome can be worded exactly:
// "We didn't get an answer about your order. It may or may not have been placed."
export interface Write {
  object: string
  done: string
  check: string
}

const gateway: Record<string, [string, string]> = {
  bad_request: ['Fetch couldn’t accept this request.', 'Some details weren’t in the form Fetch expects. Check them and try again.'],
  unsupported_media_type: ['Fetch couldn’t accept this request.', 'It wasn’t sent in a form Fetch understands.'],
  payload_too_large: ['This is too long to send.', 'Shorten it and try again.'],
  unauthenticated: ['You need to sign in.', 'Sign in, then try again.'],
  forbidden: ['Your account can’t do this right now.', 'Fetch isn’t allowing it for your account at the moment. If you think it should, tell Fetch support.'],
  account_blocked: ['This account is blocked.', 'Fetch has paused this account. Contact Fetch if you think this is a mistake.'],
  not_found: ['This isn’t available.', 'It may have moved. Go back and try another way.'],
  method_not_allowed: ['This action isn’t available here.', 'Go back and try another way.'],
  rate_limited: ['Too many requests in a short time.', 'Wait a minute, then try again.'],
  upstream_unavailable: ['That part of Fetch isn’t answering right now.', 'Try again in a moment.'],
  gateway_unavailable: ['Fetch can’t take requests right now.', 'Try again shortly.'],
  upstream_timeout: ['That part of Fetch took too long to answer.', 'Try again in a moment.'],
}

const service: Record<string, string> = {
  out_of_stock: 'Something in your basket has run out.',
  insufficient_stock: 'There isn’t that much stock to remove.',
  item_unavailable: 'An item is no longer available.',
  mixed_merchants: 'An order can only come from one shop.',
  merchant_closed: 'This shop is closed right now.',
  insufficient_wallet: 'Your Fetch wallet doesn’t have enough.',
  invalid_transition: 'This has already moved on.',
  already_assigned: 'Another rider has already taken this job.',
  idempotency_conflict: 'This order was already sent with different details.',
  invalid_credentials: 'That email and password don’t match a Fetch account.',
  invalid_change: 'There’s nothing to change.',
  invalid_query: 'Fetch couldn’t search for that.',
  inventory_unavailable: 'Fetch couldn’t confirm Inventory.',
  not_found: 'We couldn’t find this.',
}

export function describe(answer: Answer<unknown>, write?: Write): Problem | null {
  if (answer.kind === 'ok' || answer.kind === 'cancelled') return null
  const base = { by: answeredBy(answer), ref: 'ref' in answer ? answer.ref : null, status: 'status' in answer ? answer.status : null, code: 'code' in answer ? answer.code : null }

  if (write && outcomeOfWrite(answer) === 'unknown') {
    return {
      ...base,
      title: answer.kind === 'no-answer' ? `No answer about ${write.object}.` : `Fetch couldn’t confirm ${write.object}.`,
      detail: `It may or may not have been ${write.done}. ${write.check} before trying again.`,
      unknown: true,
    }
  }

  if (answer.kind === 'no-answer') {
    return {
      ...base,
      title: answer.reason === 'timeout' ? 'Fetch took too long to answer.' : 'No answer from Fetch.',
      detail: 'Check your connection and try again.',
      unknown: false,
    }
  }

  // A service states its own reason in a plain sentence.
  if (answer.service) {
    const title = service[answer.code] ?? 'Fetch couldn’t do this.'
    const detail = answer.message && answer.message !== title ? answer.message : null
    return { ...base, title, detail, unknown: false }
  }

  const [title, detail] = gateway[answer.code] ?? [`Fetch answered with an error (${answer.status}).`, 'Try again in a moment.']
  return { ...base, title, detail, unknown: false }
}
