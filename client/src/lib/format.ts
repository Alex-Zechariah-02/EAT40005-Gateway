import type { Merchant, OrderStatus } from './types'

// Money is an integer number of sen; Malaysia writes it as "RM 12.90".
export function rm(sen: number): string {
  const sign = sen < 0 ? '−' : ''
  const value = Math.abs(sen)
  return `${sign}RM ${Math.floor(value / 100).toLocaleString('en-MY')}.${String(value % 100).padStart(2, '0')}`
}

export function minutes(merchant: Merchant): string {
  return `${merchant.delivery_minutes.min}–${merchant.delivery_minutes.max} min`
}

const timeFormat = new Intl.DateTimeFormat('en-MY', { hour: 'numeric', minute: '2-digit', timeZone: 'Asia/Kuala_Lumpur' })
const dateFormat = new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short', timeZone: 'Asia/Kuala_Lumpur' })
const dayFormat = new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'Asia/Kuala_Lumpur' })

// Times are shown in Malaysian time, regardless of the computer's time zone.
export function time(iso: string): string {
  return timeFormat.format(new Date(iso))
}

export function when(iso: string): string {
  const date = new Date(iso)
  const today = dateFormat.format(new Date())
  const day = dateFormat.format(date)
  return day === today ? `Today, ${timeFormat.format(date)}` : `${day}, ${timeFormat.format(date)}`
}

// reference shows an order or ticket ID the way people quote it, as a short
// number: ORD-1001 reads #1001. The full ID stays in links and requests.
export function reference(id: string): string {
  return `#${id.replace(/^[A-Z]+-/, '')}`
}

export function day(isoDate: string): string {
  return dayFormat.format(new Date(isoDate))
}

export const statusNames: Record<OrderStatus, string> = {
  placed: 'Placed',
  accepted: 'Being prepared',
  ready: 'Ready for pickup',
  picked_up: 'On the way',
  delivered: 'Delivered',
  cancelled: 'Cancelled',
}

export function plural(count: number, one: string, many = `${one}s`): string {
  return `${count.toLocaleString('en-MY')} ${count === 1 ? one : many}`
}
