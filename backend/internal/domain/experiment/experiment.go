// Package experiment assigns customers to campaign variants and compares
// the variants with a two-proportion z-test (campaign A/B report).
package experiment

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strconv"
)

// Variant is one arm of an experiment: the coupon offered to its users.
type Variant struct {
	Name        string `json:"name"`
	CouponCode  string `json:"coupon_code"`
	Description string `json:"description"`
}

// Experiment is a running campaign test.
type Experiment struct {
	Key      string    `json:"key"`
	Title    string    `json:"title"`
	Variants []Variant `json:"variants"`
}

// Assign returns the variant index of a user. It is deterministic (same
// user, same experiment => same variant, on every replica) and independent
// across experiments because the key is part of the hash.
func Assign(key string, userID int64, n int) int {
	if n <= 1 {
		return 0
	}
	// SHA-256, not FNV: FNV's low bits barely depend on the key, which made
	// different experiments assign every user to the same arm.
	sum := sha256.Sum256([]byte(key + ":" + strconv.FormatInt(userID, 10)))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(n))
}

// Arm aggregates the outcome of one variant.
type Arm struct {
	Variant     Variant `json:"variant"`
	Exposed     int     `json:"exposed"`    // users who opened a cart in the period
	Converted   int     `json:"converted"`  // exposed users with at least one paid order
	Carts       int     `json:"carts"`      // carts (holds) opened: the trials of the test
	PaidCarts   int     `json:"paid_carts"` // carts that became an order: the successes
	Orders      int     `json:"orders"`
	CouponUses  int     `json:"coupon_uses"` // orders that used this arm's coupon
	ComboOrders int     `json:"combo_orders"`
	Tickets     int64   `json:"tickets"`
	Revenue     int64   `json:"revenue"`  // kuruş paid after discount
	Discount    int64   `json:"discount"` // kuruş given away

	ConversionRate   float64 `json:"conversion_rate"` // paid carts / carts
	UserConversion   float64 `json:"user_conversion_rate"`
	RevenuePerUser   int64   `json:"revenue_per_exposed_user"`
	AvgOrderValue    int64   `json:"avg_order_value"`
	ComboAttachRate  float64 `json:"combo_attach_rate"`
	DiscountPerOrder int64   `json:"discount_per_order"`
}

// Finish computes the derived rates.
func (a *Arm) Finish() {
	if a.Carts > 0 {
		a.ConversionRate = round4(float64(a.PaidCarts) / float64(a.Carts))
	}
	if a.Exposed > 0 {
		a.UserConversion = round4(float64(a.Converted) / float64(a.Exposed))
		a.RevenuePerUser = a.Revenue / int64(a.Exposed)
	}
	if a.Orders > 0 {
		a.AvgOrderValue = a.Revenue / int64(a.Orders)
		a.ComboAttachRate = round4(float64(a.ComboOrders) / float64(a.Orders))
		a.DiscountPerOrder = a.Discount / int64(a.Orders)
	}
}

// Comparison is B against A.
type Comparison struct {
	AbsoluteLift float64 `json:"absolute_lift"` // conversion B - A
	RelativeLift float64 `json:"relative_lift"` // (B - A) / A
	ZScore       float64 `json:"z_score"`
	PValue       float64 `json:"p_value"` // two-sided
	Significant  bool    `json:"significant"`
	MinSample    bool    `json:"enough_data"` // every cell has >= 5 successes and failures
	Winner       string  `json:"winner"`      // variant name, or "" when not significant
}

// Alpha is the significance level of the report.
const Alpha = 0.05

// Compare runs a two-sided two-proportion z-test on cart conversion (paid
// carts / carts). The cart, not the user, is the unit: a returning customer
// opens many carts, and "bought at least once" saturates near 100%.
func Compare(a, b Arm) Comparison {
	var c Comparison
	if a.Carts == 0 || b.Carts == 0 {
		return c
	}
	pa := float64(a.PaidCarts) / float64(a.Carts)
	pb := float64(b.PaidCarts) / float64(b.Carts)
	c.AbsoluteLift = round4(pb - pa)
	if pa > 0 {
		c.RelativeLift = round4((pb - pa) / pa)
	}
	pool := float64(a.PaidCarts+b.PaidCarts) / float64(a.Carts+b.Carts)
	se := math.Sqrt(pool * (1 - pool) * (1/float64(a.Carts) + 1/float64(b.Carts)))
	c.PValue = 1
	if se > 0 {
		c.ZScore = round4((pb - pa) / se)
		c.PValue = round4(math.Erfc(math.Abs(c.ZScore) / math.Sqrt2))
	}
	c.MinSample = a.PaidCarts >= 5 && b.PaidCarts >= 5 && a.Carts-a.PaidCarts >= 5 && b.Carts-b.PaidCarts >= 5
	c.Significant = c.MinSample && c.PValue < Alpha
	if c.Significant {
		if pb > pa {
			c.Winner = b.Variant.Name
		} else {
			c.Winner = a.Variant.Name
		}
	}
	return c
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }
