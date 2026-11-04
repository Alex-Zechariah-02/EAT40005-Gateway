import { lazy, Suspense, type ReactNode } from 'react'
import { Compass, LockKey } from '@phosphor-icons/react'
import { match, useLocation } from '../lib/router'
import { hasRole, roleNames, useSession } from '../lib/session'
import type { Role } from '../lib/types'
import { Shell } from './shell'
import { ButtonLink } from '../ui/button'
import { Empty, Skeleton } from '../ui/notice'
import { Home } from '../shop/home'
import { SectionPage } from '../shop/section'
import { StoreRoute } from '../shop/store'
import { ItemPage } from '../shop/item'
import { BasketPage } from '../shop/basket'
import { SignInPage } from '../account/signin'

// Pages used by one role load on demand, so a customer never downloads the
// merchant, rider, support or operations screens.
const CheckoutPage = lazy(() => import('../orders/checkout').then((m) => ({ default: m.CheckoutPage })))
const OrdersPage = lazy(() => import('../orders/orders').then((m) => ({ default: m.OrdersPage })))
const OrderPage = lazy(() => import('../orders/order').then((m) => ({ default: m.OrderPage })))
const HelpPage = lazy(() => import('../help/help').then((m) => ({ default: m.HelpPage })))
const TicketPage = lazy(() => import('../help/ticket').then((m) => ({ default: m.TicketPage })))
const MerchantPage = lazy(() => import('../staff/merchant').then((m) => ({ default: m.MerchantPage })))
const RiderPage = lazy(() => import('../staff/rider').then((m) => ({ default: m.RiderPage })))
const SupportPage = lazy(() => import('../staff/support').then((m) => ({ default: m.SupportPage })))
const OpsPage = lazy(() => import('../staff/ops').then((m) => ({ default: m.OpsPage })))
const LabPage = lazy(() => import('../lab/lab').then((m) => ({ default: m.LabPage })))
const AccountPage = lazy(() => import('../account/account').then((m) => ({ default: m.AccountPage })))

export function App() {
  const { path, params } = useLocation()
  return (
    <Shell>
      <Suspense fallback={<PageLoading />}>
        <Route path={path} item={params.get('item')} />
      </Suspense>
    </Shell>
  )
}

function Route({ path, item }: { path: string; item: string | null }) {
  let found: Record<string, string> | null
  if (path === '/') return <Home />
  if (path === '/food' || path === '/mart') return <SectionPage section={path === '/food' ? 'food' : 'mart'} />
  if ((found = match('/store/:address', path))) return <StoreRoute key={`${found.address}?${item ?? ''}`} address={found.address} item={item} />
  if ((found = match('/item/:id', path))) return <ItemPage key={found.id} id={found.id} />
  if (path === '/cart') return <BasketPage />
  if (path === '/signin') return <SignInPage />
  if (path === '/checkout') return <Gate role="CUSTOMER"><CheckoutPage /></Gate>
  if (path === '/my-orders') return <Gate role="CUSTOMER"><OrdersPage /></Gate>
  if ((found = match('/my-orders/:id', path))) return <Gate role="CUSTOMER"><OrderPage key={found.id} id={found.id} /></Gate>
  if (path === '/help') return <Gate role="CUSTOMER"><HelpPage /></Gate>
  if ((found = match('/help/:id', path))) return <Gate role="CUSTOMER"><TicketPage key={found.id} id={found.id} /></Gate>
  if (path === '/merchant') return <Gate role="MERCHANT"><MerchantPage /></Gate>
  if (path === '/rider') return <Gate role="RIDER"><RiderPage /></Gate>
  if (path === '/support') return <Gate role="SUPPORT"><SupportPage /></Gate>
  if (path === '/ops') return <Gate role="OPERATIONS"><OpsPage /></Gate>
  // The security lab is open to anyone, signed in or not: it shows what the
  // gateway refuses, as whoever is signed in on this tab.
  if (path === '/lab') return <LabPage />
  if (path === '/account') return <Gate><AccountPage /></Gate>
  return <NotFound />
}

// Gate only chooses what to show: the gateway still decides every request.
// Gate asks for sign-in, and for role when one is given.
function Gate({ role, children }: { role?: Role; children: ReactNode }) {
  const { session } = useSession()
  const { path, params } = useLocation()
  const here = path + (params.size ? `?${params}` : '')
  if (!session) {
    return (
      <Empty
        className="mt-10"
        icon={<LockKey size={26} weight="bold" />}
        title="Sign in to continue"
        body={`This page shows your own ${!role ? 'account' : role === 'CUSTOMER' ? 'orders and details' : 'work'}, so Fetch needs to know who you are.`}
        action={<ButtonLink to={`/signin?next=${encodeURIComponent(here)}`}>Sign in</ButtonLink>}
      />
    )
  }
  if (role && !hasRole(session.user, role)) {
    return (
      <Empty
        className="mt-10"
        icon={<LockKey size={26} weight="bold" />}
        title={`This page is for ${roleNames[role].toLowerCase()} accounts`}
        body={`You’re signed in as ${session.user.name} (${session.user.roles.map((r) => roleNames[r]).join(', ')}).`}
        action={<ButtonLink to={session.user.home}>Go to your home</ButtonLink>}
      />
    )
  }
  return children
}

function PageLoading() {
  return (
    <div className="mt-8 flex flex-col gap-4" aria-busy="true">
      <span className="sr-only" role="status">Loading the page</span>
      <Skeleton className="h-10 w-72" />
      <Skeleton className="h-48 w-full" />
    </div>
  )
}

function NotFound() {
  return (
    <Empty
      className="mt-10"
      icon={<Compass size={26} weight="bold" />}
      title="This page isn’t on Fetch"
      body="The link may be old or mistyped."
      action={<ButtonLink to="/">Go to the home page</ButtonLink>}
    />
  )
}
