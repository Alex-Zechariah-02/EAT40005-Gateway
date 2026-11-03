import { useEffect, useRef } from 'react'
import { explain, type HistoryEntry } from '../shared/api'
import { useResource } from '../shared/hooks'
import { ReplyView } from '../shared/replies'
import { CodeBlock } from '../shared/ui'
import { Time } from '../shared/time'
import type { ViewProps } from '../app/views'
import { kinds } from './history'

// One opened history record, shown field by field.

const entryFields: [keyof HistoryEntry, string][] = [
  ['seq', 'Sequence'],
  ['transition_id', 'Transition'],
  ['kind', 'Kind'],
  ['actor', 'By'],
  ['target', 'Target'],
  ['expected_revision', 'Based on version'],
  ['outcome', 'Outcome'],
  ['reason', 'Reason'],
  ['revision', 'Resulting Version'],
  ['candidate', 'Draft Change'],
  ['recorded_at', 'Recorded'],
  ['published_at', 'Published'],
  ['published_by', 'Published by'],
  ['audited_at', 'Recorded in the security log'],
]

export function Entry({ admin, id }: { admin: ViewProps['admin']; id: string }) {
  const heading = useRef<HTMLHeadingElement>(null)
  const entry = useResource<HistoryEntry>((signal) => admin.get(`/admin/history/${encodeURIComponent(id)}`, signal), [id])
  useEffect(() => {
    heading.current?.focus()
  }, [])
  return (
    <section className="entry-detail" aria-labelledby="entry-title">
      <h2 id="entry-title" ref={heading} tabIndex={-1}>
        Transition <code>{id}</code>
      </h2>
      <ReplyView reply={entry.reply}>
        {(h) => (
          <>
            <p className="entry-what">{kinds[h.kind]?.label ?? h.kind}</p>
            {h.reason && explain(h.reason) ? <p className="hint">{explain(h.reason)}</p> : null}
            <dl className="facts">
              {entryFields
                .filter(([key]) => h[key] !== undefined && h[key] !== '')
                .map(([key, label]) => (
                  <div key={key} className="fact">
                    <dt>{label}</dt>
                    <dd>{key.endsWith('_at') ? <Time value={String(h[key])} /> : <code>{String(h[key])}</code>}</dd>
                  </div>
                ))}
            </dl>
            <details className="document">
              <summary>Record as Stored</summary>
              <CodeBlock label={`Entry ${h.seq} as stored`}>{JSON.stringify(h, null, 2)}</CodeBlock>
            </details>
          </>
        )}
      </ReplyView>
    </section>
  )
}
