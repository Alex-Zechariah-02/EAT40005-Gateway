import { plainChange, type Change } from './diff'
import { Arrowed } from './ui'

// The review of policy differences: change lists and plain-sentence change sets.

// ChangeList shows policy differences, each item with the members that
// differ.
export function ChangeList({ changes }: { changes: Change[] }) {
  return (
    <ul className="changes changes-columns">
      {changes.map((change) => (
        <li key={change.text} className={`change change-${change.kind}`}>
          <span className="change-head">
            <Arrowed text={change.text} />
          </span>
          {change.details.length > 0 ? (
            <ul className="change-details">
              {change.details.map((detail) => (
                <li key={detail}>
                  <Arrowed text={detail} />
                </li>
              ))}
            </ul>
          ) : null}
        </li>
      ))}
    </ul>
  )
}

const changeKinds = [
  ['added', 'Added'],
  ['changed', 'Changed'],
  ['removed', 'Removed'],
] as const
// The review shows this many sentences per kind; the page lists them all.
const reviewLength = 8

// ChangeSet is the review of an edit: what it adds, changes and removes, in
// plain sentences, from the same differences the change lists show.
export function ChangeSet({ changes }: { changes: Change[] }) {
  return (
    <div className="change-set">
      {changeKinds.map(([kind, label]) => {
        const list = changes.filter((change) => change.kind === kind)
        if (list.length === 0) return null
        return (
          <section key={kind} aria-label={`${label} (${list.length})`}>
            <h3>
              {label} ({list.length})
            </h3>
            <ul>
              {list.slice(0, reviewLength).map((change) => (
                <li key={change.text}>{plainChange(change)}</li>
              ))}
            </ul>
            {list.length > reviewLength ? <p className="hint">And {list.length - reviewLength} more, listed on the page.</p> : null}
          </section>
        )
      })}
    </div>
  )
}
