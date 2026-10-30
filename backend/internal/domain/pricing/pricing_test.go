package pricing

import (
	"errors"
	"testing"
	"time"
)

var ist = time.FixedZone("TRT", 3*3600)

// 2026-10-26 is a Monday.
func at(day, hour int) time.Time { return time.Date(2026, 10, 26+day, hour, 0, 0, 0, ist) }

func TestTicketPrice_Rules(t *testing.T) {
	cases := []struct {
		name  string
		start time.Time
		cat   string
		taken int
		want  int64
	}{
		{"monday afternoon full", at(0, 15), CategoryFull, 0, 20000},
		{"monday matinee", at(0, 11), CategoryFull, 0, 17000},
		{"monday prime", at(0, 20), CategoryFull, 0, 22000},
		{"wednesday", at(2, 15), CategoryFull, 0, 16000},
		{"wednesday student no stacking", at(2, 15), CategoryStudent, 0, 15000},
		{"saturday prime", at(5, 20), CategoryFull, 0, 24200},
		{"saturday prime student", at(5, 20), CategoryStudent, 0, 18150},
		{"monday occupancy 50", at(0, 15), CategoryFull, 50, 21000},
		{"monday occupancy 80", at(0, 15), CategoryFull, 85, 23000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _, err := TicketPrice(ShowContext{Start: c.start, BasePrice: 20000, Capacity: 100, Taken: c.taken}, c.cat)
			if err != nil {
				t.Fatal(err)
			}
			if p != c.want {
				t.Fatalf("want %d got %d", c.want, p)
			}
		})
	}
}

func TestTicketPrice_UnknownCategory(t *testing.T) {
	if _, _, err := TicketPrice(ShowContext{Start: at(0, 15), BasePrice: 100}, "vip"); !errors.Is(err, ErrUnknownCategory) {
		t.Fatal("want ErrUnknownCategory")
	}
}

func TestSplit_SumsExactly(t *testing.T) {
	for _, g := range []int64{1, 99, 15000, 18150, 24200, 12345, 999999} {
		for _, k := range []string{KindTicket, KindCombo} {
			s := Split(g, k, DefaultRates)
			if s.Net+s.EntTax+s.VAT != g {
				t.Fatalf("%s %d: parts %+v do not sum", k, g, s)
			}
			if k == KindCombo && s.EntTax != 0 {
				t.Fatal("combo must not carry entertainment tax")
			}
		}
	}
	s := Split(11550, KindTicket, DefaultRates)
	if s.Net != 10000 || s.EntTax != 500 || s.VAT != 1050 {
		t.Fatalf("unexpected split %+v", s)
	}
}

func TestAllocate_SumsAndProportional(t *testing.T) {
	got := allocate(100, []int64{1, 1, 1})
	if got[0]+got[1]+got[2] != 100 {
		t.Fatalf("sum mismatch %v", got)
	}
	got = allocate(3000, []int64{20000, 10000, 0})
	if got[0] != 2000 || got[1] != 1000 || got[2] != 0 {
		t.Fatalf("got %v", got)
	}
}

func lines() []LineInput {
	return []LineInput{
		{Kind: KindTicket, SeatID: 1, Category: CategoryFull, Gross: 20000},
		{Kind: KindTicket, SeatID: 2, Category: CategoryStudent, Gross: 15000},
		{Kind: KindCombo, Gross: ComboPrice},
	}
}

func TestBuildQuote_NoDiscount(t *testing.T) {
	q, err := BuildQuote(lines(), nil, 0, DefaultRates)
	if err != nil {
		t.Fatal(err)
	}
	if q.Subtotal != 47000 || q.Total != 47000 || q.Payable != 47000 {
		t.Fatalf("%+v", q)
	}
	if q.Tax.Net+q.Tax.EntTax+q.Tax.VAT != q.Total {
		t.Fatal("tax does not sum to total")
	}
}

func TestBuildQuote_PercentCouponOnlyTickets(t *testing.T) {
	q, err := BuildQuote(lines(), &Coupon{Code: "X", Percent: true, Value: 1000}, 0, DefaultRates)
	if err != nil {
		t.Fatal(err)
	}
	if q.Discount != 3500 {
		t.Fatalf("10%% of 35000 tickets = 3500, got %d", q.Discount)
	}
	if q.Lines[2].Discount != 0 {
		t.Fatal("combo must not be discounted")
	}
	if q.Lines[0].Discount != 2000 || q.Lines[1].Discount != 1500 {
		t.Fatalf("proportional allocation wrong: %+v", q.Lines)
	}
	if q.Total != 43500 {
		t.Fatalf("total %d", q.Total)
	}
}

func TestBuildQuote_AmountCouponCappedAndMinTotal(t *testing.T) {
	q, _ := BuildQuote(lines(), &Coupon{Code: "BIG", Value: 999999}, 0, DefaultRates)
	if q.Discount != 35000 || q.Total != ComboPrice {
		t.Fatalf("discount must be capped at ticket total: %+v", q)
	}
	if _, err := BuildQuote(lines(), &Coupon{Code: "MIN", Value: 100, MinTotal: 100000}, 0, DefaultRates); !errors.Is(err, ErrCouponMinTotal) {
		t.Fatal("want ErrCouponMinTotal")
	}
	if _, err := BuildQuote(lines(), &Coupon{Code: "ZERO"}, 0, DefaultRates); !errors.Is(err, ErrCouponInvalid) {
		t.Fatal("want ErrCouponInvalid")
	}
}

func TestBuildQuote_PartialGiftCard(t *testing.T) {
	q, _ := BuildQuote(lines(), nil, 10000, DefaultRates)
	if q.GiftApplied != 10000 || q.Payable != 37000 {
		t.Fatalf("%+v", q)
	}
	var sum int64
	for _, l := range q.Lines {
		sum += l.GiftPaid
	}
	if sum != 10000 {
		t.Fatalf("gift allocation sums to %d", sum)
	}
	q, _ = BuildQuote(lines(), nil, 100000, DefaultRates)
	if q.GiftApplied != 47000 || q.Payable != 0 {
		t.Fatalf("gift card must cover at most the total: %+v", q)
	}
}

func TestRefundLines_SplitsGiftAndTax(t *testing.T) {
	q, _ := BuildQuote(lines(), &Coupon{Code: "X", Percent: true, Value: 1000}, 10000, DefaultRates)
	rf, err := RefundLines(q.Lines[:1], false)
	if err != nil {
		t.Fatal(err)
	}
	l := q.Lines[0]
	if rf.Amount != l.Paid || rf.ToGiftCard != l.GiftPaid || rf.ToOriginal != l.Paid-l.GiftPaid {
		t.Fatalf("%+v vs %+v", rf, l)
	}
	if rf.Tax != l.Tax || rf.RestoreCoupon {
		t.Fatalf("%+v", rf)
	}
	full, _ := RefundLines(q.Lines, true)
	if full.Amount != q.Total || full.ToGiftCard != q.GiftApplied || !full.RestoreCoupon || full.Tax != q.Tax {
		t.Fatalf("full refund mismatch %+v", full)
	}
	if _, err := RefundLines(nil, true); !errors.Is(err, ErrNothingToRefund) {
		t.Fatal("want ErrNothingToRefund")
	}
}

func TestDistributorShare(t *testing.T) {
	terms := ShareTerms{Week1BP: 5500, Week2BP: 5000, LaterBP: 4500}
	rel := at(0, 0)
	if w := WeekNo(rel, at(3, 20)); w != 1 {
		t.Fatalf("week %d", w)
	}
	if w := WeekNo(rel, rel.AddDate(0, 0, 8)); w != 2 {
		t.Fatalf("week %d", w)
	}
	if w := WeekNo(rel, rel.AddDate(0, 0, -2)); w != 1 {
		t.Fatalf("preview before release counts as week 1, got %d", w)
	}
	if s := DistributorShare(100000, 1, terms); s != 55000 {
		t.Fatal(s)
	}
	if s := DistributorShare(100000, 2, terms); s != 50000 {
		t.Fatal(s)
	}
	if s := DistributorShare(100000, 5, terms); s != 45000 {
		t.Fatal(s)
	}
}
