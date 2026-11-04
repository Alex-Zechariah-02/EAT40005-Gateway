import { useState, type FormEvent } from 'react'
import { Eye, EyeSlash } from '@phosphor-icons/react'
import { describe, type Problem } from '../lib/messages'
import { navigate, useLocation } from '../lib/router'
import { roleNames, signIn, useSession } from '../lib/session'
import type { Role } from '../lib/types'
import { Button } from '../ui/button'
import { TextField } from '../ui/field'
import { ProblemNotice } from '../ui/notice'

// The demonstration accounts of the Fetch Accounts seed. Every account uses the
// same published demo password, shown here on purpose: these are fictional
// people on a demonstration platform, and Fetch Accounts still checks it.
const demoPassword = 'FetchDemo2026!'
const demoAccounts: { name: string; email: string; roles: Role[]; note?: string }[] = [
  { name: 'Customer 1', email: 'customer1@fetch.test', roles: ['CUSTOMER'] },
  { name: 'Customer 2', email: 'customer2@fetch.test', roles: ['CUSTOMER'] },
  { name: 'Customer 3', email: 'customer3@fetch.test', roles: ['CUSTOMER'], note: 'First Mart order is free' },
  { name: 'Merchant 1', email: 'merchant1@fetch.test', roles: ['MERCHANT'], note: 'Warung Nasi Lemak' },
  { name: 'Merchant 5', email: 'merchant5@fetch.test', roles: ['MERCHANT'], note: 'Fetch Mart Express' },
  { name: 'Rider 1', email: 'rider1@fetch.test', roles: ['RIDER'] },
  { name: 'Rider 2', email: 'rider2@fetch.test', roles: ['RIDER'] },
  { name: 'Support 1', email: 'support1@fetch.test', roles: ['SUPPORT'] },
  { name: 'Admin 1', email: 'admin1@fetch.test', roles: ['OPERATIONS'] },
  { name: 'Admin 2', email: 'admin2@fetch.test', roles: ['SUPPORT', 'OPERATIONS'] },
]

// next must be a path on this site: never an absolute or protocol-relative URL.
function safeNext(value: string | null): string | null {
  return value && value.startsWith('/') && !value.startsWith('//') && !value.startsWith('/\\') ? value : null
}

export function SignInPage() {
  const { params } = useLocation()
  const { session } = useSession()
  const next = safeNext(params.get('next'))
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [show, setShow] = useState(false)
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<Problem | null>(null)
  const [tried, setTried] = useState(false)
  const emailError = tried && !/^[^\s@]+@[^\s@]+$/.test(email.trim()) ? 'Enter the email address of your Fetch account.' : null
  const passwordError = tried && !password ? 'Enter your password.' : null

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setTried(true)
    if (!/^[^\s@]+@[^\s@]+$/.test(email.trim()) || !password) return
    setBusy(true)
    setProblem(null)
    const answer = await signIn(email.trim().toLowerCase(), password)
    setBusy(false)
    if (answer.kind === 'ok') {
      navigate(next ?? answer.data.user.home, { replace: true })
      return
    }
    setProblem(describe(answer))
  }

  const choose = (address: string) => {
    setEmail(address)
    setPassword(demoPassword)
    setProblem(null)
  }

  return (
    <div className="mx-auto mt-10 grid max-w-[1040px] grid-cols-1 items-start gap-8 lg:grid-cols-[1fr_1.1fr]">
      <section className="rounded-[28px] bg-surface p-8 shadow-raised" aria-labelledby="signin-heading">
        <h1 id="signin-heading" className="text-[32px] leading-none font-extrabold tracking-[-0.035em]">Sign in to Fetch</h1>
        {session ? (
          <p className="mt-2 text-ink-2">You’re signed in as {session.user.name}. Signing in again switches this tab to another account.</p>
        ) : null}
        <form onSubmit={submit} noValidate className="mt-6 flex flex-col gap-4">
          <TextField label="Email" type="email" autoComplete="username" value={email} onChange={(e) => setEmail(e.target.value)} error={emailError} maxLength={254} />
          <div className="relative">
            <TextField label="Password" type={show ? 'text' : 'password'} autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} error={passwordError} maxLength={256} />
            <button type="button" onClick={() => setShow((s) => !s)} className="absolute top-[30px] right-1.5 flex size-9 items-center justify-center rounded-lg text-ink-3 hover:bg-surface hover:text-ink" aria-label={show ? 'Hide password' : 'Show password'} aria-pressed={show}>
              {show ? <EyeSlash size={18} /> : <Eye size={18} />}
            </button>
          </div>
          {problem && <ProblemNotice problem={problem} />}
          <Button type="submit" size="lg" block busy={busy} busyLabel="Signing in…">Sign in</Button>
          <p className="text-xs text-ink-3">This tab stays signed in until you close it or sign out. Other tabs can be other people.</p>
        </form>
      </section>
      <section aria-labelledby="demo-heading">
        <h2 id="demo-heading" className="text-lg font-extrabold tracking-[-0.02em]">Test accounts</h2>
        <p className="mt-1 text-sm text-ink-2">
          Password for every account: <span className="rounded bg-sunken px-1.5 py-0.5 font-bold text-ink ring-1 ring-line">{demoPassword}</span>. Select one to fill the form.
        </p>
        <ul className="mt-4 grid grid-cols-1 gap-2 sm:grid-cols-2">
          {demoAccounts.map((account) => (
            <li key={account.email}>
              <button type="button" onClick={() => choose(account.email)} aria-pressed={email === account.email} className="flex w-full flex-col items-start rounded-2xl bg-surface p-3.5 text-left shadow-card transition-shadow hover:shadow-raised aria-pressed:ring-2 aria-pressed:ring-brand">
                <span className="font-bold">{account.name}</span>
                <span className="text-sm text-ink-3">{account.email}</span>
                <span className="mt-2 flex flex-wrap gap-1">
                  {account.roles.map((role) => <span key={role} className="rounded-md bg-brand-soft px-1.5 py-0.5 text-xs font-bold text-brand-strong">{roleNames[role]}</span>)}
                  {account.note && <span className="rounded-md bg-sunken px-1.5 py-0.5 text-xs font-semibold text-ink-2">{account.note}</span>}
                </span>
              </button>
            </li>
          ))}
        </ul>
      </section>
    </div>
  )
}
