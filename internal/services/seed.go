package services

type User struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Roles    []string `json:"roles"`
	Merchant *string  `json:"merchant"`
	Home     string   `json:"home"`
}

func Users() []User {
	merchant := func(id string) *string { return &id }
	return []User{
		{"cust-001", "Customer 1", "customer1@fetch.test", []string{"CUSTOMER"}, nil, "/"},
		{"cust-002", "Customer 2", "customer2@fetch.test", []string{"CUSTOMER"}, nil, "/"},
		{"cust-003", "Customer 3", "customer3@fetch.test", []string{"CUSTOMER"}, nil, "/"},
		{"merchant-001", "Merchant 1", "merchant1@fetch.test", []string{"MERCHANT"}, merchant("MER-001"), "/merchant"},
		{"merchant-002", "Merchant 2", "merchant2@fetch.test", []string{"MERCHANT"}, merchant("MER-002"), "/merchant"},
		{"merchant-003", "Merchant 3", "merchant3@fetch.test", []string{"MERCHANT"}, merchant("MER-003"), "/merchant"},
		{"merchant-004", "Merchant 4", "merchant4@fetch.test", []string{"MERCHANT"}, merchant("MER-004"), "/merchant"},
		{"merchant-005", "Merchant 5", "merchant5@fetch.test", []string{"MERCHANT"}, merchant("MER-005"), "/merchant"},
		{"merchant-006", "Merchant 6", "merchant6@fetch.test", []string{"MERCHANT"}, merchant("MER-006"), "/merchant"},
		{"rider-01", "Rider 1", "rider1@fetch.test", []string{"RIDER"}, nil, "/rider"},
		{"rider-02", "Rider 2", "rider2@fetch.test", []string{"RIDER"}, nil, "/rider"},
		{"support-01", "Support 1", "support1@fetch.test", []string{"SUPPORT"}, nil, "/support"},
		{"ops-01", "Admin 1", "admin1@fetch.test", []string{"OPERATIONS"}, nil, "/ops"},
		{"lead-01", "Admin 2", "admin2@fetch.test", []string{"SUPPORT", "OPERATIONS"}, nil, "/ops"},
	}
}

func userByID(id string) (User, bool) {
	for _, user := range Users() {
		if user.ID == id {
			return user, true
		}
	}
	return User{}, false
}

type Category struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Section string `json:"section"`
}
type DeliveryMinutes struct {
	Min int `json:"min"`
	Max int `json:"max"`
}
type Merchant struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Section         string          `json:"section"`
	Category        string          `json:"category"`
	Area            string          `json:"area"`
	DeliveryMinutes DeliveryMinutes `json:"delivery_minutes"`
	DeliveryFee     int64           `json:"delivery_fee_sen"`
	Open            bool            `json:"open"`
}
type Item struct {
	ID          string `json:"id"`
	Merchant    string `json:"merchant"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Group       string `json:"group"`
	Price       int64  `json:"price_sen"`
	Available   bool   `json:"available"`
	StockState  string `json:"stock_state"`
}
type stockItem struct {
	Item     Item `json:"item"`
	Stock    *int `json:"stock"`
	Reserved int  `json:"reserved"`
}

func seedCategories() []Category {
	return []Category{{"nasi-lemak", "Nasi lemak", "food"}, {"mamak", "Mamak", "food"}, {"kopitiam", "Kopitiam", "food"}, {"hawker", "Hawker favourites", "food"}, {"groceries", "Groceries", "mart"}, {"essentials", "Health and household", "mart"}}
}
func seedMerchants() []Merchant {
	return []Merchant{
		{"MER-001", "Warung Nasi Lemak", "food", "nasi-lemak", "Bangsar, Kuala Lumpur", DeliveryMinutes{20, 35}, 399, true},
		{"MER-002", "Restoran Bintang Mamak", "food", "mamak", "Petaling Jaya, Selangor", DeliveryMinutes{15, 30}, 299, true},
		{"MER-003", "Kopitiam Lama", "food", "kopitiam", "Cheras, Kuala Lumpur", DeliveryMinutes{20, 35}, 399, true},
		{"MER-004", "Dapur Penang", "food", "hawker", "Subang Jaya, Selangor", DeliveryMinutes{25, 40}, 499, true},
		{"MER-005", "Fetch Mart Express", "mart", "groceries", "Mont Kiara, Kuala Lumpur", DeliveryMinutes{25, 40}, 499, true},
		{"MER-006", "Sihat Essentials", "mart", "essentials", "Kuala Lumpur", DeliveryMinutes{30, 45}, 499, true},
	}
}

func seedItems() []stockItem {
	rows := []struct {
		id, merchant, name, description, group string
		price                                  int64
		stock                                  int
	}{
		{"ITM-001", "MER-001", "Nasi Lemak Ayam Goreng", "Coconut rice, sambal, fried chicken, egg and anchovies.", "Mains", 1290, -1},
		{"ITM-002", "MER-001", "Nasi Lemak Rendang Daging", "Coconut rice with slow-cooked beef rendang, sambal and egg.", "Mains", 1590, -1},
		{"ITM-003", "MER-001", "Nasi Lemak Telur Mata", "Coconut rice, sambal and a freshly fried egg.", "Mains", 750, -1},
		{"ITM-004", "MER-001", "Teh Tarik", "Freshly pulled milk tea.", "Drinks", 350, -1},
		{"ITM-011", "MER-002", "Roti Canai", "Flaky flatbread served with dhal and curry.", "Mains", 180, -1},
		{"ITM-012", "MER-002", "Mee Goreng Mamak", "Stir-fried yellow noodles with vegetables and egg.", "Mains", 990, -1},
		{"ITM-013", "MER-002", "Maggi Goreng", "Wok-fried instant noodles with vegetables and egg.", "Mains", 850, -1},
		{"ITM-014", "MER-002", "Milo Ais", "Iced chocolate malt drink.", "Drinks", 450, -1},
		{"ITM-021", "MER-003", "Kaya Butter Toast", "Toasted bread with coconut jam and butter.", "Breakfast", 450, -1},
		{"ITM-022", "MER-003", "Half-boiled Eggs", "Two soft eggs served with pepper and soy sauce.", "Breakfast", 400, -1},
		{"ITM-023", "MER-003", "Kopi O Peng", "Traditional black coffee served over ice.", "Drinks", 380, -1},
		{"ITM-024", "MER-003", "Chicken Chop", "Grilled chicken chop with vegetables and sauce.", "Mains", 1890, -1},
		{"ITM-031", "MER-004", "Char Kway Teow", "Wok-fried flat rice noodles with egg and bean sprouts.", "Mains", 1250, -1},
		{"ITM-032", "MER-004", "Assam Laksa", "Rice noodles in a tangy fish broth.", "Mains", 1190, -1},
		{"ITM-033", "MER-004", "Cendol", "Shaved ice, coconut milk and palm sugar with pandan noodles.", "Desserts", 650, -1},
		{"ITM-501", "MER-005", "Rice 5 kg", "A five-kilogram bag of everyday white rice.", "Staples", 2690, 40},
		{"ITM-502", "MER-005", "Cooking Oil 1 kg", "One kilogram of cooking oil for everyday meals.", "Staples", 690, 12},
		{"ITM-503", "MER-005", "Fresh Eggs (30)", "A tray of thirty fresh eggs.", "Dairy and eggs", 1590, 0},
		{"ITM-504", "MER-005", "Full Cream Milk 1 L", "One litre of full cream milk.", "Dairy and eggs", 790, 60},
		{"ITM-505", "MER-005", "Wholemeal Bread", "A loaf of sliced wholemeal bread.", "Bakery", 450, 25},
		{"ITM-506", "MER-005", "Instant Noodles (5 pack)", "Five individually wrapped packs of instant noodles.", "Staples", 590, 80},
		{"ITM-507", "MER-005", "Mineral Water 1.5 L (6 pack)", "Six bottles of mineral water, one and a half litres each.", "Beverages", 1090, 50},
		{"ITM-508", "MER-005", "Bananas 1 kg", "One kilogram of fresh bananas.", "Fresh produce", 690, 30},
		{"ITM-601", "MER-006", "Hand Sanitiser 500 ml", "A five-hundred-millilitre bottle of hand sanitiser.", "Health", 1290, 20},
		{"ITM-602", "MER-006", "Face Masks (50)", "A box containing fifty disposable face masks.", "Health", 1590, 35},
		{"ITM-603", "MER-006", "Vitamin C 1000 mg (30)", "Thirty vitamin C tablets of one thousand milligrams.", "Health", 2490, 12},
		{"ITM-604", "MER-006", "Tissue Box (5 pack)", "Five boxes of household tissues.", "Household", 1290, 45},
	}
	items := make([]stockItem, 0, len(rows))
	for _, row := range rows {
		var stock *int
		if row.stock >= 0 {
			value := row.stock
			stock = &value
		}
		items = append(items, stockItem{Item: Item{row.id, row.merchant, row.name, row.description, row.group, row.price, row.id != "ITM-033", ""}, Stock: stock})
	}
	return items
}
