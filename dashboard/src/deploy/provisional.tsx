import { useEffect, useRef, useState } from 'react'
import { newKey, type Admin } from '../shared/http'
import type { Outcome, ProvisionalView, Reply, Status } from '../shared/api'
import { useGatewayState, useMutation, liveMs, usePoll, useRereadWhen, useResource } from '../shared/hooks'
import type { Traffic } from '../monitor/summary'
import { MutationResult, Problem, Receipt } from '../shared/replies'
import { age, Remaining, staleAfterMs, Time } from '../shared/time'
import { Confirm } from '../shared/dialogs'
import { type Asking, type Check, KeepDialog, sinceStart, trafficWindow, TrialTraffic } from './trial'
import { DeadlineBar, TrialSide, TrialTile } from './activation'

// The change on trial: keep it or undo it, in full on Changes or as a tile on the Overview.

// What the administrator was looking at when they chose Confirm.
interface Seen {
  revision: number
  activatedAt: string
  deadline: string
  generation?: string
  overrides: string
  trafficAt: Date | null
  failed: string[]
}

// TrialDecision shows the change on trial (the provisional policy) with the
// server's deadline and its changed controls, and offers confirmation and
// rollback. Only an explicit confirmation makes it permanent; traffic, page
// loads and the countdown never do. The Overview shows it as a tile with
// rollback; Changes shows it in full, with the trial's traffic, and is where
// it is confirmed.
export function TrialDecision(props: { admin: Admin; manage: boolean; variant: 'tile' | 'full' }) {
  const { admin, manage, variant } = props
  const state = useGatewayState()
  const current = useResource<ProvisionalView>((signal) => admin.get('/admin/provisional', signal), [])
  useRereadWhen(state.version, current.refresh)
  // Rereading the status rereads this trial and everything else that
  // depends on it.
  const refresh = state.refresh
  const mutation = useMutation(admin, refresh)
  const [asking, setAsking] = useState<Asking>(null)
  const [check, setCheck] = useState<Check>({ phase: 'reading' })
  const [reviewed, setReviewed] = useState(false)
  const seen = useRef<Seen | null>(null)
  // The trial the open confirmation is about. The dialog stays open on it
  // when the trial ends meanwhile, to say why it can no longer be confirmed.
  const confirming = useRef<ProvisionalView | null>(null)
  const checks = useRef(0)

  const none = current.reply?.kind === 'refused' && current.reply.error === 'no_provisional'
  // After a failed reread the last reading stays shown, marked as such.
  const view = none ? undefined : current.last?.body
  // The ETag names the confirmed revision a rollback is based on; the
  // server refuses it if that has changed.
  const etag = current.reply && current.reply.kind !== 'unknown' && current.reply.kind !== 'aborted' ? current.reply.etag : null

  // The trial's traffic, reread every 3 s while it is shown in full.
  const activatedAt = view?.activation.activated_at
  const traffic = useResource<Traffic>(
    (signal) =>
      activatedAt && variant === 'full'
        ? admin.get(`/admin/traffic?window=${trafficWindow(activatedAt)}`, signal)
        : Promise.resolve({ kind: 'aborted' } as Reply<Traffic>),
    [activatedAt, variant],
  )
  usePoll(traffic.refresh, activatedAt && variant === 'full' ? liveMs : null, true)

  // runCheck reads the trial, the status and the trial's traffic again and
  // compares them with what the administrator was looking at. It never
  // sends anything: the administrator decides with the result in front of
  // them.
  async function runCheck(whileOpen: boolean) {
    const before = seen.current
    if (!before) return
    const mine = ++checks.current
    setCheck({ phase: 'reading' })
    setReviewed(false)
    const [trial, status, figures] = await Promise.all([
      admin.get<ProvisionalView>('/admin/provisional'),
      admin.get<Status>('/admin/status'),
      admin.get<Traffic>(`/admin/traffic?window=${trafficWindow(before.activatedAt)}`),
    ])
    if (mine !== checks.current) return
    if (trial.kind === 'refused' && trial.error === 'no_provisional') {
      setCheck({ phase: 'blocked', why: 'No change is on trial any more: it was kept or undone elsewhere, or its deadline passed. Nothing was sent.' })
      state.refresh()
      return
    }
    if (trial.kind !== 'ok') {
      const why = trial.kind === 'unknown' ? trial.message : trial.kind === 'refused' ? `HTTP ${trial.status} ${trial.error}` : 'cancelled'
      setCheck({
        phase: 'blocked',
        why: `The trial could not be read again (${why}), so it cannot be kept from here. Undoing it stays possible, and the gateway undoes it by itself at the deadline.`,
      })
      return
    }
    if (trial.body.activation.revision !== before.revision) {
      setCheck({ phase: 'blocked', why: `A different change is on trial now, version ${trial.body.activation.revision}. Nothing was sent.` })
      state.refresh()
      return
    }
    const reasons: string[] = []
    if (whileOpen) reasons.push("The gateway's status changed while this dialog was open.")
    if (trial.body.activation.deadline !== before.deadline) reasons.push('Its deadline is different from the one you were looking at.')
    if (trial.body.generation !== before.generation) reasons.push('The policy that requests use changed (a new generation is active).')
    if (status.kind === 'ok' && JSON.stringify(status.body.overrides ?? []) !== before.overrides) reasons.push('The active temporary rules changed.')
    if (before.failed.length > 0) reasons.push(`An earlier read of ${before.failed.join(' and ')} had failed, so what you saw may not have been current.`)
    if (status.kind !== 'ok') reasons.push("The gateway's status could not be read again.")
    if (figures.kind !== 'ok') reasons.push("The trial's traffic could not be read again.")
    if (before.trafficAt === null) reasons.push('No traffic figures for the trial were shown when you chose Keep change.')
    else if (Date.now() - before.trafficAt.getTime() > staleAfterMs) reasons.push(`The traffic figures you were looking at were read ${age(before.trafficAt)}.`)
    if (figures.kind === 'ok' && sinceStart(figures.body, before.activatedAt).total === 0) {
      reasons.push('No request has been decided since the trial started, so nothing shows yet how the change behaves.')
    }
    setCheck({ phase: 'ready', at: new Date(), view: trial.body, etag: trial.etag, traffic: figures.kind === 'ok' ? figures.body : undefined, reasons })
  }

  function askConfirm() {
    if (!view) return
    confirming.current = view
    seen.current = {
      revision: view.activation.revision,
      activatedAt: view.activation.activated_at,
      deadline: view.activation.deadline,
      generation: view.generation,
      overrides: JSON.stringify(state.facts?.overrides ?? []),
      trafficAt: traffic.last?.at ?? null,
      failed: [current.failed ? 'the trial' : '', state.failed ? "the gateway's status" : '', traffic.failed ? "the trial's traffic" : ''].filter(Boolean),
    }
    setAsking('confirm')
    void runCheck(false)
  }

  // A status change while the confirmation is open is checked again, and
  // the administrator reviews the result before anything can be sent.
  useEffect(() => {
    if (asking === 'confirm' && check.phase === 'ready') void runCheck(true)
  }, [state.version])

  function cancel() {
    checks.current++
    seen.current = null
    setAsking(null)
  }

  function confirm() {
    if (check.phase !== 'ready' || (check.reasons.length > 0 && !reviewed)) return
    cancel()
    mutation.run({
      method: 'POST',
      path: '/admin/provisional/confirm',
      body: { revision: check.view.activation.revision },
      ifMatch: check.etag ?? undefined,
      key: newKey(),
    })
  }

  function rollback() {
    setAsking(null)
    if (!view) return
    mutation.run({
      method: 'POST',
      path: '/admin/provisional/rollback',
      body: { revision: view.activation.revision },
      ifMatch: etag ?? undefined,
      key: newKey(),
    })
  }

  const result = (
    <MutationResult<Outcome>
      mutation={mutation}
      onRefresh={refresh}
      success={(o) => (
        <>
          <p className="notice-title">
            {o.disposition === 'confirmed' ? `Kept: version ${o.revision} is now the active policy.` : `Undone: version ${o.revision} is active again.`}
          </p>
          {variant === 'full' ? <Receipt outcome={o} /> : null}
        </>
      )}
    />
  )

  const ready = check.phase === 'ready' ? check : null
  const asked = confirming.current
  const confirmDialog =
    variant === 'full' && asked ? (
      <KeepDialog
        asked={asked}
        asking={asking}
        check={check}
        ready={ready}
        reviewed={reviewed}
        setReviewed={setReviewed}
        cancel={cancel}
        confirm={confirm}
      />
    ) : null
  const dialogs = (
    <>
      {confirmDialog}
      {view ? (
        <Confirm
          open={asking === 'rollback'}
          title={`Undo the change and return to version ${view.activation.rollback_target}?`}
          confirmLabel="Undo Change"
          danger
          onCancel={() => setAsking(null)}
          onConfirm={rollback}
        >
          <p>
            Version {view.activation.rollback_target} is active again from the next request, and the trial of version {view.activation.revision} ends.
          </p>
          <p className="hint">The Admin API calls this “roll back”.</p>
        </Confirm>
      ) : null}
    </>
  )

  const stale = current.failed && !none && view ? (
    <p className="notice tone-pending">
      Not current: this trial was read at {current.last ? <Time value={current.last.at.toISOString()} /> : null} and the latest read
      failed.
    </p>
  ) : null

  if (variant === 'tile') {
    return (
      <TrialTile
        view={view}
        current={current}
        none={none}
        manage={manage}
        mutation={mutation}
        refresh={refresh}
        setAsking={setAsking}
        stale={stale}
        result={result}
        dialogs={dialogs}
      />
    )
  }

  // With no trial nothing is shown, except an open confirmation saying why
  // it can no longer go ahead.
  if (none && mutation.phase.state === 'idle' && asking !== 'confirm') return null
  return (
    <>
      {current.reply && current.reply.kind !== 'ok' && !none && !view ? <Problem reply={current.reply} /> : null}
      {view ? (
        <section className="section card trial tone-pending" aria-labelledby="trial-title">
          <div className="trial-main">
            <p className="tile-label tile-label-tone">Change on Trial</p>
            <h2 id="trial-title">Version {view.activation.revision} is on trial</h2>
            <div className="trial-clock">
              <p className="tile-countdown">
                <Remaining key={view.activation.deadline} until={view.activation.deadline} onPassed={refresh} format="clock" />
              </p>
              <p className="hint">
                left to keep it, until <Time value={view.activation.deadline} />. Then the gateway undoes it and returns to version{' '}
                {view.activation.rollback_target} by itself.
              </p>
            </div>
            <DeadlineBar from={view.activation.activated_at} until={view.activation.deadline} />
            {stale}
            {manage ? (
              <div className="actions">
                <button type="button" className="btn" onClick={() => setAsking('rollback')} disabled={mutation.pending}>
                  Undo Change
                </button>
                <button type="button" className="btn btn-primary" onClick={askConfirm} disabled={mutation.pending}>
                  Keep Change
                </button>
              </div>
            ) : null}
            <TrialTraffic traffic={traffic} activatedAt={view.activation.activated_at} />
          </div>
          <TrialSide view={view} />
        </section>
      ) : null}
      {result}
      {dialogs}
    </>
  )
}
