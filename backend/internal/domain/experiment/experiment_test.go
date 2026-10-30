package experiment

import (
	"math"
	"testing"
)

func TestAssign_DeterministicAndBalanced(t *testing.T) {
	counts := [2]int{}
	for id := int64(1); id <= 10000; id++ {
		v := Assign("kasim", id, 2)
		if v != Assign("kasim", id, 2) {
			t.Fatal("not deterministic")
		}
		counts[v]++
	}
	if math.Abs(float64(counts[0]-counts[1])) > 400 { // within 4% of 50/50
		t.Fatalf("unbalanced split %v", counts)
	}
	if Assign("x", 5, 1) != 0 || Assign("x", 5, 0) != 0 {
		t.Fatal("single-variant experiments always return 0")
	}
}

func TestAssign_IndependentAcrossExperiments(t *testing.T) {
	same := 0
	for id := int64(1); id <= 2000; id++ {
		if Assign("a", id, 2) == Assign("b", id, 2) {
			same++
		}
	}
	if same < 850 || same > 1150 { // ~50% if independent
		t.Fatalf("assignments look correlated: %d/2000 equal", same)
	}
}

func TestCompare_SignificantDifference(t *testing.T) {
	a := Arm{Variant: Variant{Name: "A"}, Carts: 1000, PaidCarts: 100}
	b := Arm{Variant: Variant{Name: "B"}, Carts: 1000, PaidCarts: 150}
	c := Compare(a, b)
	// pooled p = 0.125, se = sqrt(.125*.875*.002) = 0.014790, z = 0.05/0.014790 = 3.3806
	if math.Abs(c.ZScore-3.3806) > 0.001 {
		t.Fatalf("z = %v", c.ZScore)
	}
	if c.PValue > 0.001 || !c.Significant || c.Winner != "B" {
		t.Fatalf("%+v", c)
	}
	if c.AbsoluteLift != 0.05 || c.RelativeLift != 0.5 {
		t.Fatalf("lift %+v", c)
	}
}

func TestCompare_NotSignificantOrTooSmall(t *testing.T) {
	c := Compare(Arm{Carts: 1000, PaidCarts: 100}, Arm{Carts: 1000, PaidCarts: 104})
	if c.Significant || c.PValue < 0.5 {
		t.Fatalf("tiny difference reported as significant: %+v", c)
	}
	c = Compare(Arm{Carts: 6, PaidCarts: 1}, Arm{Carts: 6, PaidCarts: 5})
	if c.MinSample || c.Significant {
		t.Fatalf("small sample must not be significant: %+v", c)
	}
	c = Compare(Arm{}, Arm{Carts: 10, PaidCarts: 3})
	if c.ZScore != 0 || c.Significant {
		t.Fatalf("empty arm: %+v", c)
	}
}

func TestArm_Finish(t *testing.T) {
	a := Arm{Exposed: 40, Converted: 10, Carts: 50, PaidCarts: 12, Orders: 12, ComboOrders: 3, Revenue: 600000, Discount: 24000}
	a.Finish()
	if a.ConversionRate != 0.24 || a.UserConversion != 0.25 || a.RevenuePerUser != 15000 || a.AvgOrderValue != 50000 || a.ComboAttachRate != 0.25 || a.DiscountPerOrder != 2000 {
		t.Fatalf("%+v", a)
	}
	var z Arm
	z.Finish()
	if z.ConversionRate != 0 || z.AvgOrderValue != 0 {
		t.Fatal("zero arm must stay zero")
	}
}
