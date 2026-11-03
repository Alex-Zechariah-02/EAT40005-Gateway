import { useId, useState } from 'react'
import { connectApi } from './edits'
import { Wizard } from '../../shared/dialogs'
import { Choice, Review, type WizardProps } from './wizards'
import { blank, dialProblem, endpoints, idProblem, rowErrors, rowId, writes, type Row } from './inputs'
import { BodyStep, EndpointStep, Entry, RateStep, RoleStep } from './routes'

// Connect an API: a side drawer that adds a service address, an API and its
// endpoints, permissions, rate limits and optional body rules to the draft only.

const fix = 'Fix the marked fields to continue.'

// field writes one policy object the way the technical lines show it.
function field(members: [string, string][]): string {
  return `{${members.map(([name, value]) => `"${name}": ${JSON.stringify(value)}`).join(', ')}}`
}

export function ConnectWizard({ policy, apply, close }: WizardProps) {
  const id = useId()
  const [existing, setExisting] = useState('')
  const [upstream, setUpstream] = useState('')
  const [dial, setDial] = useState('')
  const [api, setApi] = useState('')
  const [rows, setRows] = useState<Row[]>([blank(1)])
  // revealed lists the steps whose Next was pressed early; their fields show what to fix.
  const [revealed, setRevealed] = useState<number[]>([])
  const shown = (step: number) => revealed.includes(step)
  const reveal = (step: number) => () => setRevealed((list) => (list.includes(step) ? list : [...list, step]))
  const change = (key: number, patch: Partial<Row>) => setRows((list) => list.map((row) => (row.key === key ? { ...row, ...patch } : row)))

  const target = existing || upstream
  const upstreamError = existing
    ? undefined
    : idProblem(upstream, policy.upstreams.some((item) => item.id === upstream) ? 'This draft already has a service address with this ID.' : undefined)
  const dialError = existing ? undefined : dialProblem(dial)
  const apiError = idProblem(api, policy.apis.some((item) => item.id === api) ? 'This draft already has an API with this ID.' : undefined)
  const errors = rows.map((_, index) => rowErrors(policy, api, rows, index))
  // marks are a revealed step's field errors by row.
  const marks = (step: number) => (shown(step) ? Object.fromEntries(rows.map((row, index) => [row.key, errors[index]])) : {})
  const failing = (names: string[]) => errors.some((error) => names.some((name) => error[name]))
  const complete = !upstreamError && !dialError && !apiError && !failing(['target', 'id', 'count', 'seconds', 'bytes', 'schema'])
  const next = complete ? connectApi(policy, existing ? { id: existing } : { id: upstream, dial }, api, endpoints(api, rows)) : policy
  const permits = rows.reduce((sum, row) => sum + (row.open ? 0 : row.roles.length), 0)
  const unsaved = existing !== '' || upstream !== '' || dial !== '' || api !== '' || JSON.stringify(rows) !== JSON.stringify([blank(1)])
  const named = rows.map((row) => ({ row, id: rowId(api, row) }))
  const writing = named.filter(({ row }) => writes(row))

  return (
    <Wizard
      drawer
      unsaved={unsaved}
      title="Connect an API"
      finishLabel="Add to the Draft"
      onClose={close}
      onFinish={() => apply(next, `API ${api} connected with ${rows.length} ${rows.length === 1 ? 'endpoint' : 'endpoints'}.`)}
      steps={[
        {
          title: 'Service Address',
          plain: "Where the gateway forwards this API's requests: the address of the service behind it.",
          technical: existing ? (
            <>
              Writes nothing to <code>upstreams[]</code>: the API forwards to <code>{existing}</code>.
            </>
          ) : (
            <>
              Writes <code>upstreams[]</code>: <code>{field([['id', upstream], ['dial', dial]])}</code>
            </>
          ),
          problem: upstreamError || dialError ? fix : undefined,
          reveal: reveal(0),
          body: (
            <>
              <Choice
                id={`${id}-existing`}
                label="Service Address"
                value={existing}
                options={[['', 'New service address'], ...policy.upstreams.map((item): [string, string] => [item.id, `${item.id} (${item.dial})`])]}
                onChange={setExisting}
              />
              {existing ? null : (
                <div className="form-grid">
                  <Entry id={`${id}-upstream`} label="Service Address ID" value={upstream} onChange={setUpstream} error={shown(0) ? upstreamError : undefined} />
                  <Entry
                    id={`${id}-dial`}
                    label="Address (Host and Port)"
                    value={dial}
                    onChange={setDial}
                    placeholder="127.0.0.1:9600"
                    error={shown(0) ? dialError : undefined}
                  />
                </div>
              )}
            </>
          ),
        },
        {
          title: 'API',
          plain: 'A name that groups the service’s endpoints on the Policy and APIs pages.',
          technical: (
            <>
              Writes <code>apis[]</code>: <code>{field([['id', api], ['upstream', target]])}</code>
            </>
          ),
          problem: apiError ? fix : undefined,
          reveal: reveal(1),
          body: <Entry id={`${id}-api`} label="API ID" value={api} onChange={setApi} placeholder="billing" error={shown(1) ? apiError : undefined} />,
        },
        {
          title: 'Endpoints',
          plain: 'The requests this API answers: each one’s method and path, and whether callers must sign in.',
          technical: (
            <>
              Writes <code>operations[]</code>: one entry per endpoint, with <code>"access": "allow"</code> and <code>"auth": "required"</code> or{' '}
              <code>"none"</code>.
            </>
          ),
          problem: failing(['target', 'id']) ? fix : undefined,
          reveal: reveal(2),
          body: (
            <EndpointStep
              id={id}
              api={api}
              rows={rows}
              errors={marks(2)}
              change={change}
              add={() => setRows((list) => [...list, blank(Math.max(...list.map((row) => row.key)) + 1)])}
              remove={(key) => setRows((list) => list.filter((row) => row.key !== key))}
            />
          ),
        },
        {
          title: 'Roles',
          plain: 'Which roles may call each endpoint that needs a sign-in. A role comes from the caller’s sign-in token.',
          technical: (
            <>
              Writes <code>permits[]</code>: <code>{'{"role", "operation"}'}</code>, one entry per role ticked.
            </>
          ),
          body: <RoleStep roles={policy.roles.map((role) => role.id)} named={named} change={change} />,
        },
        {
          title: 'Rate',
          plain: 'How many requests one account, or one client address for a public endpoint, may send in a time window.',
          technical: (
            <>
              Writes <code>operations[].rate</code>: <code>{'{"algorithm": "sliding-window", "count", "window_seconds", "key"}'}</code>.
            </>
          ),
          problem: failing(['count', 'seconds']) ? fix : undefined,
          reveal: reveal(4),
          body: <RateStep id={id} named={named} errors={marks(4)} change={change} />,
        },
        {
          title: 'Validate Request Bodies',
          plain: 'Optional: the gateway refuses a write request whose body does not match its schema, before the service sees it.',
          technical: (
            <>
              Writes <code>operations[].body</code>: <code>{'{"max_bytes", "schema"}'}</code>. Only the gateway checks the schema, when the draft is saved.
            </>
          ),
          problem: failing(['bytes', 'schema']) ? fix : undefined,
          reveal: reveal(5),
          skip: writing.length ? () => setRows((list) => list.map((row) => ({ ...row, checked: false }))) : undefined,
          body: <BodyStep id={id} named={writing} errors={marks(5)} change={change} />,
        },
        {
          title: 'Review',
          plain: 'Exactly what the flow adds to the draft, and nothing else.',
          technical: (
            <>
              Adds {existing ? 0 : 1} to <code>upstreams[]</code>, 1 to <code>apis[]</code>, {rows.length} to <code>operations[]</code> and {permits} to{' '}
              <code>permits[]</code>.
            </>
          ),
          body: <Review before={policy} after={next} />,
        },
      ]}
    />
  )
}
