package services

import (
	"fmt"
	"time"
)

func seedAddress() Address {
	return Address{"12 Jalan Telawi 3", "", "59100", "Kuala Lumpur", "Wilayah Persekutuan Kuala Lumpur"}
}
func seedOrder(id, customer string, merchant Merchant, item Item, status string, created time.Time, quantity int) Order {
	timeline := []TimelineEntry{}
	for i, current := range orderStatuses[:5] {
		timeline = append(timeline, TimelineEntry{current, created.Add(time.Duration(i) * 5 * time.Minute)})
		if current == status {
			break
		}
	}
	var rider *string
	if status == "delivered" {
		value := "rider-01"
		rider = &value
	}
	subtotal := item.Price * int64(quantity)
	return Order{ID: id, Customer: customer, Merchant: merchant.ID, MerchantName: merchant.Name, Items: []PricedLine{{item.ID, item.Name, quantity, item.Price}}, Subtotal: subtotal, DeliveryFee: merchant.DeliveryFee, Total: subtotal + merchant.DeliveryFee, Payment: "cash", Address: seedAddress(), Note: "", Status: status, Rider: rider, Timeline: timeline, CreatedAt: created}
}
func seedOrders() []Order {
	merchants := seedMerchants()
	items := seedItems()
	first := map[string]Item{}
	for _, stock := range items {
		if first[stock.Item.Merchant].ID == "" {
			first[stock.Item.Merchant] = stock.Item
		}
	}
	orders := []Order{}
	number := 1
	dayIndex := 0
	for day := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC); !day.After(time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)); day = day.AddDate(0, 0, 1) {
		for i, merchant := range merchants {
			customer := (dayIndex+i)%3 + 1
			if merchant.Section == "mart" && customer == 3 {
				customer = 1
			}
			orders = append(orders, seedOrder(fmt.Sprintf("ORD-%04d", number), fmt.Sprintf("cust-%03d", customer), merchant, first[merchant.ID], "delivered", day.Add(time.Duration(i)*time.Hour), 1+(dayIndex+i)%3))
			number++
		}
		dayIndex++
	}
	for i, row := range []struct {
		customer string
		merchant int
		status   string
	}{{"cust-001", 0, "placed"}, {"cust-002", 4, "delivered"}, {"cust-001", 1, "accepted"}, {"cust-003", 2, "ready"}} {
		merchant := merchants[row.merchant]
		orders = append(orders, seedOrder(fmt.Sprintf("ORD-%d", 1001+i), row.customer, merchant, first[merchant.ID], row.status, time.Date(2026, 10, 26, 1+i, 0, 0, 0, time.UTC), 1))
	}
	return orders
}
