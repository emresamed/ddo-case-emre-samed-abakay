package service

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"sahnebilet/internal/domain/schedule"
)

// DefaultBasePrice is the list price before dynamic pricing (200,00 TL).
const DefaultBasePrice int64 = 20000

type ScheduleResult struct {
	From     string             `json:"from"`
	Days     int                `json:"days"`
	Shows    int                `json:"shows"`
	Problems []schedule.Problem `json:"problems"`
	Saved    bool               `json:"saved"`
	Plan     []schedule.Show    `json:"plan,omitempty"`
}

func (s *Service) scheduleInputs(ctx context.Context, q querier) ([]schedule.Hall, []schedule.Movie, error) {
	rows, err := q.Query(ctx, `SELECT id, name FROM halls ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	halls, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (schedule.Hall, error) {
		var h schedule.Hall
		return h, r.Scan(&h.ID, &h.Name)
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = q.Query(ctx, `SELECT id, title, duration_min, min_weekly_shows, min_prime_bp FROM movies ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	movies, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (schedule.Movie, error) {
		var m schedule.Movie
		return m, r.Scan(&m.ID, &m.Title, &m.DurationMin, &m.MinWeeklyShows, &m.MinPrimeBP)
	})
	return halls, movies, err
}

func (s *Service) parseDay(day string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", day, s.Cfg.TimeZone)
	if err != nil {
		return t, invalid("from must be YYYY-MM-DD")
	}
	return t, nil
}

// GenerateSchedule plans [from, from+days) for all halls. With save=true the
// plan is written, but only into an empty period: existing showtimes may
// already have sold tickets and are never replaced silently.
func (s *Service) GenerateSchedule(ctx context.Context, a Actor, from string, days int, save bool) (ScheduleResult, error) {
	start, err := s.parseDay(from)
	if err != nil {
		return ScheduleResult{}, err
	}
	if days < 1 || days > 28 {
		return ScheduleResult{}, invalid("days must be between 1 and 28")
	}
	halls, movies, err := s.scheduleInputs(ctx, s.DB)
	if err != nil {
		return ScheduleResult{}, err
	}
	cfg := schedule.DefaultConfig(start, days)
	plan, err := schedule.Generate(cfg, halls, movies)
	if err != nil {
		return ScheduleResult{}, invalid(err.Error())
	}
	res := ScheduleResult{From: from, Days: days, Shows: len(plan), Problems: schedule.Validate(cfg, halls, movies, plan)}
	if res.Problems == nil {
		res.Problems = []schedule.Problem{}
	}
	if !save {
		res.Plan = plan
		return res, nil
	}
	if len(res.Problems) > 0 {
		return res, E(http.StatusUnprocessableEntity, "SCHEDULE_INVALID", "generated plan violates constraints, not saved")
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM showtimes WHERE starts_at >= $1 AND starts_at < $2`, start, start.AddDate(0, 0, days)).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return E(http.StatusConflict, "PERIOD_NOT_EMPTY", "period already has showtimes")
		}
		if err := InsertShowtimes(ctx, tx, plan, cfg.CleaningMin, DefaultBasePrice); err != nil {
			return err
		}
		return audit(ctx, tx, a.UserID, "schedule.generate", "schedule", from, nil, map[string]any{"days": days, "shows": len(plan)})
	})
	res.Saved = err == nil
	return res, err
}

// InsertShowtimes writes showtimes and their per-seat inventory rows.
func InsertShowtimes(ctx context.Context, tx pgx.Tx, plan []schedule.Show, cleaningMin int, basePrice int64) error {
	for _, sh := range plan {
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO showtimes (movie_id, hall_id, starts_at, ends_at, busy_until, base_price_kurus, capacity)
			VALUES ($1,$2,$3,$4,$5,$6,(SELECT count(*) FROM seats WHERE hall_id = $2 AND kind <> 'blocked')) RETURNING id`,
			sh.MovieID, sh.HallID, sh.Start, sh.End, sh.End.Add(time.Duration(cleaningMin)*time.Minute), basePrice).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO show_seats (showtime_id, seat_id) SELECT $1, id FROM seats WHERE hall_id = $2`, id, sh.HallID); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSchedule checks the stored showtimes of a period.
func (s *Service) ValidateSchedule(ctx context.Context, from string, days int) (ScheduleResult, error) {
	start, err := s.parseDay(from)
	if err != nil {
		return ScheduleResult{}, err
	}
	if days < 1 || days > 28 {
		return ScheduleResult{}, invalid("days must be between 1 and 28")
	}
	halls, movies, err := s.scheduleInputs(ctx, s.DB)
	if err != nil {
		return ScheduleResult{}, err
	}
	rows, err := s.DB.Query(ctx, `SELECT hall_id, movie_id, starts_at, ends_at FROM showtimes WHERE starts_at >= $1 AND starts_at < $2`,
		start, start.AddDate(0, 0, days))
	if err != nil {
		return ScheduleResult{}, err
	}
	shows, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (schedule.Show, error) {
		var sh schedule.Show
		err := r.Scan(&sh.HallID, &sh.MovieID, &sh.Start, &sh.End)
		sh.Start, sh.End = sh.Start.In(s.Cfg.TimeZone), sh.End.In(s.Cfg.TimeZone)
		return sh, err
	})
	if err != nil {
		return ScheduleResult{}, err
	}
	p := schedule.Validate(schedule.DefaultConfig(start, days), halls, movies, shows)
	if p == nil {
		p = []schedule.Problem{}
	}
	return ScheduleResult{From: from, Days: days, Shows: len(shows), Problems: p}, nil
}
