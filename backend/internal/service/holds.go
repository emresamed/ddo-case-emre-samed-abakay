package service

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"sahnebilet/internal/domain/seating"
)

// MaxSeatsPerHold limits one cart.
const MaxSeatsPerHold = 10

// MaxExtensions is how many times a hold can be extended.
const MaxExtensions = 2

type Hold struct {
	ID         string    `json:"id"`
	ShowtimeID int64     `json:"showtime_id"`
	Status     string    `json:"status"`
	ExpiresAt  time.Time `json:"expires_at"`
	Extensions int       `json:"extensions"`
	SeatIDs    []int64   `json:"seat_ids"`
}

type HoldRequest struct {
	ShowtimeID          int64   `json:"showtime_id"`
	SeatIDs             []int64 `json:"seat_ids"`
	OverrideSingleSeat  bool    `json:"override_single_seat"`
}

// CreateHold atomically holds all requested seats or none.
//
// Concurrency: all seats in the affected rows are locked with
// SELECT ... FOR UPDATE in seat id order (no deadlocks between carts).
// Locking whole rows, not only the requested seats, makes the single-seat
// check race free: two carts in the same row are serialised, so they cannot
// each pass the check and together leave an orphan seat.
func (s *Service) CreateHold(ctx context.Context, a Actor, req HoldRequest) (Hold, error) {
	ids := uniq(req.SeatIDs)
	if len(ids) == 0 || len(ids) > MaxSeatsPerHold {
		return Hold{}, invalid("seat_ids must contain 1 to 10 distinct seats")
	}
	if req.OverrideSingleSeat && a.Role != RoleHallManager {
		return Hold{}, E(http.StatusForbidden, "OVERRIDE_NOT_ALLOWED", "only a hall manager can override the single-seat rule")
	}
	var h Hold
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var startsAt time.Time
		err := tx.QueryRow(ctx, `SELECT starts_at FROM showtimes WHERE id = $1`, req.ShowtimeID).Scan(&startsAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !startsAt.After(s.Now()) {
			return E(http.StatusConflict, "SHOW_STARTED", "showtime has already started")
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM holds WHERE user_id = $1 AND status = 'active' AND expires_at > now()`, a.UserID).Scan(&active); err != nil {
			return err
		}
		if active >= 3 && a.Role == RoleCustomer {
			return E(http.StatusConflict, "TOO_MANY_HOLDS", "release or buy one of your active carts first")
		}

		rows, err := tx.Query(ctx, `
			SELECT s.id, s.row_no, s.col_no, s.kind,
			       ss.status = 'sold' OR (ss.status = 'held' AND ss.held_until > now())
			FROM show_seats ss JOIN seats s ON s.id = ss.seat_id
			WHERE ss.showtime_id = $1
			  AND s.row_no IN (SELECT row_no FROM seats s2 JOIN show_seats x ON x.seat_id = s2.id AND x.showtime_id = $1 WHERE s2.id = ANY($2))
			ORDER BY ss.seat_id
			FOR UPDATE OF ss`, req.ShowtimeID, ids)
		if err != nil {
			return err
		}
		seats, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seating.Seat, error) {
			var st seating.Seat
			return st, r.Scan(&st.ID, &st.Row, &st.Col, &st.Kind, &st.Taken)
		})
		if err != nil {
			return err
		}

		violations, err := seating.CheckSingleSeat(seats, ids)
		switch {
		case errors.Is(err, seating.ErrSeatTaken):
			return E(http.StatusConflict, "SEAT_TAKEN", "one or more seats are no longer available")
		case errors.Is(err, seating.ErrUnknownSeat):
			return invalid("one or more seats do not belong to this showtime")
		case errors.Is(err, seating.ErrSeatBlocked):
			return invalid("one or more seats are blocked")
		case err != nil:
			return err
		}
		if len(violations) > 0 && !req.OverrideSingleSeat {
			e := E(http.StatusUnprocessableEntity, "SINGLE_SEAT_RULE", "selection would leave a single empty seat")
			e.Details = violations
			return e
		}

		h = Hold{ID: newID(), ShowtimeID: req.ShowtimeID, Status: "active", ExpiresAt: s.Now().Add(s.Cfg.HoldTTL).UTC(), SeatIDs: ids}
		if _, err := tx.Exec(ctx, `INSERT INTO holds (id, user_id, showtime_id, expires_at) VALUES ($1::uuid,$2,$3,$4)`,
			h.ID, a.UserID, h.ShowtimeID, h.ExpiresAt); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE show_seats SET status = 'held', hold_id = $3::uuid, held_until = $4
			WHERE showtime_id = $1 AND seat_id = ANY($2)`, req.ShowtimeID, ids, h.ID, h.ExpiresAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(len(ids)) {
			return E(http.StatusConflict, "SEAT_TAKEN", "one or more seats are no longer available")
		}
		if len(violations) > 0 {
			return audit(ctx, tx, a.UserID, "hold.single_seat_override", "hold", h.ID, nil, map[string]any{"seats": ids, "violations": violations})
		}
		return nil
	})
	if err == nil {
		holdsCreated.Inc()
	} else {
		holdsRejected.Inc()
	}
	return h, err
}

func uniq(ids []int64) []int64 {
	m := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if id > 0 && !m[id] {
			m[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// GetHold returns a hold of the caller.
func (s *Service) GetHold(ctx context.Context, a Actor, id string) (Hold, error) {
	var h Hold
	err := s.DB.QueryRow(ctx, `SELECT id::text, showtime_id,
			CASE WHEN status = 'active' AND expires_at <= now() THEN 'expired' ELSE status END,
			expires_at, extensions,
			COALESCE((SELECT array_agg(seat_id ORDER BY seat_id) FROM show_seats WHERE hold_id = holds.id), '{}')
		FROM holds WHERE id::text = $1 AND user_id = $2`, id, a.UserID).
		Scan(&h.ID, &h.ShowtimeID, &h.Status, &h.ExpiresAt, &h.Extensions, &h.SeatIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// ExtendHold pushes the expiry to now + TTL, at most MaxExtensions times.
func (s *Service) ExtendHold(ctx context.Context, a Actor, id string) (Hold, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var status string
		var exp time.Time
		var ext int
		err := tx.QueryRow(ctx, `SELECT status, expires_at, extensions FROM holds WHERE id::text = $1 AND user_id = $2 FOR UPDATE`, id, a.UserID).
			Scan(&status, &exp, &ext)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "active" || !exp.After(s.Now()) {
			return E(http.StatusConflict, "HOLD_EXPIRED", "hold is no longer active")
		}
		if ext >= MaxExtensions {
			return E(http.StatusConflict, "EXTENSION_LIMIT", "hold can be extended at most twice")
		}
		newExp := s.Now().Add(s.Cfg.HoldTTL)
		if _, err := tx.Exec(ctx, `UPDATE holds SET expires_at = $2, extensions = extensions + 1 WHERE id::text = $1`, id, newExp); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE show_seats SET held_until = $2 WHERE hold_id::text = $1 AND status = 'held'`, id, newExp)
		return err
	})
	if err != nil {
		return Hold{}, err
	}
	return s.GetHold(ctx, a, id)
}

// ReleaseHold frees the seats of a cart immediately.
func (s *Service) ReleaseHold(ctx context.Context, a Actor, id string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE holds SET status = 'released' WHERE id::text = $1 AND user_id = $2 AND status = 'active'`, id, a.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `UPDATE show_seats SET status = 'free', hold_id = NULL, held_until = NULL WHERE hold_id::text = $1 AND status = 'held'`, id)
		return err
	})
}

// ExpireHolds frees seats of expired holds in batches. SKIP LOCKED lets
// several API replicas sweep concurrently without waiting on each other or
// on checkouts in progress. Reads already treat expired holds as free, so
// the sweeper only keeps the table tidy.
func (s *Service) ExpireHolds(ctx context.Context) (int64, error) {
	tag, err := s.DB.Exec(ctx, `
		WITH expired AS (
			SELECT showtime_id, seat_id FROM show_seats
			WHERE status = 'held' AND held_until <= now()
			ORDER BY held_until LIMIT 5000
			FOR UPDATE SKIP LOCKED
		)
		UPDATE show_seats ss SET status = 'free', hold_id = NULL, held_until = NULL
		FROM expired e WHERE ss.showtime_id = e.showtime_id AND ss.seat_id = e.seat_id`)
	if err != nil {
		return 0, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE holds SET status = 'expired' WHERE status = 'active' AND expires_at <= now()`); err != nil {
		return 0, err
	}
	holdsExpired.Add(float64(tag.RowsAffected()))
	return tag.RowsAffected(), nil
}

// RunSweeper calls ExpireHolds periodically until ctx is done.
func (s *Service) RunSweeper(ctx context.Context, every time.Duration, logErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.ExpireHolds(ctx); err != nil && ctx.Err() == nil {
				logErr(err)
			}
		}
	}
}
