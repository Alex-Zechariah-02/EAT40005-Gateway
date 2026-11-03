import type { Dispatch, RefObject, SetStateAction } from 'react'
import { newKey } from '../../shared/http'
import type { Policy, Revision } from '../../shared/api'
import type { Mutation } from '../../shared/hooks'
import { changes, copy, remove, splitKey, type BoxKey, type DesignerDraft } from './model'
import { Confirm } from '../../shared/dialogs'
import { controls } from '../../deploy/activation'
import type { Staged } from './panels'

// The Policy page's confirmations: save the draft, discard it, or delete one box.

// SaveDialog confirms saving the draft as a draft change, listing what it changes.
export function SaveDialog(props: {
  draft: DesignerDraft
  stageConfirm: boolean
  setStageConfirm: (value: boolean) => void
  submitted: RefObject<Policy | null>
  changed: ReturnType<typeof changes>
  mutation: Mutation<Staged>
  canStage: boolean
}) {
  const { draft, stageConfirm, setStageConfirm, submitted, changed, mutation, canStage } = props
  return (
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
  )
}

// DiscardDialog confirms discarding the draft and starting again from the active version.
export function DiscardDialog(props: {
  setDraft: Dispatch<SetStateAction<DesignerDraft | null>>
  setJson: (value: string) => void
  setJsonError: (value: string) => void
  setBuffers: Dispatch<SetStateAction<Record<string, string>>>
  discardConfirm: boolean
  setDiscardConfirm: (value: boolean) => void
  setRefused: Dispatch<SetStateAction<{ box?: BoxKey; edge?: string; code: string } | undefined>>
  setTemplateNote: (value: string) => void
  current: Revision
  confirmed: Policy | undefined
  mutation: Mutation<Staged>
}) {
  const { setDraft, setJson, setJsonError, setBuffers, discardConfirm, setDiscardConfirm, setRefused, setTemplateNote, current, confirmed, mutation } = props
  return (
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
  )
}

// DeleteDialog confirms deleting one box from the draft.
export function DeleteDialog(props: {
  selection: BoxKey | null
  setSelection: (value: BoxKey | null) => void
  setBuffers: Dispatch<SetStateAction<Record<string, string>>>
  deleteKey: BoxKey | null
  setDeleteKey: (value: BoxKey | null) => void
  document: Policy
  update: (next: Policy) => void
}) {
  const { selection, setSelection, setBuffers, deleteKey, setDeleteKey, document, update } = props
  return (
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
  )
}
