package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"sahnebilet/internal/domain/pricing"
	"sahnebilet/internal/domain/seating"
)

type Hall struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Rows     int    `json:"rows"`
	Cols     int    `json:"cols"`
	Capacity int    `json:"capacity"`
}

type Seat struct {
	ID     int64  `json:"id"`
	Row    int    `json:"row"`
	Col    int    `json:"col"`
	Label  string `json:"label"`
	Kind   string `json:"kind"`
	Status string `json:"status,omitempty"` // free | held | sold (showtime view)
	Mine   bool   `json:"mine,omitempty"`   // held by the caller
}

func (s *Service) ListHalls(ctx context.Context) ([]Hall, error) {
	rows, err := s.DB.Query(ctx, `SELECT h.id, h.name, h.rows, h.cols,
		(SELECT count(*) FROM seats WHERE hall_id = h.id AND kind <> 'blocked') FROM halls h ORDER BY h.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hall, error) {
		var h Hall
		return h, r.Scan(&h.ID, &h.Name, &h.Rows, &h.Cols, &h.Capacity)
	})
}

// HallSeats returns the static seat map of a hall.
func (s *Service) HallSeats(ctx context.Context, hallID int64) ([]Seat, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, row_no, col_no, label, kind FROM seats WHERE hall_id = $1 ORDER BY row_no, col_no`, hallID)
	if err != nil {
		return nil, err
	}
	seats, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Seat, error) {
		var st Seat
		return st, r.Scan(&st.ID, &st.Row, &st.Col, &st.Label, &st.Kind)
	})
	if err == nil && len(seats) == 0 {
		return nil, ErrNotFound
	}
	return seats, err
}

// UpdateSeatKind lets the hall manager block a broken seat or mark it
// accessible. Seats with an active hold or a sold ticket cannot be blocked.
func (s *Service) UpdateSeatKind(ctx context.Context, a Actor, seatID int64, kind string) (Seat, error) {
	switch kind {
	case seating.KindStandard, seating.KindVIP, seating.KindAccessible, seating.KindBlocked:
	default:
		return Seat{}, invalid("kind must be standard, vip, accessible or blocked")
	}
	var out Seat
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var old string
		err := tx.QueryRow(ctx, `SELECT kind FROM seats WHERE id = $1 FOR UPDATE`, seatID).Scan(&old)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if kind == seating.KindBlocked {
			var busy bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM show_seats ss JOIN showtimes st ON st.id = ss.showtime_id
				WHERE ss.seat_id = $1 AND ss.status <> 'free' AND st.ends_at > now())`, seatID).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return E(http.StatusConflict, "SEAT_IN_USE", "seat has an active hold or a sold ticket for an upcoming show")
			}
		}
		if err := tx.QueryRow(ctx, `UPDATE seats SET kind = $2 WHERE id = $1 RETURNING id, row_no, col_no, label, kind`, seatID, kind).
			Scan(&out.ID, &out.Row, &out.Col, &out.Label, &out.Kind); err != nil {
			return err
		}
		if kind == seating.KindBlocked || old == seating.KindBlocked {
			if _, err := tx.Exec(ctx, `UPDATE showtimes st SET capacity = (SELECT count(*) FROM seats WHERE hall_id = st.hall_id AND kind <> 'blocked')
				WHERE st.hall_id = (SELECT hall_id FROM seats WHERE id = $1) AND st.ends_at > now()`, seatID); err != nil {
				return err
			}
		}
		return audit(ctx, tx, a.UserID, "seat.kind", "seat", strconv.FormatInt(seatID, 10), map[string]string{"kind": old}, map[string]string{"kind": kind})
	})
	return out, err
}

type Movie struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Kind           string `json:"kind"`
	DurationMin    int    `json:"duration_min"`
	Distributor    string `json:"distributor"`
	ReleaseDate    string `json:"release_date"`
	MinWeeklyShows int    `json:"min_weekly_shows"`
	MinPrimeBP     int64  `json:"min_prime_bp"`
	ShareWeek1BP   int64  `json:"share_week1_bp"`
	ShareWeek2BP   int64  `json:"share_week2_bp"`
	ShareLaterBP   int64  `json:"share_later_bp"`
}

const movieCols = `id, title, kind, duration_min, distributor, to_char(release_date,'YYYY-MM-DD'), min_weekly_shows, min_prime_bp, share_week1_bp, share_week2_bp, share_later_bp`

func scanMovie(r pgx.Row) (Movie, error) {
	var m Movie
	err := r.Scan(&m.ID, &m.Title, &m.Kind, &m.DurationMin, &m.Distributor, &m.ReleaseDate, &m.MinWeeklyShows, &m.MinPrimeBP, &m.ShareWeek1BP, &m.ShareWeek2BP, &m.ShareLaterBP)
	return m, err
}

func (s *Service) ListMovies(ctx context.Context) ([]Movie, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+movieCols+` FROM movies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Movie, error) { return scanMovie(r) })
}

// CreateMovie adds a film or play with its contract terms.
func (s *Service) CreateMovie(ctx context.Context, a Actor, m Movie) (Movie, error) {
	m.Title = strings.TrimSpace(m.Title)
	if m.Kind == "" {
		m.Kind = "film"
	}
	switch {
	case m.Title == "" || len(m.Title) > 200:
		return Movie{}, invalid("title is required (max 200 chars)")
	case m.Kind != "film" && m.Kind != "play":
		return Movie{}, invalid("kind must be film or play")
	case m.DurationMin < 30 || m.DurationMin > 300:
		return Movie{}, invalid("duration_min must be between 30 and 300")
	case m.Distributor == "":
		return Movie{}, invalid("distributor is required")
	case m.MinPrimeBP < 0 || m.MinPrimeBP > 10000 || m.ShareWeek1BP < 0 || m.ShareWeek1BP > 10000 ||
		m.ShareWeek2BP < 0 || m.ShareWeek2BP > 10000 || m.ShareLaterBP < 0 || m.ShareLaterBP > 10000:
		return Movie{}, invalid("basis point fields must be between 0 and 10000")
	case m.MinWeeklyShows < 0:
		return Movie{}, invalid("min_weekly_shows must be >= 0")
	}
	if _, err := time.Parse("2006-01-02", m.ReleaseDate); err != nil {
		return Movie{}, invalid("release_date must be YYYY-MM-DD")
	}
	var out Movie
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanMovie(tx.QueryRow(ctx, `INSERT INTO movies (title, kind, duration_min, distributor, release_date, min_weekly_shows, min_prime_bp, share_week1_bp, share_week2_bp, share_later_bp)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+movieCols,
			m.Title, m.Kind, m.DurationMin, m.Distributor, m.ReleaseDate, m.MinWeeklyShows, m.MinPrimeBP, m.ShareWeek1BP, m.ShareWeek2BP, m.ShareLaterBP))
		if err != nil {
			return err
		}
		return audit(ctx, tx, a.UserID, "movie.create", "movie", strconv.FormatInt(out.ID, 10), nil, out)
	})
	return out, err
}

type Showtime struct {
	ID          int64     `json:"id"`
	MovieID     int64     `json:"movie_id"`
	MovieTitle  string    `json:"movie_title"`
	HallID      int64     `json:"hall_id"`
	HallName    string    `json:"hall_name"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	BasePrice   int64     `json:"base_price_kurus"`
	Capacity    int       `json:"capacity"`
	Taken       int       `json:"taken"`
}

// Page is a keyset-paginated list.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", invalid("bad cursor")
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", invalid("bad cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", invalid("bad cursor")
	}
	return t, id, nil
}

// ShowtimeFilter filters ListShowtimes.
type ShowtimeFilter struct {
	From, To time.Time
	MovieID  int64
	HallID   int64
	Cursor   string
	Limit    int
}

const showtimeSelect = `SELECT st.id, st.movie_id, m.title, st.hall_id, h.name, st.starts_at, st.ends_at, st.base_price_kurus, st.capacity,
	(SELECT count(*) FROM show_seats ss WHERE ss.showtime_id = st.id AND (ss.status = 'sold' OR (ss.status = 'held' AND ss.held_until > now())))
	FROM showtimes st JOIN movies m ON m.id = st.movie_id JOIN halls h ON h.id = st.hall_id`

func scanShowtime(r pgx.Row) (Showtime, error) {
	var st Showtime
	err := r.Scan(&st.ID, &st.MovieID, &st.MovieTitle, &st.HallID, &st.HallName, &st.StartsAt, &st.EndsAt, &st.BasePrice, &st.Capacity, &st.Taken)
	return st, err
}

// ListShowtimes pages by (starts_at, id) using the showtimes_starts_idx index.
func (s *Service) ListShowtimes(ctx context.Context, f ShowtimeFilter) (Page[Showtime], error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.To.IsZero() {
		f.To = f.From.AddDate(0, 0, 14)
	}
	args := []any{f.From, f.To}
	where := []string{"st.starts_at >= $1", "st.starts_at < $2"}
	if f.MovieID > 0 {
		args = append(args, f.MovieID)
		where = append(where, fmt.Sprintf("st.movie_id = $%d", len(args)))
	}
	if f.HallID > 0 {
		args = append(args, f.HallID)
		where = append(where, fmt.Sprintf("st.hall_id = $%d", len(args)))
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page[Showtime]{}, err
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return Page[Showtime]{}, invalid("bad cursor")
		}
		args = append(args, t, n)
		where = append(where, fmt.Sprintf("(st.starts_at, st.id) > ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, f.Limit+1)
	q := showtimeSelect + " WHERE " + strings.Join(where, " AND ") + fmt.Sprintf(" ORDER BY st.starts_at, st.id LIMIT $%d", len(args))
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return Page[Showtime]{}, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Showtime, error) { return scanShowtime(r) })
	if err != nil {
		return Page[Showtime]{}, err
	}
	p := Page[Showtime]{Items: items}
	if len(items) > f.Limit {
		p.Items = items[:f.Limit]
		last := p.Items[f.Limit-1]
		p.NextCursor = encodeCursor(last.StartsAt, strconv.FormatInt(last.ID, 10))
	}
	if p.Items == nil {
		p.Items = []Showtime{}
	}
	return p, nil
}

func (s *Service) GetShowtime(ctx context.Context, id int64) (Showtime, error) {
	st, err := scanShowtime(s.DB.QueryRow(ctx, showtimeSelect+" WHERE st.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// SeatMap is the live seat state of a showtime with current prices.
type SeatMap struct {
	Showtime Showtime         `json:"showtime"`
	Seats    []Seat           `json:"seats"`
	Prices   map[string]int64 `json:"prices"`
}

func (s *Service) loadSeatStates(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, showtimeID int64, userID int64) ([]Seat, error) {
	rows, err := q.Query(ctx, `SELECT s.id, s.row_no, s.col_no, s.label, s.kind,
			CASE WHEN ss.status = 'held' AND ss.held_until <= now() THEN 'free' ELSE ss.status END,
			COALESCE(ss.status = 'held' AND ss.held_until > now() AND h.user_id = $2, false)
		FROM show_seats ss JOIN seats s ON s.id = ss.seat_id LEFT JOIN holds h ON h.id = ss.hold_id
		WHERE ss.showtime_id = $1 ORDER BY s.row_no, s.col_no`, showtimeID, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Seat, error) {
		var st Seat
		return st, r.Scan(&st.ID, &st.Row, &st.Col, &st.Label, &st.Kind, &st.Status, &st.Mine)
	})
}

func (s *Service) ShowtimeSeats(ctx context.Context, a Actor, id int64) (SeatMap, error) {
	st, err := s.GetShowtime(ctx, id)
	if err != nil {
		return SeatMap{}, err
	}
	seats, err := s.loadSeatStates(ctx, s.DB, id, a.UserID)
	if err != nil {
		return SeatMap{}, err
	}
	prices := map[string]int64{}
	for _, c := range []string{pricing.CategoryFull, pricing.CategoryStudent} {
		p, _, err := pricing.TicketPrice(s.showContext(st), c)
		if err != nil {
			return SeatMap{}, err
		}
		prices[c] = p
	}
	prices["combo"] = pricing.ComboPrice
	return SeatMap{Showtime: st, Seats: seats, Prices: prices}, nil
}

func (s *Service) showContext(st Showtime) pricing.ShowContext {
	return pricing.ShowContext{Start: st.StartsAt.In(s.Cfg.TimeZone), BasePrice: st.BasePrice, Capacity: st.Capacity, Taken: st.Taken}
}

func toDomain(seats []Seat) []seating.Seat {
	out := make([]seating.Seat, len(seats))
	for i, st := range seats {
		out[i] = seating.Seat{ID: st.ID, Row: st.Row, Col: st.Col, Kind: st.Kind, Taken: st.Status != "free"}
	}
	return out
}

// Suggest returns the best adjacent seat blocks for a group of n.
func (s *Service) Suggest(ctx context.Context, a Actor, showtimeID int64, n int, accessible bool) ([]seating.Suggestion, error) {
	if _, err := s.GetShowtime(ctx, showtimeID); err != nil {
		return nil, err
	}
	seats, err := s.loadSeatStates(ctx, s.DB, showtimeID, a.UserID)
	if err != nil {
		return nil, err
	}
	sg, err := seating.SuggestGroup(toDomain(seats), n, seating.Options{Accessible: accessible, Limit: 3})
	if err != nil {
		return nil, invalid(err.Error())
	}
	if sg == nil {
		sg = []seating.Suggestion{}
	}
	return sg, nil
}
