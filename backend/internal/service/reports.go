package service

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"sahnebilet/internal/domain/pricing"
)

// RevenueRow aggregates sold lines by movie and channel. Refunded lines are
// excluded from revenue and reported separately.
type RevenueRow struct {
	MovieID       int64  `json:"movie_id"`
	MovieTitle    string `json:"movie_title"`
	Channel       string `json:"channel"`
	Tickets       int    `json:"tickets"`
	Combos        int    `json:"combos"`
	Gross         int64  `json:"gross"`
	Discount      int64  `json:"discount"`
	Paid          int64  `json:"paid"`
	Net           int64  `json:"net"`
	EntTax        int64  `json:"ent_tax"`
	VAT           int64  `json:"vat"`
	RefundedCount int    `json:"refunded_count"`
	RefundedPaid  int64  `json:"refunded_paid"`
}

type RevenueReport struct {
	From   time.Time    `json:"from"`
	To     time.Time    `json:"to"`
	Rows   []RevenueRow `json:"rows"`
	Totals RevenueRow   `json:"totals"`
}

// Revenue reports box office revenue for shows starting in [from, to).
func (s *Service) Revenue(ctx context.Context, from, to time.Time) (RevenueReport, error) {
	if !to.After(from) {
		return RevenueReport{}, invalid("to must be after from")
	}
	rows, err := s.DB.Query(ctx, `
		SELECT m.id, m.title, o.channel,
			count(*) FILTER (WHERE l.kind = 'ticket' AND l.status <> 'refunded'),
			count(*) FILTER (WHERE l.kind = 'combo' AND l.status <> 'refunded'),
			COALESCE(sum(l.gross_kurus) FILTER (WHERE l.status <> 'refunded'), 0),
			COALESCE(sum(l.discount_kurus) FILTER (WHERE l.status <> 'refunded'), 0),
			COALESCE(sum(l.paid_kurus) FILTER (WHERE l.status <> 'refunded'), 0),
			COALESCE(sum(l.net_kurus) FILTER (WHERE l.status <> 'refunded'), 0),
			COALESCE(sum(l.ent_tax_kurus) FILTER (WHERE l.status <> 'refunded'), 0),
			COALESCE(sum(l.vat_kurus) FILTER (WHERE l.status <> 'refunded'), 0),
			count(*) FILTER (WHERE l.kind = 'ticket' AND l.status = 'refunded'),
			COALESCE(sum(l.paid_kurus) FILTER (WHERE l.status = 'refunded'), 0)
		FROM order_lines l
		JOIN orders o ON o.id = l.order_id
		JOIN showtimes st ON st.id = l.showtime_id
		JOIN movies m ON m.id = st.movie_id
		WHERE st.starts_at >= $1 AND st.starts_at < $2
		GROUP BY m.id, m.title, o.channel
		ORDER BY m.id, o.channel`, from, to)
	if err != nil {
		return RevenueReport{}, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RevenueRow, error) {
		var x RevenueRow
		return x, r.Scan(&x.MovieID, &x.MovieTitle, &x.Channel, &x.Tickets, &x.Combos, &x.Gross, &x.Discount, &x.Paid, &x.Net, &x.EntTax, &x.VAT, &x.RefundedCount, &x.RefundedPaid)
	})
	if err != nil {
		return RevenueReport{}, err
	}
	rep := RevenueReport{From: from, To: to, Rows: list}
	if rep.Rows == nil {
		rep.Rows = []RevenueRow{}
	}
	t := &rep.Totals
	t.MovieTitle, t.Channel = "TOTAL", "all"
	for _, x := range list {
		t.Tickets += x.Tickets
		t.Combos += x.Combos
		t.Gross += x.Gross
		t.Discount += x.Discount
		t.Paid += x.Paid
		t.Net += x.Net
		t.EntTax += x.EntTax
		t.VAT += x.VAT
		t.RefundedCount += x.RefundedCount
		t.RefundedPaid += x.RefundedPaid
	}
	return rep, nil
}

// DistributorRow is the amount owed to a distributor for one film-week.
type DistributorRow struct {
	Distributor string `json:"distributor"`
	MovieID     int64  `json:"movie_id"`
	MovieTitle  string `json:"movie_title"`
	Week        int    `json:"week"`
	Tickets     int    `json:"tickets"`
	NetRevenue  int64  `json:"net_revenue"`
	ShareBP     int64  `json:"share_bp"`
	ShareAmount int64  `json:"share_amount"`
}

// Distributor computes the distributor's share on net ticket revenue (after
// VAT and entertainment tax, combos excluded) using the sliding scale of the
// contract, per release week.
func (s *Service) Distributor(ctx context.Context, from, to time.Time) ([]DistributorRow, error) {
	if !to.After(from) {
		return nil, invalid("to must be after from")
	}
	rows, err := s.DB.Query(ctx, `
		SELECT m.id, m.title, m.distributor, m.release_date, m.share_week1_bp, m.share_week2_bp, m.share_later_bp,
		       st.starts_at, count(*), sum(l.net_kurus)
		FROM order_lines l
		JOIN showtimes st ON st.id = l.showtime_id
		JOIN movies m ON m.id = st.movie_id
		WHERE l.kind = 'ticket' AND l.status <> 'refunded' AND st.starts_at >= $1 AND st.starts_at < $2
		GROUP BY m.id, st.id`, from, to)
	if err != nil {
		return nil, err
	}
	type key struct {
		movie int64
		week  int
	}
	agg := map[key]*DistributorRow{}
	terms := map[int64]pricing.ShareTerms{}
	for rows.Next() {
		var id int64
		var title, dist string
		var release, start time.Time
		var t pricing.ShareTerms
		var n int
		var net int64
		if err := rows.Scan(&id, &title, &dist, &release, &t.Week1BP, &t.Week2BP, &t.LaterBP, &start, &n, &net); err != nil {
			rows.Close()
			return nil, err
		}
		terms[id] = t
		rel := time.Date(release.Year(), release.Month(), release.Day(), 0, 0, 0, 0, s.Cfg.TimeZone)
		k := key{id, pricing.WeekNo(rel, start.In(s.Cfg.TimeZone))}
		r := agg[k]
		if r == nil {
			r = &DistributorRow{Distributor: dist, MovieID: id, MovieTitle: title, Week: k.week}
			agg[k] = r
		}
		r.Tickets += n
		r.NetRevenue += net
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DistributorRow, 0, len(agg))
	for k, r := range agg {
		t := terms[k.movie]
		r.ShareAmount = pricing.DistributorShare(r.NetRevenue, r.Week, t)
		switch r.Week {
		case 1:
			r.ShareBP = t.Week1BP
		case 2:
			r.ShareBP = t.Week2BP
		default:
			r.ShareBP = t.LaterBP
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Distributor != out[j].Distributor {
			return out[i].Distributor < out[j].Distributor
		}
		if out[i].MovieID != out[j].MovieID {
			return out[i].MovieID < out[j].MovieID
		}
		return out[i].Week < out[j].Week
	})
	return out, nil
}

// LiveStats shows that the system is alive (simulator traffic).
type LiveStats struct {
	HoldsLastMinute  int       `json:"holds_last_minute"`
	OrdersLastMinute int       `json:"orders_last_minute"`
	ScansLastMinute  int       `json:"scans_last_minute"`
	ActiveHolds      int       `json:"active_holds"`
	TicketsToday     int       `json:"tickets_today"`
	At               time.Time `json:"at"`
}

func (s *Service) Live(ctx context.Context) (LiveStats, error) {
	var l LiveStats
	err := s.DB.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM holds WHERE created_at > now() - interval '1 minute'),
		(SELECT count(*) FROM orders WHERE created_at > now() - interval '1 minute'),
		(SELECT count(*) FROM gate_scans WHERE received_at > now() - interval '1 minute'),
		(SELECT count(*) FROM holds WHERE status = 'active' AND expires_at > now()),
		(SELECT count(*) FROM order_lines l JOIN orders o ON o.id = l.order_id WHERE l.kind = 'ticket' AND o.created_at > date_trunc('day', now())),
		now()`).Scan(&l.HoldsLastMinute, &l.OrdersLastMinute, &l.ScansLastMinute, &l.ActiveHolds, &l.TicketsToday, &l.At)
	return l, err
}

type AuditEntry struct {
	ID       int64     `json:"id"`
	ActorID  *int64    `json:"actor_id"`
	Action   string    `json:"action"`
	Entity   string    `json:"entity"`
	EntityID string    `json:"entity_id"`
	Old      []byte    `json:"-"`
	New      []byte    `json:"-"`
	OldValue any       `json:"old_value"`
	NewValue any       `json:"new_value"`
	At       time.Time `json:"at"`
}

// Audit pages the audit log newest first by id (keyset on the primary key).
func (s *Service) Audit(ctx context.Context, before int64, limit int) ([]AuditEntry, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.DB.Query(ctx, `SELECT id, actor_id, action, entity, entity_id, old_value, new_value, at FROM audit_log
		WHERE id < $1 ORDER BY id DESC LIMIT $2`, before, limit)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		err := r.Scan(&e.ID, &e.ActorID, &e.Action, &e.Entity, &e.EntityID, &e.OldValue, &e.NewValue, &e.At)
		return e, err
	})
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(list) == limit {
		next = list[len(list)-1].ID
	}
	if list == nil {
		list = []AuditEntry{}
	}
	return list, next, nil
}

type Coupon struct {
	Code      string    `json:"code"`
	Percent   bool      `json:"percent"`
	Value     int64     `json:"value"`
	MinTotal  int64     `json:"min_total"`
	MaxUses   int       `json:"max_uses"`
	UsedCount int       `json:"used_count"`
	ValidFrom time.Time `json:"valid_from"`
	ValidTo   time.Time `json:"valid_to"`
}

func (s *Service) ListCoupons(ctx context.Context) ([]Coupon, error) {
	rows, err := s.DB.Query(ctx, `SELECT code, percent, value, min_total, max_uses, used_count, valid_from, valid_to FROM coupons ORDER BY code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Coupon, error) {
		var c Coupon
		return c, r.Scan(&c.Code, &c.Percent, &c.Value, &c.MinTotal, &c.MaxUses, &c.UsedCount, &c.ValidFrom, &c.ValidTo)
	})
}

func (s *Service) CreateCoupon(ctx context.Context, a Actor, c Coupon) (Coupon, error) {
	c.Code = strings.ToUpper(strings.TrimSpace(c.Code))
	switch {
	case len(c.Code) < 3 || len(c.Code) > 32:
		return c, invalid("code must be 3-32 characters")
	case c.Value <= 0 || (c.Percent && c.Value > 10000):
		return c, invalid("value must be > 0 (percent coupons: basis points up to 10000)")
	case c.MaxUses <= 0:
		return c, invalid("max_uses must be > 0")
	case c.MinTotal < 0:
		return c, invalid("min_total must be >= 0")
	case !c.ValidTo.After(c.ValidFrom):
		return c, invalid("valid_to must be after valid_from")
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO coupons (code, percent, value, min_total, max_uses, valid_from, valid_to) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			c.Code, c.Percent, c.Value, c.MinTotal, c.MaxUses, c.ValidFrom, c.ValidTo)
		if isUniqueViolation(err) {
			return E(http.StatusConflict, "COUPON_EXISTS", "coupon code already exists")
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, a.UserID, "coupon.create", "coupon", c.Code, nil, c)
	})
	return c, err
}

type GiftCard struct {
	Code    string `json:"code"`
	Initial int64  `json:"initial_kurus"`
	Balance int64  `json:"balance_kurus"`
}

func (s *Service) GetGiftCard(ctx context.Context, code string) (GiftCard, error) {
	var g GiftCard
	err := s.DB.QueryRow(ctx, `SELECT code, initial_kurus, balance_kurus FROM gift_cards WHERE code = $1`, strings.ToUpper(strings.TrimSpace(code))).
		Scan(&g.Code, &g.Initial, &g.Balance)
	if err == pgx.ErrNoRows {
		return g, ErrNotFound
	}
	return g, err
}

// IssueGiftCard sells a new gift card at the box office.
func (s *Service) IssueGiftCard(ctx context.Context, a Actor, amount int64) (GiftCard, error) {
	if amount < 5000 || amount > 500000 {
		return GiftCard{}, invalid("amount must be between 5000 and 500000 kuruş")
	}
	g := GiftCard{Code: "HC-" + strings.ToUpper(newID()[:8]), Initial: amount, Balance: amount}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO gift_cards (code, initial_kurus, balance_kurus) VALUES ($1,$2,$2)`, g.Code, amount); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO gift_card_tx (code, amount) VALUES ($1,$2)`, g.Code, amount); err != nil {
			return err
		}
		return audit(ctx, tx, a.UserID, "giftcard.issue", "gift_card", g.Code, nil, g)
	})
	return g, err
}

// Integrity checks the no-double-sale invariant for a showtime: every sold
// seat has exactly one valid ticket and no seat has two.
type Integrity struct {
	ShowtimeID       int64 `json:"showtime_id"`
	SoldSeats        int   `json:"sold_seats"`
	ValidTickets     int   `json:"valid_tickets"`
	DuplicateSeats   int   `json:"duplicate_seats"`
	SoldWithoutTicket int  `json:"sold_without_ticket"`
	OK               bool  `json:"ok"`
}

func (s *Service) CheckIntegrity(ctx context.Context, showtimeID int64) (Integrity, error) {
	in := Integrity{ShowtimeID: showtimeID}
	err := s.DB.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM show_seats WHERE showtime_id = $1 AND status = 'sold'),
		(SELECT count(*) FROM order_lines WHERE showtime_id = $1 AND kind = 'ticket' AND status <> 'refunded'),
		(SELECT count(*) FROM (SELECT seat_id FROM order_lines WHERE showtime_id = $1 AND kind = 'ticket' AND status <> 'refunded'
			GROUP BY seat_id HAVING count(*) > 1) d),
		(SELECT count(*) FROM show_seats ss WHERE ss.showtime_id = $1 AND ss.status = 'sold' AND NOT EXISTS
			(SELECT 1 FROM order_lines l WHERE l.showtime_id = ss.showtime_id AND l.seat_id = ss.seat_id AND l.status <> 'refunded'))`,
		showtimeID).Scan(&in.SoldSeats, &in.ValidTickets, &in.DuplicateSeats, &in.SoldWithoutTicket)
	in.OK = err == nil && in.DuplicateSeats == 0 && in.SoldWithoutTicket == 0 && in.SoldSeats == in.ValidTickets
	return in, err
}
