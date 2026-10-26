import { useId, useState, type FormEvent } from 'react'
import { request, type Reply, type SessionInfo } from './api'
import { useResource } from './hooks'
import { describedBy, Field, Problem } from './ui'

// SignIn offers the two ways in: a password sign-in, and the first sign-in of
// an enrolled administrator with the one-time secret the operator was given.
export function SignIn(props: { signedIn: (session: SessionInfo) => void }) {
  const health = useResource<{ state: string }>((signal) => request('GET', '/admin/health', {}, signal), [])
  const [mode, setMode] = useState<'password' | 'enrol'>('password')

  return (
    <section className="entry-card" aria-labelledby="signin-title">
      <h1 id="signin-title">{mode === 'password' ? 'Sign in' : 'First sign-in'}</h1>
      <p className="entry-state">
        Gateway state:{' '}
        {health.reply?.kind === 'ok' ? <code>{health.reply.body.state}</code> : health.loading ? 'reading…' : 'not available'}
      </p>
      <div className="segmented" role="group" aria-label="Sign-in method">
        <button type="button" aria-pressed={mode === 'password'} onClick={() => setMode('password')}>
          Password
        </button>
        <button type="button" aria-pressed={mode === 'enrol'} onClick={() => setMode('enrol')}>
          First sign-in with an enrolment secret
        </button>
      </div>
      {mode === 'password' ? <PasswordForm signedIn={props.signedIn} /> : <EnrolForm enrolled={() => setMode('password')} />}
    </section>
  )
}

function PasswordForm(props: { signedIn: (session: SessionInfo) => void }) {
  const id = useId()
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [pending, setPending] = useState(false)
  const [reply, setReply] = useState<Reply<SessionInfo> | null>(null)

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (pending) return
    setPending(true)
    const result = await request<SessionInfo>('POST', '/admin/session', {
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, password }),
    })
    setPending(false)
    setPassword('')
    setReply(result)
    if (result.kind === 'ok') props.signedIn(result.body)
  }

  return (
    <>
      <form onSubmit={submit}>
        <Field id={`${id}-name`} label="Administrator name">
          <input id={`${id}-name`} className="input" autoComplete="username" spellCheck={false} required value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field id={`${id}-password`} label="Password">
          <input
            id={`${id}-password`}
            className="input"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        <div className="actions">
          <button type="submit" className="btn btn-primary btn-wide" disabled={pending}>
            {pending ? 'Signing in…' : 'Sign in'}
          </button>
        </div>
      </form>
      <div aria-live="polite">{reply && reply.kind !== 'ok' ? <Problem reply={reply} /> : null}</div>
    </>
  )
}

function EnrolForm(props: { enrolled: () => void }) {
  const id = useId()
  const [name, setName] = useState('')
  const [secret, setSecret] = useState('')
  const [password, setPassword] = useState('')
  const [repeat, setRepeat] = useState('')
  const [pending, setPending] = useState(false)
  const [reply, setReply] = useState<Reply<unknown> | null>(null)
  const passwordBytes = new TextEncoder().encode(password).length
  const mismatch = repeat !== '' && repeat !== password ? 'The two passwords differ.' : undefined
  const length = password !== '' && (passwordBytes < 15 || passwordBytes > 256) ? 'Use 15 to 256 bytes.' : undefined

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (pending || mismatch || length) return
    setPending(true)
    const result = await request('POST', '/admin/enrol', {
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, secret, password }),
    })
    setPending(false)
    setSecret('')
    setPassword('')
    setRepeat('')
    setReply(result)
  }

  return (
    <>
      <p className="hint form-intro">Use the one-time enrolment secret the operator received from the gateway's install or enrol command.</p>
      <form onSubmit={submit}>
        <Field id={`${id}-name`} label="Administrator name">
          <input id={`${id}-name`} className="input" autoComplete="username" spellCheck={false} required value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field id={`${id}-secret`} label="Enrolment secret">
          <input id={`${id}-secret`} className="input" type="password" autoComplete="off" required value={secret} onChange={(e) => setSecret(e.target.value)} />
        </Field>
        <Field id={`${id}-password`} label="New password" hint="15 to 256 bytes." error={length}>
          <input
            id={`${id}-password`}
            className="input"
            type="password"
            autoComplete="new-password"
            required
            aria-invalid={length ? true : undefined}
            aria-describedby={describedBy(`${id}-password`, 'hint', length)}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        <Field id={`${id}-repeat`} label="Repeat the new password" error={mismatch}>
          <input
            id={`${id}-repeat`}
            className="input"
            type="password"
            autoComplete="new-password"
            required
            aria-invalid={mismatch ? true : undefined}
            aria-describedby={describedBy(`${id}-repeat`, undefined, mismatch)}
            value={repeat}
            onChange={(e) => setRepeat(e.target.value)}
          />
        </Field>
        <div className="actions">
          <button type="submit" className="btn btn-primary btn-wide" disabled={pending}>
            {pending ? 'Enrolling…' : 'Enrol'}
          </button>
        </div>
      </form>
      <div aria-live="polite">
        {reply?.kind === 'ok' ? (
          <div className="notice tone-ok">
            <p className="notice-title">Enrolled. Sign in with the new password.</p>
            <button type="button" className="btn" onClick={props.enrolled}>
              Go to sign-in
            </button>
          </div>
        ) : reply ? (
          <Problem reply={reply} />
        ) : null}
      </div>
    </>
  )
}
