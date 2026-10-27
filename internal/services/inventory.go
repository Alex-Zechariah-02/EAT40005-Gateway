package services

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

type inventory struct {
	merchants    []Merchant
	items        []stockItem
	reservations map[string]reservation
}

func (d *inventory) reset() {
	d.merchants = seedMerchants()
	d.items = seedItems()
	d.reservations = map[string]reservation{}
}
func (d *inventory) snapshot() any {
	return struct {
		Merchants []Merchant  `json:"merchants"`
		Items     []stockItem `json:"items"`
	}{d.merchants, d.items}
}
func (d *inventory) merchant(id string) (Merchant, bool) {
	for _, merchant := range d.merchants {
		if merchant.ID == id {
			return merchant, true
		}
	}
	return Merchant{}, false
}
func (d *inventory) item(id string) *stockItem {
	for i := range d.items {
		if d.items[i].Item.ID == id {
			return &d.items[i]
		}
	}
	return nil
}
func publicItem(stock stockItem) Item {
	item := stock.Item
	switch {
	case stock.Stock == nil:
		item.StockState = "made_to_order"
	case *stock.Stock-stock.Reserved <= 0:
		item.StockState = "out"
	case *stock.Stock-stock.Reserved <= 15:
		item.StockState = "low"
	default:
		item.StockState = "in_stock"
	}
	return item
}
func ownsMerchant(r *http.Request, merchant string) bool {
	user, found := userByID(r.Header.Get("X-Gateway-Subject"))
	return found && hasRole(r, "MERCHANT") && user.Merchant != nil && *user.Merchant == merchant
}

func (d *inventory) handle(r *http.Request, body []byte) result {
	path := r.URL.Path
	if path == "/internal/reservations" && r.Method == "POST" {
		return d.reserve(r, body)
	}
	if id, ok := oneID(path, "/internal/merchants/"); ok && r.Method == "GET" {
		merchant, found := d.merchant(id)
		if !found {
			return notFound()
		}
		return result{status: 200, body: map[string]any{"merchant": merchant}}
	}
	if id, ok := oneID(path, "/internal/reservations/"); ok && r.Method == "DELETE" {
		return d.release(id)
	}
	if id, ok := oneID(strings.TrimSuffix(path, "/commit"), "/internal/reservations/"); ok && strings.HasSuffix(path, "/commit") && r.Method == "POST" {
		return d.commit(id)
	}
	if r.Method == "GET" && path == "/inventory/categories" {
		if _, ok := query(r); !ok {
			return invalidQuery()
		}
		return result{status: 200, body: map[string]any{"items": seedCategories()}}
	}
	if r.Method == "GET" && path == "/inventory/merchants" {
		q, ok := query(r, "section", "category", "limit", "after")
		if !ok {
			return invalidQuery()
		}
		n, after, ok := pagination(q)
		if !ok || !validSection(q.Get("section")) {
			return invalidQuery()
		}
		if category := q.Get("category"); category != "" && !slices.ContainsFunc(seedCategories(), func(c Category) bool { return c.ID == category }) {
			return invalidQuery()
		}
		if after != "" {
			if _, found := d.merchant(after); !found {
				return invalidQuery()
			}
		}
		items := []Merchant{}
		for _, merchant := range d.merchants {
			if merchant.ID > after && (q.Get("section") == "" || merchant.Section == q.Get("section")) && (q.Get("category") == "" || merchant.Category == q.Get("category")) {
				items = append(items, merchant)
			}
		}
		response := map[string]any{"items": items[:min(n, len(items))]}
		if len(items) > n {
			response["next"] = items[n-1].ID
		}
		return result{status: 200, body: response}
	}
	if r.Method == "GET" && path == "/inventory/search" {
		q, ok := query(r, "q", "section")
		if !ok || !length(q.Get("q"), 2, 40) || !validSection(q.Get("section")) {
			return invalidQuery()
		}
		items := []map[string]any{}
		search := strings.ToLower(q.Get("q"))
		for _, stock := range d.items {
			merchant, _ := d.merchant(stock.Item.Merchant)
			if (q.Get("section") == "" || merchant.Section == q.Get("section")) && strings.Contains(strings.ToLower(stock.Item.Name+" "+stock.Item.Description+" "+merchant.Name), search) {
				items = append(items, map[string]any{"merchant": merchant, "item": publicItem(stock)})
			}
		}
		return result{status: 200, body: map[string]any{"items": items}}
	}
	if id, ok := oneID(path, "/inventory/merchants/"); ok && r.Method == "GET" {
		if _, ok := query(r); !ok {
			return invalidQuery()
		}
		merchant, found := d.merchant(id)
		if !found {
			return notFound()
		}
		type group struct {
			Group string `json:"group"`
			Items []Item `json:"items"`
		}
		menu := []group{}
		for _, stock := range d.items {
			if stock.Item.Merchant != id {
				continue
			}
			index := slices.IndexFunc(menu, func(g group) bool { return g.Group == stock.Item.Group })
			if index < 0 {
				menu = append(menu, group{stock.Item.Group, []Item{}})
				index = len(menu) - 1
			}
			menu[index].Items = append(menu[index].Items, publicItem(stock))
		}
		return result{status: 200, body: map[string]any{"merchant": merchant, "menu": menu}}
	}
	if id, ok := oneID(path, "/inventory/items/"); ok {
		stock := d.item(id)
		if stock == nil {
			return notFound()
		}
		switch r.Method {
		case "GET":
			if _, ok := query(r); !ok {
				return invalidQuery()
			}
			merchant, _ := d.merchant(stock.Item.Merchant)
			return result{status: 200, body: map[string]any{"item": publicItem(*stock), "merchant": merchant}}
		case "PATCH":
			if !ownsMerchant(r, stock.Item.Merchant) {
				return notFound()
			}
			var change struct {
				Price     *int64 `json:"price_sen"`
				Available *bool  `json:"available"`
			}
			if rejected := decodeBody(r, body, 512, &change); rejected != nil {
				return *rejected
			}
			if change.Price == nil && change.Available == nil || change.Price != nil && (*change.Price < 50 || *change.Price > 100000) {
				return failure(422, "invalid_change", "Choose a price or availability change within the allowed range.", "")
			}
			if change.Price != nil {
				stock.Item.Price = *change.Price
			}
			if change.Available != nil {
				stock.Item.Available = *change.Available
			}
			return result{status: 200, body: map[string]any{"item": publicItem(*stock)}, effect: "updated " + id}
		}
	}
	if id, ok := oneID(path, "/inventory/stock/"); ok && (r.Method == "GET" || r.Method == "PATCH") {
		stock := d.item(id)
		if stock == nil || r.Header.Get("X-Gateway-Subject") == "" {
			return notFound()
		}
		// Authorization is the gateway's decision, including temporary grants.
		// Only merchant ownership belongs here; Operations can see all stores.
		if hasRole(r, "MERCHANT") && !hasRole(r, "OPERATIONS") && !ownsMerchant(r, stock.Item.Merchant) {
			return notFound()
		}
		if stock.Stock == nil {
			return failure(422, "invalid_change", "This item is made to order and has no stock count.", "item")
		}
		if r.Method == "PATCH" {
			var change struct {
				Delta *int `json:"delta"`
			}
			if rejected := decodeBody(r, body, 256, &change); rejected != nil {
				return *rejected
			}
			if change.Delta == nil || *change.Delta == 0 || *change.Delta < -100 || *change.Delta > 100 {
				return failure(422, "invalid_change", "Choose a stock change from -100 to 100, excluding zero.", "delta")
			}
			if *stock.Stock+*change.Delta < stock.Reserved {
				return failure(409, "insufficient_stock", "That change would remove stock reserved for orders.", "delta")
			}
			*stock.Stock += *change.Delta
		} else if _, ok := query(r); !ok {
			return invalidQuery()
		}
		return result{status: 200, body: map[string]any{"item": id, "stock": *stock.Stock, "reserved": stock.Reserved}}
	}
	return notFound()
}

type Line struct {
	Item     string `json:"item"`
	Quantity int    `json:"quantity"`
}
type PricedLine struct {
	Item      string `json:"item"`
	Name      string `json:"name"`
	Quantity  int    `json:"quantity"`
	UnitPrice int64  `json:"unit_price_sen"`
}
type purchase struct {
	Merchant string `json:"merchant"`
	Items    []Line `json:"items"`
}
type reservationResult struct {
	Merchant Merchant     `json:"merchant"`
	Items    []PricedLine `json:"items"`
	Subtotal int64        `json:"subtotal_sen"`
}
type reservation struct {
	digest              [32]byte
	counts              map[string]int
	response            reservationResult
	released, committed bool
}

func (d *inventory) reserve(r *http.Request, body []byte) result {
	key := r.Header.Get("Idempotency-Key")
	if !idempotencyKey(key) {
		return failure(400, "invalid_request", "A valid idempotency key is required.", "Idempotency-Key")
	}
	digest := sha256.Sum256(body)
	if previous, found := d.reservations[key]; found {
		if previous.digest != digest || previous.released {
			return failure(409, "idempotency_conflict", "That reservation identity is already used.", "")
		}
		return result{status: 200, body: previous.response}
	}
	var request purchase
	if rejected := decodeBody(r, body, 4096, &request); rejected != nil {
		return *rejected
	}
	merchant, found := d.merchant(request.Merchant)
	if !found {
		return notFound()
	}
	if !merchant.Open {
		return failure(422, "merchant_closed", "This merchant is closed.", "merchant")
	}
	if len(request.Items) < 1 || len(request.Items) > 20 {
		return failure(422, "invalid_change", "Choose from one to twenty items.", "items")
	}
	response := reservationResult{Merchant: merchant, Items: []PricedLine{}}
	counts := map[string]int{}
	for i, line := range request.Items {
		field := fmt.Sprintf("items[%d].item", i)
		stock := d.item(line.Item)
		if stock == nil || !stock.Item.Available {
			return failure(422, "item_unavailable", "This item is unavailable.", field)
		}
		if stock.Item.Merchant != request.Merchant {
			return failure(422, "mixed_merchants", "Choose items from one merchant.", field)
		}
		if line.Quantity < 1 || line.Quantity > 50 {
			return failure(422, "invalid_change", "Choose a quantity from one to fifty.", fmt.Sprintf("items[%d].quantity", i))
		}
		counts[line.Item] += line.Quantity
		if stock.Stock != nil && counts[line.Item] > *stock.Stock-stock.Reserved {
			return failure(409, "out_of_stock", stock.Item.Name+" is out of stock.", field)
		}
		response.Items = append(response.Items, PricedLine{line.Item, stock.Item.Name, line.Quantity, stock.Item.Price})
		response.Subtotal += int64(line.Quantity) * stock.Item.Price
	}
	for id, quantity := range counts {
		if stock := d.item(id); stock.Stock != nil {
			stock.Reserved += quantity
		}
	}
	d.reservations[key] = reservation{digest: digest, counts: counts, response: response}
	return result{status: 200, body: response, effect: "reserved stock"}
}

func (d *inventory) release(key string) result {
	if !idempotencyKey(key) {
		return failure(400, "invalid_request", "A valid reservation identity is required.", "")
	}
	entry, found := d.reservations[key]
	if found && entry.committed {
		return failure(409, "invalid_transition", "This reservation was already consumed.", "")
	}
	if found && !entry.released {
		for id, quantity := range entry.counts {
			if stock := d.item(id); stock.Stock != nil {
				stock.Reserved -= quantity
			}
		}
	}
	// A release may arrive before a timed-out prepare. Keep a tombstone so
	// that delayed request cannot reserve orphaned stock afterwards.
	entry.released = true
	d.reservations[key] = entry
	return result{status: 200, body: map[string]any{"released": true}}
}

func (d *inventory) commit(key string) result {
	entry, found := d.reservations[key]
	if !found || entry.released {
		return notFound()
	}
	if !entry.committed {
		for id, quantity := range entry.counts {
			if stock := d.item(id); stock.Stock != nil {
				*stock.Stock -= quantity
				stock.Reserved -= quantity
			}
		}
		entry.committed = true
		d.reservations[key] = entry
	}
	return result{status: 200, body: map[string]any{"committed": true}}
}

func validSection(section string) bool {
	return section == "" || section == "food" || section == "mart"
}
func oneID(path, prefix string) (string, bool) {
	id, ok := strings.CutPrefix(path, prefix)
	return id, ok && id != "" && !strings.Contains(id, "/")
}
