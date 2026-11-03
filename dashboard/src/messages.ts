// Plain words for the gateway's error and refusal codes.

// Message is how the Dashboard words a server code: a plain title saying
// what happened, and what to do next. The code itself is shown beside it
// under More details, exactly as sent.
export interface Message {
  title: string
  action: string
}

// messages word the server's error and refusal codes. A code is looked up as
// sent, in capitals and in lower case, because blocked-account and key
// changes send the controller's reasons in lower case while history records
// them in capitals.
export const messages: Record<string, Message> = {
  bad_request: { title: 'The request was not accepted', action: 'A value is missing or not allowed. Check what you entered, then try again.' },
  gateway_unavailable: {
    title: 'The gateway could not finish this',
    action: 'For a change, this does not show whether it was applied. Check its outcome in History before sending anything else.',
  },
  cursor_gone: {
    title: 'The security log file this search stopped in has been archived',
    action: 'Start the search again from the newest records.',
  },
  candidate_invalid: {
    title: 'This draft change cannot be read as a complete policy',
    action: 'Its impact cannot be previewed. Save a new draft from the current policy.',
  },
  unauthenticated: { title: 'Not signed in', action: 'Your session has ended, or the name or password is wrong. Sign in again.' },
  forbidden: { title: 'Your account cannot do this', action: 'Your administrator account does not have the permission for this action.' },
  not_found: { title: 'Not found', action: 'It does not exist, or it was removed. Refresh to see the current list.' },
  http_404: { title: 'This gateway does not offer that', action: 'The gateway has no such address. It may run a version without this feature.' },
  stream_limit: { title: 'Too many live views are open', action: 'At most 8 can be open at once. Close one, for example in another tab, then reload.' },
  keys_invalid: {
    title: 'The signing keys could not be loaded',
    action: 'The gateway kept the keys it had. Check the key files in the operator settings, then reload the keys again.',
  },
  precondition_required: { title: 'The current version is needed', action: 'Reload the page, so the change is based on the active version, then try again.' },
  unsupported_media_type: { title: 'The request must be JSON', action: 'Send the document as JSON.' },
  payload_too_large: { title: 'The request is too large', action: 'Make the document smaller, then try again.' },
  too_many_attempts: { title: 'Too many failed sign-in attempts', action: 'Sign-in is locked for a while. Wait, then try again.' },
  unavailable: { title: 'The gateway cannot do this now', action: 'Try again shortly. If it goes on, open the Overview to see why.' },
  busy: { title: 'The gateway is busy', action: 'It is checking other passwords. Try again in a moment.' },
  session_limit: { title: 'Too many sessions are open', action: 'Sign out of a session you no longer use, then try again.' },
  no_confirmed_policy: { title: 'No policy is active yet', action: 'Put the first active policy from Changes first.' },
  no_provisional: { title: 'No change is on trial', action: 'There is nothing to keep or undo. Refresh to see the current state.' },
  STALE: { title: 'The policy changed while you were editing', action: 'Reload to see the latest version, then make your change again.' },
  PRECONDITION_FAILED: {
    title: 'This change is out of date',
    action: 'Someone updated the policy after you opened it. Reload, check your edits still apply, then try again.',
  },
  PROVISIONAL_ACTIVE: { title: 'A change is already on trial', action: 'Keep or undo the change on trial first (Changes page), then try again.' },
  INVALID_STATE: { title: 'The gateway cannot do this in its current state', action: 'Open the Overview to see what the gateway is doing now.' },
  CANDIDATE_UNAVAILABLE: {
    title: 'This draft can no longer be used',
    action: 'It was withdrawn, already activated, or set aside after a restore. Save a new draft from the current policy.',
  },
  CANDIDATE_STALE: {
    title: 'This draft is based on an older version',
    action: 'The active version changed after it was saved. Save it again from the current policy.',
  },
  CANDIDATE_INTEGRITY: { title: 'This draft change is damaged', action: 'Its stored copy no longer matches its checksum. Save a new draft.' },
  NOT_FOUND: { title: 'Not found', action: 'It does not exist, or it was removed. Refresh to see the current list.' },
  TRANSITION_ID_REUSED: {
    title: 'This request key was already used',
    action: 'It belongs to a different request. Start the action again; the Dashboard then sends a new key.',
  },
  NO_PROVISIONAL: { title: 'No change is on trial', action: 'There is nothing to keep or undo. Refresh to see the current state.' },
  PROVISIONAL_MISMATCH: {
    title: 'A different change is on trial now',
    action: 'Refresh to see which change is on trial before keeping or undoing it.',
  },
  ALREADY_RESOLVED: { title: 'This has already ended', action: 'It was kept, undone or revoked already. Refresh to see the current state.' },
  DEADLINE_PASSED: {
    title: 'The trial deadline has passed',
    action: 'The gateway undoes the change by itself. Refresh to see the active version.',
  },
  TARGET_INVALID: {
    title: 'The earlier version could not be restored',
    action: 'The gateway stopped accepting requests to stay safe. Open the Overview to see how to recover.',
  },
  INVALID_DEADLINE: { title: 'This trial length is not allowed', action: 'Choose 1 to 86,400 seconds (24 hours).' },
  INVALID_EXPIRY: { title: 'This rule length is not allowed', action: 'Choose 1 to 86,400 seconds (24 hours).' },
  OVERRIDE_CONFLICT: {
    title: 'A temporary rule already covers this',
    action: 'Revoke the other temporary rule or wait for it to end, or choose another endpoint or role.',
  },
  OVERRIDE_LIMIT: { title: 'Too many temporary rules', action: 'The most that can be active at once is reached. Revoke one first.' },
  UNSUPPORTED_CONTROL: { title: 'A temporary rule cannot change this', action: 'Make a permanent change instead: save a draft change and put it on trial.' },
  UNSUPPORTED_ACTION: { title: 'This kind of temporary rule is not available here', action: 'Choose another action for this endpoint.' },
  INVALID_VALUE: { title: 'A value is not allowed', action: 'Check the value named under More details, then try again.' },
  UNKNOWN_REFERENCE: {
    title: 'It names something the policy does not have',
    action: 'Check that every role, endpoint, API and service address it refers to exists in the policy.',
  },
  NOT_DENY_ALL: {
    title: 'The first policy must block everything',
    action: 'Start from the policy that denies everything; add permissions after it is active.',
  },
  OUTCOME_AUDIT_PENDING: {
    title: 'Applied, but the security log could not record it',
    action: 'The change is active. The gateway stops accepting requests until it restarts and records it.',
  },
  UNKNOWN_FIELD: { title: 'The document has a field the gateway does not know', action: 'Remove or correct the field named under More details.' },
  DUPLICATE_MEMBER: { title: 'The document repeats a name', action: 'Each name may appear once in an object. Remove the repeat.' },
  DUPLICATE_ID: { title: 'An ID is used twice', action: 'Give each API, endpoint, role and service address its own ID.' },
  SCHEMA_INVALID: {
    title: 'The document is not shaped like a policy',
    action: 'Correct the field named under More details, or start again from the current policy.',
  },
  SCHEMA_VERSION_UNSUPPORTED: { title: 'This policy format version is not supported', action: 'Use schema version 4.' },
  UNSUPPORTED_FEATURE: { title: 'The document uses something the gateway does not support', action: 'Remove the feature named under More details.' },
  LIMIT_EXCEEDED: { title: 'The document is too large', action: 'It exceeds a size or count limit. Make it smaller.' },
  INVALID_UPSTREAM: {
    title: 'A service address is not allowed',
    action: 'Use a local address with a port, such as 127.0.0.1:9502, and give each service its own address.',
  },
  INVALID_METHOD: { title: 'An endpoint method is not allowed', action: 'Use GET, HEAD, POST, PUT, PATCH, DELETE or OPTIONS.' },
  INVALID_TARGET: { title: 'An endpoint path is not valid', action: 'Use an exact path, such as /orders, or a prefix ending in /*.' },
  DUPLICATE_OPERATION: { title: 'Two endpoints have the same method and path', action: 'Remove one, or change its method or path.' },
  UNAVAILABLE: { title: 'The gateway is unavailable', action: 'It changed nothing. Try again shortly.' },
  AUDIT_UNAVAILABLE: {
    title: 'The security log cannot be written',
    action: 'Nothing was changed. The gateway accepts no change until its security log works again.',
  },
  STORE_ERROR: {
    title: 'The gateway could not use its database',
    action: 'It did not complete this request. If you sent the identical request before, check in History whether that one was applied.',
  },
  PUBLICATION_FAILED: {
    title: 'Applied, but not activated',
    action: 'The change was stored but could not be activated, so the gateway stopped accepting requests. Restart the gateway.',
  },
  CREDENTIAL_CHANGED: { title: 'Your password changed during this request', action: 'Sign in again.' },
  ENROLMENT_CONSUMED: { title: 'This enrolment secret was already used', action: 'Ask the operator for a new one.' },
}
