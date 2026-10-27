package testfixture

import (
	"fmt"
	"net/http"
)

// inventoryVersion identifies the inventory seed data and business rules.
const inventoryVersion = "inventory-v1"

const maxStockAdjustment = 100

type item struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Stock int    `json:"stock"`
}

type inventory struct {
	items []item
}

func (d *inventory) reset() {
	d.items = []item{
		{ID: "ITM-001", Name: "widget", Stock: 40},
		{ID: "ITM-002", Name: "gadget", Stock: 15},
		{ID: "ITM-003", Name: "gizmo", Stock: 0},
	}
}

func (d *inventory) snapshot() any {
	return map[string]any{"items": d.items}
}

func (d *inventory) operations() []operation {
	return []operation{
		{id: "inventory.health", method: http.MethodGet, pattern: "/inventory/health", run: d.health},
		{id: "inventory.get", method: http.MethodGet, pattern: "/inventory/items/{id}", run: d.getItem},
		{id: "inventory.adjust", method: http.MethodPatch, pattern: "/inventory/items/{id}", run: d.adjustStock},
	}
}

func (d *inventory) health(_ string, _ *http.Request, _ []byte) result {
	return result{status: http.StatusOK, body: map[string]string{"status": "ok"}}
}

func (d *inventory) find(id string) *item {
	for i := range d.items {
		if d.items[i].ID == id {
			return &d.items[i]
		}
	}
	return nil
}

func (d *inventory) getItem(id string, _ *http.Request, _ []byte) result {
	found := d.find(id)
	if found == nil {
		return result{status: http.StatusNotFound, body: errorBody("item_not_found")}
	}
	return result{status: http.StatusOK, body: *found}
}

// adjustStock applies a signed change to one item's stock. Stock can never
// become negative.
func (d *inventory) adjustStock(id string, r *http.Request, body []byte) result {
	var request struct {
		Delta int `json:"delta"`
	}
	if rejected := decodeBody(r, body, &request); rejected != nil {
		return *rejected
	}

	found := d.find(id)
	if found == nil {
		return result{status: http.StatusNotFound, body: errorBody("item_not_found")}
	}
	if request.Delta == 0 || request.Delta < -maxStockAdjustment || request.Delta > maxStockAdjustment {
		return result{status: http.StatusUnprocessableEntity, body: errorBody("invalid_delta")}
	}
	if found.Stock+request.Delta < 0 {
		return result{status: http.StatusConflict, body: errorBody("insufficient_stock")}
	}

	before := found.Stock
	found.Stock += request.Delta
	effect := fmt.Sprintf("%s stock %d->%d", found.ID, before, found.Stock)
	return result{status: http.StatusOK, body: *found, effect: effect}
}
