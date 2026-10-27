package testfixture

import "net/http"

// reportsVersion identifies the reports seed data.
const reportsVersion = "reports-v1"

// monthlySales is a preseeded report snapshot. Reports never query the other
// fixtures, so their content depends only on this seed.
type monthlySales struct {
	Month        string `json:"month"`
	Orders       int    `json:"orders"`
	RevenueCents int    `json:"revenue_cents"`
}

type reports struct {
	sales []monthlySales
}

func (d *reports) reset() {
	d.sales = []monthlySales{
		{Month: "2026-07", Orders: 12, RevenueCents: 184500},
		{Month: "2026-08", Orders: 15, RevenueCents: 230250},
		{Month: "2026-09", Orders: 9, RevenueCents: 121000},
	}
}

func (d *reports) snapshot() any {
	return map[string]any{"sales": d.sales}
}

func (d *reports) operations() []operation {
	return []operation{
		{id: "reports.sales", method: http.MethodGet, pattern: "/reports/sales", run: d.listSales},
		{id: "reports.summary", method: http.MethodGet, pattern: "/reports/summary", run: d.summary},
	}
}

func (d *reports) listSales(_ string, _ *http.Request, _ []byte) result {
	return result{status: http.StatusOK, body: map[string]any{"sales": d.sales}}
}

func (d *reports) summary(_ string, _ *http.Request, _ []byte) result {
	orders, revenue := 0, 0
	for _, month := range d.sales {
		orders += month.Orders
		revenue += month.RevenueCents
	}
	body := map[string]int{"months": len(d.sales), "orders": orders, "revenue_cents": revenue}
	return result{status: http.StatusOK, body: body}
}
