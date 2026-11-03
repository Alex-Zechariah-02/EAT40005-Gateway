import type { Candidate } from './api'
import { type Change } from './diff'
import { Badge, CodeBlock, MoreDetails } from './ui'
import { ChangeList } from './review'
import { Time } from './time'

// Parts of a draft change shown in several places: its status, its steps to a trial and its facts.

const stepNames = ['Review changes', 'Preview impact', 'Start trial']

// Stepper shows where a change is in its path to a trial. A finished step
// can be opened again; the next one opens with Next.
export function Stepper({ step, onStep }: { step: number; onStep: (step: number) => void }) {
  return (
    <ol className="stepper" aria-label="Steps">
      {stepNames.map((name, n) => (
        <li key={name} className={n === step ? 'is-current' : n < step ? 'is-done' : undefined}>
          <button type="button" aria-current={n === step ? 'step' : undefined} disabled={n > step} onClick={() => onStep(n)}>
            <span className="stepper-number" aria-hidden="true">
              {n + 1}
            </span>
            {name}
          </button>
        </li>
      ))}
    </ol>
  )
}

export const statusWords: Record<string, string> = {
  validated: 'Checked',
  established: 'Activated',
  quarantined: 'Set Aside After a Restore',
  withdrawn: 'Withdrawn',
}

export function CandidateStatus({ status }: { status: string }) {
  const words = statusWords[status] ?? status
  if (status === 'validated')
    return (
      <Badge tone="neutral" glyph="ok">
        {words}
      </Badge>
    )
  return <Badge tone={status === 'established' ? 'ok' : status === 'quarantined' ? 'refused' : 'neutral'}>{words}</Badge>
}
// CandidateFacts is the review step: what the draft change is based on, how it
// differs from the active version, and its full document.
export function CandidateFacts({ candidate, changes }: { candidate: Candidate; changes: Change[] | null }) {
  return (
    <>
      <dl className="facts facts-inline">
        <div className="fact">
          <dt>Based on</dt>
          <dd>{candidate.parent ? `version ${candidate.parent}` : 'no active version'}</dd>
        </div>
        <div className="fact">
          <dt>Denies Everything</dt>
          <dd>{candidate.deny_all ? 'yes' : 'no'}</dd>
        </div>
        <div className="fact">
          <dt>Saved</dt>
          <dd>
            <Time value={candidate.created_at} /> by {candidate.actor}
          </dd>
        </div>
        <div className="fact">
          <dt>Exact Form</dt>
          <dd>
            <MoreDetails facts={[['Checksum (SHA-256)', candidate.checksum]]} />
          </dd>
        </div>
      </dl>

      <h3>Differences from the Active Version</h3>
      {changes ? (
        changes.length === 0 ? (
          <p className="hint">No differences.</p>
        ) : (
          <ChangeList changes={changes} />
        )
      ) : (
        <p className="hint">No version is active to compare with.</p>
      )}

      <details className="document">
        <summary>Full Document</summary>
        <CodeBlock label={`Change ${candidate.id} document`}>{JSON.stringify(candidate.document, null, 2)}</CodeBlock>
      </details>
    </>
  )
}
