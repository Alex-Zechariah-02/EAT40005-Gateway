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
import { forgetPositions, readPositions, writePositions } from './designer-layout'
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
  kindNames,
  type DesignerDraft,
  type Positions,
} from './designer-model'
import { closeOtherMenus, Templates } from './designer-templates'
import { StepByStep } from './designer-wizards'
import { trace, traceExplanation } from './designer-trace'
import { Badge, ChangeList, Confirm, Empty, Field, MutationResult, PageHeader, Problem, ReadStamp, Remaining } from './ui'
import { controls } from './provisional'
import type { ViewProps } from './views'

type Mode = 'draft' | 'confirmed' | 'provisional'
type Tab = 'canvas' | 'form' | 'json'
interface Staged {
  receipt: Outcome
  candidate: Candidate
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
  const [section, setSection] = useState<string | undefined>()
  const [json, setJson] = useState('')
  const [jsonError, setJsonError] = useState('')
  const [buffers, setBuffers] = useState<Record<string, string>>({})
  const [positions, setPositions] = useState<Positions>(readPositions)
  const [reset, setReset] = useState(0)
  const [showLive, setShowLive] = useState(true)
  const [showTrace, setShowTrace] = useState(false)
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
  const positionsTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
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
  useEffect(
    () => () => {
      if (positionsTimer.current) clearTimeout(positionsTimer.current)
    },
    [],
  )
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

  const select = useCallback((key: BoxKey, check?: string) => {
    setSelection(key)
    setSection(check)
    if (key.startsWith('operation:')) setWhat(splitKey(key)[1])
  }, [])
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
  function renamePosition(before: BoxKey, after: BoxKey, policy: Policy) {
    if (selection === before) setSelection(after)
    if (positionsTimer.current) clearTimeout(positionsTimer.current)
    if (before in positions) {
      const next = { ...positions, [after]: positions[before] }
      delete next[before]
      setPositions(next)
      writePositions(policy, next)
    }
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
    ? { policy: document, editable, onChange: update, onRename: renamePosition, onDelete: setDeleteKey, onBufferIssue }
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
        lede="The policy as a picture: who may call which endpoint, the checks each request meets in the gateway's fixed order, and where it is forwarded. Edit a draft here and save it as a draft change; nothing is in force until a trial of it is kept."
      >
        <a className="btn btn-small" href="#/policy">
          Versions and tables
        </a>
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
              Version {pending.activation.revision} is on trial until {new Date(pending.activation.deadline).toLocaleString()}. Putting another change
              on trial is refused until it is kept or undone.
              <Remaining until={pending.activation.deadline} onPassed={refresh} />
            </div>
          ) : null}
          {overrides.reply && overrides.reply.kind !== 'ok' ? (
            <>
              <Problem reply={overrides.reply} />
              <button className="btn btn-small" type="button" onClick={overrides.refresh}>
                Retry live overrides
              </button>
            </>
          ) : null}
          {provisional.reply &&
          provisional.reply.kind !== 'ok' &&
          !(provisional.reply.kind === 'refused' && provisional.reply.status === 404) ? (
            <>
              <Problem reply={provisional.reply} />
              <button className="btn btn-small" type="button" onClick={provisional.refresh}>
                Retry reading the trial
              </button>
            </>
          ) : null}
          <div className="designer-toolbar">
            <div className="segmented" aria-label="Policy mode">
              {(['draft', 'confirmed', ...(pendingId !== undefined ? ['provisional'] : [])] as Mode[]).map((choice) => (
                <button className="btn" type="button" key={choice} aria-pressed={mode === choice} onClick={() => changeMode(choice)}>
                  {choice === 'draft' ? 'Draft' : choice === 'confirmed' ? `In force · version ${current.id}` : `On trial · version ${pendingId}`}
                </button>
              ))}
            </div>
            <p className="designer-draft-state">
              {dirty
                ? `Unsaved draft · ${changed.length} ${changed.length === 1 ? 'change' : 'changes'} against version ${draft.baseRevision}`
                : `No draft changes against version ${draft.baseRevision}`}
            </p>
            <div className="designer-toolbar-actions">
              <button
                className="btn"
                type="button"
                aria-pressed={showTrace}
                onClick={() => {
                  if (tab === 'json' && !applyJson()) return
                  setTab('canvas')
                  setShowTrace((value) => !value)
                }}
              >
                Trace
              </button>
              <label className="designer-option">
                <input type="checkbox" checked={showLive} onChange={(event) => setShowLive(event.target.checked)} /> Show what is in force
              </label>
              {editable ? (
                <details className="designer-add" onToggle={closeOtherMenus}>
                  <summary className="btn">Add</summary>
                  <div>
                    {(['role', 'operation', 'api', 'upstream'] as BoxKind[]).map((kind) => (
                      <div key={kind}>
                        <button
                          className="btn btn-small"
                          type="button"
                          disabled={(kind === 'operation' && !document.apis.length) || (kind === 'api' && !document.upstreams.length)}
                          onClick={(event) => {
                            addBox(kind)
                            event.currentTarget.closest('details')?.removeAttribute('open')
                          }}
                        >
                          Add {kindNames[kind].one}
                        </button>
                        {kind === 'operation' && !document.apis.length ? (
                          <p className="hint">Add an API first.</p>
                        ) : kind === 'api' && !document.upstreams.length ? (
                          <p className="hint">Add a service address first.</p>
                        ) : null}
                      </div>
                    ))}
                  </div>
                </details>
              ) : null}
              {editable && document ? (
                <>
                  <StepByStep
                    policy={document}
                    apply={(next, summary) => {
                      update(next)
                      setTemplateNote(`Applied to the draft: ${summary} Nothing is saved: review it, then choose Save draft change.`)
                    }}
                  />
                  <Templates
                    policy={document}
                    apply={(next, summary) => {
                      update(next)
                      setTemplateNote(`Template applied to the draft: ${summary} Nothing is saved: review it, then choose Save draft change.`)
                    }}
                  />
                </>
              ) : null}
              {can('manage_policy') ? (
                <>
                  <button
                    className="btn btn-danger-quiet"
                    type="button"
                    disabled={!canDiscard || mutation.pending}
                    onClick={() => setDiscardConfirm(true)}
                  >
                    Discard draft
                  </button>
                  <button className="btn btn-primary" type="button" disabled={!canStage} onClick={() => setStageConfirm(true)}>
                    Save draft change
                  </button>
                </>
              ) : null}
            </div>
          </div>
          {templateNote && mode === 'draft' ? (
            <p role="status" className="hint">
              {templateNote}
            </p>
          ) : null}
          {!can('manage_policy') ? (
            <p className="notice tone-neutral">
              Read-only administrator: you can inspect the policy in every view. Changing it needs the “Change the policy” permission (manage_policy).
            </p>
          ) : null}
          {tab === 'json' && editable && json !== serialised ? (
            <p className="hint">Apply the JSON before saving the draft change. The draft stays unchanged until it is applied.</p>
          ) : null}
          {stale ? (
            <section className="notice tone-pending">
              <p>
                The policy in force changed to version {current.id} since your draft was started from version {draft.baseRevision}. Your draft keeps
                its own changes; saving it unchanged would be refused.
              </p>
              <div className="actions">
                <button className="btn" type="button" aria-expanded={showChanges} onClick={() => setShowChanges((value) => !value)}>
                  Show what changed
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
                    Send the same request again
                  </button>
                  {checkMessage ? <p>{checkMessage}</p> : null}
                </div>
              ) : undefined
            }
            success={(body) => (
              <>
                <p className="notice-title">The gateway checked it: saved as draft change {body.candidate.id}. Nothing is in force yet.</p>
                <button ref={review} className="btn" type="button" onClick={() => open('candidates', { candidate: body.candidate.id })}>
                  Preview its impact and put it on trial
                </button>
              </>
            )}
          />
          <div className="designer-tab-row">
            <div className="segmented" role="tablist" aria-label="Policy views">
              {(['canvas', 'form', 'json'] as Tab[]).map((choice, index, tabs) => (
                <button
                  id={`designer-tab-${choice}`}
                  role="tab"
                  className="btn"
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
            <Badge tone={issues.length ? 'pending' : 'neutral'}>Issues: {issues.length}</Badge>
          </div>
          <div id="designer-view" role="tabpanel" aria-labelledby={`designer-tab-${tab}`}>
            {tab === 'canvas' ? (
              <div className="designer-workspace">
                <div className="designer-canvas-area">
                  <DesignerCanvas
                    policy={document}
                    editable={editable}
                    selection={selection}
                    positions={positions}
                    issues={issues}
                    overlays={live}
                    provisional={
                      mode === 'confirmed' && pending ? { revision: pending.activation.revision, changes: pending.changes } : undefined
                    }
                    refused={refused}
                    trace={tracePath}
                    reset={reset}
                    onSelect={select}
                    onChange={update}
                    onDelete={setDeleteKey}
                    onPositions={(next) => {
                      setPositions(next)
                      if (positionsTimer.current) clearTimeout(positionsTimer.current)
                      positionsTimer.current = setTimeout(() => writePositions(document, next), 300)
                    }}
                    onReset={() => {
                      if (positionsTimer.current) clearTimeout(positionsTimer.current)
                      forgetPositions()
                      setPositions({})
                      setReset((value) => value + 1)
                    }}
                  />
                </div>
                <aside className="designer-side-panel" aria-label={showTrace ? 'Trace policy document' : 'Selected policy box'}>
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
                    <ObjectForm {...formProps} selection={selection} section={section} inPanel />
                  ) : (
                    <p className="hint">Select a box to see and change it.</p>
                  )}
                  <Issues issues={issues} onSelect={select} />
                  {showLive
                    ? live.map((override) => (
                        <p className="hint" key={override.id}>
                          {override.type} {override.control}: {override.role ? `${override.role} · ` : ''}
                          {override.operation}, until {new Date(override.expires_at).toLocaleTimeString()} ·{' '}
                          <Remaining until={override.expires_at} onPassed={refresh} />
                        </p>
                      ))
                    : null}
                </aside>
              </div>
            ) : null}
            {tab === 'form' && formProps ? (
              <>
                <FormView {...formProps} onAdd={addBox} />
                <Issues
                  issues={issues}
                  onSelect={(key) => {
                    select(key)
                    changeTab('canvas')
                  }}
                />
              </>
            ) : null}
            {tab === 'json' ? (
              <div className="designer-json">
                <Field id="designer-json-document" label="Complete policy document (JSON)">
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
              </div>
            ) : null}
          </div>
          <Confirm
            open={stageConfirm}
            title="Save this as a draft change?"
            confirmLabel="Save draft change"
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
            <p>The gateway checks the whole document. Nothing is in force until the draft change is put on trial and kept.</p>
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
            confirmLabel="Discard draft"
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
            confirmLabel="Delete box"
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
              The box and its permissions are removed from this draft. Nothing changes in the gateway until it is saved as a draft change and put on
              trial.
            </p>
          </Confirm>
        </>
      ) : null}
      {mode === 'provisional' && pendingId !== undefined && provisionalDocument.reply && provisionalDocument.reply.kind !== 'ok' ? (
        <>
          <Problem reply={provisionalDocument.reply} />
          <button type="button" className="btn" onClick={provisionalDocument.refresh}>
            Retry reading the version on trial
          </button>
        </>
      ) : null}
    </>
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
