package service

import (
	"context"
	"time"

	"sahnebilet/internal/domain/experiment"
)

// CurrentExperiment is the running campaign test. Both coupons exist in the
// seed; customers see only the coupon of their own arm in the cart.
var CurrentExperiment = experiment.Experiment{
	Key:   "kasim-indirim",
	Title: "Kasım indirimi: yüzde mi, sabit tutar mı?",
	Variants: []experiment.Variant{
		{Name: "A", CouponCode: "HOSGELDIN10", Description: "Biletlerde %10 indirim"},
		{Name: "B", CouponCode: "SINEMA50", Description: "300 TL ve üzeri sepette 50 TL indirim"},
	},
}

// Offer is the campaign a customer is shown.
type Offer struct {
	Experiment string             `json:"experiment"`
	Variant    experiment.Variant `json:"variant"`
}

// OfferFor returns the customer's arm of the current experiment.
func (s *Service) OfferFor(a Actor) Offer {
	e := CurrentExperiment
	return Offer{Experiment: e.Key, Variant: e.Variants[experiment.Assign(e.Key, a.UserID, len(e.Variants))]}
}

// ABReport compares the arms for carts opened in [from, to).
type ABReport struct {
	Experiment experiment.Experiment `json:"experiment"`
	From       time.Time             `json:"from"`
	To         time.Time             `json:"to"`
	Arms       []experiment.Arm      `json:"arms"`
	Comparison experiment.Comparison `json:"comparison"`
	Definition string                `json:"definition"`
}

// ABReport aggregates per user, then per arm. The offer is shown in the
// cart, so a cart is a trial and a paid cart a success; user-level numbers
// are reported for context. Box office sales are excluded: staff are not in
// the test.
func (s *Service) ABReport(ctx context.Context, from, to time.Time) (ABReport, error) {
	if !to.After(from) {
		return ABReport{}, invalid("to must be after from")
	}
	e := CurrentExperiment
	rep := ABReport{Experiment: e, From: from, To: to,
		Definition: "trial = a cart opened by a customer; success = the cart was paid; two-sided two-proportion z-test on cart conversion, alpha 0.05; box office excluded"}
	rep.Arms = make([]experiment.Arm, len(e.Variants))
	for i, v := range e.Variants {
		rep.Arms[i].Variant = v
	}
	rows, err := s.DB.Query(ctx, `
		WITH exposed AS (
			SELECT h.user_id, count(*) AS carts, count(*) FILTER (WHERE h.status = 'converted') AS paid
			FROM holds h JOIN users u ON u.id = h.user_id
			WHERE u.role = 'customer' AND h.created_at >= $1 AND h.created_at < $2
			GROUP BY h.user_id
		), ord AS (
			SELECT o.user_id, o.id, o.total_kurus, o.discount_kurus, COALESCE(o.coupon_code, '') AS coupon,
				EXISTS (SELECT 1 FROM order_lines l WHERE l.order_id = o.id AND l.kind = 'combo') AS has_combo,
				(SELECT count(*) FROM order_lines l WHERE l.order_id = o.id AND l.kind = 'ticket') AS tickets
			FROM orders o
			WHERE o.channel = 'online' AND o.created_at >= $1 AND o.created_at < $2
		)
		SELECT e.user_id, e.carts, e.paid, o.id IS NOT NULL, COALESCE(o.total_kurus, 0), COALESCE(o.discount_kurus, 0),
			COALESCE(o.coupon, ''), COALESCE(o.has_combo, false), COALESCE(o.tickets, 0)
		FROM exposed e LEFT JOIN ord o ON o.user_id = e.user_id
		ORDER BY e.user_id`, from, to)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	converted := map[int64]bool{}
	for rows.Next() {
		var uid, tickets int64
		var carts, paid int
		var hasOrder, combo bool
		var total, discount int64
		var coupon string
		if err := rows.Scan(&uid, &carts, &paid, &hasOrder, &total, &discount, &coupon, &combo, &tickets); err != nil {
			return rep, err
		}
		arm := &rep.Arms[experiment.Assign(e.Key, uid, len(e.Variants))]
		if !seen[uid] {
			seen[uid] = true
			arm.Exposed++
			arm.Carts += carts
			arm.PaidCarts += paid
		}
		if !hasOrder {
			continue
		}
		if !converted[uid] {
			converted[uid] = true
			arm.Converted++
		}
		arm.Orders++
		arm.Revenue += total
		arm.Discount += discount
		arm.Tickets += tickets
		if combo {
			arm.ComboOrders++
		}
		if coupon == arm.Variant.CouponCode {
			arm.CouponUses++
		}
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for i := range rep.Arms {
		rep.Arms[i].Finish()
	}
	rep.Comparison = experiment.Compare(rep.Arms[0], rep.Arms[1])
	return rep, nil
}
