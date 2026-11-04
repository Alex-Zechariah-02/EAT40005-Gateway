import { useId, useState, type FormEvent, type ReactNode } from 'react'
import { Menu } from '@base-ui/react/menu'
import { Popover } from '@base-ui/react/popover'
import {
  CaretDown, Check, Info, Lifebuoy, MagnifyingGlass, MapPin, Moon, ShoppingBagOpen, SignOut, Sun, UserCircle, X,
} from '@phosphor-icons/react'
import { addressErrors, setAddress, useAddress } from '../lib/address'
import { basketCount, useBasket } from '../lib/basket'
import { Link, navigate, useLocation } from '../lib/router'
import { dismissNotice, hasRole, roleNames, signOut, useSession } from '../lib/session'
import { setTheme, useTheme, type ThemeChoice } from '../lib/theme'
import { states, type Address, type Role, type User } from '../lib/types'
import { Button, ButtonLink } from '../ui/button'
import { cx } from '../ui/cx'
import { SelectField, TextField } from '../ui/field'

// Staff workspaces, one per role. A person with several roles sees each.
const workspaces: { role: Role; to: string; label: string }[] = [
  { role: 'MERCHANT', to: '/merchant', label: 'My shop' },
  { role: 'RIDER', to: '/rider', label: 'Jobs' },
  { role: 'SUPPORT', to: '/support', label: 'Tickets' },
  { role: 'OPERATIONS', to: '/ops', label: 'Reports' },
]

export function Shell({ children }: { children: ReactNode }) {
  const { session } = useSession()
  const user = session?.user ?? null
  const staff = !!user && !hasRole(user, 'CUSTOMER')
  return (
    <div className="flex min-h-screen flex-col">
      <a href="#main" className="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus:z-50 focus:rounded-lg focus:bg-surface focus:px-4 focus:py-2 focus:font-bold focus:shadow-raised">
        Skip to content
      </a>
      {staff ? <StaffHeader user={user} /> : <ShopHeader user={user} />}
      <SessionNotice />
      <main id="main" className="mx-auto w-full max-w-[1320px] flex-1 px-4 pb-24 sm:px-8">
        {children}
      </main>
      <footer className="border-t border-line print:hidden">
        <div className="mx-auto flex max-w-[1320px] flex-wrap items-center justify-between gap-3 px-4 py-6 text-sm text-ink-3 sm:px-8">
          <Link to="/lab" className="font-semibold underline-offset-2 hover:text-ink hover:underline">
            Security lab
          </Link>
        </div>
      </footer>
    </div>
  )
}

function Logo({ to = '/', suffix }: { to?: string; suffix?: string }) {
  return (
    <Link to={to} className="flex shrink-0 items-baseline gap-2 rounded-md">
      <span className="text-[26px] leading-none font-extrabold tracking-[-0.04em] text-brand">fetch</span>
      {suffix && <span className="text-sm font-bold text-ink-2">{suffix}</span>}
    </Link>
  )
}

function ShopHeader({ user }: { user: User | null }) {
  const basket = useBasket()
  const count = basketCount(basket)
  const { path } = useLocation()
  return (
    <header className="sticky top-0 z-30 border-b border-line bg-surface/95 backdrop-blur print:hidden">
      <div className="mx-auto flex min-h-18 max-w-[1320px] flex-wrap items-center gap-2 px-4 py-2 sm:gap-4 sm:px-8 lg:gap-8">
        <div className="flex shrink-0 flex-col items-start gap-0.5 sm:flex-row sm:items-center sm:gap-2">
          <Logo />
        </div>
        <div className="hidden min-w-0 flex-1 items-center rounded-full bg-sunken ring-1 ring-line md:flex">
          <AddressPicker />
          <span className="h-6 w-px shrink-0 bg-line-strong" aria-hidden="true" />
          <SearchBox />
        </div>
        <nav aria-label="Account" className="ml-auto flex items-center gap-1 text-sm font-semibold text-ink-2 sm:gap-2 md:ml-0">
          {user && (
            <Link to="/my-orders" className={cx('hidden rounded-lg px-3 py-2 hover:bg-sunken hover:text-ink lg:block', path.startsWith('/my-orders') && 'text-ink')}>
              My orders
            </Link>
          )}
          <Link to="/help" className={cx('hidden rounded-lg px-3 py-2 hover:bg-sunken hover:text-ink lg:block', path.startsWith('/help') && 'text-ink')}>
            Help
          </Link>
          <Link to="/cart" aria-label={count ? `Basket, ${count} ${count === 1 ? 'item' : 'items'}` : 'Basket, empty'} className="relative flex size-11 items-center justify-center rounded-full bg-sunken text-ink ring-1 ring-line hover:bg-brand-soft">
            <ShoppingBagOpen size={20} />
            {count > 0 && (
              <span className="absolute -top-1 -right-1 flex h-5 min-w-5 items-center justify-center rounded-full bg-brand px-1 text-[11px] font-bold text-on-brand tabular-nums">{count}</span>
            )}
          </Link>
          {user ? <AccountMenu user={user} /> : <ButtonLink to={`/signin?next=${encodeURIComponent(path)}`} size="sm" className="ml-1">Sign in</ButtonLink>}
        </nav>
      </div>
      <div className="flex items-center gap-2 border-t border-line px-4 py-2 md:hidden">
        <div className="flex min-w-0 flex-1 items-center rounded-full bg-sunken ring-1 ring-line">
          <SearchBox />
        </div>
      </div>
    </header>
  )
}

function StaffHeader({ user }: { user: User }) {
  const { path } = useLocation()
  const mine = workspaces.filter((w) => hasRole(user, w.role))
  return (
    <header className="sticky top-0 z-30 border-b border-line bg-surface/95 backdrop-blur">
      <div className="mx-auto flex min-h-16 max-w-[1320px] flex-wrap items-center gap-x-6 gap-y-1 px-4 py-2 sm:px-8">
        <div className="flex items-center gap-2">
          <Logo to={user.home} suffix="for partners" />
        </div>
        <nav aria-label="Workspaces" className="flex items-center gap-1">
          {mine.map((w) => (
            <Link key={w.to} to={w.to} aria-current={path === w.to ? 'page' : undefined} className={cx('rounded-lg px-3 py-2 text-sm font-semibold', path === w.to ? 'bg-brand-soft text-brand-strong' : 'text-ink-2 hover:bg-sunken hover:text-ink')}>
              {w.label}
            </Link>
          ))}
        </nav>
        <div className="ml-auto">
          <AccountMenu user={user} />
        </div>
      </div>
    </header>
  )
}

function initials(name: string): string {
  return name.split(/\s+/).map((part) => part[0]).slice(0, 2).join('').toUpperCase()
}

const themeChoices: { value: ThemeChoice; label: string; icon: ReactNode }[] = [
  { value: 'system', label: 'Match this computer', icon: <Info size={16} /> },
  { value: 'light', label: 'Light', icon: <Sun size={16} /> },
  { value: 'dark', label: 'Dark', icon: <Moon size={16} /> },
]

const menuItem = 'flex h-9 cursor-default items-center gap-2.5 rounded-lg px-2.5 text-sm text-ink outline-none select-none data-[highlighted]:bg-sunken'

function AccountMenu({ user }: { user: User }) {
  const { choice } = useTheme()
  const [first, ...rest] = user.name.split(' ')
  // The button's name is its visible text plus hidden words, so it reads as
  // "Account: Customer 3, Customer" and still contains what is shown.
  return (
    <Menu.Root>
      <Menu.Trigger className="flex items-center gap-2 rounded-full py-1 pr-2.5 pl-1 text-left hover:bg-sunken data-[popup-open]:bg-sunken">
        <span className="sr-only">Account: </span>
        <span aria-hidden="true" className="flex size-9 items-center justify-center rounded-full bg-brand-soft text-sm font-bold text-brand-strong">{initials(user.name)}</span>
        <span className="sr-only leading-tight sm:not-sr-only sm:flex sm:flex-col">
          <span className="text-sm font-bold text-ink">{first}<span className="sr-only">{rest.length ? ` ${rest.join(' ')}` : ''}, </span></span>
          <span className="text-[11px] font-semibold text-ink-3">{user.roles.map((r) => roleNames[r]).join(' · ')}</span>
        </span>
        <CaretDown size={14} className="text-ink-3" />
      </Menu.Trigger>
      <Menu.Portal>
        <Menu.Positioner sideOffset={8} align="end" className="z-40">
          <Menu.Popup className="w-72 origin-[var(--transform-origin)] rounded-2xl bg-surface p-1.5 text-ink shadow-raised transition-[opacity,scale] duration-150 ease-out-quint data-[ending-style]:scale-95 data-[ending-style]:opacity-0 data-[starting-style]:scale-95 data-[starting-style]:opacity-0">
            <div className="px-2.5 pt-2 pb-2.5">
              <p className="font-bold">{user.name}</p>
              <p className="text-sm text-ink-3">{user.email}</p>
              <div className="mt-2 flex flex-wrap gap-1">
                {user.roles.map((role) => (
                  <span key={role} className="rounded-md bg-brand-soft px-1.5 py-0.5 text-xs font-bold text-brand-strong">{roleNames[role]}</span>
                ))}
              </div>
            </div>
            <Menu.Separator className="mx-2 my-1 h-px bg-line" />
            <Menu.Item className={menuItem} onClick={() => navigate('/account')}><UserCircle size={16} />Your account</Menu.Item>
            {hasRole(user, 'CUSTOMER') && (
              <>
                <Menu.Item className={menuItem} onClick={() => navigate('/my-orders')}><ShoppingBagOpen size={16} />My orders</Menu.Item>
                <Menu.Item className={menuItem} onClick={() => navigate('/help')}><Lifebuoy size={16} />Help and tickets</Menu.Item>
                <Menu.Separator className="mx-2 my-1 h-px bg-line" />
              </>
            )}
            <Menu.Group>
              <Menu.GroupLabel className="px-2.5 pt-1.5 pb-1 text-xs font-semibold text-ink-3">Appearance</Menu.GroupLabel>
              <Menu.RadioGroup value={choice} onValueChange={(value) => setTheme(value as ThemeChoice)}>
                {themeChoices.map((option) => (
                  <Menu.RadioItem key={option.value} value={option.value} closeOnClick={false} className={menuItem}>
                    {option.icon}
                    <span className="flex-1">{option.label}</span>
                    <Menu.RadioItemIndicator><Check size={15} weight="bold" className="text-brand" /></Menu.RadioItemIndicator>
                  </Menu.RadioItem>
                ))}
              </Menu.RadioGroup>
            </Menu.Group>
            <Menu.Separator className="mx-2 my-1 h-px bg-line" />
            <Menu.Item className={menuItem} onClick={() => { signOut(); navigate('/') }}><SignOut size={16} />Sign out</Menu.Item>
          </Menu.Popup>
        </Menu.Positioner>
      </Menu.Portal>
    </Menu.Root>
  )
}

// SearchBox is drawn twice, in the wide and the narrow header, so each copy
// has its own id for its label.
function SearchBox() {
  const id = useId()
  const { path, params } = useLocation()
  const [text, setText] = useState(path === '/' ? params.get('q') ?? '' : '')
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const q = text.trim()
    navigate(q ? `/?q=${encodeURIComponent(q)}` : '/')
  }
  return (
    <form role="search" onSubmit={submit} className="flex h-11 min-w-0 flex-1 items-center gap-3 rounded-full px-4 text-ink-3 focus-within:outline-2 focus-within:-outline-offset-2 focus-within:outline-brand">
      <MagnifyingGlass size={18} aria-hidden="true" />
      <label htmlFor={id} className="sr-only">Search dishes, shops or groceries</label>
      <input id={id} type="search" value={text} onChange={(e) => setText(e.target.value)} maxLength={40} placeholder="Search dishes, shops or groceries" className="h-full min-w-0 flex-1 bg-transparent text-[15px] text-ink outline-none" />
    </form>
  )
}

const blankAddress: Address = { line1: '', line2: '', postcode: '', city: 'Kuala Lumpur', state: 'Wilayah Persekutuan Kuala Lumpur' }

function AddressPicker() {
  const address = useAddress()
  const [open, setOpen] = useState(false)
  return (
    <Popover.Root open={open} onOpenChange={setOpen}>
      <Popover.Trigger className="flex h-11 max-w-72 shrink-0 items-center gap-2 rounded-l-full pr-4 pl-5 text-sm font-semibold text-ink hover:bg-surface/60 data-[popup-open]:bg-surface/60">
        <MapPin size={18} weight="fill" className="shrink-0 text-brand" aria-hidden="true" />
        <span className="truncate">{address ? address.line1 : 'Add a delivery address'}</span>
        <CaretDown size={14} className="shrink-0 text-ink-3" aria-hidden="true" />
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Positioner sideOffset={10} align="start" className="z-40">
          <Popover.Popup className="w-[380px] origin-[var(--transform-origin)] rounded-3xl bg-surface p-6 text-ink shadow-raised transition-[opacity,scale] duration-150 ease-out-quint data-[ending-style]:scale-95 data-[ending-style]:opacity-0 data-[starting-style]:scale-95 data-[starting-style]:opacity-0">
            <div className="flex items-start justify-between">
              <Popover.Title className="text-lg font-extrabold tracking-[-0.015em]">Deliver to</Popover.Title>
              <Popover.Close className="-mt-1 -mr-2 flex size-9 items-center justify-center rounded-full text-ink-3 hover:bg-sunken hover:text-ink" aria-label="Close"><X size={18} /></Popover.Close>
            </div>
            <Popover.Description className="mt-1 text-sm text-ink-2">Used for delivery times and at checkout. You can change it there too.</Popover.Description>
            <AddressForm initial={address ?? blankAddress} onSave={(next) => { setAddress(next); setOpen(false) }} />
          </Popover.Popup>
        </Popover.Positioner>
      </Popover.Portal>
    </Popover.Root>
  )
}

export function AddressForm({ initial, onSave, submitLabel = 'Save address' }: { initial: Address; onSave: (address: Address) => void; submitLabel?: string }) {
  const [draft, setDraft] = useState<Address>(initial)
  const [tried, setTried] = useState(false)
  const errors = tried ? addressErrors(draft) : {}
  const change = (field: keyof Address) => (value: string) => setDraft((d) => ({ ...d, [field]: value }))
  const submit = (event: FormEvent) => {
    event.preventDefault()
    setTried(true)
    if (Object.keys(addressErrors(draft)).length === 0) onSave({ ...draft, line1: draft.line1.trim(), line2: draft.line2.trim(), city: draft.city.trim() })
  }
  return (
    <form onSubmit={submit} noValidate className="mt-4 grid grid-cols-2 gap-3">
      <TextField className="col-span-2" label="Street address" autoComplete="address-line1" value={draft.line1} onChange={(e) => change('line1')(e.target.value)} error={errors.line1} maxLength={100} />
      <TextField className="col-span-2" label="Unit, floor or building" optional autoComplete="address-line2" value={draft.line2} onChange={(e) => change('line2')(e.target.value)} error={errors.line2} maxLength={100} />
      <TextField label="Postcode" inputMode="numeric" autoComplete="postal-code" value={draft.postcode} onChange={(e) => change('postcode')(e.target.value.replace(/\D/g, '').slice(0, 5))} error={errors.postcode} hint="5 digits" />
      <TextField label="Town or city" autoComplete="address-level2" value={draft.city} onChange={(e) => change('city')(e.target.value)} error={errors.city} maxLength={40} />
      <SelectField className="col-span-2" label="State" autoComplete="address-level1" value={draft.state} onChange={(e) => change('state')(e.target.value)} error={errors.state}>
        {states.map((state) => <option key={state} value={state}>{state}</option>)}
      </SelectField>
      <Button type="submit" block className="col-span-2 mt-1">{submitLabel}</Button>
    </form>
  )
}

function SessionNotice() {
  const { notice } = useSession()
  if (!notice) return null
  return (
    <div role="status" className="border-b border-line bg-warn-soft">
      <div className="mx-auto flex max-w-[1320px] items-center gap-3 px-4 py-2.5 text-sm font-semibold text-ink sm:px-8">
        <Info size={18} className="shrink-0 text-warn" aria-hidden="true" />
        <span className="flex-1">{notice}</span>
        <ButtonLink to="/signin" size="sm" variant="secondary">Sign in</ButtonLink>
        <button type="button" onClick={dismissNotice} className="flex size-9 items-center justify-center rounded-full text-ink-3 hover:bg-surface hover:text-ink" aria-label="Dismiss"><X size={16} /></button>
      </div>
    </div>
  )
}
