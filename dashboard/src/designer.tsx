import { useCallback, useEffect, useMemo, useRef, useState, type Dispatch, type SetStateAction } from 'react'
import {
  newKey,
  type Candidate,
  type Outcome,
  type OverridesView,
  type Page,
  type Policy,
  type ProvisionalView,
  type Revision,
} from './api'
import { differences } from './diff'
import { useGatewayState, useMutation, useResource } from './hooks'
import { DesignerCanvas, type TracePath } from './designer-canvas'
import { FormView, Issues, ObjectForm } from './designer-form'
import {
  add,
  canonical,
  changes,
  copy,
  fieldBox,
  localIssues,
  remove,
  shapeIssue,
  splitKey,
  type BoxKey,
  type BoxKind,
  type GroupAction,
  type DesignerDraft,
} from './designer-model'
import { Templates } from './designer-templates'
import { GuidedChange, type GuidedTask } from './designer-wizards'
import { trace, traceExplanation } from './designer-trace'
import { Badge, ChangeList, CodeBlock, Confirm, Empty, Field, MutationResult, PageHeader, Problem, ReadStamp, Remaining, TimeOfDay, useFocus } from './ui'
import { controls } from './provisional'
import type { ViewProps } from './views'

type Mode = 'draft' | 'confirmed' | 'provisional'
type Tab = 'canvas' | 'form' | 'json'
interface Staged {
  receipt: Outcome
  candidate: Candidate
}

// What each view of the policy is for, said once under the view tabs.
const viewNotes: Record<Tab, string> = {
  canvas: 'Map of roles, endpoints, APIs and service addresses. Select a role to see what it may call.',
  form: 'Edit each role, endpoint, API and service address, or tick permissions in the matrix.',
  json: 'The complete policy document, to copy, download or load from a file.',
}

export function Designer({
  admin,
  can,
  open,
  draft,
  setDraft,
}: ViewProps & { draft: DesignerDraft | null; setDraft: Dispatch<SetStateAction<DesignerDraft | null>> }) {
  const state = useGatewayState()
  const facts = state.facts
  const policy = useResource<Revision>((signal) => admin.get('/admin/policy', signal), [])
  const overrides = useResource<OverridesView>((signal) => admin.get('/admin/overrides', signal), [])
  const provisional = useResource<ProvisionalView>((signal) => admin.get('/admin/provisional', signal), [])
  const pendingId = facts?.provisional
  const provisionalDocument = useResource<Revision>(
    (signal) => (pendingId !== undefined ? admin.get(`/admin/revisions/${pendingId}`, signal) : Promise.resolve({ kind: 'aborted' })),
    [pendingId],
  )
  const [mode, setMode] = useState<Mode>('draft')
  const [tab, setTab] = useState<Tab>('canvas')
  const [selection, setSelection] = useState<BoxKey | null>(null)
  const [json, setJson] = useState('')
  const [jsonError, setJsonError] = useState('')
  const [buffers, setBuffers] = useState<Record<string, string>>({})
  const [expanded, setExpanded] = useState(false)
  const [showLive, setShowLive] = useState(true)
  const [showTrace, setShowTrace] = useState(false)
  const [wizard, setWizard] = useState<{ task: GuidedTask; api?: string } | null>(null)
  const [who, setWho] = useState('none')
  const [what, setWhat] = useState('')
  const [stageConfirm, setStageConfirm] = useState(false)
  const [discardConfirm, setDiscardConfirm] = useState(false)
  const [deleteKey, setDeleteKey] = useState<BoxKey | null>(null)
  const [refused, setRefused] = useState<{ box?: BoxKey; edge?: string; code: string }>()
  const [showChanges, setShowChanges] = useState(false)
  const [checkMessage, setCheckMessage] = useState('')
  const [templateNote, setTemplateNote] = useState('')
  const review = useRef<HTMLButtonElement>(null)
  const submitted = useRef<Policy | null>(null)

  const current = policy.reply?.kind === 'ok' ? policy.reply.body : undefined
  const confirmed = current?.document
  const none = policy.reply?.kind === 'refused' && policy.reply.error === 'no_confirmed_policy'
  const document =
    mode === 'draft'
      ? draft?.document
      : mode === 'confirmed'
        ? confirmed
        : provisionalDocument.reply?.kind === 'ok'
          ? provisionalDocument.reply.body.document
          : undefined
  const editable = can('manage_policy') && mode === 'draft'
  const dirty = !!draft && canonical(draft.document) !== canonical(draft.base)
  const changed = useMemo(() => (draft ? changes(draft.base, draft.document) : []), [draft])
  const issues = useMemo(
    () =>
      document ? [...localIssues(document), ...Object.entries(buffers).map(([field, message]) => ({ id: 'shape', field, message }))] : [],
    [document, buffers],
  )
  const live = useMemo(
    () => (showLive && overrides.reply?.kind === 'ok' ? overrides.reply.body.overrides.filter((item) => item.status === 'active') : []),
    [showLive, overrides.reply],
  )
  const pending = provisional.reply?.kind === 'ok' ? provisional.reply.body : undefined
  const refresh = () => {
    policy.refresh()
    overrides.refresh()
    provisional.refresh()
    if (pendingId !== undefined) provisionalDocument.refresh()
    state.refresh()
  }
  const mutation = useMutation<Staged>(admin, refresh)
  const phase = mutation.phase

  useEffect(() => {
    if (!draft && current?.document) {
      const etag = policy.reply?.kind === 'ok' ? policy.reply.etag : null
      setDraft({
        document: copy(current.document),
        base: copy(current.document),
        baseRevision: Number(etag?.match(/^"r(\d+)"$/)?.[1] ?? current.id),
      })
    }
  }, [draft, current, policy.reply, setDraft])
  // The confirmed policy moves on after an activation or a rollback. A draft
  // with nothing of its own to keep follows it, so no stale change is staged
  // later; a draft with its own changes is kept and the notice below says so.
  useEffect(() => {
    if (!draft || !current?.document || draft.baseRevision === current.id) return
    const document = canonical(draft.document)
    if (document === canonical(draft.base) || document === canonical(current.document)) {
      setDraft({ document: copy(current.document), base: copy(current.document), baseRevision: current.id })
    }
  }, [draft, current, setDraft])
  const serialised = document ? JSON.stringify(document, null, 2) : ''
  useEffect(() => {
    setJson(serialised)
    setJsonError('')
  }, [serialised, mode])
  useEffect(() => {
    if (!dirty && !(editable && tab === 'json' && json !== serialised)) return
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty, editable, tab, json, serialised])
  useEffect(() => {
    if (mode === 'provisional' && pendingId === undefined) setMode('confirmed')
  }, [mode, pendingId])
  useEffect(() => {
    if (phase.state !== 'replied') return
    if (phase.reply.kind === 'ok') requestAnimationFrame(() => review.current?.focus())
    if (phase.reply.kind !== 'refused') return
    const field = phase.reply.outcome?.field ?? phase.reply.field ?? ''
    const code = phase.reply.outcome?.reason ?? phase.reply.error
    if (phase.reply.status === 412) {
      policy.refresh()
      setShowChanges(false)
    }
    const box = submitted.current ? fieldBox(submitted.current, field) : null
    const permitIndex = /^permits\[(\d+)\]/.exec(field)
    const permit = permitIndex && submitted.current ? submitted.current.permits[Number(permitIndex[1])] : undefined
    setRefused({ code, box: box ?? undefined, edge: permit ? `permit:${permit.role}:${permit.operation}` : undefined })
    if (box) {
      setSelection(box)
      setTab('canvas')
      setShowTrace(false)
    }
  }, [phase])

  const select = useCallback((key: BoxKey) => {
    setSelection(key)
    if (key.startsWith('operation:')) setWhat(splitKey(key)[1])
  }, [])
  // A link from another page opens the policy on one box: the canvas shows it
  // selected, its group open, with its editor beside it.
  const linked = useFocus('select')
  const linkedReady = !!draft
  useEffect(() => {
    if (!linked || !draft) return
    const [kind, id] = splitKey(linked as BoxKey)
    const known: Record<string, { id: string }[]> = {
      role: draft.document.roles,
      operation: draft.document.operations,
      api: draft.document.apis,
      upstream: draft.document.upstreams,
    }
    if (!known[kind]?.some((item) => item.id === id)) return
    setTab('canvas')
    setShowTrace(false)
    select(linked as BoxKey)
  }, [linked, linkedReady])
  function update(next: Policy) {
    if (editable) {
      setDraft((previous) => (previous ? { ...previous, document: next } : previous))
      setRefused(undefined)
    }
  }
  function onBufferIssue(key: string, issue: string | null) {
    setBuffers((previous) => {
      if ((!issue && !(key in previous)) || previous[key] === issue) return previous
      const next = { ...previous }
      if (issue) next[key] = issue
      else delete next[key]
      return next
    })
  }
  function renameBox(before: BoxKey, after: BoxKey) {
    if (selection === before) setSelection(after)
    setBuffers((previous) =>
      Object.fromEntries(
        Object.entries(previous).map(([key, value]) => [key.startsWith(`${before}.`) ? after + key.slice(before.length) : key, value]),
      ),
    )
  }
  function addBox(kind: BoxKind) {
    if (!document || !editable) return
    const added = add(document, kind)
    if (added) {
      update(added.document)
      setSelection(added.key)
      setShowTrace(false)
    }
  }
  // addActions are what a group's + offers: a blank box opened in the side
  // form, or a guided change that starts in that group.
  function addActions(kind: BoxKind, api?: string): GroupAction[] {
    if (!document || !editable) return []
    const noApi = document.apis.length ? undefined : 'Add an API first.'
    switch (kind) {
      case 'role':
        return [
          { label: 'Add Role', adds: 'role', run: () => addBox('role') },
          {
            label: 'Give a Role Access',
            run: () => setWizard({ task: 'access' }),
            unavailable: !document.roles.length ? 'Add a role first.' : !document.operations.length ? 'Add an endpoint first.' : undefined,
          },
        ]
      case 'operation':
        return [
          { label: 'Add Endpoint', run: () => setWizard({ task: 'endpoint', api }), unavailable: noApi },
          { label: 'Change a Rate Limit', run: () => setWizard({ task: 'rate', api }), unavailable: document.operations.length ? undefined : 'Add an endpoint first.' },
        ]
      case 'api':
        return [{ label: 'Add API', adds: 'api', run: () => addBox('api'), unavailable: document.upstreams.length ? undefined : 'Add a service address first.' }]
      case 'upstream':
        return [{ label: 'Add Service Address', adds: 'upstream', run: () => addBox('upstream') }]
    }
  }
  function applyJson(): boolean {
    if (!editable || !draft || json === serialised) return true
    try {
      const parsed: unknown = JSON.parse(json)
      const issue = shapeIssue(parsed)
      if (issue) {
        setJsonError(`${issue.field}: ${issue.message}`)
        return false
      }
      update(parsed as Policy)
      setBuffers({})
      setJsonError('')
      return true
    } catch (error) {
      setJsonError(`$: ${error instanceof Error ? error.message : 'Enter valid JSON.'}`)
      return false
    }
  }
  function changeTab(next: Tab) {
    if (tab !== 'json' || applyJson()) {
      setTab(next)
      requestAnimationFrame(() => window.document.getElementById(`designer-tab-${next}`)?.focus())
    }
  }
  function changeMode(next: Mode) {
    if (tab === 'json' && !applyJson()) return
    setMode(next)
    setBuffers({})
    setRefused(undefined)
    setSelection(null)
  }
  async function checkCandidates() {
    setCheckMessage('Reading draft changes…')
    const answer = await admin.get<Page<Candidate>>('/admin/candidates')
    setCheckMessage(
      answer.kind === 'ok'
        ? `${answer.body.items.length} draft changes listed. Look for this draft on the Changes page; until you find it, whether it was saved stays unknown.`
        : 'The draft changes could not be read. Whether this draft was saved stays unknown; try Check again.',
    )
  }
  const currentOperation = document?.operations.find((item) => item.id === what) ?? document?.operations[0]
  const steps = document && currentOperation ? trace(document, who, currentOperation) : []
  const traceRoles = who.startsWith('role:') ? [who.slice(5)] : []
  const tracePath: TracePath | undefined =
    showTrace && currentOperation
      ? { operation: currentOperation.id, roles: traceRoles, forward: steps.find((step) => step.name === 'Forward')?.state === 'passes' }
      : undefined
  const stale = current && draft && current.id !== draft.baseRevision
  const formProps = document
    ? { policy: document, editable, onChange: update, onRename: renameBox, onDelete: setDeleteKey, onBufferIssue }
    : null
  const canStage =
    editable &&
    dirty &&
    issues.length === 0 &&
    !mutation.pending &&
    !(phase.state === 'replied' && phase.reply.kind === 'unknown') &&
    !(tab === 'json' && json !== serialised)
  const canDiscard =
    !!draft && (dirty || draft.baseRevision !== current?.id || (tab === 'json' && json !== serialised) || Object.keys(buffers).length > 0)

  return (
    <>
      <PageHeader
        title="Policy"
        lede="Define which roles may call each endpoint and where each API forwards requests."
      >
        <ReadStamp okAt={policy.last?.at ?? null} failed={policy.failed} loading={policy.loading || overrides.loading || provisional.loading} onRefresh={refresh} />
      </PageHeader>
      {none ? (
        <Empty title="Establish the first policy before designing changes.">
          <button className="btn" type="button" onClick={() => open('candidates')}>
            Go to Changes
          </button>
        </Empty>
      ) : null}
      {policy.reply && policy.reply.kind !== 'ok' && !none ? (
        <>
          <Problem reply={policy.reply} />
          <button className="btn" type="button" onClick={refresh}>
            Retry
          </button>
        </>
      ) : null}
      {!none && current && document && draft ? (
        <>
          {pending ? (
            <div className="notice tone-pending">
              Version {pending.activation.revision} is on trial until <TimeOfDay value={pending.activation.deadline} seconds /> (
              <Remaining until={pending.activation.deadline} onPassed={refresh} />
              ). Another change can go on trial once it is kept or undone.
            </div>
          ) : null}
          {overrides.reply && overrides.reply.kind !== 'ok' ? (
            <>
              <Problem reply={overrides.reply} />
              <button className="btn btn-small" type="button" onClick={overrides.refresh}>
                Retry Live Overrides
              </button>
            </>
          ) : null}
          {provisional.reply &&
          provisional.reply.kind !== 'ok' &&
          !(provisional.reply.kind === 'refused' && provisional.reply.status === 404) ? (
            <>
              <Problem reply={provisional.reply} />
              <button className="btn btn-small" type="button" onClick={provisional.refresh}>
                Retry Reading the Trial
              </button>
            </>
          ) : null}
          {/* One command row: the views on the left with what each is for, and
              which version is shown, the draft's state and its actions on the
              right, so the canvas below can take the rest of the window. */}
          <div className="designer-bar">
            <div className="designer-views">
                <div className="section-tabs designer-view-tabs" role="tablist" aria-label="Policy views">
                  {(['canvas', 'form', 'json'] as Tab[]).map((choice, index, tabs) => (
                    <button
                      id={`designer-tab-${choice}`}
                      role="tab"
                      type="button"
                      key={choice}
                      aria-selected={tab === choice}
                      aria-controls="designer-view"
                      tabIndex={tab === choice ? 0 : -1}
                      onClick={() => changeTab(choice)}
                      onKeyDown={(event) => {
                        let next: Tab | undefined
                        if (event.key === 'ArrowRight') next = tabs[(index + 1) % tabs.length]
                        if (event.key === 'ArrowLeft') next = tabs[(index + tabs.length - 1) % tabs.length]
                        if (event.key === 'Home') next = tabs[0]
                        if (event.key === 'End') next = tabs[tabs.length - 1]
                        if (next) {
                          event.preventDefault()
                          changeTab(next)
                        }
                      }}
                    >
                      {choice === 'json' ? 'JSON' : choice[0].toUpperCase() + choice.slice(1)}
                    </button>
                  ))}
                </div>
              <p className="designer-view-note">{viewNotes[tab]}</p>
            </div>
            <div className="designer-bar-actions">
              <label className="designer-viewing">
                <span>Viewing</span>
                <select className="input" value={mode} onChange={(event) => changeMode(event.target.value as Mode)}>
                  <option value="draft">Draft</option>
                  <option value="confirmed">Active · Version {current.id}</option>
                  {pendingId !== undefined ? <option value="provisional">On Trial · Version {pendingId}</option> : null}
                </select>
              </label>
              <Badge tone={issues.length ? 'pending' : 'ok'}>{issues.length ? `${issues.length} ${issues.length === 1 ? 'Issue' : 'Issues'}` : 'No Issues'}</Badge>
              {can('manage_policy') ? (
                <>
                  <p className={`designer-draft-state${dirty ? ' is-dirty' : ''}`} role="status">
                    {mode !== 'draft'
                      ? 'Edits go to the draft.'
                      : dirty
                        ? `${changed.length} unsaved ${changed.length === 1 ? 'change' : 'changes'}`
                        : `No changes to version ${draft.baseRevision}`}
                  </p>
                  <button className="btn btn-danger-quiet" type="button" disabled={!canDiscard || mutation.pending} onClick={() => setDiscardConfirm(true)}>
                    Discard
                  </button>
                  <button className="btn btn-primary" type="button" disabled={!canStage} onClick={() => setStageConfirm(true)}>
                    Save Changes
                  </button>
                </>
              ) : null}
            </div>
          </div>
          {wizard && editable && document ? (
            <GuidedChange
              policy={document}
              task={wizard.task}
              api={wizard.api}
              close={() => setWizard(null)}
              apply={(next, summary) => {
                setWizard(null)
                update(next)
                setTemplateNote(`Applied to the draft: ${summary} Nothing is saved: review it, then choose Save Changes.`)
              }}
            />
          ) : null}
          {templateNote && mode === 'draft' ? (
            <p role="status" className="hint">
              {templateNote}
            </p>
          ) : null}
          {!can('manage_policy') ? (
            <p className="notice tone-neutral">
              Read-only. Editing needs the “Change the policy” permission (manage_policy).
            </p>
          ) : null}
          {tab === 'json' && editable && json !== serialised ? (
            <p className="hint">Apply the JSON to update the draft before saving.</p>
          ) : null}
          {stale ? (
            <section className="notice tone-pending">
              <p>
                The active policy changed to version {current.id} since your draft was started from version {draft.baseRevision}. Your draft keeps
                its own changes; saving it unchanged would be refused.
              </p>
              <div className="actions">
                <button className="btn" type="button" aria-expanded={showChanges} onClick={() => setShowChanges((value) => !value)}>
                  Show What Changed
                </button>
                <button
                  className="btn"
                  type="button"
                  onClick={() => {
                    setDraft({ ...draft, base: copy(confirmed!), baseRevision: current.id })
                    mutation.clear()
                    setShowChanges(false)
                  }}
                >
                  Keep my draft and base it on r{current.id}
                </button>
              </div>
              {showChanges ? <ChangeList changes={differences(draft.base, confirmed!)} /> : null}
            </section>
          ) : null}
          <MutationResult
            mutation={mutation}
            onRefresh={refresh}
            unknown={
              phase.state === 'replied' && phase.reply.kind === 'unknown' ? (
                <div role="alert" className="notice tone-unknown">
                  <p>No answer from the gateway: the draft may or may not have been saved.</p>
                  <button className="btn" type="button" onClick={() => void checkCandidates()}>
                    Check
                  </button>{' '}
                  <button className="btn" type="button" onClick={() => void mutation.run(phase.request)}>
                    Send the Same Request Again
                  </button>
                  {checkMessage ? <p>{checkMessage}</p> : null}
                </div>
              ) : undefined
            }
            success={(body) => (
              <>
                <p className="notice-title">The gateway checked it: saved as draft change {body.candidate.id}. Nothing is active yet.</p>
                <button ref={review} className="btn" type="button" onClick={() => open('candidates', { candidate: body.candidate.id })}>
                  Preview Its Impact and Put It on Trial
                </button>
              </>
            )}
          />
          {/* Issues stand above the work, where they are seen before saving. */}
          <Issues
            issues={issues}
            onSelect={(key) => {
              select(key)
              if (tab === 'json') changeTab('canvas')
            }}
          />
          {showLive && live.length ? (
            <ul className="designer-live" aria-label="Active temporary rules">
              {live.map((override) => (
                <li key={override.id}>
                  <Badge tone={override.type === 'containment' ? 'refused' : 'pending'}>Temporary</Badge>{' '}
                  {override.type === 'containment' ? 'Restricts' : 'Allows'} {override.role ? `${override.role} · ` : ''}
                  <code>{override.operation}</code> until {new Date(override.expires_at).toLocaleTimeString()} ·{' '}
                  <Remaining until={override.expires_at} onPassed={refresh} />
                </li>
              ))}
            </ul>
          ) : null}
          <div id="designer-view" role="tabpanel" aria-labelledby={`designer-tab-${tab}`}>
            {tab === 'canvas' ? (
              <div className={`designer-workspace${expanded ? ' is-expanded' : ''}`}>
                <div className="designer-canvas-area">
                  <DesignerCanvas
                    policy={document}
                    editable={editable}
                    selection={selection}
                    issues={issues}
                    overlays={live}
                    provisional={
                      mode === 'confirmed' && pending ? { revision: pending.activation.revision, changes: pending.changes } : undefined
                    }
                    refused={refused}
                    trace={tracePath}
                    expanded={expanded}
                    panelOpen={showTrace || (selection !== null && formProps !== null)}
                    onExpand={() => setExpanded((value) => !value)}
                    onSelect={select}
                    onDelete={setDeleteKey}
                    actions={addActions}
                    tools={
                      <>
                        <button
                          className="btn btn-small"
                          type="button"
                          aria-pressed={showTrace}
                          onClick={() => setShowTrace((value) => !value)}
                        >
                          Trace
                        </button>
                        <label className="designer-option">
                          <input type="checkbox" checked={showLive} onChange={(event) => setShowLive(event.target.checked)} /> Temporary Rules
                        </label>
                        {editable ? (
                          <Templates
                            policy={document}
                            apply={(next, summary) => {
                              update(next)
                              setTemplateNote(`Template applied to the draft: ${summary} Nothing is saved: review it, then choose Save Changes.`)
                            }}
                          />
                        ) : null}
                      </>
                    }
                  />
                </div>
                {showTrace || (selection && formProps) ? (
                  <aside className="designer-side-panel" aria-label={showTrace ? 'Trace policy document' : 'Selected policy box'}>
                    <button
                      type="button"
                      className="btn btn-small designer-panel-close"
                      onClick={() => (showTrace ? setShowTrace(false) : setSelection(null))}
                    >
                      Close
                    </button>
                    {showTrace ? (
                      <TracePanel
                        policy={document}
                        who={who}
                        setWho={setWho}
                        what={currentOperation?.id ?? ''}
                        setWhat={(operation) => {
                          setWhat(operation)
                          setSelection(`operation:${operation}`)
                        }}
                        steps={steps}
                      />
                    ) : selection && formProps ? (
                      <ObjectForm {...formProps} selection={selection} inPanel />
                    ) : null}
                  </aside>
                ) : null}
              </div>
            ) : null}
            {tab === 'form' && formProps ? <FormView {...formProps} actions={addActions} selection={selection} onSelect={select} /> : null}
            {tab === 'json' ? (
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
            ) : null}
          </div>
          <Confirm
            open={stageConfirm}
            title="Save this as a draft change?"
            confirmLabel="Save Draft Change"
            onCancel={() => setStageConfirm(false)}
            onConfirm={() => {
              setStageConfirm(false)
              if (!canStage) return
              submitted.current = copy(draft.document)
              void mutation.run({
                method: 'POST',
                path: '/admin/candidates',
                rawBody: '{"policy":' + JSON.stringify(draft.document) + '}',
                ifMatch: `"r${draft.baseRevision}"`,
                key: newKey(),
              })
            }}
          >
            <p>The gateway checks the whole document. Nothing is active until the draft change is put on trial and kept.</p>
            <ul>
              {changed.map((change) => (
                <li key={`${change.control}:${change.target}`}>
                  {controls[change.control] ?? change.control}: <code>{change.target}</code>
                </li>
              ))}
            </ul>
          </Confirm>
          <Confirm
            open={discardConfirm}
            title="Discard your draft?"
            confirmLabel="Discard Draft"
            danger
            onCancel={() => setDiscardConfirm(false)}
            onConfirm={() => {
              setDiscardConfirm(false)
              setDraft({ document: copy(confirmed!), base: copy(confirmed!), baseRevision: current.id })
              setJson(JSON.stringify(confirmed, null, 2))
              setJsonError('')
              setBuffers({})
              mutation.clear()
              setRefused(undefined)
              setTemplateNote('')
            }}
          >
            <p>Your changes are lost.</p>
          </Confirm>
          <Confirm
            open={deleteKey !== null}
            title={`Delete ${deleteKey ? splitKey(deleteKey).join(' ') : 'box'}?`}
            confirmLabel="Delete Box"
            danger
            onCancel={() => setDeleteKey(null)}
            onConfirm={() => {
              if (deleteKey) {
                update(remove(document, deleteKey))
                setBuffers((previous) => Object.fromEntries(Object.entries(previous).filter(([key]) => !key.startsWith(`${deleteKey}.`))))
                if (selection === deleteKey) setSelection(null)
              }
              setDeleteKey(null)
            }}
          >
            <p>
              Removes the box and its permissions from this draft. The gateway is unchanged until the draft is saved and put on trial.
            </p>
          </Confirm>
        </>
      ) : null}
      {mode === 'provisional' && pendingId !== undefined && provisionalDocument.reply && provisionalDocument.reply.kind !== 'ok' ? (
        <>
          <Problem reply={provisionalDocument.reply} />
          <button type="button" className="btn" onClick={provisionalDocument.refresh}>
            Retry Reading the Version on Trial
          </button>
        </>
      ) : null}
    </>
  )
}

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

function TracePanel({
  policy,
  who,
  setWho,
  what,
  setWhat,
  steps,
}: {
  policy: Policy
  who: string
  setWho: (value: string) => void
  what: string
  setWhat: (value: string) => void
  steps: ReturnType<typeof trace>
}) {
  return (
    <section className="designer-trace">
      <h2>Trace</h2>
      <Field id="designer-trace-who" label="Who">
        <select className="input" id="designer-trace-who" value={who} onChange={(event) => setWho(event.target.value)}>
          <option value="none">no token</option>
          {policy.roles.map((role) => (
            <option value={`role:${role.id}`} key={`role:${role.id}`}>
              a token with role {role.id}
            </option>
          ))}
        </select>
      </Field>
      <Field id="designer-trace-what" label="What">
        <select className="input" id="designer-trace-what" value={what} onChange={(event) => setWhat(event.target.value)}>
          {policy.operations.map((operation) => (
            <option value={operation.id} key={operation.id}>
              {operation.id}: {operation.method} {operation.target}
            </option>
          ))}
        </select>
      </Field>
      {steps.length ? (
        <ol className="designer-trace-steps">
          {steps.map((step) => (
            <li key={step.name}>
              <strong>{step.name}</strong>{' '}
              {step.state === 'not reached' ? (
                <span className="hint">not reached</span>
              ) : (
                <Badge tone={step.state === 'passes' ? 'ok' : step.state === 'refused' ? 'refused' : 'neutral'}>{step.state}</Badge>
              )}
              {step.state === 'not reached' ? null : <p>{step.text}</p>}
            </li>
          ))}
        </ol>
      ) : (
        <p className="hint">Add an endpoint to trace a request.</p>
      )}
      <p className="notice tone-neutral">{traceExplanation}</p>
    </section>
  )
}
