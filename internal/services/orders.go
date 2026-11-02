package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type Address struct {
	Line1    string `json:"line1"`
	Line2    string `json:"line2"`
	Postcode string `json:"postcode"`
	City     string `json:"city"`
	State    string `json:"state"`
}
type TimelineEntry struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}
type Order struct {
	ID           string          `json:"id"`
	Customer     string          `json:"customer"`
	Merchant     string          `json:"merchant"`
	MerchantName string          `json:"merchant_name"`
	Items        []PricedLine    `json:"items"`
	Subtotal     int64           `json:"subtotal_sen"`
	DeliveryFee  int64           `json:"delivery_fee_sen"`
	Total        int64           `json:"total_sen"`
	Payment      string          `json:"payment"`
	Address      Address         `json:"address"`
	Note         string          `json:"note"`
	Status       string          `json:"status"`
	Rider        *string         `json:"rider"`
	Timeline     []TimelineEntry `json:"timeline"`
	CreatedAt    time.Time       `json:"created_at"`
}
type orderRequest struct {
	Merchant string  `json:"merchant"`
	Items    []Line  `json:"items"`
	Address  Address `json:"address"`
	Payment  string  `json:"payment"`
	Note     string  `json:"note"`
}
type savedOrderResult struct {
	digest [32]byte
	status int
	body   json.RawMessage
}
type orders struct {
	inventory      *serviceClient
	list           []Order
	wallets        map[string]int64
	next           int
	idempotency    map[string]savedOrderResult
	reservations   map[string]string
	pendingRelease map[string]bool
}

var orderStatuses = []string{"placed", "accepted", "ready", "picked_up", "delivered", "cancelled"}
var states = []string{"Johor", "Kedah", "Kelantan", "Melaka", "Negeri Sembilan", "Pahang", "Perak", "Perlis", "Pulau Pinang", "Sabah", "Sarawak", "Selangor", "Terengganu", "Wilayah Persekutuan Kuala Lumpur", "Wilayah Persekutuan Labuan", "Wilayah Persekutuan Putrajaya"}

func (d *orders) reset() {
	d.list = seedOrders()
	d.wallets = map[string]int64{"cust-001": 10000, "cust-002": 10000, "cust-003": 10000}
	d.next = 1005
	d.idempotency = map[string]savedOrderResult{}
	d.reservations = map[string]string{}
	d.pendingRelease = map[string]bool{}
}
func (d *orders) close() { d.inventory.close() }
func (d *orders) snapshot() any {
	return struct {
		Orders  []Order          `json:"orders"`
		Wallets map[string]int64 `json:"wallets"`
		Next    int              `json:"next"`
		Pending int              `json:"pending_releases"`
	}{d.list, d.wallets, d.next, len(d.pendingRelease)}
}
func (d *orders) find(id string) *Order {
	for i := range d.list {
		if d.list[i].ID == id {
			return &d.list[i]
		}
	}
	return nil
}
func (d *orders) fee(subject string, merchant Merchant) (int64, *string) {
	if merchant.Section == "mart" {
		for _, order := range d.list {
			if order.Customer == subject && order.Status != "cancelled" && (order.Merchant == "MER-005" || order.Merchant == "MER-006") {
				return merchant.DeliveryFee, nil
			}
		}
		reason := "first_mart_order"
		return 0, &reason
	}
	return merchant.DeliveryFee, nil
}
func (d *orders) release(ctx context.Context, key string) bool {
	// Compensation survives caller cancellation and uses one stable identity.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	for range 2 {
		status, _, err := d.inventory.call(cleanup, "DELETE", "/internal/reservations/"+key, nil, key)
		if err == nil && status == 200 {
			delete(d.pendingRelease, key)
			return true
		}
		if cleanup.Err() != nil {
			break
		}
	}
	d.pendingRelease[key] = true
	return false
}
func inventoryUnavailable() result {
	return failure(503, "inventory_unavailable", "Inventory could not confirm this request. Check the order before trying again.", "")
}

func (d *orders) create(r *http.Request, body []byte) result {
	subject := r.Header.Get("X-Gateway-Subject")
	if _, found := d.wallets[subject]; !found || !hasRole(r, "CUSTOMER") {
		return notFound()
	}
	values := r.Header.Values("Idempotency-Key")
	if len(values) > 1 || len(values) == 1 && !idempotencyKey(values[0]) {
		return failure(400, "invalid_request", "Use an idempotency key of 16 to 64 letters, digits, underscores or hyphens.", "Idempotency-Key")
	}
	key := r.Header.Get("Idempotency-Key")
	scope := subject + ":" + key
	digest := sha256.Sum256(body)
	if previous, found := d.idempotency[scope]; key != "" && found {
		if previous.digest != digest {
			return failure(409, "idempotency_conflict", "This checkout key was used for a different order.", "")
		}
		return result{status: previous.status, body: previous.body}
	}
	res := d.place(r, body, key)
	if key != "" {
		encoded, _ := json.Marshal(res.body)
		d.idempotency[scope] = savedOrderResult{digest, res.status, encoded}
		res.body = json.RawMessage(encoded)
	}
	return res
}
func (d *orders) place(r *http.Request, body []byte, key string) result {
	var request orderRequest
	if rejected := decodeBody(r, body, 4096, &request); rejected != nil {
		return *rejected
	}
	if !validOrderRequest(request) {
		return failure(400, "invalid_request", "Check the items, delivery address and payment choice.", "")
	}
	subject := r.Header.Get("X-Gateway-Subject")
	if key == "" {
		key = randomID()
	}
	hash := sha256.Sum256([]byte(subject + ":" + key))
	reservationKey := hex.EncodeToString(hash[:])
	data, _ := json.Marshal(purchase{request.Merchant, request.Items})
	status, reply, err := d.inventory.call(r.Context(), "POST", "/internal/reservations", data, reservationKey)
	if err != nil || status >= 500 || status == 401 {
		d.release(r.Context(), reservationKey)
		return inventoryUnavailable()
	}
	if status != 200 {
		var problem apiError
		if json.Unmarshal(reply, &problem) != nil || problem.Error == "" {
			return inventoryUnavailable()
		}
		return result{status: status, body: problem}
	}
	var reserved reservationResult
	if json.Unmarshal(reply, &reserved) != nil || reserved.Merchant.ID != request.Merchant || len(reserved.Items) != len(request.Items) {
		d.release(r.Context(), reservationKey)
		return inventoryUnavailable()
	}
	fee, _ := d.fee(subject, reserved.Merchant)
	total := reserved.Subtotal + fee
	if request.Payment == "wallet" && d.wallets[subject] < total {
		d.release(r.Context(), reservationKey)
		return failure(409, "insufficient_wallet", "Your Fetch wallet balance is too low for this order.", "payment")
	}
	now := time.Now().UTC()
	created := Order{ID: fmt.Sprintf("ORD-%04d", d.next), Customer: subject, Merchant: request.Merchant, MerchantName: reserved.Merchant.Name, Items: reserved.Items, Subtotal: reserved.Subtotal, DeliveryFee: fee, Total: total, Payment: request.Payment, Address: request.Address, Note: request.Note, Status: "placed", Timeline: []TimelineEntry{{"placed", now}}, CreatedAt: now}
	if request.Payment == "wallet" {
		d.wallets[subject] -= total
	}
	d.list = append(d.list, created)
	d.next++
	d.reservations[created.ID] = reservationKey
	return result{status: 201, body: map[string]any{"order": created}, effect: "created " + created.ID}
}
func validOrderRequest(request orderRequest) bool {
	if !length(request.Merchant, 7, 7) ||
		len(request.Items) < 1 || len(request.Items) > 20 ||
		!length(request.Note, 0, 200) ||
		!length(request.Address.Line1, 1, 100) ||
		!length(request.Address.Line2, 0, 100) ||
		!length(request.Address.Postcode, 5, 5) ||
		!length(request.Address.City, 1, 40) ||
		!slices.Contains(states, request.Address.State) ||
		(request.Payment != "cash" && request.Payment != "wallet") {
		return false
	}
	for _, line := range request.Items {
		if !length(line.Item, 7, 7) || line.Quantity < 1 || line.Quantity > 50 {
			return false
		}
	}
	return true
}
func seesOrder(r *http.Request, order Order) bool {
	subject := r.Header.Get("X-Gateway-Subject")
	return hasRole(r, "SUPPORT") || hasRole(r, "OPERATIONS") ||
		hasRole(r, "CUSTOMER") && order.Customer == subject ||
		ownsMerchant(r, order.Merchant) ||
		hasRole(r, "RIDER") && order.Rider != nil && *order.Rider == subject
}
func (d *orders) transition(r *http.Request, id, action string) result {
	order := d.find(id)
	if order == nil {
		return notFound()
	}
	subject := r.Header.Get("X-Gateway-Subject")
	want, next := "", ""
	switch action {
	case "accept", "ready":
		if !ownsMerchant(r, order.Merchant) {
			return notFound()
		}
		if action == "accept" {
			want, next = "placed", "accepted"
		} else {
			want, next = "accepted", "ready"
		}
	case "cancel":
		if !hasRole(r, "CUSTOMER") || order.Customer != subject {
			return notFound()
		}
		want, next = "placed", "cancelled"
	case "pickup":
		if !hasRole(r, "RIDER") || subject == "" {
			return notFound()
		}
		if order.Rider != nil && *order.Rider != subject {
			return failure(409, "already_assigned", "Another rider has taken this order.", "")
		}
		want, next = "ready", "picked_up"
	case "deliver":
		if !hasRole(r, "RIDER") || order.Rider == nil || *order.Rider != subject {
			return notFound()
		}
		want, next = "picked_up", "delivered"
	default:
		return notFound()
	}
	if order.Status != want {
		return failure(409, "invalid_transition", "That action is not available for the current order status.", "")
	}
	releaseConfirmed := true
	if key := d.reservations[id]; key != "" {
		if action == "cancel" {
			releaseConfirmed = d.release(r.Context(), key)
		}
		if action == "deliver" {
			status, _, err := d.inventory.call(r.Context(), "POST", "/internal/reservations/"+key+"/commit", nil, key)
			if err != nil || status != 200 {
				return inventoryUnavailable()
			}
		}
	}
	if action == "pickup" {
		rider := subject
		order.Rider = &rider
	}
	if action == "cancel" && order.Payment == "wallet" {
		d.wallets[subject] += order.Total
	}
	order.Status = next
	order.Timeline = append(order.Timeline, TimelineEntry{next, time.Now().UTC()})
	if !releaseConfirmed {
		return inventoryUnavailable()
	}
	return result{status: 200, body: map[string]any{"order": *order}, effect: next + " " + id}
}

func (d *orders) handle(r *http.Request, body []byte) result {
	// Retry one outstanding compensation per arrival, never the checkout.
	// Every attempt has its own bound and reuses the reservation identity.
	if pending := slices.Sorted(maps.Keys(d.pendingRelease)); len(pending) > 0 {
		d.release(r.Context(), pending[0])
	}
	if r.Method == "POST" && r.URL.Path == "/orders" {
		return d.create(r, body)
	}
	if r.Method == "POST" {
		if len(body) != 0 {
			return failure(400, "invalid_request", "This operation accepts no request body.", "")
		}
		for _, action := range []string{"cancel", "accept", "ready", "pickup", "deliver"} {
			if id, ok := oneID(r.URL.Path, "/orders/"+action+"/"); ok {
				return d.transition(r, id, action)
			}
		}
	}
	if r.Method != "GET" {
		return notFound()
	}
	path := r.URL.Path
	subject := r.Header.Get("X-Gateway-Subject")
	if path == "/orders/wallet" {
		if _, ok := query(r); !ok {
			return invalidQuery()
		}
		balance, found := d.wallets[subject]
		if !found || !hasRole(r, "CUSTOMER") {
			return notFound()
		}
		return result{status: 200, body: map[string]any{"balance_sen": balance}}
	}
	if path == "/orders/quote" {
		q, ok := query(r, "merchant")
		if !ok || q.Get("merchant") == "" {
			return invalidQuery()
		}
		if _, found := d.wallets[subject]; !found || !hasRole(r, "CUSTOMER") {
			return notFound()
		}
		status, body, err := d.inventory.call(r.Context(), "GET", "/internal/merchants/"+url.PathEscape(q.Get("merchant")), nil, "")
		if err != nil || status >= 500 {
			return inventoryUnavailable()
		}
		if status != 200 {
			return notFound()
		}
		var response struct {
			Merchant Merchant `json:"merchant"`
		}
		if json.Unmarshal(body, &response) != nil || response.Merchant.ID == "" {
			return inventoryUnavailable()
		}
		fee, reason := d.fee(subject, response.Merchant)
		return result{status: 200, body: map[string]any{"merchant": response.Merchant.ID, "delivery_fee_sen": fee, "free_delivery": reason != nil, "reason": reason}}
	}
	if id, ok := oneID(path, "/orders/detail/"); ok {
		if _, ok := query(r); !ok {
			return invalidQuery()
		}
		order := d.find(id)
		if order == nil || !seesOrder(r, *order) {
			return notFound()
		}
		return result{status: 200, body: map[string]any{"order": *order}}
	}
	if path == "/orders/jobs" {
		if _, ok := query(r); !ok {
			return invalidQuery()
		}
		if !hasRole(r, "RIDER") {
			return notFound()
		}
		available, mine := []Order{}, []Order{}
		for _, order := range d.list {
			if order.Status == "ready" && order.Rider == nil {
				available = append(available, order)
			}
			if order.Rider != nil && *order.Rider == subject && order.Status == "picked_up" {
				mine = append(mine, order)
			}
		}
		return result{status: 200, body: map[string]any{"available": available, "mine": mine}}
	}
	if path == "/orders" || path == "/orders/mine" || path == "/orders/merchant" || path == "/internal/orders" {
		q, ok := query(r, "status", "limit", "after")
		if !ok {
			return invalidQuery()
		}
		if q.Get("status") != "" && !slices.Contains(orderStatuses, q.Get("status")) {
			return invalidQuery()
		}
		n, after, ok := pagination(q)
		if !ok {
			return invalidQuery()
		}
		if path == "/orders/mine" && !hasRole(r, "CUSTOMER") {
			return notFound()
		}
		if path == "/orders" && !hasRole(r, "SUPPORT") && !hasRole(r, "OPERATIONS") {
			return notFound()
		}
		var ownMerchant string
		if path == "/orders/merchant" {
			user, found := userByID(subject)
			if !found || !hasRole(r, "MERCHANT") || user.Merchant == nil {
				return notFound()
			}
			ownMerchant = *user.Merchant
			if q.Has("limit") || q.Has("after") {
				return invalidQuery()
			}
			n = len(d.list)
		}
		if path == "/orders/mine" && q.Has("status") {
			return invalidQuery()
		}
		ordered := slices.Clone(d.list)
		slices.SortFunc(ordered, func(a, b Order) int {
			if compared := b.CreatedAt.Compare(a.CreatedAt); compared != 0 {
				return compared
			}
			return strings.Compare(b.ID, a.ID)
		})
		items := []Order{}
		seen := after == ""
		for _, order := range ordered {
			if !seen {
				if order.ID == after {
					seen = true
				}
				continue
			}
			if path == "/orders/mine" && order.Customer != subject || ownMerchant != "" && order.Merchant != ownMerchant || q.Get("status") != "" && order.Status != q.Get("status") {
				continue
			}
			items = append(items, order)
		}
		if !seen {
			return invalidQuery()
		}
		response := map[string]any{"items": items[:min(n, len(items))]}
		if len(items) > n {
			response["next"] = items[n-1].ID
		}
		return result{status: 200, body: response}
	}
	return notFound()
}
