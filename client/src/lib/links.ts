import type { List, Merchant, MerchantPage } from './types'
import { useQuery } from './query'

// Shops and items have readable addresses made from their names:
// /store/warung-nasi-lemak and /store/warung-nasi-lemak?item=teh-tarik. The
// services still name them by ID; the pages below resolve one to the other.
export function slugOf(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')
}

export function storeLink(merchantName: string): string {
  return `/store/${slugOf(merchantName)}`
}

export function itemLink(merchantName: string, itemName: string): string {
  return `${storeLink(merchantName)}?item=${slugOf(itemName)}`
}

// An older link names the shop by its ID, which still opens it.
const merchantId = /^MER-\d+$/

// useMerchantId resolves a shop address to the shop's ID. It is undefined
// while the list is read and null when no shop has that address.
export function useMerchantId(address: string): { id: string | null | undefined; list: ReturnType<typeof useQuery<List<Merchant>>> | null } {
  const byId = merchantId.test(address)
  const list = useQuery<List<Merchant>>(byId ? null : '/inventory/merchants?limit=50')
  if (byId) return { id: address, list: null }
  if (!list.data) return { id: undefined, list }
  return { id: list.data.items.find((merchant) => slugOf(merchant.name) === address)?.id ?? null, list }
}

// itemIdIn finds an item of a shop's menu by its address.
export function itemIdIn(page: MerchantPage, address: string): string | null {
  for (const group of page.menu) for (const item of group.items) if (slugOf(item.name) === address || item.id === address) return item.id
  return null
}
