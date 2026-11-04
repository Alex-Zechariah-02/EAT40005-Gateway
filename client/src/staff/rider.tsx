import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import type { Jobs } from '../lib/types'
import { Button } from '../ui/button'
import { Loading, ProblemNotice, RefreshProblem, Skeleton } from '../ui/notice'
import { Column, OrderCard, WorkspaceTitle, useAction } from './staff'

// The rider job board: ready orders any rider may take, and the rider's
// own deliveries. When two riders tap the same job, the Orders service lets
// the first win and refuses the second (409 already_assigned).
export function RiderPage() {
  const jobs = useQuery<Jobs>('/orders/jobs', { poll: 5_000 })
  const action = useAction(jobs.reload)
  const problem = jobs.answer && !jobs.data ? describe(jobs.answer) : null
  const cardProblem = (key: string) => (action.problem?.key === key ? action.problem.problem : null)
  return (
    <>
      <WorkspaceTitle title="Jobs" subtitle="Updates automatically." />
      <div className="mt-6">
        <RefreshProblem query={jobs} className="mb-4" />
        {problem ? (
          <ProblemNotice problem={problem} action={<Button size="sm" variant="secondary" onClick={jobs.reload}>Try again</Button>} />
        ) : !jobs.data ? (
          <Loading label="Loading jobs"><div className="grid grid-cols-1 gap-4 lg:grid-cols-2">{[0, 1].map((n) => <Skeleton key={n} className="h-72 rounded-3xl" />)}</div></Loading>
        ) : (
          <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
            <Column title="Your deliveries" hint="Hand these to the customer, then mark them delivered." count={jobs.data.mine.length}>
              {jobs.data.mine.length === 0 && <p className="px-2 pb-2 text-sm text-ink-3">No deliveries in progress. Take a job from the board.</p>}
              {jobs.data.mine.map((o) => (
                <OrderCard key={o.id} order={o} view="rider" problem={cardProblem(o.id)} action={<Button block busyLabel="Updating…" busy={action.busy === o.id} onClick={() => void action.run(o.id, `/orders/deliver/${encodeURIComponent(o.id)}`, { object: `delivering ${o.id}`, done: 'marked delivered', check: 'Wait for the board to refresh' })}>Mark delivered</Button>} />
              ))}
            </Column>
            <Column title="Ready for pickup" hint="The first rider to take a job gets it." count={jobs.data.available.length}>
              {jobs.data.available.length === 0 && <p className="px-2 pb-2 text-sm text-ink-3">No jobs right now. New ones appear here as shops finish orders.</p>}
              {jobs.data.available.map((o) => (
                <OrderCard key={o.id} order={o} view="rider" problem={cardProblem(o.id)} action={<Button block busyLabel="Taking…" busy={action.busy === o.id} onClick={() => void action.run(o.id, `/orders/pickup/${encodeURIComponent(o.id)}`, { object: `taking ${o.id}`, done: 'assigned to you', check: 'Look for it under Your deliveries' })}>Take this job</Button>} />
              ))}
            </Column>
          </div>
        )}
      </div>
    </>
  )
}
