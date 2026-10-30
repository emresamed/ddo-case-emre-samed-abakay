package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"sahnebilet/internal/domain/pricing"
	"sahnebilet/internal/domain/ticket"
)

type TicketSel struct {
	SeatID   int64  `json:"seat_id"`
	Category string `json:"category"`
}

type OrderRequest struct {
	HoldID       string      `json:"hold_id"`
	Tickets      []TicketSel `json:"tickets"`
	Combos       int         `json:"combos"`
	CouponCode   string      `json:"coupon_code"`
	GiftCardCode string      `json:"gift_card_code"`
}

type OrderLine struct {
	ID        int64       `json:"id"`
	Kind      string      `json:"kind"`
	TicketID  string      `json:"ticket_id,omitempty"`
	SeatID    int64       `json:"seat_id,omitempty"`
	SeatLabel string      `json:"seat_label,omitempty"`
	Category  string      `json:"category,omitempty"`
	Gross     int64       `json:"gross"`
	Discount  int64       `json:"discount"`
	Paid      int64       `json:"paid"`
	GiftPaid  int64       `json:"gift_paid"`
	Tax       pricing.Tax `json:"tax"`
	Status    string      `json:"status"`
}

type Order struct {
	ID           string      `json:"id"`
	UserID       int64       `json:"user_id"`
	ShowtimeID   int64       `json:"showtime_id"`
	MovieTitle   string      `json:"movie_title"`
	HallName     string      `json:"hall_name"`
	StartsAt     time.Time   `json:"starts_at"`
	Channel      string      `json:"channel"`
	Status       string      `json:"status"`
	Subtotal     int64       `json:"subtotal"`
	Discount     int64       `json:"discount"`
	Total        int64       `json:"total"`
	GiftPaid     int64       `json:"gift_paid"`
	CouponCode   *string     `json:"coupon_code,omitempty"`
	GiftCardCode *string     `json:"gift_card_code,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	Lines        []OrderLine `json:"lines"`
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type priced struct {
	quote      pricing.Quote
	showtime   Showtime
	labels     map[int64]string
	coupon     *pricing.Coupon
	giftCode   string
	giftBal    int64
}

// price computes an order. With lock=true it locks the hold, its seats, the
// coupon and the gift card rows so that the subsequent writes are safe.
func (s *Service) price(ctx context.Context, q querier, a Actor, req OrderRequest, lock bool) (priced, error) {
	var p priced
	forUpdate := ""
	if lock {
		forUpdate = " FOR UPDATE"
	}
	if req.Combos < 0 || req.Combos > 10 {
		return p, invalid("combos must be between 0 and 10")
	}
	var owner, showtimeID int64
	var status string
	var exp time.Time
	err := q.QueryRow(ctx, `SELECT user_id, showtime_id, status, expires_at FROM holds WHERE id::text = $1`+forUpdate, req.HoldID).
		Scan(&owner, &showtimeID, &status, &exp)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != a.UserID) {
		return p, E(http.StatusNotFound, "HOLD_NOT_FOUND", "hold not found")
	}
	if err != nil {
		return p, err
	}
	if status != "active" || !exp.After(s.Now()) {
		return p, E(http.StatusConflict, "HOLD_EXPIRED", "hold expired, select your seats again")
	}

	rows, err := q.Query(ctx, `SELECT ss.seat_id, s.label FROM show_seats ss JOIN seats s ON s.id = ss.seat_id
		WHERE ss.hold_id::text = $1 AND ss.status = 'held' ORDER BY ss.seat_id`+strings.Replace(forUpdate, "UPDATE", "UPDATE OF ss", 1), req.HoldID)
	if err != nil {
		return p, err
	}
	p.labels = map[int64]string{}
	var held []int64
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			rows.Close()
			return p, err
		}
		p.labels[id] = label
		held = append(held, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}
	if len(held) == 0 {
		return p, E(http.StatusConflict, "HOLD_EXPIRED", "hold has no seats")
	}

	cat := map[int64]string{}
	for _, t := range req.Tickets {
		if _, ok := p.labels[t.SeatID]; !ok {
			return p, invalid(fmt.Sprintf("seat %d is not in this hold", t.SeatID))
		}
		cat[t.SeatID] = t.Category
	}

	st, err := scanShowtime(q.QueryRow(ctx, showtimeSelect+" WHERE st.id = $1", showtimeID))
	if err != nil {
		return p, err
	}
	p.showtime = st

	var lines []pricing.LineInput
	for _, id := range held {
		c := cat[id]
		if c == "" {
			c = pricing.CategoryFull
		}
		price, _, err := pricing.TicketPrice(s.showContext(st), c)
		if err != nil {
			return p, invalid("category must be full or student")
		}
		lines = append(lines, pricing.LineInput{Kind: pricing.KindTicket, SeatID: id, Category: c, Gross: price})
	}
	for i := 0; i < req.Combos; i++ {
		lines = append(lines, pricing.LineInput{Kind: pricing.KindCombo, Gross: pricing.ComboPrice})
	}

	if code := strings.ToUpper(strings.TrimSpace(req.CouponCode)); code != "" {
		var c pricing.Coupon
		var used, maxUses int
		var from, to time.Time
		err := q.QueryRow(ctx, `SELECT code, percent, value, min_total, used_count, max_uses, valid_from, valid_to FROM coupons WHERE code = $1`+forUpdate, code).
			Scan(&c.Code, &c.Percent, &c.Value, &c.MinTotal, &used, &maxUses, &from, &to)
		if errors.Is(err, pgx.ErrNoRows) {
			return p, E(http.StatusUnprocessableEntity, "COUPON_INVALID", "coupon not found")
		}
		if err != nil {
			return p, err
		}
		now := s.Now()
		if now.Before(from) || now.After(to) {
			return p, E(http.StatusUnprocessableEntity, "COUPON_INVALID", "coupon is not valid at this time")
		}
		if used >= maxUses {
			return p, E(http.StatusConflict, "COUPON_EXHAUSTED", "coupon has no uses left")
		}
		p.coupon = &c
	}

	if code := strings.ToUpper(strings.TrimSpace(req.GiftCardCode)); code != "" {
		err := q.QueryRow(ctx, `SELECT balance_kurus FROM gift_cards WHERE code = $1`+forUpdate, code).Scan(&p.giftBal)
		if errors.Is(err, pgx.ErrNoRows) {
			return p, E(http.StatusUnprocessableEntity, "GIFT_CARD_INVALID", "gift card not found")
		}
		if err != nil {
			return p, err
		}
		p.giftCode = code
	}

	p.quote, err = pricing.BuildQuote(lines, p.coupon, p.giftBal, pricing.DefaultRates)
	switch {
	case errors.Is(err, pricing.ErrCouponMinTotal):
		return p, E(http.StatusUnprocessableEntity, "COUPON_MIN_TOTAL", err.Error())
	case err != nil:
		return p, E(http.StatusUnprocessableEntity, "COUPON_INVALID", err.Error())
	}
	return p, nil
}

// Quote prices a cart without changing anything.
func (s *Service) Quote(ctx context.Context, a Actor, req OrderRequest) (pricing.Quote, error) {
	p, err := s.price(ctx, s.DB, a, req, false)
	return p.quote, err
}

// CreateOrder converts a hold into paid tickets in one transaction.
func (s *Service) CreateOrder(ctx context.Context, a Actor, req OrderRequest) (Order, error) {
	channel := "online"
	if a.Role == RoleBoxOffice {
		channel = "box_office"
	}
	var orderID string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := s.price(ctx, tx, a, req, true)
		if err != nil {
			return err
		}
		q := p.quote
		orderID = newID()
		var coupon, gift any
		if p.coupon != nil {
			coupon = p.coupon.Code
			if _, err := tx.Exec(ctx, `UPDATE coupons SET used_count = used_count + 1 WHERE code = $1`, p.coupon.Code); err != nil {
				return err
			}
		}
		if p.giftCode != "" && q.GiftApplied > 0 {
			gift = p.giftCode
		}
		if _, err := tx.Exec(ctx, `INSERT INTO orders (id, user_id, showtime_id, channel, status, subtotal_kurus, discount_kurus, total_kurus, gift_kurus, coupon_code, gift_card_code)
			VALUES ($1::uuid,$2,$3,$4,'paid',$5,$6,$7,$8,$9,$10)`,
			orderID, a.UserID, p.showtime.ID, channel, q.Subtotal, q.Discount, q.Total, q.GiftApplied, coupon, gift); err != nil {
			return err
		}
		if gift != nil {
			if _, err := tx.Exec(ctx, `UPDATE gift_cards SET balance_kurus = balance_kurus - $2 WHERE code = $1`, p.giftCode, q.GiftApplied); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO gift_card_tx (code, order_id, amount) VALUES ($1,$2::uuid,$3)`, p.giftCode, orderID, -q.GiftApplied); err != nil {
				return err
			}
		}
		batch := &pgx.Batch{}
		var seatIDs []int64
		for _, l := range q.Lines {
			var ticketID, seat, category any
			if l.Kind == pricing.KindTicket {
				ticketID, seat, category = newID(), l.SeatID, l.Category
				seatIDs = append(seatIDs, l.SeatID)
			}
			batch.Queue(`INSERT INTO order_lines (order_id, kind, ticket_id, showtime_id, seat_id, category, gross_kurus, discount_kurus, paid_kurus, gift_kurus, net_kurus, ent_tax_kurus, vat_kurus)
				VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
				orderID, l.Kind, ticketID, p.showtime.ID, seat, category, l.Gross, l.Discount, l.Paid, l.GiftPaid, l.Tax.Net, l.Tax.EntTax, l.Tax.VAT)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			if isUniqueViolation(err) {
				return E(http.StatusConflict, "SEAT_TAKEN", "seat already sold")
			}
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE show_seats SET status = 'sold', hold_id = NULL, held_until = NULL
			WHERE showtime_id = $1 AND seat_id = ANY($2) AND hold_id::text = $3 AND status = 'held'`, p.showtime.ID, seatIDs, req.HoldID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(len(seatIDs)) {
			return E(http.StatusConflict, "HOLD_EXPIRED", "hold lost its seats")
		}
		if _, err := tx.Exec(ctx, `UPDATE holds SET status = 'converted' WHERE id::text = $1`, req.HoldID); err != nil {
			return err
		}
		return audit(ctx, tx, a.UserID, "order.create", "order", orderID, nil, map[string]any{
			"channel": channel, "showtime_id": p.showtime.ID, "seats": seatIDs, "total": q.Total, "discount": q.Discount,
			"gift": q.GiftApplied, "coupon": coupon,
		})
	})
	if err != nil {
		return Order{}, err
	}
	o, err := s.GetOrder(ctx, a, orderID)
	if err == nil {
		n := 0
		for _, l := range o.Lines {
			if l.Kind == pricing.KindTicket {
				n++
			}
		}
		ticketsSold.WithLabelValues(channel).Add(float64(n))
	}
	return o, err
}

const orderSelect = `SELECT o.id::text, o.user_id, o.showtime_id, m.title, h.name, st.starts_at, o.channel, o.status,
	o.subtotal_kurus, o.discount_kurus, o.total_kurus, o.gift_kurus, o.coupon_code, o.gift_card_code, o.created_at
	FROM orders o JOIN showtimes st ON st.id = o.showtime_id JOIN movies m ON m.id = st.movie_id JOIN halls h ON h.id = st.hall_id`

func scanOrder(r pgx.Row) (Order, error) {
	var o Order
	err := r.Scan(&o.ID, &o.UserID, &o.ShowtimeID, &o.MovieTitle, &o.HallName, &o.StartsAt, &o.Channel, &o.Status,
		&o.Subtotal, &o.Discount, &o.Total, &o.GiftPaid, &o.CouponCode, &o.GiftCardCode, &o.CreatedAt)
	return o, err
}

func (s *Service) loadLines(ctx context.Context, q querier, orderID string, forUpdate bool) ([]OrderLine, error) {
	sql := `SELECT l.id, l.kind, COALESCE(l.ticket_id::text,''), COALESCE(l.seat_id,0), COALESCE(se.label,''), COALESCE(l.category,''),
			l.gross_kurus, l.discount_kurus, l.paid_kurus, l.gift_kurus, l.net_kurus, l.ent_tax_kurus, l.vat_kurus, l.status
		FROM order_lines l LEFT JOIN seats se ON se.id = l.seat_id WHERE l.order_id::text = $1 ORDER BY l.id`
	if forUpdate {
		sql += " FOR UPDATE OF l"
	}
	rows, err := q.Query(ctx, sql, orderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderLine, error) {
		var l OrderLine
		return l, r.Scan(&l.ID, &l.Kind, &l.TicketID, &l.SeatID, &l.SeatLabel, &l.Category, &l.Gross, &l.Discount, &l.Paid, &l.GiftPaid,
			&l.Tax.Net, &l.Tax.EntTax, &l.Tax.VAT, &l.Status)
	})
}

// canSeeAll: staff roles that work on any customer's orders.
func canSeeAll(a Actor) bool {
	return hasRole(a, RoleBoxOffice, RoleAccounting, RoleHallManager)
}

func (s *Service) GetOrder(ctx context.Context, a Actor, id string) (Order, error) {
	o, err := scanOrder(s.DB.QueryRow(ctx, orderSelect+" WHERE o.id::text = $1", id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && o.UserID != a.UserID && !canSeeAll(a)) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	o.Lines, err = s.loadLines(ctx, s.DB, id, false)
	return o, err
}

// ListOrders pages newest first by (created_at, id).
func (s *Service) ListOrders(ctx context.Context, a Actor, cursor string, limit int) (Page[Order], error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	args := []any{}
	var where []string
	if !canSeeAll(a) {
		args = append(args, a.UserID)
		where = append(where, fmt.Sprintf("o.user_id = $%d", len(args)))
	}
	if cursor != "" {
		t, id, err := decodeCursor(cursor)
		if err != nil {
			return Page[Order]{}, err
		}
		args = append(args, t, id)
		where = append(where, fmt.Sprintf("(o.created_at, o.id) < ($%d, $%d::uuid)", len(args)-1, len(args)))
	}
	q := orderSelect
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit+1)
	q += fmt.Sprintf(" ORDER BY o.created_at DESC, o.id DESC LIMIT $%d", len(args))
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return Page[Order]{}, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
	if err != nil {
		return Page[Order]{}, err
	}
	p := Page[Order]{Items: items}
	if len(items) > limit {
		p.Items = items[:limit]
		last := p.Items[limit-1]
		p.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	for i := range p.Items {
		if p.Items[i].Lines, err = s.loadLines(ctx, s.DB, p.Items[i].ID, false); err != nil {
			return Page[Order]{}, err
		}
	}
	if p.Items == nil {
		p.Items = []Order{}
	}
	return p, nil
}

// TicketQR returns the signed QR payload of a ticket.
func (s *Service) TicketQR(ctx context.Context, a Actor, ticketID string) (string, error) {
	var owner, showtimeID int64
	var label, status string
	var starts, ends time.Time
	err := s.DB.QueryRow(ctx, `SELECT o.user_id, l.showtime_id, se.label, l.status, st.starts_at, st.ends_at
		FROM order_lines l JOIN orders o ON o.id = l.order_id JOIN seats se ON se.id = l.seat_id JOIN showtimes st ON st.id = l.showtime_id
		WHERE l.ticket_id::text = $1`, ticketID).Scan(&owner, &showtimeID, &label, &status, &starts, &ends)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != a.UserID && a.Role != RoleBoxOffice) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if status == "refunded" {
		return "", E(http.StatusConflict, "TICKET_REFUNDED", "ticket was refunded")
	}
	return ticket.Sign(s.signKey, ticket.Claims{
		TicketID: ticketID, ShowtimeID: showtimeID, Seat: label,
		NotBefore: starts.Add(-24 * time.Hour).Unix(), NotAfter: ends.Unix(),
	})
}

// RefundResult is returned by Refund.
type RefundResult struct {
	OrderID  string         `json:"order_id"`
	Status   string         `json:"status"`
	Refund   pricing.Refund `json:"refund"`
	LineIDs  []int64        `json:"line_ids"`
}

// Refund refunds tickets (all remaining lines when ticketIDs is empty).
// Customers can refund their own unused tickets until 60 minutes before the
// show; box office and hall managers until the show ends. Combos are
// refunded together with the last ticket of the order.
func (s *Service) Refund(ctx context.Context, a Actor, orderID string, ticketIDs []string) (RefundResult, error) {
	var res RefundResult
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var owner, showtimeID int64
		var status string
		var coupon, gift *string
		var starts, ends time.Time
		err := tx.QueryRow(ctx, `SELECT o.user_id, o.showtime_id, o.status, o.coupon_code, o.gift_card_code, st.starts_at, st.ends_at
			FROM orders o JOIN showtimes st ON st.id = o.showtime_id WHERE o.id::text = $1 FOR UPDATE OF o`, orderID).
			Scan(&owner, &showtimeID, &status, &coupon, &gift, &starts, &ends)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		staff := hasRole(a, RoleBoxOffice, RoleHallManager)
		if !staff && owner != a.UserID {
			return ErrNotFound
		}
		now := s.Now()
		if !staff && now.After(starts.Add(-60*time.Minute)) {
			return E(http.StatusConflict, "REFUND_WINDOW_CLOSED", "online refunds close 60 minutes before the show")
		}
		if now.After(ends) {
			return E(http.StatusConflict, "REFUND_WINDOW_CLOSED", "show has ended")
		}
		lines, err := s.loadLines(ctx, tx, orderID, true)
		if err != nil {
			return err
		}
		all := len(ticketIDs) == 0
		want := map[string]bool{}
		for _, id := range ticketIDs {
			want[id] = true
		}
		var pick []OrderLine
		remainingTickets := 0
		for _, l := range lines {
			if l.Kind != pricing.KindTicket || l.Status == "refunded" {
				continue
			}
			if all || want[l.TicketID] {
				if l.Status == "used" {
					return E(http.StatusConflict, "TICKET_USED", "ticket "+l.TicketID+" was already used at the gate")
				}
				pick = append(pick, l)
				delete(want, l.TicketID)
			} else {
				remainingTickets++
			}
		}
		if len(want) > 0 {
			return invalid("one or more tickets do not belong to this order or are already refunded")
		}
		if len(pick) == 0 {
			return E(http.StatusConflict, "NOTHING_TO_REFUND", "order is already refunded")
		}
		whole := remainingTickets == 0
		if whole {
			for _, l := range lines {
				if l.Kind == pricing.KindCombo && l.Status != "refunded" {
					pick = append(pick, l)
				}
			}
		}
		dl := make([]pricing.Line, len(pick))
		ids := make([]int64, len(pick))
		var seatIDs []int64
		for i, l := range pick {
			dl[i] = pricing.Line{LineInput: pricing.LineInput{Kind: l.Kind, Gross: l.Gross}, Discount: l.Discount, Paid: l.Paid, GiftPaid: l.GiftPaid, Tax: l.Tax}
			ids[i] = l.ID
			if l.Kind == pricing.KindTicket {
				seatIDs = append(seatIDs, l.SeatID)
			}
		}
		rf, err := pricing.RefundLines(dl, whole)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE order_lines SET status = 'refunded', refunded_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE show_seats SET status = 'free' WHERE showtime_id = $1 AND seat_id = ANY($2) AND status = 'sold'`, showtimeID, seatIDs); err != nil {
			return err
		}
		if rf.ToGiftCard > 0 && gift != nil {
			if _, err := tx.Exec(ctx, `UPDATE gift_cards SET balance_kurus = balance_kurus + $2 WHERE code = $1`, *gift, rf.ToGiftCard); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO gift_card_tx (code, order_id, amount) VALUES ($1,$2::uuid,$3)`, *gift, orderID, rf.ToGiftCard); err != nil {
				return err
			}
		}
		if rf.RestoreCoupon && coupon != nil {
			if _, err := tx.Exec(ctx, `UPDATE coupons SET used_count = used_count - 1 WHERE code = $1 AND used_count > 0`, *coupon); err != nil {
				return err
			}
		}
		newStatus := "partially_refunded"
		if whole {
			newStatus = "refunded"
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2 WHERE id::text = $1`, orderID, newStatus); err != nil {
			return err
		}
		res = RefundResult{OrderID: orderID, Status: newStatus, Refund: rf, LineIDs: ids}
		ticketsRefunded.Add(float64(len(seatIDs)))
		return audit(ctx, tx, a.UserID, "order.refund", "order", orderID, map[string]any{"status": status}, res)
	})
	return res, err
}
