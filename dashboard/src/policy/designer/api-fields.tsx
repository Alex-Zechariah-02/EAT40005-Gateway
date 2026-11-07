import { useEffect, useId, useState } from 'react'
import type { Policy } from '../../shared/api'
import type { ApiEntry, Cors, ResponseHeaders } from '../../shared/rules'
import { Field, describedBy } from '../../shared/ui'
import { copy } from './model'

// An API in the box editor: how its requests are forwarded, which browsers
// on other origins may call it, the headers added to or removed from its
// answers, and the client addresses it accepts. The gateway's compiler checks
// every value when the draft is saved.

// ListField edits a list one entry per line; the draft gets the non-empty lines.
function ListField(props: { label: string; hint: string; value: string[]; editable: boolean; onChange: (value: string[]) => void }) {
  const id = useId()
  const joined = props.value.join('\n')
  const [text, setText] = useState(joined)
  useEffect(() => {
    if (text.split('\n').map((line) => line.trim()).filter(Boolean).join('\n') !== joined) setText(joined)
  }, [joined])
  return (
    <Field id={id} label={props.label} hint={props.hint} wide>
      <textarea
        id={id}
        className="input mono"
        rows={Math.min(Math.max(props.value.length + 1, 2), 8)}
        value={text}
        readOnly={!props.editable}
        spellCheck={false}
        aria-describedby={describedBy(id, props.hint)}
        onChange={(event) => {
          setText(event.target.value)
          props.onChange(event.target.value.split('\n').map((line) => line.trim()).filter(Boolean))
        }}
      />
    </Field>
  )
}

// compact drops members set to undefined, so the draft holds no empty members.
function compact<T extends object>(value: T): T {
  return Object.fromEntries(Object.entries(value).filter(([, member]) => member !== undefined)) as T
}

function Check(props: { label: string; hint?: string; checked: boolean; disabled: boolean; onChange: (checked: boolean) => void }) {
  return (
    <div className="field field-wide">
      <label className="designer-option">
        <input type="checkbox" checked={props.checked} disabled={props.disabled} onChange={(event) => props.onChange(event.target.checked)} />{' '}
        {props.label}
      </label>
      {props.hint ? <p className="hint">{props.hint}</p> : null}
    </div>
  )
}

export function ApiFields(props: { api: ApiEntry; policy: Policy; editable: boolean; onChange: (policy: Policy) => void }) {
  const { api, policy, editable, onChange } = props
  const id = useId()
  const endpoints = policy.operations.filter((operation) => operation.api === api.id)
  const methods = [...new Set(endpoints.map((operation) => operation.method))]
  const allPublic = endpoints.every((operation) => operation.auth === 'none')
  // update changes the API in a copy of the draft; members left empty are removed.
  function update(change: (entry: ApiEntry) => void) {
    const next = copy(policy)
    const entry = next.apis.find((candidate) => candidate.id === api.id)!
    change(entry)
    if (entry.forward) entry.forward = compact(entry.forward)
    if (entry.response_headers) entry.response_headers = compact(entry.response_headers)
    if (entry.forward && !Object.keys(entry.forward).length) delete entry.forward
    if (entry.response_headers && !Object.keys(entry.response_headers).length) delete entry.response_headers
    onChange(next)
  }
  const cors = (change: (rule: Cors) => void) => update((entry) => change(entry.cors!))
  const headers = (change: (rule: ResponseHeaders) => void) =>
    update((entry) => {
      entry.response_headers = { ...entry.response_headers }
      change(entry.response_headers)
    })
  const text = (name: string, label: string, value: string, set: (value: string) => void, hint: string) => (
    <Field id={`${id}-${name}`} label={label} hint={hint}>
      <input
        id={`${id}-${name}`}
        className="input"
        value={value}
        readOnly={!editable}
        spellCheck={false}
        aria-describedby={describedBy(`${id}-${name}`, hint)}
        onChange={(event) => set(event.target.value)}
      />
    </Field>
  )
  const choice = (name: string, label: string, value: string, options: [string, string][], set: (value: string) => void) => (
    <Field id={`${id}-${name}`} label={label}>
      <select id={`${id}-${name}`} className="input" value={value} disabled={!editable} onChange={(event) => set(event.target.value)}>
        {options.map(([option, words]) => (
          <option key={option} value={option}>
            {words}
          </option>
        ))}
      </select>
    </Field>
  )

  return (
    <>
      {text('strip_prefix', 'Remove Prefix', api.forward?.strip_prefix ?? '', (value) => update((entry) => (entry.forward = { ...entry.forward, strip_prefix: value || undefined })),
        'Removed from the request path before forwarding, for example /api. Checks still use the path the client sent.')}
      {text('add_prefix', 'Add Prefix', api.forward?.add_prefix ?? '', (value) => update((entry) => (entry.forward = { ...entry.forward, add_prefix: value || undefined })),
        'Added in front of the forwarded path, for example /v2.')}

      <fieldset className="designer-group field-wide" aria-describedby={`${id}-cors-hint`}>
        <legend>Browser Access (CORS)</legend>
        <p className="hint" id={`${id}-cors-hint`}>
          Lets web pages on other sites call this API from a browser. The gateway answers their preflight requests itself; every other check still applies.
        </p>
        <Check
          label="Allow Browsers on Other Sites"
          checked={!!api.cors}
          disabled={!editable}
          onChange={(on) => update((entry) => (on ? (entry.cors = { origins: [], methods }) : delete entry.cors))}
        />
        {api.cors ? (
          <>
            <Check
              label="Any Origin (*)"
              hint={allPublic && !api.cors.credentials ? 'Any web page may call this API.' : 'Only for APIs whose endpoints are all public, without credentials.'}
              checked={api.cors.origins.includes('*')}
              disabled={!editable || ((!allPublic || !!api.cors.credentials) && !api.cors.origins.includes('*'))}
              onChange={(on) => cors((rule) => (rule.origins = on ? ['*'] : []))}
            />
            {api.cors.origins.includes('*') ? null : (
              <ListField label="Allowed Origins" hint="One per line, for example https://shop.example.com (no path)." value={api.cors.origins}
                editable={editable} onChange={(origins) => cors((rule) => (rule.origins = origins))} />
            )}
            <fieldset className="designer-choice field-wide">
              <legend>Allowed Methods</legend>
              {methods.map((method) => (
                <label key={method}>
                  <input
                    type="checkbox"
                    checked={api.cors!.methods.includes(method)}
                    disabled={!editable}
                    onChange={(event) =>
                      cors((rule) => (rule.methods = event.target.checked ? [...rule.methods, method] : rule.methods.filter((m) => m !== method)))
                    }
                  />{' '}
                  {method}
                </label>
              ))}
            </fieldset>
            <ListField label="Allowed Request Headers" hint="Headers a browser may send, one per line, for example Content-Type." value={api.cors.headers ?? []}
              editable={editable} onChange={(list) => cors((rule) => (list.length ? (rule.headers = list) : delete rule.headers))} />
            <ListField label="Headers Browsers May Read" hint="Answer headers the page's script may read, one per line." value={api.cors.expose ?? []}
              editable={editable} onChange={(list) => cors((rule) => (list.length ? (rule.expose = list) : delete rule.expose))} />
            <Check
              label="Allow Credentials"
              hint="Browsers send cookies and may read the answer. Not available with any origin."
              checked={!!api.cors.credentials}
              disabled={!editable || api.cors.origins.includes('*')}
              onChange={(on) => cors((rule) => (on ? (rule.credentials = true) : delete rule.credentials))}
            />
            {text('max_age', 'Cache Preflight For (Seconds)', String(api.cors.max_age_seconds ?? 600),
              (value) => cors((rule) => (rule.max_age_seconds = Number(value))), 'From 0 to 86400. Browsers reuse a preflight answer this long.')}
          </>
        ) : null}
      </fieldset>

      <fieldset className="designer-group field-wide">
        <legend>Response Headers</legend>
        <Check label="Tell Browsers to Use HTTPS Only (HSTS)" hint="Sent only on connections that use TLS." checked={!!api.response_headers?.hsts}
          disabled={!editable} onChange={(on) => headers((rule) => (rule.hsts = on || undefined))} />
        <Check label="Stop Browsers Guessing Content Types" checked={!!api.response_headers?.nosniff}
          disabled={!editable} onChange={(on) => headers((rule) => (rule.nosniff = on || undefined))} />
        {choice('frame', 'Framing by Other Pages', api.response_headers?.frame ?? '', [['', 'Not Set'], ['deny', 'Deny'], ['sameorigin', 'Same Site Only']],
          (value) => headers((rule) => (rule.frame = (value || undefined) as ResponseHeaders['frame'])))}
        {choice('referrer', 'Referrer Sent by Browsers', api.response_headers?.referrer ?? '',
          [['', 'Not Set'], ['no-referrer', 'None'], ['strict-origin-when-cross-origin', 'Origin Only for Other Sites']],
          (value) => headers((rule) => (rule.referrer = (value || undefined) as ResponseHeaders['referrer'])))}
        <ListField label="Remove Service Headers" hint="Removed from the service's answers, one per line, for example Server or X-Powered-By."
          value={api.response_headers?.remove ?? []} editable={editable} onChange={(list) => headers((rule) => (rule.remove = list.length ? list : undefined))} />
      </fieldset>

      <fieldset className="designer-group field-wide">
        <legend>Allowed Client Addresses</legend>
        <Check label="Accept Only Listed Addresses" hint="Requests from other addresses are refused before any other check."
          checked={!!api.allow_addresses} disabled={!editable}
          onChange={(on) => update((entry) => (on ? (entry.allow_addresses = []) : delete entry.allow_addresses))} />
        {api.allow_addresses ? (
          <ListField label="Address Ranges" hint="One per line, for example 10.0.0.0/8 or 203.0.113.7/32." value={api.allow_addresses}
            editable={editable} onChange={(list) => update((entry) => (entry.allow_addresses = list))} />
        ) : null}
      </fieldset>
    </>
  )
}
