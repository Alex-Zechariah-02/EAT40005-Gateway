import { answeredBy, type Answer } from '../lib/api'
import type { By, Expected } from './attacks'

// The security lab's verdict: how one test's answer compares with the baseline's.

export interface Result {
  answers: Answer<unknown>[]
  expected: Expected
  // unsent is true when the request could not be built, so nothing was sent.
  unsent?: boolean
}

export function observedBy(answer: Answer<unknown>): By | null {
  const by = answeredBy(answer)
  return by === null ? null : by === 'gateway' ? 'gateway' : 'service'
}

// Verdict compares the last answer with the baseline policy's answer.
// Expectations are written for the baseline because this page cannot read the
// policy in force, so a difference is not by itself a fault: a change on
// trial or a temporary rule may decide differently. A blocked account is
// named by the gateway's own answer, so it is reported as that, not as a
// difference.
export type Verdict = 'match' | 'blocked' | 'differs'

export function verdict(result: Result): Verdict | null {
  const last = result.answers[result.answers.length - 1]
  if (!last || !('status' in last)) return null
  if (last.status === result.expected.status && observedBy(last) === result.expected.by) return 'match'
  if (last.kind === 'refused' && last.code === 'account_blocked' && observedBy(last) === 'gateway') return 'blocked'
  return 'differs'
}

export const verdicts: Record<Verdict, { label: string; tone: 'ok' | 'warn' | 'neutral'; note?: string }> = {
  match: { label: 'Matches the baseline', tone: 'ok' },
  blocked: {
    label: 'Account blocked',
    tone: 'neutral',
    note: 'This account is on the blocked list, so the gateway refuses it right after checking the token, before any other check. Unblock it in the Dashboard or switch person to test the other checks.',
  },
  differs: {
    label: 'Differs from the baseline',
    tone: 'warn',
    note: 'The policy in force may not be the baseline: a change on trial or a temporary rule can decide differently. Paste the Ref into the Dashboard’s header search to see which check decided.',
  },
}
