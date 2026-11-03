import type { Admin } from './http'
import { useMutation } from './hooks'
import { Badge, TableScroll } from './ui'
import { Problem } from './replies'
import { Time } from './time'
import { reasonOf } from './requests'

// The impact preview: recent requests a draft change would decide differently.

// requests = previewed + identity_unknown. A request whose sign-in was never
// checked (refused before the token check, or a public operation) cannot be
// replayed under a policy that would check it; the gateway counts it apart.
interface Impact {
  window: { from: string; to: string; requests: number; previewed: number; identity_unknown: number }
  flips: { operation: string; subject: string | null; roles: string[]; before: string; after: string; after_code: string | null; count: number }[]
  not_previewed: string[]
}

const notPreviewed: Record<string, string> = {
  rate: 'rate limits',
  body: 'body checks (bodies are never stored)',
  unobserved_identity: 'sign-in on requests whose token was never checked',
}

function listOf(items: string[]): string {
  return items.length < 2 ? items.join('') : `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`
}

function noneChanged(previewed: number): string {
  if (previewed === 0) return 'No recent request could be replayed.'
  if (previewed === 1) return 'The one replayed request would not be decided differently.'
  return `None of the ${previewed.toLocaleString()} replayed requests would be decided differently.`
}

// unknownText explains the requests the preview leaves out: the gateway never
// checked their sign-in token (a public operation, or a refusal before the
// sign-in check), and this change would need to know it.
function unknownText(count: number): string {
  const requests = count === 1 ? '1 request was' : `${count.toLocaleString()} requests were`
  const which = count === 1 ? 'a public operation, or a refusal before the sign-in check' : 'public operations, or refusals before the sign-in check'
  return `${requests} decided without the gateway checking a sign-in token (${which}), so the gateway cannot tell how this change would decide ${count === 1 ? 'it' : 'them'}; ${count === 1 ? 'it is' : 'they are'} left out.`
}

// ImpactPreview asks the gateway which recent requests a staged change would
// decide differently. The gateway's own compiler evaluates it; rate
// limits and bodies cannot be replayed and are said so.
export function ImpactPreview({ admin, candidate }: { admin: Admin; candidate: number }) {
  const preview = useMutation<Impact>(admin, () => {})
  const result = preview.phase.state === 'replied' && preview.phase.reply.kind === 'ok' ? preview.phase.reply.body : null
  const changed = result ? result.flips.reduce((n, f) => n + f.count, 0) : 0
  const refused = result ? result.flips.filter((f) => f.after !== 'allowed').reduce((n, f) => n + f.count, 0) : 0
  const unknown = result?.window.identity_unknown ?? 0
  return (
    <section className="impact" aria-labelledby={`impact-${candidate}`}>
      <div className="impact-head">
        <h3 id={`impact-${candidate}`}>Impact on Recent Traffic</h3>
        <button type="button" className="btn btn-small" disabled={preview.pending} onClick={() => void preview.run({ method: 'POST', path: `/admin/candidates/${candidate}/impact` })}>
          {preview.pending ? 'Replaying…' : result ? 'Preview Again' : 'Preview Impact'}
        </button>
      </div>
      {result ? (
        <>
          {result.flips.length === 0 ? (
            <p className={unknown ? 'notice tone-pending' : 'notice tone-ok'}>
              <span className="notice-title">
                {unknown
                  ? noneChanged(result.window.previewed)
                  : 'No recent request would be decided differently.'}
              </span>
              {unknown ? <> {unknownText(unknown)}</> : null}
            </p>
          ) : (
            <>
              <p className={refused ? 'notice tone-refused' : 'notice tone-pending'}>
                <span className="notice-title">
                  {changed === 1 ? '1 recent request' : `${changed.toLocaleString()} recent requests`} would be decided differently
                  {refused ? (changed === 1 ? '; it would now be refused' : `; ${refused.toLocaleString()} of them would now be refused`) : ''}.
                </span>
                {unknown ? <> {unknownText(unknown)}</> : null}
              </p>
              <TableScroll label="Decisions that would change">
                <table>
                  <thead>
                    <tr>
                      <th scope="col">Endpoint</th>
                      <th scope="col">Account</th>
                      <th scope="col">Now</th>
                      <th scope="col">With This Change</th>
                      <th scope="col" className="num end">
                        Requests
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {result.flips.map((f) => (
                      <tr key={`${f.operation}|${f.subject}|${f.before}|${f.after}`}>
                        <td>
                          <code>{f.operation}</code>
                        </td>
                        <td>
                          <code>{f.subject ?? 'anyone'}</code>
                          {f.roles.length ? <span className="muted"> {f.roles.join(', ')}</span> : null}
                        </td>
                        <td>
                          <Badge tone={f.before === 'allowed' ? 'ok' : 'refused'}>{f.before === 'allowed' ? 'Allowed' : 'Refused'}</Badge>
                        </td>
                        <td>
                          <Badge tone={f.after === 'allowed' ? 'ok' : 'refused'}>{f.after === 'allowed' ? 'Allowed' : 'Refused'}</Badge>
                          {f.after_code ? <span className="muted"> {reasonOf(f.after_code)}</span> : null}
                        </td>
                        <td className="num end">{f.count.toLocaleString()}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </TableScroll>
            </>
          )}
          <p className="hint">
            Replayed {result.window.previewed.toLocaleString()} of {result.window.requests.toLocaleString()} requests from <Time value={result.window.from} /> to{' '}
            <Time value={result.window.to} />. Not replayed: {listOf(result.not_previewed.map((n) => notPreviewed[n] ?? n))}.
          </p>
        </>
      ) : preview.phase.state === 'replied' ? (
        <Problem reply={preview.phase.reply} />
      ) : (
        <p className="hint">Replays recent requests against this change. Read-only.</p>
      )}
    </section>
  )
}
