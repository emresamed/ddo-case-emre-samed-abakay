// Package pricing computes ticket prices, discounts, gift card usage, tax
// breakdowns, refunds and distributor revenue share (task T-4).
// All amounts are integer kuruş; rates are basis points (1 bp = 0.01%).
package pricing

import (
	"errors"
	"time"
)

// Ticket categories.
const (
	CategoryFull    = "full"
	CategoryStudent = "student"
)

// Line kinds.
const (
	KindTicket = "ticket"
	KindCombo  = "combo"
)

// Rates holds tax rates. Ticket prices and combo prices are tax inclusive.
// VAT is levied on (net + entertainment tax); entertainment tax only applies
// to tickets.
type Rates struct {
	TicketVATBP int64
	ComboVATBP  int64
	EntTaxBP    int64
}

// DefaultRates are used by the API; they are configuration, not law.
var DefaultRates = Rates{TicketVATBP: 1000, ComboVATBP: 1000, EntTaxBP: 500}

// ComboPrice is the list price of one popcorn + drink menu sold with a ticket.
const ComboPrice int64 = 12000

// Adjustment is one rule that moved the price, kept for transparency.
type Adjustment struct {
	Code string `json:"code"`
	BP   int64  `json:"bp"`
}

// ShowContext is what the dynamic price depends on. Start must already be in
// the cinema's local time zone.
type ShowContext struct {
	Start     time.Time
	BasePrice int64
	Capacity  int
	Taken     int // sold + actively held seats
}

var (
	ErrUnknownCategory = errors.New("unknown ticket category")
	ErrCouponMinTotal  = errors.New("order total is below the coupon minimum")
	ErrCouponInvalid   = errors.New("coupon is not valid")
	ErrNothingToRefund = errors.New("nothing to refund")
)

// mulBP multiplies v by (10000+bp)/10000 rounding half up.
func mulBP(v, bp int64) int64 {
	return divRound(v*(10000+bp), 10000)
}

// divRound divides rounding half away from zero; b must be > 0.
func divRound(a, b int64) int64 {
	if a >= 0 {
		return (a + b/2) / b
	}
	return -((-a + b/2) / b)
}

// roundTo50 rounds to the nearest 50 kuruş, half up.
func roundTo50(v int64) int64 { return divRound(v, 50) * 50 }

// TicketPrice applies, in order: day of week, time of day, occupancy and
// category rules. Wednesday is "halk günü" (-20%). The student discount
// (-25%) does not stack with the Wednesday discount: the larger one wins.
func TicketPrice(sc ShowContext, category string) (int64, []Adjustment, error) {
	if category != CategoryFull && category != CategoryStudent {
		return 0, nil, ErrUnknownCategory
	}
	var adj []Adjustment
	wd := sc.Start.Weekday()
	wednesday := wd == time.Wednesday
	switch {
	case wednesday && category == CategoryStudent:
		adj = append(adj, Adjustment{"student", -2500})
	case wednesday:
		adj = append(adj, Adjustment{"wednesday", -2000})
	case wd == time.Friday || wd == time.Saturday || wd == time.Sunday:
		adj = append(adj, Adjustment{"weekend", 1000})
	}

	h := sc.Start.Hour()
	switch {
	case h < 13:
		adj = append(adj, Adjustment{"matinee", -1500})
	case h >= 18 && h < 23:
		adj = append(adj, Adjustment{"prime_time", 1000})
	}

	if sc.Capacity > 0 {
		occ := sc.Taken * 100 / sc.Capacity
		switch {
		case occ >= 80:
			adj = append(adj, Adjustment{"occupancy_80", 1500})
		case occ >= 50:
			adj = append(adj, Adjustment{"occupancy_50", 500})
		}
	}

	if category == CategoryStudent && !wednesday {
		adj = append(adj, Adjustment{"student", -2500})
	}

	p := sc.BasePrice
	for _, a := range adj {
		p = mulBP(p, a.BP)
	}
	return roundTo50(p), adj, nil
}

// Tax is the split of a tax-inclusive amount.
type Tax struct {
	Net    int64 `json:"net"`
	EntTax int64 `json:"ent_tax"`
	VAT    int64 `json:"vat"`
}

// Add returns t + o.
func (t Tax) Add(o Tax) Tax {
	return Tax{Net: t.Net + o.Net, EntTax: t.EntTax + o.EntTax, VAT: t.VAT + o.VAT}
}

// Split breaks a gross amount into net, entertainment tax and VAT so that the
// three parts always sum exactly to gross.
func Split(gross int64, kind string, r Rates) Tax {
	vatBP, entBP := r.ComboVATBP, int64(0)
	if kind == KindTicket {
		vatBP, entBP = r.TicketVATBP, r.EntTaxBP
	}
	preVAT := divRound(gross*10000, 10000+vatBP)
	net := divRound(preVAT*10000, 10000+entBP)
	return Tax{Net: net, EntTax: preVAT - net, VAT: gross - preVAT}
}

// Coupon is a discount code. Percent coupons use Value as bp, amount coupons
// as kuruş. Coupons only discount tickets, never combos.
type Coupon struct {
	Code     string
	Percent  bool
	Value    int64
	MinTotal int64
}

// LineInput is one item to price.
type LineInput struct {
	Kind     string `json:"kind"`
	SeatID   int64  `json:"seat_id,omitempty"`
	Category string `json:"category,omitempty"`
	Gross    int64  `json:"gross"` // list price after dynamic pricing
}

// Line is a priced line. Paid = Gross - Discount; GiftPaid is the part of
// Paid settled by gift card, the rest is settled by the original payment.
type Line struct {
	LineInput
	Discount int64 `json:"discount"`
	Paid     int64 `json:"paid"`
	GiftPaid int64 `json:"gift_paid"`
	Tax      Tax   `json:"tax"`
}

// Quote is the full price calculation of an order.
type Quote struct {
	Lines       []Line `json:"lines"`
	Subtotal    int64  `json:"subtotal"`
	Discount    int64  `json:"discount"`
	Total       int64  `json:"total"`
	GiftApplied int64  `json:"gift_applied"`
	Payable     int64  `json:"payable"`
	Tax         Tax    `json:"tax"`
}

// allocate distributes amount over weights proportionally using the
// largest remainder method, so the parts sum exactly to amount.
func allocate(amount int64, weights []int64) []int64 {
	out := make([]int64, len(weights))
	var total int64
	for _, w := range weights {
		total += w
	}
	if total == 0 || amount == 0 {
		return out
	}
	type rem struct {
		i int
		r int64
	}
	var given int64
	rems := make([]rem, len(weights))
	for i, w := range weights {
		out[i] = amount * w / total
		given += out[i]
		rems[i] = rem{i, amount * w % total}
	}
	// hand out the leftover kuruş to the largest remainders, stable by index
	for left := amount - given; left > 0; left-- {
		best := -1
		for j, r := range rems {
			if best == -1 || r.r > rems[best].r {
				best = j
			}
		}
		out[rems[best].i]++
		rems[best].r = -1
	}
	return out
}

// BuildQuote prices an order: applies the coupon to ticket lines, then the
// gift card balance to the remaining total, and computes tax per line.
func BuildQuote(lines []LineInput, coupon *Coupon, giftBalance int64, r Rates) (Quote, error) {
	q := Quote{Lines: make([]Line, len(lines))}
	var ticketTotal int64
	ticketW := make([]int64, len(lines))
	for i, l := range lines {
		q.Lines[i].LineInput = l
		q.Subtotal += l.Gross
		if l.Kind == KindTicket {
			ticketTotal += l.Gross
			ticketW[i] = l.Gross
		}
	}

	if coupon != nil {
		if coupon.Value <= 0 {
			return Quote{}, ErrCouponInvalid
		}
		if q.Subtotal < coupon.MinTotal {
			return Quote{}, ErrCouponMinTotal
		}
		d := coupon.Value
		if coupon.Percent {
			d = divRound(ticketTotal*coupon.Value, 10000)
		}
		if d > ticketTotal {
			d = ticketTotal
		}
		q.Discount = d
		for i, a := range allocate(d, ticketW) {
			q.Lines[i].Discount = a
		}
	}

	paidW := make([]int64, len(lines))
	for i := range q.Lines {
		q.Lines[i].Paid = q.Lines[i].Gross - q.Lines[i].Discount
		paidW[i] = q.Lines[i].Paid
		q.Total += q.Lines[i].Paid
	}

	if giftBalance > 0 {
		q.GiftApplied = min(giftBalance, q.Total)
		for i, a := range allocate(q.GiftApplied, paidW) {
			q.Lines[i].GiftPaid = a
		}
	}
	q.Payable = q.Total - q.GiftApplied

	for i := range q.Lines {
		q.Lines[i].Tax = Split(q.Lines[i].Paid, q.Lines[i].Kind, r)
		q.Tax = q.Tax.Add(q.Lines[i].Tax)
	}
	return q, nil
}

// Refund is what goes back to the customer.
type Refund struct {
	Amount        int64 `json:"amount"`
	ToGiftCard    int64 `json:"to_gift_card"`
	ToOriginal    int64 `json:"to_original_payment"`
	Tax           Tax   `json:"tax"`
	RestoreCoupon bool  `json:"restore_coupon"`
}

// RefundLines refunds the given lines in full. The gift card part of each
// line goes back to the gift card, the rest to the original payment, and the
// tax reversal is the stored per-line breakdown. The coupon use is restored
// only when the whole order is refunded (wholeOrder).
func RefundLines(lines []Line, wholeOrder bool) (Refund, error) {
	if len(lines) == 0 {
		return Refund{}, ErrNothingToRefund
	}
	var rf Refund
	for _, l := range lines {
		rf.Amount += l.Paid
		rf.ToGiftCard += l.GiftPaid
		rf.Tax = rf.Tax.Add(l.Tax)
	}
	rf.ToOriginal = rf.Amount - rf.ToGiftCard
	rf.RestoreCoupon = wholeOrder
	return rf, nil
}

// ShareTerms is a distributor contract: a sliding scale by week since the
// film's release in this cinema chain.
type ShareTerms struct {
	Week1BP int64 `json:"week1_bp"`
	Week2BP int64 `json:"week2_bp"`
	LaterBP int64 `json:"later_bp"`
}

// WeekNo returns 1 for the first 7 days after release, 2 for the next 7, ...
func WeekNo(release, show time.Time) int {
	d := int(show.Sub(release).Hours() / 24)
	if d < 0 {
		return 1
	}
	return d/7 + 1
}

// DistributorShare is the distributor's part of net revenue (after VAT and
// entertainment tax) for a show in the given week.
func DistributorShare(net int64, week int, t ShareTerms) int64 {
	bp := t.LaterBP
	switch week {
	case 1:
		bp = t.Week1BP
	case 2:
		bp = t.Week2BP
	}
	return divRound(net*bp, 10000)
}
