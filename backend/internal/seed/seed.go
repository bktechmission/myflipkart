// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package seed builds the dummy MyFlipKart.com catalog, users, orders, policy
// docs and the golden eval set. Ground truth is computed from the same data.
package seed

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"asksauby/internal/model"
)

type feature struct{ Spec, Use string }
type prod struct {
	Brand, Model string
	Price        float64
	Tags         []string
}
type category struct {
	Name     string
	Features map[string]feature
	Products []prod
}

var Catalog = []category{
	{"headphones", map[string]feature{
		"anc":         {"active noise cancellation", "Blocks out engine hum on flights and noisy commutes."},
		"longbattery": {"40-hour battery life", "Lasts a full week of listening on one charge."},
		"wireless":    {"Bluetooth 5.3 wireless", "No cables to tangle in your bag."},
		"hires":       {"hi-res studio drivers", "Accurate sound for mixing and critical listening."},
		"wired":       {"detachable 3.5mm cable", "Plug straight into an audio interface."},
		"sport":       {"IPX5 sweat resistance", "Stays put during workouts and runs."},
		"bass":        {"extra bass boost", "Deep low end for hip hop and EDM."},
	}, []prod{
		{"Sonix", "WH-900", 349, []string{"anc", "longbattery", "wireless", "hires"}},
		{"Sonix", "Air Lite", 39, []string{"wireless"}},
		{"AuraBeat", "Pulse 2", 89, []string{"wireless", "sport"}},
		{"AuraBeat", "Studio X", 199, []string{"wired", "hires"}},
		{"Quietude", "Zen 7", 279, []string{"anc", "wireless", "longbattery"}},
		{"Quietude", "Bass Pro", 59, []string{"wireless", "bass"}},
	}},
	{"laptops", map[string]feature{
		"gaming":      {"RTX 4070 graphics and 165Hz display", "Runs the latest AAA titles at high settings."},
		"ultralight":  {"2.2 lb aluminum chassis", "Easy to carry between classes and meetings."},
		"longbattery": {"18-hour battery", "Gets through a full day unplugged."},
		"dev":         {"32GB RAM and 1TB SSD", "Handles Docker, IDEs and data work with ease."},
		"student":     {"budget-friendly 8GB RAM", "Great for homework, browsing and video calls."},
		"oled":        {"3K OLED display", "Vivid colors for photo and video editing."},
	}, []prod{
		{"Vertex", "Book 14", 649, []string{"student", "longbattery"}},
		{"Vertex", "Blade 16", 1899, []string{"gaming", "dev"}},
		{"Nimbus", "Air 13", 999, []string{"ultralight", "longbattery"}},
		{"Nimbus", "Pro 15", 1499, []string{"dev", "oled"}},
		{"Krono", "G7", 1299, []string{"gaming"}},
		{"Krono", "Lite 11", 399, []string{"student", "ultralight"}},
	}},
	{"smartphones", map[string]feature{
		"camera":  {"50MP triple camera with night mode", "Sharp, bright photos even in low light."},
		"battery": {"5000mAh battery", "Two days of use between charges."},
		"compact": {"5.8-inch compact body", "Fits comfortably in one hand and small pockets."},
		"5g":      {"5G connectivity", "Fast downloads and streaming on the go."},
		"rugged":  {"MIL-STD drop-proof rugged case", "Survives drops on job sites and outdoor trips."},
	}, []prod{
		{"Pixelon", "8 Pro", 899, []string{"camera", "5g"}},
		{"Pixelon", "8a", 449, []string{"5g", "battery"}},
		{"Orbi", "Mini", 599, []string{"compact", "5g"}},
		{"Orbi", "Max", 1099, []string{"camera", "battery", "5g"}},
		{"Terra", "X Rugged", 379, []string{"rugged", "battery"}},
		{"Terra", "Lite", 199, []string{"compact"}},
	}},
	{"smartwatches", map[string]feature{
		"gps":         {"built-in multi-band GPS", "Maps your runs and rides without your phone."},
		"heartrate":   {"continuous heart-rate sensor", "Tracks training zones and resting heart rate."},
		"sleep":       {"sleep stage tracking", "Scores your rest and suggests a bedtime."},
		"waterproof":  {"5ATM water resistance", "Counts laps and strokes in the pool."},
		"longbattery": {"14-day battery", "Goes two weeks without charging."},
	}, []prod{
		{"Pulsar", "Run 3", 249, []string{"gps", "heartrate", "waterproof"}},
		{"Pulsar", "Fit", 79, []string{"heartrate", "sleep"}},
		{"Chrona", "S2", 399, []string{"gps", "heartrate", "sleep", "waterproof"}},
		{"Chrona", "Classic", 179, []string{"sleep", "longbattery"}},
		{"Tidewear", "Swim", 149, []string{"waterproof", "heartrate"}},
		{"Tidewear", "Trail X", 329, []string{"gps", "longbattery"}},
	}},
	{"running shoes", map[string]feature{
		"cushion":    {"max-cushion foam midsole", "Soft landings for easy miles and recovery days."},
		"trail":      {"lugged trail outsole", "Grips rocks, roots and muddy mountain paths."},
		"racing":     {"carbon fiber plate", "Built for race day and personal records."},
		"wide":       {"wide toe box", "Roomy fit for wide feet."},
		"waterproof": {"Gore-Tex waterproof upper", "Keeps feet dry through puddles and rain."},
	}, []prod{
		{"Stride", "Cloud 5", 139, []string{"cushion"}},
		{"Stride", "Rocket", 249, []string{"racing"}},
		{"Peakfoot", "Ridge", 159, []string{"trail", "waterproof"}},
		{"Peakfoot", "Wide Comfort", 119, []string{"cushion", "wide"}},
		{"Velo", "Tempo", 179, []string{"racing", "cushion"}},
		{"Velo", "Mud GTX", 189, []string{"trail", "waterproof"}},
	}},
	{"coffee makers", map[string]feature{
		"espresso":     {"15-bar espresso pump", "Pulls cafe-style shots and lattes at home."},
		"drip":         {"12-cup drip carafe", "Brews a full pot for the whole household."},
		"grinder":      {"built-in burr grinder", "Grinds fresh beans right before brewing."},
		"programmable": {"24-hour programmable timer", "Coffee is ready the moment you wake up."},
		"portable":     {"compact portable design", "Brew at the campsite or in a hotel room."},
	}, []prod{
		{"Brewmaster", "Barista One", 549, []string{"espresso", "grinder"}},
		{"Brewmaster", "Drip 12", 79, []string{"drip", "programmable"}},
		{"Kafe", "Pocket", 49, []string{"portable", "espresso"}},
		{"Kafe", "Smart Drip", 129, []string{"drip", "programmable", "grinder"}},
		{"Moka", "Classic", 35, []string{"portable"}},
		{"Moka", "Duo", 299, []string{"espresso", "drip"}},
	}},
	{"backpacks", map[string]feature{
		"laptop":     {"padded 16-inch laptop sleeve", "Protects your computer on the daily commute."},
		"hiking":     {"hydration bladder port", "Sip water on the trail without stopping."},
		"waterproof": {"roll-top waterproof shell", "Gear stays dry in downpours and river crossings."},
		"travel":     {"carry-on size with clamshell opening", "Fits airline overhead bins for one-bag travel."},
		"antitheft":  {"hidden zippers and RFID pocket", "Keeps wallets and passports safe in crowds."},
		"kids":       {"small 12L kid-size fit", "Sized for school children."},
	}, []prod{
		{"Trekka", "Daypack 20", 69, []string{"laptop", "hiking"}},
		{"Trekka", "Summit 45", 189, []string{"hiking", "waterproof"}},
		{"Urbano", "Commuter", 99, []string{"laptop", "antitheft"}},
		{"Urbano", "Carry-On 35", 149, []string{"travel", "laptop"}},
		{"Nomad", "Dry Bag 30", 119, []string{"waterproof", "travel"}},
		{"Nomad", "Kids Mini", 29, []string{"kids"}},
	}},
	{"monitors", map[string]feature{
		"4k":        {"4K UHD resolution", "Crisp text and detail for creative work."},
		"gaming":    {"165Hz refresh with 1ms response", "Smooth, tear-free motion for competitive play."},
		"ultrawide": {"34-inch ultrawide curved panel", "Room for several windows side by side."},
		"usbc":      {"USB-C with 90W charging", "One cable powers and connects your laptop."},
		"color":     {"99% Adobe RGB factory calibrated", "Accurate color for print and photo pros."},
		"office":    {"anti-glare 1080p panel", "Simple, affordable screen for office tasks."},
	}, []prod{
		{"Lumen", "U27 4K", 429, []string{"4k", "usbc"}},
		{"Lumen", "G24", 179, []string{"gaming"}},
		{"Vista", "Ultra 34", 599, []string{"ultrawide", "usbc"}},
		{"Vista", "Pro 32", 899, []string{"4k", "color"}},
		{"Optix", "Swift 27", 349, []string{"gaming", "4k"}},
		{"Optix", "Basic 22", 99, []string{"office"}},
	}},
}

var CategorySynonyms = map[string][]string{
	"headphones":    {"headphone", "headphones", "earbuds", "headset"},
	"laptops":       {"laptop", "laptops", "notebook"},
	"smartphones":   {"phone", "phones", "smartphone", "smartphones", "mobile"},
	"smartwatches":  {"watch", "watches", "smartwatch", "smartwatches", "tracker"},
	"running shoes": {"shoe", "shoes", "sneakers", "running shoes", "trainers"},
	"coffee makers": {"coffee", "espresso", "coffee maker", "coffee makers", "brewer"},
	"backpacks":     {"backpack", "backpacks", "bag", "pack", "daypack"},
	"monitors":      {"monitor", "monitors", "screen", "display"},
}

type PolicyDoc = model.PolicyDoc

var Policies = []PolicyDoc{
	{ID: "returns", Title: "Return Policy", Version: 1, Paras: []string{
		"You can return most items within 30 days of delivery for a full refund. Items must be in original condition with all accessories.",
		"Opened headphones and earbuds can be returned only if defective, for hygiene reasons.",
		"To start a return, ask Sauby to return your order and you will get a return authorization (RMA) number and a prepaid label.",
	}},
	{ID: "cancellation", Title: "Order Cancellation", Version: 1, Paras: []string{
		"Orders can be cancelled any time before they ship, while the status is 'placed'. Cancelled orders are refunded automatically.",
		"Once an order has shipped it can no longer be cancelled. Wait for delivery and start a return instead.",
	}},
	{ID: "shipping", Title: "Shipping", Version: 1, Paras: []string{
		"Standard shipping is free on orders over $50 and arrives in 3-5 business days.",
		"Express shipping costs $15 and arrives in 1-2 business days. MyFlipKart.com ships to all 50 US states.",
	}},
	{ID: "refunds", Title: "Refunds", Version: 1, Paras: []string{
		"Refunds are issued to the original payment method within 5-7 business days after a return is approved.",
		"Shipping fees are not refunded unless the item arrived damaged or defective.",
	}},
	{ID: "warranty", Title: "Warranty", Version: 1, Paras: []string{
		"All electronics include a 1-year limited manufacturer warranty covering defects in materials and workmanship.",
		"Accidental damage such as drops or water damage is not covered by the warranty.",
	}},
}

func Products() []model.Product {
	rng := rand.New(rand.NewSource(7))
	var out []model.Product
	pid := 1001
	for _, c := range Catalog {
		for _, p := range c.Products {
			var feats, uses []string
			for _, t := range p.Tags {
				feats = append(feats, c.Features[t].Spec)
				uses = append(uses, c.Features[t].Use)
			}
			name := p.Brand + " " + p.Model
			out = append(out, model.Product{
				ID: fmt.Sprintf("P%d", pid), Name: name, Brand: p.Brand, Category: c.Name,
				Price: p.Price, Rating: math.Round((3.6+rng.Float64()*1.3)*10) / 10, Stock: rng.Intn(41),
				Tags: p.Tags, Features: feats,
				Description: fmt.Sprintf("The %s is a %s from %s. %s", name,
					strings.TrimSuffix(c.Name, "s"), p.Brand, strings.Join(uses, " ")),
			})
			pid++
		}
	}
	return out
}

func PageText(p model.Product) string {
	return fmt.Sprintf("%s | %s | %s\n%s\nFeatures: %s.\nPrice: $%.0f",
		p.Name, p.Category, p.Brand, p.Description, strings.Join(p.Features, "; "), p.Price)
}

// Demo identities. These are the accounts the mock IdP offers.
type DemoUser struct{ CustomerID, Email, Name string }

var DemoUsers = []DemoUser{
	{"C1001", "aarav@example.com", "Aarav Mehta"}, // admin in the demo (ADMIN_EMAILS)
	{"C1002", "meera@example.com", "Meera Iyer"},
	{"C1003", "sam@example.com", "Sam Lee"}, // no orders: tests empty states
}

type orderSpec struct {
	ID, Status string
	DaysAgo    int
	Items      []string
}

var orderSpecs = map[string][]orderSpec{
	"C1001": {
		{"O5001", "delivered", 12, []string{"P1001"}},
		{"O5002", "placed", 0, []string{"P1009", "P1035"}},
		{"O5003", "shipped", 2, []string{"P1020"}},
		{"O5004", "placed", 1, []string{"P1031"}},
		{"O5005", "delivered", 5, []string{"P1025", "P1043"}},
		{"O5006", "delivered", 60, []string{"P1014"}},
		{"O5007", "cancelled", 20, []string{"P1002"}},
		{"O5008", "delivered", 25, []string{"P1027"}},
		{"O5009", "delivered", 6, []string{"P1042"}}, // $29: under the auto-refund limit
	},
	"C1002": {
		{"O5101", "delivered", 3, []string{"P1019"}},
		{"O5102", "shipped", 1, []string{"P1040"}},
		{"O5103", "placed", 0, []string{"P1033"}},
		{"O5104", "delivered", 45, []string{"P1011"}},
		{"O5105", "return_requested", 10, []string{"P1036"}},
		{"O5106", "delivered", 8, []string{"P1005"}},
	},
}

// Orders builds the demo orders relative to today. extraEmailCID, if set, gets
// a copy of C1001's orders (IDs O6xxx) so a real Google account can try refunds.
func Orders(products []model.Product, today time.Time, extraCID string) []model.Order {
	price := map[string]float64{}
	for _, p := range products {
		price[p.ID] = p.Price
	}
	mk := func(id, cid string, s orderSpec) model.Order {
		delivered := today.AddDate(0, 0, -s.DaysAgo)
		o := model.Order{ID: id, CustomerID: cid, Status: s.Status, Items: s.Items,
			PlacedOn: delivered.AddDate(0, 0, -3).Format("2006-01-02"), UpdatedAt: today}
		if s.Status == "delivered" || s.Status == "return_requested" {
			o.DeliveredOn = delivered.Format("2006-01-02")
		}
		if s.Status == "return_requested" {
			o.RMA = "RMA-" + id[1:]
		}
		for _, i := range s.Items {
			o.Total += price[i]
		}
		return o
	}
	var out []model.Order
	for _, cid := range []string{"C1001", "C1002"} {
		for _, s := range orderSpecs[cid] {
			out = append(out, mk(s.ID, cid, s))
		}
	}
	if extraCID != "" {
		for _, s := range orderSpecs["C1001"] {
			out = append(out, mk("O6"+s.ID[2:], extraCID, s))
		}
	}
	return out
}
