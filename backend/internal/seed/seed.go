// Package seed loads deterministic demo data: the same SEED always yields
// the same halls, seat maps, movies and schedule (relative to the seed date).
package seed

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sahnebilet/internal/domain/schedule"
	"sahnebilet/internal/service"
)

// DemoPassword is the password of every demo account (documented in TESTING.md).
const DemoPassword = "Demo1234!"

// LoadUsers is the number of customer accounts for load tests and the simulator.
const LoadUsers = 200

// DemoUsers: one account per role.
var DemoUsers = []struct{ Email, Name, Role string }{
	{"musteri@sahnebilet.test", "Deniz Müşteri", service.RoleCustomer},
	{"gise@sahnebilet.test", "Gökhan Gişe", service.RoleBoxOffice},
	{"amir@sahnebilet.test", "Ayla Salon Amiri", service.RoleHallManager},
	{"programci@sahnebilet.test", "Pelin Programcı", service.RoleProgrammer},
	{"muhasebe@sahnebilet.test", "Murat Muhasebe", service.RoleAccounting},
	{"kapi@sahnebilet.test", "Kaan Giriş Kontrol", service.RoleGate},
}

type movie struct {
	title, kind, distributor string
	duration, minWeekly      int
	minPrime                 int64
	releaseOffsetDays        int
	w1, w2, later            int64
}

var movies = []movie{
	{"Kuzey Yıldızı", "film", "Anadolu Film Dağıtım", 128, 30, 2500, -3, 5500, 5000, 4500},
	{"Son Vapur", "film", "Boğaz Yapım", 104, 25, 2000, -10, 5000, 4500, 4000},
	{"Bozkırda Bir Gece", "film", "Anadolu Film Dağıtım", 142, 20, 2500, 0, 5500, 5000, 4500},
	{"Ada Çocukları (Animasyon)", "film", "Kuzey Işık Dağıtım", 95, 30, 1000, -17, 5000, 4500, 4000},
	{"Kayıp Defter", "film", "Boğaz Yapım", 117, 20, 2000, -24, 5000, 4500, 4000},
	{"Hamlet (Sahne Kaydı)", "play", "Devlet Tiyatroları Arşivi", 165, 8, 3000, -40, 4000, 4000, 4000},
	{"Kırmızı Çizgi", "film", "Kuzey Işık Dağıtım", 110, 25, 2500, -5, 5500, 5000, 4500},
	{"Sessiz Liman", "film", "Anadolu Film Dağıtım", 99, 15, 1500, -31, 5000, 4500, 4000},
}

// IsEmpty reports whether the database has no users yet.
func IsEmpty(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n == 0, err
}

// Run inserts all demo data in one transaction.
func Run(ctx context.Context, pool *pgxpool.Pool, svc *service.Service, seedValue int64, days int, today time.Time) error {
	rng := rand.New(rand.NewPCG(uint64(seedValue), uint64(seedValue)*7919))
	hash, err := svc.HashPassword(DemoPassword)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	for _, u := range DemoUsers {
		if _, err := tx.Exec(ctx, `INSERT INTO users (email, password_hash, full_name, role) VALUES ($1,$2,$3,$4)`, u.Email, hash, u.Name, u.Role); err != nil {
			return err
		}
	}
	for i := 1; i <= LoadUsers; i++ {
		if _, err := tx.Exec(ctx, `INSERT INTO users (email, password_hash, full_name, role) VALUES ($1,$2,$3,'customer')`,
			fmt.Sprintf("yuk%03d@sahnebilet.test", i), hash, fmt.Sprintf("Yük Testi %03d", i)); err != nil {
			return err
		}
	}

	var halls []schedule.Hall
	for h := 1; h <= 10; h++ {
		rows, cols := 8+rng.IntN(6), 12+rng.IntN(7)
		name := fmt.Sprintf("Salon %d", h)
		if h == 1 {
			rows, cols, name = 14, 20, "Salon 1 (Büyük Ekran)"
		}
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO halls (name, rows, cols) VALUES ($1,$2,$3) RETURNING id`, name, rows, cols).Scan(&id); err != nil {
			return err
		}
		halls = append(halls, schedule.Hall{ID: id, Name: name})
		aisle := 0
		if cols >= 16 {
			aisle = cols/2 + 1 // a centre aisle: one blocked column
		}
		for r := 1; r <= rows; r++ {
			for c := 1; c <= cols; c++ {
				kind := "standard"
				switch {
				case c == aisle:
					kind = "blocked"
				case r == rows && (c <= 2 || c > cols-2):
					kind = "accessible"
				case r >= rows-1:
					kind = "vip"
				}
				label := fmt.Sprintf("%c-%d", 'A'+r-1, c)
				if _, err := tx.Exec(ctx, `INSERT INTO seats (hall_id, row_no, col_no, label, kind) VALUES ($1,$2,$3,$4,$5)`, id, r, c, label, kind); err != nil {
					return err
				}
			}
		}
	}

	var sm []schedule.Movie
	for _, m := range movies {
		var id int64
		rel := today.AddDate(0, 0, m.releaseOffsetDays)
		if err := tx.QueryRow(ctx, `INSERT INTO movies (title, kind, duration_min, distributor, release_date, min_weekly_shows, min_prime_bp, share_week1_bp, share_week2_bp, share_later_bp)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
			m.title, m.kind, m.duration, m.distributor, rel.Format("2006-01-02"), m.minWeekly, m.minPrime, m.w1, m.w2, m.later).Scan(&id); err != nil {
			return err
		}
		sm = append(sm, schedule.Movie{ID: id, Title: m.title, DurationMin: m.duration, MinWeeklyShows: m.minWeekly, MinPrimeBP: m.minPrime})
	}

	cfg := schedule.DefaultConfig(today, days)
	plan, err := schedule.Generate(cfg, halls, sm)
	if err != nil {
		return err
	}
	if p := schedule.Validate(cfg, halls, sm, plan); len(p) > 0 {
		return fmt.Errorf("seed schedule invalid: %v", p[0])
	}
	if err := service.InsertShowtimes(ctx, tx, plan, cfg.CleaningMin, service.DefaultBasePrice); err != nil {
		return err
	}

	from, to := today.AddDate(0, 0, -1), today.AddDate(0, 0, 60)
	for _, c := range []struct {
		code      string
		percent   bool
		value     int64
		minTotal  int64
		maxUses   int
	}{
		{"HOSGELDIN10", true, 1000, 0, 100000},
		{"SINEMA50", false, 5000, 30000, 100000},
		{"TEKKULLANIM", true, 2000, 0, 1},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO coupons (code, percent, value, min_total, max_uses, valid_from, valid_to) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			c.code, c.percent, c.value, c.minTotal, c.maxUses, from, to); err != nil {
			return err
		}
	}
	for code, amount := range map[string]int64{"HC-DEMO-500": 50000, "HC-DEMO-100": 10000} {
		if _, err := tx.Exec(ctx, `INSERT INTO gift_cards (code, initial_kurus, balance_kurus) VALUES ($1,$2,$2)`, code, amount); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (action, entity, entity_id, new_value) VALUES ('seed', 'database', $1, $2)`,
		fmt.Sprint(seedValue), map[string]any{"halls": len(halls), "movies": len(sm), "showtimes": len(plan)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Today returns local midnight in loc.
func Today(loc *time.Location) time.Time {
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}
