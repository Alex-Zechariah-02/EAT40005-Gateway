import { expect, test } from '@playwright/test'
import { committedMessage, explain, message } from '../src/api'

// Every error and refusal code the Admin API can send, as the gateway's
// source produced them on 28 October 2026: the protected Admin API's own
// codes, the controller's refusal reasons, and those reasons in lower case as
// blocked-account and key changes send them.
const adminCodes = [
  'bad_request',
  'busy',
  'candidate_invalid',
  'forbidden',
  'gateway_unavailable',
  'no_confirmed_policy',
  'no_provisional',
  'not_found',
  'payload_too_large',
  'precondition_required',
  'session_limit',
  'stream_limit',
  'too_many_attempts',
  'unauthenticated',
  'unavailable',
  'unsupported_media_type',
]

const controllerReasons = [
  'ALREADY_RESOLVED',
  'AUDIT_UNAVAILABLE',
  'CANDIDATE_INTEGRITY',
  'CANDIDATE_STALE',
  'CANDIDATE_UNAVAILABLE',
  'CREDENTIAL_CHANGED',
  'DEADLINE_PASSED',
  'DUPLICATE_ID',
  'DUPLICATE_MEMBER',
  'DUPLICATE_OPERATION',
  'ENROLMENT_CONSUMED',
  'INVALID_DEADLINE',
  'INVALID_EXPIRY',
  'INVALID_METHOD',
  'INVALID_STATE',
  'INVALID_TARGET',
  'INVALID_UPSTREAM',
  'INVALID_VALUE',
  'KEYS_INVALID',
  'LIMIT_EXCEEDED',
  'NO_PROVISIONAL',
  'NOT_DENY_ALL',
  'NOT_FOUND',
  'OUTCOME_AUDIT_PENDING',
  'OVERRIDE_CONFLICT',
  'OVERRIDE_LIMIT',
  'PRECONDITION_FAILED',
  'PROVISIONAL_ACTIVE',
  'PROVISIONAL_MISMATCH',
  'PUBLICATION_FAILED',
  'SCHEMA_INVALID',
  'SCHEMA_VERSION_UNSUPPORTED',
  'STALE',
  'STORE_ERROR',
  'TARGET_INVALID',
  'TRANSITION_ID_REUSED',
  'UNAVAILABLE',
  'UNKNOWN_FIELD',
  'UNKNOWN_REFERENCE',
  'UNSUPPORTED_ACTION',
  'UNSUPPORTED_CONTROL',
  'UNSUPPORTED_FEATURE',
]

const fallback = message('a_code_no_gateway_sends')

test('every code the Admin API can send has its own plain title and next step, and an unknown code still gets one', () => {
  expect(fallback.action).toContain('no description')
  const codes = [...adminCodes, ...controllerReasons, ...controllerReasons.map((code) => code.toLowerCase())]
  const missing = codes.filter((code) => {
    const words = message(code)
    return (words.title === fallback.title && words.action === fallback.action) || !words.title || !words.action
  })
  expect(missing).toEqual([])
  // A lost answer is never described as "nothing changed".
  for (const code of ['gateway_unavailable', 'STORE_ERROR', 'store_error']) expect(explain(code)).not.toMatch(/nothing (was )?changed/i)
  // A committed change whose answer reports a problem is always titled as applied.
  for (const code of ['OUTCOME_AUDIT_PENDING', 'PUBLICATION_FAILED', 'STORE_ERROR', 'unavailable']) expect(committedMessage(code).title).toMatch(/^Applied, but/)
})
