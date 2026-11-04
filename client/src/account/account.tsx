import { useState } from 'react'
import { Lifebuoy, MapPin, ShoppingBagOpen, SignOut, Wallet } from '@phosphor-icons/react'
import { setAddress, useAddress } from '../lib/address'
import { rm } from '../lib/format'
import { describe } from '../lib/messages'
import { useQuery } from '../lib/query'
import { navigate } from '../lib/router'
import { hasRole, roleNames, signOut, useSession } from '../lib/session'
import type { Address, User } from '../lib/types'
import { AddressForm } from '../app/shell'
import { Button, ButtonLink } from '../ui/button'
import { Loading, ProblemNotice, Skeleton } from '../ui/notice'

// The account page shows what Fetch Accounts returns for the signed-in person,
// read through the gateway, and this tab's delivery address. Nothing here
// grants or changes a role.

const blankAddress: Address = { line1: '', line2: '', postcode: '', city: 'Kuala Lumpur', state: 'Wilayah Persekutuan Kuala Lumpur' }

export function AccountPage() {
  const { session } = useSession()
  const me = useQuery<{ user: User }>('/accounts/me')
  const problem = me.answer && !me.data ? describe(me.answer) : null
  const user = me.data?.user
  const customer = hasRole(session?.user, 'CUSTOMER')
  return (
    <div className="mx-auto mt-8 max-w-[860px]">
      <h1 className="text-[36px] leading-none font-extrabold tracking-[-0.035em]">Your account</h1>
      {problem ? (
        <ProblemNotice className="mt-6" problem={problem} report action={<button type="button" onClick={me.reload} className="text-sm font-bold text-brand hover:underline">Try again</button>} />
      ) : !user ? (
        <Loading label="Loading your account">
          <Skeleton className="mt-6 h-40" />
        </Loading>
      ) : (
        <Profile user={user} />
      )}
      {customer && (
        <div className="mt-6 grid grid-cols-1 gap-4 md:grid-cols-2">
          <WalletCard />
          <DeliveryAddress />
        </div>
      )}
      <section className="mt-6 flex flex-wrap items-center gap-3" aria-label="More">
        {customer ? (
          <>
            <ButtonLink to="/my-orders" variant="secondary"><ShoppingBagOpen size={16} weight="bold" />My orders</ButtonLink>
            <ButtonLink to="/help" variant="secondary"><Lifebuoy size={16} weight="bold" />Help and tickets</ButtonLink>
          </>
        ) : (
          session && session.user.home !== '/' && <ButtonLink to={session.user.home} variant="secondary">Go to your workspace</ButtonLink>
        )}
        <Button variant="secondary" className="ml-auto" onClick={() => { signOut(); navigate('/') }}><SignOut size={16} weight="bold" />Sign out</Button>
      </section>
    </div>
  )
}

function Profile({ user }: { user: User }) {
  const rows: [string, string][] = [
    ['Email', user.email],
    ['Account ID', user.id],
    ['Roles', user.roles.map((role) => roleNames[role]).join(', ')],
  ]
  if (user.merchant) rows.push(['Shop', user.merchant])
  return (
    <section className="mt-6 rounded-2xl bg-surface p-6 shadow-card" aria-labelledby="account-name">
      <h2 id="account-name" className="text-xl font-extrabold tracking-[-0.015em]">{user.name}</h2>
      <dl className="mt-4 grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-[8rem_minmax(0,1fr)]">
        {rows.map(([label, value]) => (
          <div key={label} className="contents">
            <dt className="text-ink-3">{label}</dt>
            <dd className="font-semibold break-words text-ink">{value}</dd>
          </div>
        ))}
      </dl>
    </section>
  )
}

function WalletCard() {
  const wallet = useQuery<{ balance_sen: number }>('/orders/wallet')
  const balance = wallet.data?.balance_sen
  return (
    <section className="rounded-2xl bg-surface p-6 shadow-card" aria-labelledby="account-wallet">
      <h2 id="account-wallet" className="flex items-center gap-2 font-bold text-ink">
        <Wallet size={18} weight="bold" className="text-brand" aria-hidden="true" />
        Fetch wallet
      </h2>
      {balance !== undefined ? (
        <p className="mt-3 text-[28px] font-extrabold tracking-[-0.02em] tabular-nums">{rm(balance)}</p>
      ) : wallet.answer ? (
        <p className="mt-3 text-sm text-ink-2">
          Balance unavailable right now.{' '}
          <button type="button" onClick={wallet.reload} className="font-bold text-brand hover:underline">Try again</button>
        </p>
      ) : (
        <Loading label="Reading your balance"><Skeleton className="mt-3 h-9 w-32" /></Loading>
      )}
      <p className="mt-2 text-sm text-ink-3">Pay with it at checkout. An order paid this way is charged when you place it.</p>
    </section>
  )
}

function DeliveryAddress() {
  const address = useAddress()
  const [editing, setEditing] = useState(false)
  return (
    <section className="rounded-2xl bg-surface p-6 shadow-card" aria-labelledby="account-address">
      <h2 id="account-address" className="flex items-center gap-2 font-bold text-ink">
        <MapPin size={18} weight="bold" className="text-brand" aria-hidden="true" />
        Delivery address
      </h2>
      {editing ? (
        <AddressForm initial={address ?? blankAddress} onSave={(next) => { setAddress(next); setEditing(false) }} />
      ) : address ? (
        <>
          <p className="mt-3 text-sm text-ink">
            {address.line1}
            {address.line2 && <>, {address.line2}</>}
            <br />
            {address.postcode} {address.city}, {address.state}
          </p>
          <div className="mt-3 flex gap-3">
            <Button size="sm" variant="secondary" onClick={() => setEditing(true)}>Change</Button>
            <Button size="sm" variant="secondary" onClick={() => setAddress(null)}>Remove</Button>
          </div>
        </>
      ) : (
        <>
          <p className="mt-3 text-sm text-ink-2">No address yet.</p>
          <Button size="sm" variant="secondary" className="mt-3" onClick={() => setEditing(true)}>Add an address</Button>
        </>
      )}
      <p className="mt-3 text-sm text-ink-3">Kept in this browser tab only, and cleared when you sign out.</p>
    </section>
  )
}
