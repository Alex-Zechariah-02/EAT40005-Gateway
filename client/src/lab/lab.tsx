import { useEffect, useRef, useState } from 'react'
import { Flask } from '@phosphor-icons/react'
import { Link } from '../lib/router'
import { onIdentityChange, roleNames, useSession } from '../lib/session'
import { ButtonLink } from '../ui/button'
import { WorkspaceTitle } from '../staff/staff'
import { groups, type Attack } from './attacks'
import { AttackCard } from './results'
import { verdict, type Result } from './verdict'

// The security lab: each button sends one deliberately wrong request
// through the gateway, as the person signed in on this tab, and sets what the
// Fetch baseline policy should do beside what the
// gateway actually answered. The lab decides nothing and changes no policy;
// it sends with the tab's token directly, so a deliberate 401 never ends the
// person's session. Changing person cancels pending requests and clears the
// results.

export function LabPage() {
  const { session } = useSession()
  const user = session?.user ?? null
  const [results, setResults] = useState<Record<string, Result>>({})
  const [busy, setBusy] = useState<string | null>(null)
  const controller = useRef(new AbortController())

  // A change of person cancels what is pending and clears earlier answers.
  useEffect(
    () =>
      onIdentityChange(() => {
        controller.current.abort()
        controller.current = new AbortController()
        setResults({})
        setBusy(null)
      }),
    [],
  )
  // Leaving the page cancels what is pending. The controller is made here so
  // a remount (React's development double mount) starts with a fresh one.
  useEffect(() => {
    controller.current = new AbortController()
    return () => controller.current.abort()
  }, [])

  async function fire(attack: Attack) {
    if (busy) return
    const signal = controller.current.signal
    const expected = attack.expected(user)
    setBusy(attack.id)
    let result: Result
    try {
      const answers = await attack.run(session?.token ?? null, signal, user)
      result = { answers: answers.filter((a) => a.kind !== 'cancelled'), expected }
    } catch {
      // Building the request failed (for example a token that is not a
      // JWT), so nothing was sent.
      result = { answers: [], expected, unsent: true }
    }
    if (signal.aborted) return
    setBusy(null)
    setResults((all) => ({ ...all, [attack.id]: result }))
  }

  const ran = Object.values(results)
  const matched = ran.filter((result) => verdict(result) === 'match').length
  const blocked = ran.filter((result) => verdict(result) === 'blocked').length

  return (
    <>
      <WorkspaceTitle
        title="Security lab"
        subtitle="Each test sends one real request through the gateway and compares its answer with the baseline policy’s. A change on trial, a temporary rule or a blocked account can change the answer."
      >
        <div className="flex flex-wrap items-center gap-3 rounded-2xl bg-surface px-4 py-3 shadow-card">
          <span className="flex size-9 items-center justify-center rounded-full bg-brand-soft text-brand" aria-hidden="true">
            <Flask size={18} weight="bold" />
          </span>
          <div className="text-sm">
            <p className="font-bold text-ink">{user ? `Sending as ${user.name}` : 'Sending with no sign-in'}</p>
            <p className="text-ink-3">{user ? `${user.id} · ${user.roles.map((r) => roleNames[r]).join(', ')}` : 'Token attacks need a signed-in person'}</p>
          </div>
          <ButtonLink to="/signin?next=%2Flab" size="sm" variant="secondary">
            {user ? 'Switch person' : 'Sign in'}
          </ButtonLink>
        </div>
      </WorkspaceTitle>

      {ran.length > 0 && (
        <p className="mt-6 text-sm text-ink-2" role="status">
          {matched} of {ran.length} match the baseline.{blocked > 0 && ` ${blocked} refused because this account is blocked.`}{' '}
          <button type="button" className="font-semibold text-brand hover:underline" onClick={() => setResults({})}>
            Clear results
          </button>
        </p>
      )}

      <div className="mt-6 flex flex-col gap-10">
        {groups.map((group) => (
          <section key={group.id} aria-labelledby={`lab-${group.id}`}>
            <h2 id={`lab-${group.id}`} className="flex items-center gap-2 text-lg font-extrabold tracking-[-0.015em]">
              <span className="text-brand" aria-hidden="true">
                {group.icon}
              </span>
              {group.title}
            </h2>
            <ul className="mt-3 grid grid-cols-1 gap-3 lg:grid-cols-2">
              {group.attacks.map((attack) => (
                <AttackCard
                  key={attack.id}
                  attack={attack}
                  user={user}
                  result={results[attack.id]}
                  busy={busy === attack.id}
                  disabled={busy !== null || (attack.token && !session)}
                  onFire={() => void fire(attack)}
                />
              ))}
            </ul>
          </section>
        ))}
      </div>

      <p className="mt-10 max-w-[70ch] text-sm text-ink-3">
        Address-based flooding (public search and sign-in) runs from a local terminal with <code className="font-mono text-ink-2">demo attack -from 127.0.0.50</code>, so this
        browser’s address stays usable. Use Copy full reference beside any Ref and paste it into the Dashboard’s header search to see which check decided it; the Dashboard’s Security Log finds older requests.{' '}
        <Link to="/" className="font-semibold text-brand hover:underline">
          Back to Fetch
        </Link>
      </p>
    </>
  )
}
