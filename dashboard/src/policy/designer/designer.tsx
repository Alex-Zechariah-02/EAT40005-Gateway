import { useCallback, useEffect, useMemo, useRef, useState, type Dispatch, type SetStateAction } from 'react'
import type { Candidate, OverridesView, Page, Policy, ProvisionalView, Revision } from '../../shared/api'
import { useGatewayState, useMutation, useResource } from '../../shared/hooks'
import { DesignerCanvas } from './canvas'
import type { TracePath } from './graph'
import { FormView, Issues } from './form'
import {
  add,
  canonical,
  changes,
  copy,
  fieldBox,
  localIssues,
  shapeIssue,
  splitKey,
  type BoxKey,
  type BoxKind,
  type GroupAction,
  type DesignerDraft,
} from './model'
import { Templates } from './templates'
import { GuidedChange, type GuidedTask } from './wizards'
import { trace } from './trace'
import { Badge, Empty } from '../../shared/ui'
import { MutationResult, Problem } from '../../shared/replies'
import { PageHeader } from '../../app/header'
import { ReadStamp, Remaining, TimeOfDay } from '../../shared/time'
import { useFocus } from '../../shared/links'
import type { ViewProps } from '../../app/views'
import { DesignerBar, type Mode, SidePanel, type Staged, StaleNotice, type Tab } from './panels'
import { JsonView } from './json'
import { DeleteDialog, DiscardDialog, SaveDialog } from './prompts'

// The Policy page's Designer: one draft policy shown as a canvas, a form or JSON.

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
      ? { operation: currentOperation.id, roles: traceRoles, forward: steps.find((step) => step.name === 'Forward')?.state === 'passed' }
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
          <button className="btn" type="button" onClick={() => open('changes')}>
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
          <DesignerBar
            can={can}
            draft={draft}
            pendingId={pendingId}
            mode={mode}
            tab={tab}
            setStageConfirm={setStageConfirm}
            setDiscardConfirm={setDiscardConfirm}
            current={current}
            dirty={dirty}
            changed={changed}
            issues={issues}
            mutation={mutation}
            changeTab={changeTab}
            changeMode={changeMode}
            canStage={canStage}
            canDiscard={canDiscard}
          />
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
            <StaleNotice
              draft={draft}
              setDraft={setDraft}
              showChanges={showChanges}
              setShowChanges={setShowChanges}
              current={current}
              confirmed={confirmed}
              mutation={mutation}
            />
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
                <button ref={review} className="btn" type="button" onClick={() => open('changes', { candidate: body.candidate.id })}>
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
                  <SidePanel
                    selection={selection}
                    setSelection={setSelection}
                    showTrace={showTrace}
                    setShowTrace={setShowTrace}
                    who={who}
                    setWho={setWho}
                    setWhat={setWhat}
                    document={document}
                    currentOperation={currentOperation}
                    steps={steps}
                    formProps={formProps}
                  />
                ) : null}
              </div>
            ) : null}
            {tab === 'form' && formProps ? <FormView {...formProps} actions={addActions} selection={selection} onSelect={select} /> : null}
            {tab === 'json' ? (
              <JsonView
                draft={draft}
                pendingId={pendingId}
                mode={mode}
                json={json}
                setJson={setJson}
                jsonError={jsonError}
                setJsonError={setJsonError}
                current={current}
                editable={editable}
                applyJson={applyJson}
              />
            ) : null}
          </div>
          <SaveDialog
            draft={draft}
            stageConfirm={stageConfirm}
            setStageConfirm={setStageConfirm}
            submitted={submitted}
            changed={changed}
            mutation={mutation}
            canStage={canStage}
          />
          <DiscardDialog
            setDraft={setDraft}
            setJson={setJson}
            setJsonError={setJsonError}
            setBuffers={setBuffers}
            discardConfirm={discardConfirm}
            setDiscardConfirm={setDiscardConfirm}
            setRefused={setRefused}
            setTemplateNote={setTemplateNote}
            current={current}
            confirmed={confirmed}
            mutation={mutation}
          />
          <DeleteDialog
            selection={selection}
            setSelection={setSelection}
            setBuffers={setBuffers}
            deleteKey={deleteKey}
            setDeleteKey={setDeleteKey}
            document={document}
            update={update}
          />
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
