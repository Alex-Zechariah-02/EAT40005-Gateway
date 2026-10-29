import { useEffect, useId, useState, type FormEvent } from 'react'
import type { Admin, SessionInfo } from './api'
import { useMutation } from './hooks'
import { describedBy, Field, fieldError, MutationResult, PageHeader, Time } from './ui'

// The administrator permissions in plain words; the exact names stay beside them.
const permissionWords: Record<string, string> = {
  read: 'View the gateway',
  read_history: 'View the history',
  manage_subjects: 'Block and unblock accounts',
  manage_policy: 'Change the policy',
}

// Account shows the signed-in administrator's permissions and session, and
// changes their password. A password change ends all of their sessions.
export function Account(props: { admin: Admin; session: SessionInfo; signedOut: (message: string) => void; showGuide: () => void }) {
  const id = useId()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [repeat, setRepeat] = useState('')
  const mutation = useMutation<unknown>(props.admin, () => undefined)
  const lastReply = mutation.phase.state === 'replied' ? mutation.phase.reply : undefined
  const bytes = new TextEncoder().encode(next).length
  const lengthError = next !== '' && (bytes < 15 || bytes > 256) ? 'Use 15 to 256 bytes.' : fieldError(lastReply, 'new_password')
  const repeatError = repeat !== '' && repeat !== next ? 'The two passwords differ.' : undefined
  // The client has already confirmed the session is live, so a 401 here can
  // only mean the current password was wrong.
  const currentError = lastReply?.kind === 'refused' && lastReply.status === 401 ? 'The current password is not correct.' : undefined

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (lengthError || repeatError) return
    await mutation.run({ method: 'PUT', path: '/admin/password', body: { current_password: current, new_password: next } })
    setCurrent('')
    setNext('')
    setRepeat('')
  }

  const { phase } = mutation
  const { signedOut } = props
  useEffect(() => {
    if (phase.state === 'replied' && phase.reply.kind === 'ok') {
      signedOut('Password changed. Every session of this administrator has ended; sign in with the new password.')
    }
  }, [phase, signedOut])

  return (
    <>
      <PageHeader title="My Account" lede="Your session, when it expires, and your password." />
      <section className="section" aria-labelledby={`${id}-session`}>
        <h2 id={`${id}-session`}>Session</h2>
        <dl className="facts">
          <div className="fact">
            <dt>Administrator</dt>
            <dd>{props.session.name}</dd>
          </div>
          <div className="fact">
            <dt>Permissions</dt>
            <dd>
              <ul className="permission-list">
                {props.session.permissions.map((permission) => (
                  <li key={permission}>
                    {permissionWords[permission] ?? permission} <code className="muted">{permission}</code>
                  </li>
                ))}
              </ul>
            </dd>
          </div>
          <div className="fact">
            <dt>Gateway Address</dt>
            <dd>
              <code>{window.location.host}</code>
            </dd>
          </div>
          <div className="fact">
            <dt>Session Ends at the Latest</dt>
            <dd>
              <Time value={props.session.expires_at} />
            </dd>
          </div>
        </dl>
        <p className="hint">Sessions also end after an idle timeout set by the operator.</p>
        <button type="button" className="btn btn-small" onClick={props.showGuide}>
          Show the Getting Started Guide
        </button>
      </section>
      <section className="section panel narrow" aria-labelledby={`${id}-password`}>
        <h2 id={`${id}-password`}>Change Password</h2>
        <p className="hint form-intro">Changing the password ends every session of this administrator, including this one.</p>
        <form onSubmit={submit}>
          {/* Names the account the passwords belong to, for password managers. */}
          <input type="text" name="username" autoComplete="username" value={props.session.name} readOnly hidden />
          <Field id={`${id}-current`} label="Current Password" error={currentError}>
            <input
              id={`${id}-current`}
              className="input"
              type="password"
              autoComplete="current-password"
              required
              aria-invalid={currentError ? true : undefined}
              aria-describedby={describedBy(`${id}-current`, undefined, currentError)}
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
            />
          </Field>
          <Field id={`${id}-next`} label="New Password" hint="15 to 256 bytes." error={lengthError}>
            <input
              id={`${id}-next`}
              className="input"
              type="password"
              autoComplete="new-password"
              required
              value={next}
              aria-invalid={lengthError ? true : undefined}
              aria-describedby={describedBy(`${id}-next`, 'hint', lengthError)}
              onChange={(e) => setNext(e.target.value)}
            />
          </Field>
          <Field id={`${id}-repeat`} label="Repeat the New Password" error={repeatError}>
            <input
              id={`${id}-repeat`}
              className="input"
              type="password"
              autoComplete="new-password"
              required
              value={repeat}
              aria-invalid={repeatError ? true : undefined}
              aria-describedby={describedBy(`${id}-repeat`, undefined, repeatError)}
              onChange={(e) => setRepeat(e.target.value)}
            />
          </Field>
          <div className="actions">
            <button type="submit" className="btn btn-primary" disabled={mutation.pending}>
              Change Password
            </button>
          </div>
        </form>
        {/* A wrong current password is shown at its field. After an
            unanswered change, rereading the session shows whether it ended:
            a changed password ends every session. */}
        {currentError ? null : (
          <MutationResult<unknown> mutation={mutation} onRefresh={() => props.admin.get('/admin/session')} success={() => <p className="notice-title">Password changed.</p>} />
        )}
      </section>
    </>
  )
}
