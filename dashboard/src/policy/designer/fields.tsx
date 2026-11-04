import { useEffect, useId, useState, type ReactNode } from 'react'
import type { Operation, Policy } from '../../shared/api'
import { copy, type BoxKey } from './model'
import { RequestRules } from '../request-rules'
import { Field, describedBy } from '../../shared/ui'

// An endpoint in the box editor: its fields, the JSON editor for its schemas, and its checks in order.

function SchemaEditor({
  label,
  value,
  editable,
  onChange,
  onIssue,
}: {
  label: string
  value: unknown
  editable: boolean
  onChange: (value: unknown) => void
  onIssue: (message: string | null) => void
}) {
  const id = useId()
  const serialised = JSON.stringify(value, null, 2)
  const [text, setText] = useState(serialised)
  const [error, setError] = useState('')
  useEffect(() => {
    setText(serialised)
    setError('')
    onIssue(null)
  }, [serialised])
  return (
    <Field id={id} label={label} hint="The gateway validates the schema on save." error={error || undefined} wide>
      <textarea
        id={id}
        className="input code-input"
        value={text}
        readOnly={!editable}
        spellCheck={false}
        aria-invalid={!!error}
        aria-describedby={describedBy(id, 'hint', error)}
        onChange={(event) => {
          const next = event.target.value
          setText(next)
          try {
            const parsed: unknown = JSON.parse(next)
            if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Use a JSON object.')
            setError('')
            onIssue(null)
            onChange(parsed)
          } catch (caught) {
            const message = caught instanceof Error ? caught.message : 'Enter a JSON object.'
            setError(message)
            onIssue(message)
          }
        }}
      />
    </Field>
  )
}

// The form helpers ObjectForm lends to the endpoint fields.
type Input = (name: string, label: string, value: string | number, update: (value: string) => void, hint?: string, type?: string) => ReactNode
type Select = (name: string, label: string, value: string, values: string[], update: (value: string) => void) => ReactNode

// OperationFields edits an endpoint: its route, API, access, token, rate limit,
// and body and header rules.
export function OperationFields(props: {
  operation: Operation
  policy: Policy
  editable: boolean
  id: string
  selection: BoxKey
  fieldId: (name: string) => string
  input: Input
  select: Select
  patch: (update: Record<string, unknown>) => void
  onChange: (policy: Policy) => void
  onBufferIssue: (key: string, issue: string | null) => void
  setMakingPublic: (value: boolean) => void
}) {
  const { operation, policy, editable, id, selection, fieldId, input, select, patch, onChange, onBufferIssue, setMakingPublic } = props
  return (
    <>
      {select('method', 'Method', operation.method, ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'], (method) =>
        patch({ method }),
      )}
      {input(
        'target',
        'Target',
        operation.target,
        (target) => patch({ target }),
        'Exact path, or prefix ending /*. Starts with /; no empty, . or .. segments, encoded characters or backslashes.',
      )}
      {select(
        'api',
        'API',
        operation.api,
        policy.apis.map((api) => api.id),
        (api) => patch({ api }),
      )}
      <fieldset id={fieldId('Access')} className="designer-choice">
        <legend>Endpoint Access</legend>
        {['allow', 'deny'].map((access) => (
          <label key={access}>
            <input
              type="radio"
              name={fieldId('access')}
              value={access}
              checked={operation.access === access}
              disabled={!editable}
              onChange={() => patch({ access })}
            />{' '}
            {access}
          </label>
        ))}
      </fieldset>
      <fieldset id={fieldId('Sign-in')} className="designer-choice">
        <legend>Token</legend>
        {[
          { value: 'required', label: 'Required' },
          { value: 'none', label: 'Public (No Token)' },
        ].map((auth) => (
          <label key={auth.value}>
            <input
              type="radio"
              name={fieldId('auth')}
              checked={operation.auth === auth.value}
              disabled={!editable}
              onChange={() => {
                const rateChanges = operation.rate && operation.rate.key !== 'client_address'
                if (auth.value === 'none' && (rateChanges || policy.permits.some((permit) => permit.operation === id)))
                  setMakingPublic(true)
                else patch({ auth: auth.value })
              }}
            />{' '}
            {auth.label}
          </label>
        ))}
      </fieldset>
      <div className="field field-wide" id={fieldId('Rate')}>
        <label className="designer-option">
          <input
            type="checkbox"
            checked={!!operation.rate}
            disabled={!editable}
            onChange={(event) => {
              const next = copy(policy)
              const target = next.operations.find((entry) => entry.id === id)!
              const key = operation.auth === 'none' ? 'client_address' : 'subject'
              if (event.target.checked) target.rate = { algorithm: 'sliding-window', key, count: 100, window_seconds: 60 }
              else delete target.rate
              onChange(next)
            }}
          />{' '}
          Rate limit
        </label>
        <p className="hint">Sliding window, per endpoint. Counters survive policy changes.</p>
      </div>
      {operation.rate ? (
        <>
          <fieldset id={fieldId('rate-key')} className="designer-choice">
            <legend>Count Requests</legend>
            {[
              { value: 'subject', label: 'Per Account' },
              { value: 'client_address', label: 'Per Client Address' },
            ].map((key) => (
              <label key={key.value}>
                <input
                  type="radio"
                  name={fieldId('rate-key')}
                  checked={operation.rate!.key === key.value}
                  disabled={!editable || (key.value === 'subject' && operation.auth === 'none')}
                  onChange={() => patch({ rate: { ...operation.rate!, key: key.value } })}
                />{' '}
                {key.label}
                {key.value === 'subject' && operation.auth === 'none' ? <span className="hint"> (needs a token)</span> : null}
              </label>
            ))}
          </fieldset>
          {input(
            'rate-count',
            'Request count',
            operation.rate.count,
            (value) => patch({ rate: { ...operation.rate!, count: Number(value) } }),
            undefined,
            'number',
          )}
          {input(
            'rate-window',
            'Window (seconds)',
            operation.rate.window_seconds,
            (value) => patch({ rate: { ...operation.rate!, window_seconds: Number(value) } }),
            undefined,
            'number',
          )}
        </>
      ) : null}
      <div className="field field-wide" id={fieldId('Body')}>
        <label className="designer-option">
          <input
            type="checkbox"
            checked={!!operation.body}
            disabled={!editable}
            onChange={(event) => {
              const next = copy(policy)
              const target = next.operations.find((entry) => entry.id === id)!
              if (event.target.checked)
                target.body = { max_bytes: 1024, schema: { type: 'object', properties: {}, additionalProperties: false } }
              else delete target.body
              onChange(next)
            }}
          />{' '}
          Body rule
        </label>
      </div>
      {operation.body ? (
        <>
          {input(
            'body-max',
            'Maximum body bytes',
            operation.body.max_bytes,
            (value) => patch({ body: { ...operation.body!, max_bytes: Number(value) } }),
            undefined,
            'number',
          )}
          <SchemaEditor
            label="Body Schema (JSON)"
            value={operation.body.schema}
            editable={editable}
            onChange={(schema) => patch({ body: { ...operation.body!, schema } })}
            onIssue={(issue) => onBufferIssue(`${selection}.body`, issue)}
          />
        </>
      ) : null}
      <div className="field field-wide">
        <label className="designer-option">
          <input
            type="checkbox"
            checked={operation.headers !== undefined}
            disabled={!editable}
            onChange={(event) => {
              const next = copy(policy)
              const target = next.operations.find((entry) => entry.id === id)!
              if (event.target.checked) target.headers = { type: 'object', properties: {}, additionalProperties: true }
              else delete target.headers
              onChange(next)
            }}
          />{' '}
          Header rule
        </label>
      </div>
      {operation.headers !== undefined ? (
        <SchemaEditor
          label="Header Schema (JSON)"
          value={operation.headers}
          editable={editable}
          onChange={(headers) => patch({ headers })}
          onIssue={(issue) => onBufferIssue(`${selection}.headers`, issue)}
        />
      ) : null}
      {operation.body || operation.headers ? (
        <details className="api-rules">
          <summary>These Rules in Words</summary>
          <RequestRules operation={operation} />
        </details>
      ) : null}
    </>
  )
}

// checks lists what the gateway checks on an endpoint, in its fixed order;
// code marks a value that is a route or IDs.
export function checks(policy: Policy, operation: Operation): { name: string; value: string; tone: 'neutral' | 'refused' | 'pending' | null; code?: boolean }[] {
  const api = policy.apis.find((item) => item.id === operation.api)
  return [
    { name: 'Route', value: `${operation.method} ${operation.target}`, tone: 'neutral', code: true },
    { name: 'Access', value: operation.access, tone: operation.access === 'deny' ? 'refused' : 'neutral' },
    { name: 'Sign-In', value: operation.auth === 'none' ? 'public' : 'required', tone: operation.auth === 'none' ? 'pending' : 'neutral' },
    {
      name: 'Role',
      value: operation.auth === 'none' ? 'n/a' : String(policy.permits.filter((permit) => permit.operation === operation.id).length),
      tone: 'neutral',
    },
    {
      name: 'Rate',
      value: operation.rate ? `${operation.rate.count}/${operation.rate.window_seconds}s` : 'none',
      tone: operation.rate ? 'neutral' : null,
    },
    {
      name: 'Body',
      value: operation.body ? `≤ ${operation.body.max_bytes} B${operation.headers ? ' + headers' : ''}` : operation.headers ? 'headers' : 'none',
      tone: operation.body || operation.headers ? 'neutral' : null,
    },
    { name: 'Record', value: 'always', tone: 'neutral' },
    { name: 'Forward', value: `${operation.api} → ${api?.upstream ?? 'missing service address'}`, tone: 'neutral', code: api?.upstream !== undefined },
  ]
}
