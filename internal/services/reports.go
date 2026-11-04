package services

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type reports struct{ orders *serviceClient }

// Reports own no order data. Reset clears the service's receipts; every report
// reads the current Orders API rather than a separately seeded or stale copy.
func (d *reports) reset()        {}
func (d *reports) snapshot() any { return map[string]any{"source": "orders"} }
func (d *reports) close()        { d.orders.close() }
func (d *reports) load(ctx context.Context) ([]Order, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	orders := []Order{}
	after := ""
	seen := map[string]bool{}
	for len(orders) < 20000 {
		path := "/internal/orders?limit=100"
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		status, body, err := d.orders.call(ctx, "GET", path, nil, "")
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, errors.New("orders unavailable")
		}
		var page struct {
			Items []Order `json:"items"`
			Next  string  `json:"next"`
		}
		if json.Unmarshal(body, &page) != nil || page.Items == nil {
			return nil, errors.New("invalid orders response")
		}
		orders = append(orders, page.Items...)
		if page.Next == "" {
			return orders, nil
		}
		if seen[page.Next] || len(page.Items) == 0 {
			return nil, errors.New("invalid orders cursor")
		}
		seen[page.Next] = true
		after = page.Next
	}
	return nil, errors.New("report source exceeds its bound")
}

type reportCount struct {
	Orders  int   `json:"orders"`
	Revenue int64 `json:"revenue_sen"`
}

func (d *reports) handle(r *http.Request, body []byte) result {
	if r.Method != "GET" {
		return notFound()
	}
	if !slices.Contains([]string{"/reports/merchant", "/reports/summary", "/reports/sales", "/reports/top-items"}, r.URL.Path) {
		return notFound()
	}
	if r.Header.Get("X-Gateway-Subject") == "" {
		return notFound()
	}
	q, ok := query(r, "from", "to", "limit")
	if !ok || r.URL.Path != "/reports/top-items" && q.Has("limit") {
		return invalidQuery()
	}
	to := time.Now().UTC().Truncate(24 * time.Hour)
	from := to.AddDate(0, 0, -89)
	var err error
	if q.Has("from") {
		from, err = time.Parse(time.DateOnly, q.Get("from"))
		if err != nil {
			return invalidQuery()
		}
	}
	if q.Has("to") {
		to, err = time.Parse(time.DateOnly, q.Get("to"))
		if err != nil {
			return invalidQuery()
		}
	}
	if from.After(to) {
		return invalidQuery()
	}
	limit, _, ok := pagination(q)
	if !ok {
		return invalidQuery()
	}
	merchantID := ""
	if r.URL.Path == "/reports/merchant" {
		user, found := userByID(r.Header.Get("X-Gateway-Subject"))
		if !found || !hasRole(r, "MERCHANT") || user.Merchant == nil {
			return notFound()
		}
		merchantID = *user.Merchant
	}
	orders, err := d.load(r.Context())
	if err != nil {
		// A caller that went away is not an Orders outage; the receipt says which.
		if r.Context().Err() != nil {
			return failure(499, "request_cancelled", "The caller cancelled the request before the report was ready.", "")
		}
		return failure(503, "orders_unavailable", "Orders could not supply a complete report. Try again later.", "")
	}
	selected := []Order{}
	for _, order := range orders {
		if order.Status != "cancelled" && !order.CreatedAt.Before(from) && order.CreatedAt.Before(to.AddDate(0, 0, 1)) && (merchantID == "" || order.Merchant == merchantID) {
			selected = append(selected, order)
		}
	}
	summary := reportCount{}
	customers, merchants := map[string]bool{}, map[string]bool{}
	for _, order := range selected {
		summary.Orders++
		summary.Revenue += order.Total
		customers[order.Customer] = true
		merchants[order.Merchant] = true
	}
	switch r.URL.Path {
	case "/reports/summary":
		average := int64(0)
		if summary.Orders > 0 {
			average = (summary.Revenue + int64(summary.Orders)/2) / int64(summary.Orders)
		}
		return result{status: 200, body: map[string]any{"from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly), "orders": summary.Orders, "revenue_sen": summary.Revenue, "customers": len(customers), "merchants": len(merchants), "average_order_sen": average}}
	case "/reports/merchant":
		type day struct {
			Date string `json:"date"`
			reportCount
		}
		days := map[string]*day{}
		for _, order := range selected {
			date := order.CreatedAt.Format(time.DateOnly)
			if days[date] == nil {
				days[date] = &day{Date: date}
			}
			days[date].Orders++
			days[date].Revenue += order.Total
		}
		byDay := []day{}
		for _, date := range slices.Sorted(maps.Keys(days)) {
			byDay = append(byDay, *days[date])
		}
		return result{status: 200, body: map[string]any{"merchant": merchantID, "from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly), "orders": summary.Orders, "revenue_sen": summary.Revenue, "by_day": byDay}}
	case "/reports/sales":
		type month struct {
			Month   string `json:"month"`
			Section string `json:"section"`
			reportCount
		}
		months := map[string]*month{}
		sections := map[string]string{}
		for _, merchant := range seedMerchants() {
			sections[merchant.ID] = merchant.Section
		}
		for _, order := range selected {
			date := order.CreatedAt.Format("2006-01")
			section := sections[order.Merchant]
			key := date + ":" + section
			if months[key] == nil {
				months[key] = &month{Month: date, Section: section}
			}
			months[key].Orders++
			months[key].Revenue += order.Total
		}
		items := []month{}
		for _, key := range slices.Sorted(maps.Keys(months)) {
			items = append(items, *months[key])
		}
		return result{status: 200, body: map[string]any{"months": items}}
	default:
		type topItem struct {
			Item     string `json:"item"`
			Name     string `json:"name"`
			Merchant string `json:"merchant"`
			Quantity int    `json:"quantity"`
			Revenue  int64  `json:"revenue_sen"`
		}
		groups := map[string]*topItem{}
		for _, order := range selected {
			for _, line := range order.Items {
				if groups[line.Item] == nil {
					groups[line.Item] = &topItem{Item: line.Item, Name: line.Name, Merchant: order.Merchant}
				}
				groups[line.Item].Quantity += line.Quantity
				groups[line.Item].Revenue += int64(line.Quantity) * line.UnitPrice
			}
		}
		items := []topItem{}
		for _, item := range groups {
			items = append(items, *item)
		}
		slices.SortFunc(items, func(a, b topItem) int {
			if a.Quantity != b.Quantity {
				return b.Quantity - a.Quantity
			}
			return strings.Compare(a.Item, b.Item)
		})
		return result{status: 200, body: map[string]any{"items": items[:min(limit, len(items))]}}
	}
}
