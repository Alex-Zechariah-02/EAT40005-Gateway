import { useEffect, useRef, useState } from 'react'
import type { Revision } from './api'
import type { DesignerDraft } from './designer-model'
import { CodeBlock, Field } from './ui'
import type { Mode } from './designer-views'

// The Policy page's JSON tab: the whole document to copy, download, load, edit and apply.

// JsonTools copies or downloads the document shown, or loads one from a
// file into the editor; loading changes nothing until Apply JSON.
function JsonTools(props: { text: string; name: string; editable: boolean; onLoad: (text: string) => void }) {
  const [copied, setCopied] = useState(false)
  const [href, setHref] = useState('')
  const [fileProblem, setFileProblem] = useState('')
  const file = useRef<HTMLInputElement>(null)
  // The download link is a blob URL of the shown text, released when the
  // text changes or the page closes.
  useEffect(() => {
    setCopied(false)
    const url = URL.createObjectURL(new Blob([props.text], { type: 'application/json' }))
    setHref(url)
    return () => URL.revokeObjectURL(url)
  }, [props.text])
  return (
    <div className="json-tools">
      <button
        type="button"
        className="btn btn-small"
        onClick={() =>
          navigator.clipboard.writeText(props.text).then(
            () => setCopied(true),
            () => setCopied(false),
          )
        }
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
      <a className="btn btn-small" href={href || undefined} download={props.name}>
        Download JSON
      </a>
      {props.editable ? (
        <>
          <button type="button" className="btn btn-small" onClick={() => file.current?.click()}>
            Load from a File
          </button>
          <input
            ref={file}
            type="file"
            accept="application/json,.json"
            hidden
            onChange={(event) => {
              const chosen = event.target.files?.[0]
              event.target.value = ''
              if (!chosen) return
              chosen.text().then(
                (text) => {
                  setFileProblem('')
                  props.onLoad(text)
                },
                () => setFileProblem('The file could not be read.'),
              )
            }}
          />
        </>
      ) : null}
      {fileProblem ? (
        <p role="alert" className="field-error">
          {fileProblem}
        </p>
      ) : null}
    </div>
  )
}

// JsonView is the JSON tab: the whole document to copy, download, load, edit and apply.
export function JsonView(props: {
  draft: DesignerDraft
  pendingId: number | undefined
  mode: Mode
  json: string
  setJson: (value: string) => void
  jsonError: string
  setJsonError: (value: string) => void
  current: Revision
  editable: boolean
  applyJson: () => boolean
}) {
  const { draft, pendingId, mode, json, setJson, jsonError, setJsonError, current, editable, applyJson } = props
  return (
    <div className="designer-json">
      <JsonTools
        text={json}
        name={mode === 'draft' ? `gateway-policy-draft-r${draft.baseRevision}.json` : `gateway-policy-r${mode === 'confirmed' ? current.id : pendingId}.json`}
        editable={editable}
        onLoad={(text) => {
          setJson(text)
          setJsonError('')
        }}
      />
      <Field id="designer-json-document" label="Complete Policy Document (JSON)">
        <textarea
          id="designer-json-document"
          className="input code-input"
          value={json}
          readOnly={!editable}
          aria-invalid={!!jsonError}
          aria-describedby={jsonError ? 'designer-json-document-error' : undefined}
          spellCheck={false}
          onChange={(event) => {
            setJson(event.target.value)
            setJsonError('')
          }}
        />
      </Field>
      {jsonError ? (
        <p id="designer-json-document-error" role="alert" className="notice tone-refused">
          {jsonError}
        </p>
      ) : null}
      {editable ? (
        <button className="btn" type="button" onClick={applyJson}>
          Apply JSON
        </button>
      ) : null}
      <details className="api-requests">
        <summary>Save a Draft Change with the Admin API</summary>
        <p>
          Each request needs a signed-in session cookie and its CSRF token; saving a draft change (API: candidate) also needs the base
          version and a new idempotency key. The gateway validates the whole document on save.
        </p>
        <CodeBlock label="Admin API requests" className="code-block code-block-short">{`GET /admin/policy

POST /admin/candidates
Content-Type: application/json
If-Match: "r<active version>"
Idempotency-Key: <a new key: 16 to 64 letters, digits, _ or ->
X-CSRF-Token: <the session's CSRF token>

{"policy": <the complete policy document>}`}</CodeBlock>
      </details>
    </div>
  )
}
