// Development only (npm run dev:mock). A copy of the services' seed data so
// screens can run locally without the services. Never part of a production
// build. Descriptions other than ITM-001's are placeholders: the Inventory
// seed owns the real ones.
import type { Category, Item, Merchant, Order, Ticket, User } from '../lib/types'

export const password = 'FetchDemo2026!'

export const users: User[] = [
  { id: 'cust-001', name: 'Customer 1', email: 'customer1@fetch.test', roles: ['CUSTOMER'], merchant: null, home: '/' },
  { id: 'cust-002', name: 'Customer 2', email: 'customer2@fetch.test', roles: ['CUSTOMER'], merchant: null, home: '/' },
  { id: 'cust-003', name: 'Customer 3', email: 'customer3@fetch.test', roles: ['CUSTOMER'], merchant: null, home: '/' },
  { id: 'merchant-001', name: 'Merchant 1', email: 'merchant1@fetch.test', roles: ['MERCHANT'], merchant: 'MER-001', home: '/merchant' },
  { id: 'merchant-002', name: 'Merchant 2', email: 'merchant2@fetch.test', roles: ['MERCHANT'], merchant: 'MER-002', home: '/merchant' },
  { id: 'merchant-003', name: 'Merchant 3', email: 'merchant3@fetch.test', roles: ['MERCHANT'], merchant: 'MER-003', home: '/merchant' },
  { id: 'merchant-004', name: 'Merchant 4', email: 'merchant4@fetch.test', roles: ['MERCHANT'], merchant: 'MER-004', home: '/merchant' },
  { id: 'merchant-005', name: 'Merchant 5', email: 'merchant5@fetch.test', roles: ['MERCHANT'], merchant: 'MER-005', home: '/merchant' },
  { id: 'merchant-006', name: 'Merchant 6', email: 'merchant6@fetch.test', roles: ['MERCHANT'], merchant: 'MER-006', home: '/merchant' },
  { id: 'rider-01', name: 'Rider 1', email: 'rider1@fetch.test', roles: ['RIDER'], merchant: null, home: '/rider' },
  { id: 'rider-02', name: 'Rider 2', email: 'rider2@fetch.test', roles: ['RIDER'], merchant: null, home: '/rider' },
  { id: 'support-01', name: 'Support 1', email: 'support1@fetch.test', roles: ['SUPPORT'], merchant: null, home: '/support' },
  { id: 'ops-01', name: 'Admin 1', email: 'admin1@fetch.test', roles: ['OPERATIONS'], merchant: null, home: '/ops' },
  { id: 'lead-01', name: 'Admin 2', email: 'admin2@fetch.test', roles: ['SUPPORT', 'OPERATIONS'], merchant: null, home: '/ops' },
]

export const categories: Category[] = [
  { id: 'nasi-lemak', name: 'Nasi lemak', section: 'food' },
  { id: 'mamak', name: 'Mamak', section: 'food' },
  { id: 'kopitiam', name: 'Kopitiam', section: 'food' },
  { id: 'hawker', name: 'Hawker favourites', section: 'food' },
  { id: 'groceries', name: 'Groceries', section: 'mart' },
  { id: 'essentials', name: 'Health and household', section: 'mart' },
]

const m = (id: string, name: string, section: 'food' | 'mart', category: string, area: string, min: number, max: number, fee: number): Merchant => ({
  id, name, section, category, area, delivery_minutes: { min, max }, delivery_fee_sen: fee, open: true,
})

export const merchants: Merchant[] = [
  m('MER-001', 'Warung Nasi Lemak', 'food', 'nasi-lemak', 'Bangsar, Kuala Lumpur', 20, 35, 399),
  m('MER-002', 'Restoran Bintang Mamak', 'food', 'mamak', 'Petaling Jaya, Selangor', 15, 30, 299),
  m('MER-003', 'Kopitiam Lama', 'food', 'kopitiam', 'Cheras, Kuala Lumpur', 20, 35, 399),
  m('MER-004', 'Dapur Penang', 'food', 'hawker', 'Subang Jaya, Selangor', 25, 40, 499),
  m('MER-005', 'Fetch Mart Express', 'mart', 'groceries', 'Mont Kiara, Kuala Lumpur', 25, 40, 499),
  m('MER-006', 'Sihat Essentials', 'mart', 'essentials', 'Kuala Lumpur', 30, 45, 499),
]

// [id, merchant, name, group, price_sen, stock (null = made to order), available, description]
type Row = [string, string, string, string, number, number | null, boolean, string]
const rows: Row[] = [
  ['ITM-001', 'MER-001', 'Nasi Lemak Ayam Goreng', 'Mains', 1290, null, true, 'Coconut rice, sambal, fried chicken, egg and anchovies.'],
  ['ITM-002', 'MER-001', 'Nasi Lemak Rendang Daging', 'Mains', 1590, null, true, 'Coconut rice with slow-cooked beef rendang and sambal.'],
  ['ITM-003', 'MER-001', 'Nasi Lemak Telur Mata', 'Mains', 750, null, true, 'Coconut rice, sambal, anchovies and a fried egg.'],
  ['ITM-004', 'MER-001', 'Teh Tarik', 'Drinks', 350, null, true, 'Pulled milk tea, hot.'],
  ['ITM-011', 'MER-002', 'Roti Canai', 'Mains', 180, null, true, 'Flaky flatbread with dhal and curry.'],
  ['ITM-012', 'MER-002', 'Mee Goreng Mamak', 'Mains', 990, null, true, 'Wok-fried yellow noodles, egg, tofu and potato.'],
  ['ITM-013', 'MER-002', 'Maggi Goreng', 'Mains', 850, null, true, 'Fried instant noodles with egg and vegetables.'],
  ['ITM-014', 'MER-002', 'Milo Ais', 'Drinks', 450, null, true, 'Iced chocolate malt drink.'],
  ['ITM-021', 'MER-003', 'Kaya Butter Toast', 'Breakfast', 450, null, true, 'Charcoal-toasted bread with kaya and cold butter.'],
  ['ITM-022', 'MER-003', 'Half-boiled Eggs', 'Breakfast', 400, null, true, 'Two soft eggs with soy sauce and white pepper.'],
  ['ITM-023', 'MER-003', 'Kopi O Peng', 'Drinks', 380, null, true, 'Iced black coffee, sweetened.'],
  ['ITM-024', 'MER-003', 'Chicken Chop', 'Mains', 1890, null, true, 'Grilled chicken thigh, black pepper sauce, fries and coleslaw.'],
  ['ITM-031', 'MER-004', 'Char Kway Teow', 'Mains', 1250, null, true, 'Flat rice noodles fried with prawns, cockles and bean sprouts.'],
  ['ITM-032', 'MER-004', 'Assam Laksa', 'Mains', 1190, null, true, 'Thick rice noodles in a sour fish broth with pineapple and mint.'],
  ['ITM-033', 'MER-004', 'Cendol', 'Desserts', 650, null, false, 'Shaved ice, pandan jelly, coconut milk and palm sugar.'],
  ['ITM-501', 'MER-005', 'Rice 5 kg', 'Staples', 2690, 40, true, 'Local white rice, long grain.'],
  ['ITM-502', 'MER-005', 'Cooking Oil 1 kg', 'Staples', 690, 12, true, 'Refined palm olein for everyday cooking.'],
  ['ITM-503', 'MER-005', 'Fresh Eggs (30)', 'Dairy and eggs', 1590, 0, true, 'Grade A, one tray of thirty.'],
  ['ITM-504', 'MER-005', 'Full Cream Milk 1 L', 'Dairy and eggs', 790, 60, true, 'Fresh pasteurised milk, chilled.'],
  ['ITM-505', 'MER-005', 'Wholemeal Bread', 'Bakery', 450, 25, true, 'Sliced wholemeal loaf, baked daily.'],
  ['ITM-506', 'MER-005', 'Instant Noodles (5 pack)', 'Staples', 590, 80, true, 'Curry flavour, five packets.'],
  ['ITM-507', 'MER-005', 'Mineral Water 1.5 L (6 pack)', 'Beverages', 1090, 50, true, 'Six bottles of natural mineral water.'],
  ['ITM-508', 'MER-005', 'Bananas 1 kg', 'Fresh produce', 690, 30, true, 'Pisang berangan, ripe and ready.'],
  ['ITM-601', 'MER-006', 'Hand Sanitiser 500 ml', 'Health', 1290, 20, true, 'Alcohol-based hand rub.'],
  ['ITM-602', 'MER-006', 'Face Masks (50)', 'Health', 1590, 35, true, 'Three-ply disposable masks.'],
  ['ITM-603', 'MER-006', 'Vitamin C 1000 mg (30)', 'Health', 2490, 12, true, 'Thirty tablets.'],
  ['ITM-604', 'MER-006', 'Tissue Box (5 pack)', 'Household', 1290, 45, true, 'Five boxes of soft facial tissue.'],
]

export interface StockRow { stock: number | null; reserved: number }
export const stock = new Map<string, StockRow>(rows.map((r) => [r[0], { stock: r[5], reserved: 0 }]))

export const items: Item[] = rows.map(([id, merchant, name, group, price_sen, , available, description]) => ({
  id, merchant, name, description, group, price_sen, available, stock_state: 'made_to_order',
}))

const at = (minutesAgo: number) => new Date(Date.now() - minutesAgo * 60_000).toISOString()
const address = { line1: '12 Jalan Telawi 3', line2: '', postcode: '59100', city: 'Kuala Lumpur', state: 'Wilayah Persekutuan Kuala Lumpur' }

function order(id: string, customer: string, merchantId: string, lines: [string, number][], status: Order['status'], minutesAgo: number, payment: Order['payment'] = 'cash'): Order {
  const merchant = merchants.find((x) => x.id === merchantId)!
  const orderLines = lines.map(([itemId, quantity]) => {
    const item = items.find((x) => x.id === itemId)!
    return { item: itemId, name: item.name, quantity, unit_price_sen: item.price_sen }
  })
  const subtotal = orderLines.reduce((sum, line) => sum + line.quantity * line.unit_price_sen, 0)
  const path: Order['status'][] = status === 'cancelled' ? ['placed', 'cancelled'] : (['placed', 'accepted', 'ready', 'picked_up', 'delivered'] as const).slice(0, ['placed', 'accepted', 'ready', 'picked_up', 'delivered'].indexOf(status) + 1)
  return {
    id, customer, merchant: merchantId, merchant_name: merchant.name, items: orderLines,
    subtotal_sen: subtotal, delivery_fee_sen: merchant.delivery_fee_sen, total_sen: subtotal + merchant.delivery_fee_sen,
    payment, address, note: '', status, rider: status === 'picked_up' || status === 'delivered' ? 'rider-01' : null,
    timeline: path.map((s, i) => ({ status: s, at: at(minutesAgo - i * 6) })), created_at: at(minutesAgo),
  }
}

export const orders: Order[] = [
  order('ORD-1001', 'cust-001', 'MER-001', [['ITM-001', 1], ['ITM-004', 2]], 'placed', 4),
  order('ORD-1002', 'cust-002', 'MER-005', [['ITM-501', 1], ['ITM-504', 2]], 'delivered', 1440, 'wallet'),
  order('ORD-1003', 'cust-001', 'MER-002', [['ITM-011', 2], ['ITM-014', 1]], 'accepted', 18),
  order('ORD-1004', 'cust-003', 'MER-003', [['ITM-021', 1], ['ITM-022', 1], ['ITM-023', 1]], 'ready', 25),
]
// cust-001 has a past delivered Mart order, so free delivery does not apply to them.
orders.push({ ...order('ORD-0412', 'cust-001', 'MER-005', [['ITM-505', 1]], 'delivered', 20_160), created_at: at(20_160) })

export const tickets: Ticket[] = [
  {
    id: 'TKT-1001', customer: 'cust-002', customer_name: 'Customer 2', order: 'ORD-1002', request_ref: null, subject: 'Missing item', status: 'open',
    messages: [{ from: 'customer', author: 'cust-002', author_name: 'Customer 2', body: 'My order arrived without the second bottle of milk.', at: at(600) }],
    created_at: at(600),
  },
]

export const wallets = new Map<string, number>([['cust-001', 10000], ['cust-002', 10000], ['cust-003', 10000]])
