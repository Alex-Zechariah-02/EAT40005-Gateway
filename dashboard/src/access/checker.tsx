import { useId, useState, type FormEvent } from 'react'
import type { Candidate, Page, Reply, Revision } from '../shared/api'
import { useResource } from '../shared/hooks'
import { Badge, Field } from '../shared/ui'
import { PageHeader } from '../app/header'
import { Problem } from '../shared/replies'
import type { ViewProps } from '../app/views'

// The Access Checker page: the gateway's answer to who may call an endpoint.

interface AccessAnswer {
  decision: 'allowed' | 'refused'
  code: string | null
  check: 'route' | 'access' | 'blocked' | 'role' | 'forward' | null
  operation: string | null
  public: boolean
  forwarded_path: string | null
  roles: string[]
  ignored_roles: string[]
  generation: string
  candidate: number | null
  not_checked: string[]
}

const checkWords: Record<string, string> = {
  route: 'No endpoint matches this method and path.',
  access: 'The endpoint is switched off for everyone (access deny).',
  blocked: 'The account is on the blocked list.',
  role: 'None of these roles has permission for the endpoint.',
  forward: "The API's forward rule makes this path invalid, so it is never forwarded.",
}

const uncheckedWords: Record<string, string> = {
  token: 'a valid sign-in token',
  rate: 'the rate limit, which depends on live traffic',
  body: 'the request body rules, which depend on the real body',
}

export function AccessCheckView({ admin }: ViewProps) {
  const id = useId()
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const drafts = useResource<Page<Candidate>>((signal) => admin.get('/admin/candidates', signal), [])
  const [form, setForm] = useState({ role: '', subject: '', endpoint: '', candidate: '' })
  const [reply, setReply] = useState<Reply<AccessAnswer> | null>(null)
  const [pending, setPending] = useState(false)
  const document = policy.reply?.kind === 'ok' ? policy.reply.body.document : undefined
  const operations = document?.operations ?? []
  const open = drafts.reply?.kind === 'ok' ? drafts.reply.body.items.filter((c) => c.status === 'validated') : []

  async function check(event: FormEvent) {
    event.preventDefault()
    const operation = operations.find((op) => op.id === form.endpoint)
    if (!operation) return
    setPending(true)
    const answer = await admin.send<AccessAnswer>({
      method: 'POST',
      path: '/admin/access-check',
      body: {
        method: operation.method,
        // A prefix endpoint is checked with a sample path inside it.
        path: operation.target.endsWith('/*') ? operation.target.slice(0, -1) + 'example' : operation.target,
        roles: form.role ? [form.role] : [],
        subject: form.subject.trim() || null,
        candidate: form.candidate ? Number(form.candidate) : null,
      },
    })
    setPending(false)
    setReply(answer)
  }

  return (
    <>
      <PageHeader title="Access Checker" lede="Ask the gateway whether a role may call an endpoint, and which check decides." />
      <form className="section card card-pad log-form" onSubmit={check} aria-label="Check Access">
        <Field id={`${id}-role`} label="Role">
          <select id={`${id}-role`} className="input" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
            <option value="">No role</option>
            {document?.roles.map((role) => (
              <option key={role.id} value={role.id}>
                {role.id}
              </option>
            ))}
          </select>
        </Field>
        <Field id={`${id}-endpoint`} label="Endpoint" wide>
          <select id={`${id}-endpoint`} className="input" value={form.endpoint} onChange={(e) => setForm({ ...form, endpoint: e.target.value })} required>
            <option value="">Choose an endpoint</option>
            {operations.map((op) => (
              <option key={op.id} value={op.id}>
                {op.method} {op.target} ({op.id})
              </option>
            ))}
          </select>
        </Field>
        <Field id={`${id}-subject`} label="Account (optional)">
          <input id={`${id}-subject`} className="input mono" value={form.subject} onChange={(e) => setForm({ ...form, subject: e.target.value })} spellCheck={false} autoComplete="off" />
        </Field>
        <Field id={`${id}-candidate`} label="Policy">
          <select id={`${id}-candidate`} className="input" value={form.candidate} onChange={(e) => setForm({ ...form, candidate: e.target.value })}>
            <option value="">The policy in force</option>
            {open.map((c) => (
              <option key={c.id} value={c.id}>
                Draft change {c.id}
              </option>
            ))}
          </select>
        </Field>
        <div className="log-form-actions">
          <button type="submit" className="btn btn-primary" disabled={pending || !form.endpoint}>
            {pending ? 'Checking…' : 'Check Access'}
          </button>
        </div>
        <p className="hint log-form-hint">The role stands for a role in a verified sign-in token. Give an account to also check the blocked list.</p>
      </form>
      {policy.reply && policy.reply.kind !== 'ok' ? <Problem reply={policy.reply} /> : null}
      {reply && reply.kind !== 'ok' ? <Problem reply={reply} /> : null}
      {reply?.kind === 'ok' ? <AccessResult answer={reply.body} /> : null}
    </>
  )
}

function AccessResult({ answer }: { answer: AccessAnswer }) {
  const allowed = answer.decision === 'allowed'
  return (
    <section className="section card card-pad" aria-labelledby="access-result" aria-live="polite">
      <h2 id="access-result" className="access-verdict">
        <Badge tone={allowed ? 'ok' : 'refused'}>{allowed ? 'Allowed' : 'Refused'}</Badge>{' '}
        {allowed ? (answer.public ? 'A public endpoint: anyone may call it.' : 'These checks pass.') : checkWords[answer.check ?? ''] ?? answer.code}
      </h2>
      <p className="hint">
        {answer.candidate ? `Checked against draft change ${answer.candidate}, with the temporary rules in force.` : 'Checked against the policy in force, including temporary rules.'}{' '}
        {answer.ignored_roles.length ? `Roles the policy does not define are ignored: ${answer.ignored_roles.join(', ')}.` : ''}
      </p>
      {answer.forwarded_path ? (
        <p className="hint">
          Forwarded As: the service receives <code>{answer.forwarded_path}</code>.
        </p>
      ) : null}
      {answer.not_checked.length ? (
        <p className="hint">
          A real request must also pass {answer.not_checked.length === 1 ? 'one check' : `${answer.not_checked.length} checks`} not made here:{' '}
          {answer.not_checked.map((step) => uncheckedWords[step] ?? step).join('; ')}. These were not checked.
        </p>
      ) : null}
    </section>
  )
}
