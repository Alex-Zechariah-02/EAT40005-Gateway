import { describedBy, Field } from '../../shared/ui'
import { Choice, methods, Whole } from './wizards'
import { rowId, type Row } from './inputs'

// Connect an API's per-endpoint steps: endpoints, roles, rate limits and request bodies.

type Change = (key: number, patch: Partial<Row>) => void
type Marks = Record<number, Record<string, string | undefined>>
type Named = { row: Row; id: string }[]

// Entry is one text field for an ID, a path or an address, with what to fix beside it.
export function Entry(props: { id: string; label: string; value: string; onChange: (value: string) => void; placeholder?: string; error?: string }) {
  return (
    <Field id={props.id} label={props.label} error={props.error}>
      <input
        id={props.id}
        className="input mono"
        value={props.value}
        placeholder={props.placeholder}
        spellCheck={false}
        autoComplete="off"
        aria-invalid={props.error ? true : undefined}
        aria-describedby={describedBy(props.id, undefined, props.error)}
        onChange={(event) => props.onChange(event.target.value)}
      />
    </Field>
  )
}

function legend(row: Row, id: string): string {
  return `${id} (${row.method} ${row.target})`
}

export function EndpointStep(props: { id: string; api: string; rows: Row[]; errors: Marks; change: Change; add: () => void; remove: (key: number) => void }) {
  return (
    <>
      {props.rows.map((row, index) => {
        const base = `${props.id}-${row.key}`
        const error = props.errors[row.key] ?? {}
        return (
          <fieldset key={row.key} className="wizard-group">
            <legend>Endpoint {index + 1}</legend>
            <div className="form-grid">
              <Choice id={`${base}-method`} label="Method" value={row.method} options={methods.map((item) => [item, item])} onChange={(method) => props.change(row.key, { method })} />
              <Choice
                id={`${base}-auth`}
                label="Who May Call It"
                value={row.open ? 'none' : 'required'}
                options={[
                  ['required', 'Signed-in accounts with a permission'],
                  ['none', 'Anyone, without signing in'],
                ]}
                onChange={(value) => props.change(row.key, { open: value === 'none' })}
              />
              <Entry id={`${base}-target`} label="Path" value={row.target} placeholder="/billing/invoices" onChange={(target) => props.change(row.key, { target })} error={error.target} />
              <Entry id={`${base}-id`} label="Endpoint ID" value={rowId(props.api, row)} onChange={(id) => props.change(row.key, { id })} error={error.id} />
            </div>
            {props.rows.length > 1 ? (
              <button type="button" className="btn btn-small" onClick={() => props.remove(row.key)}>
                Remove Endpoint {index + 1}
              </button>
            ) : null}
          </fieldset>
        )
      })}
      <button type="button" className="btn" onClick={props.add}>
        Add Another Endpoint
      </button>
      <p className="hint">A path ending in /* matches everything below it.</p>
    </>
  )
}

export function RoleStep(props: { roles: string[]; named: Named; change: Change }) {
  return (
    <>
      {props.named.map(({ row, id }) =>
        row.open ? (
          <p key={row.key}>
            <code>{id}</code> is public: anyone may call it, so it takes no permission.
          </p>
        ) : (
          <fieldset key={row.key} className="wizard-choices">
            <legend>
              <code>{legend(row, id)}</code>
            </legend>
            {props.roles.map((role) => (
              <label key={role} className="check">
                <input
                  type="checkbox"
                  checked={row.roles.includes(role)}
                  onChange={(event) => props.change(row.key, { roles: event.target.checked ? [...row.roles, role] : row.roles.filter((item) => item !== role) })}
                />
                <span>{role}</span>
              </label>
            ))}
            {row.roles.length === 0 ? <p className="hint">With no role ticked, no account can call it yet.</p> : null}
          </fieldset>
        ),
      )}
      {props.roles.length === 0 ? <p className="hint">This draft has no roles yet. Add one on the canvas, then give it access.</p> : null}
    </>
  )
}

export function RateStep(props: { id: string; named: Named; errors: Marks; change: Change }) {
  return (
    <>
      {props.named.map(({ row, id }) => {
        const error = props.errors[row.key] ?? {}
        return (
          <fieldset key={row.key} className="wizard-group">
            <legend>
              <code>{legend(row, id)}</code>
            </legend>
            <div className="form-grid">
              <Whole
                id={`${props.id}-${row.key}-count`}
                label="Requests"
                value={row.count}
                onChange={(count) => props.change(row.key, { count })}
                hint={`Counted per ${row.open ? 'client address' : 'account'}.`}
                error={error.count}
              />
              <Whole
                id={`${props.id}-${row.key}-seconds`}
                label="Per How Many Seconds"
                value={row.seconds}
                onChange={(seconds) => props.change(row.key, { seconds })}
                error={error.seconds}
              />
            </div>
          </fieldset>
        )
      })}
      <p className="hint">A request over the limit is refused until enough of the window has passed.</p>
    </>
  )
}

export function BodyStep(props: { id: string; named: Named; errors: Marks; change: Change }) {
  if (props.named.length === 0) return <p>None of these endpoints takes a request body (POST, PUT or PATCH), so there is nothing to check.</p>
  return (
    <>
      {props.named.map(({ row, id }) => {
        const base = `${props.id}-${row.key}`
        const error = props.errors[row.key] ?? {}
        return (
          <fieldset key={row.key} className="wizard-group">
            <legend>
              <code>{legend(row, id)}</code>
            </legend>
            <label className="check">
              <input type="checkbox" checked={row.checked} onChange={(event) => props.change(row.key, { checked: event.target.checked })} />
              <span>Check This Endpoint's Request Body</span>
            </label>
            {row.checked ? (
              <>
                <Whole
                  id={`${base}-bytes`}
                  label="Maximum Size (Bytes)"
                  value={row.bytes}
                  onChange={(bytes) => props.change(row.key, { bytes })}
                  error={error.bytes}
                />
                <Field id={`${base}-schema`} label="JSON Schema" error={error.schema}>
                  <textarea
                    id={`${base}-schema`}
                    className="input code-input"
                    rows={7}
                    value={row.schema}
                    spellCheck={false}
                    aria-invalid={error.schema ? true : undefined}
                    aria-describedby={describedBy(`${base}-schema`, undefined, error.schema)}
                    onChange={(event) => props.change(row.key, { schema: event.target.value })}
                  />
                </Field>
              </>
            ) : null}
          </fieldset>
        )
      })}
    </>
  )
}
