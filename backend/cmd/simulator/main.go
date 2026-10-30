// Command simulator generates realistic traffic against the API:
// online customers browsing and buying, box office sales, gate scans, and
// a "premiere morning" rush where many customers race for the same show.
//
// Environment:
//
//	API_URL      default http://api:8080
//	SEED         rng seed; the same seed yields the same sequence of choices
//	SIM_RATE     actions per second in steady mode (default 2)
//	SIM_USERS    customer accounts to log in (default 200)
//	SIM_RUSH_EVERY  start a premiere rush every N minutes, 0 = never (default 10)
//	SIM_RUSH_USERS  concurrent customers in a rush (default 2000)
//	SIM_MODE     "steady" (default) or "rush": run one rush, print the
//	             result and exit non-zero if the no-double-sale check fails
package main

import (
	"context"
	"fmt"
	"sort"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"sahnebilet/internal/client"
)

const password = "Demo1234!"

type sim struct {
	c        *client.Client
	log      *slog.Logger
	rng      *rand.Rand
	mu       sync.Mutex
	users    []string // customer tokens
	mgr      string   // hall manager token, for the integrity check
	offers   []offer  // A/B campaign arm of each customer
	box      string
	gate     string
	tickets  []string // QR codes of sold tickets for upcoming shows
	stats    map[string]*atomic.Int64
}

// offer is the campaign arm the API assigned to a customer.
type offer struct {
	arm    string
	coupon string
}

// abandonRate is the simulated share of carts left unpaid per campaign arm.
// It is an assumption of the simulator, so the A/B report has an effect to
// detect; real traffic decides this in production.
var abandonRate = map[string]int{"A": 30, "B": 22}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	base := os.Getenv("API_URL")
	if base == "" {
		base = "http://api:8080"
	}
	seed := uint64(envInt("SEED", 42))
	s := &sim{c: client.New(base), log: log, rng: rand.New(rand.NewPCG(seed, seed^0x5eed)), stats: map[string]*atomic.Int64{}}
	for _, k := range []string{"hold_ok", "hold_conflict", "order_ok", "order_fail", "box_office", "scan", "rush_won", "rush_lost", "errors"} {
		s.stats[k] = &atomic.Int64{}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.c.WaitHealthy(ctx, 3*time.Minute); err != nil {
		log.Error("api not reachable", "error", err.Error())
		os.Exit(1)
	}
	if err := s.login(ctx, envInt("SIM_USERS", 200)); err != nil {
		log.Error("login failed", "error", err.Error())
		os.Exit(1)
	}
	log.Info("simulator started", "seed", seed, "users", len(s.users))
	rushUsers := envInt("SIM_RUSH_USERS", 2000)

	if os.Getenv("SIM_MODE") == "rush" {
		res, err := s.rush(ctx, rushUsers)
		if err != nil {
			log.Error("rush failed", "error", err.Error())
			os.Exit(1)
		}
		fmt.Printf("\npremiere rush: %d customers on showtime %d (%s)\n", res.Customers, res.ShowtimeID, res.Movie)
		fmt.Printf("  carts won %d, seat taken / busy %d, single-seat rule %d, other status %d, no answer (network) %d\n", res.Won, res.Taken, res.Rule, res.Other, res.Network)
		fmt.Printf("  orders %d, seats sold %d, took %s\n", res.Orders, res.SeatsSold, res.Took.Round(time.Millisecond))
		fmt.Printf("  integrity: sold seats %d = valid tickets %d, duplicates %d -> ok=%v\n", res.Integrity.SoldSeats, res.Integrity.ValidTickets, res.Integrity.Duplicates, res.Integrity.OK)
		if !res.Integrity.OK || res.Other > 0 {
			os.Exit(1)
		}
		return
	}

	rate := envInt("SIM_RATE", 2)
	rushEvery := envInt("SIM_RUSH_EVERY", 10)
	tick := time.NewTicker(time.Second / time.Duration(max(rate, 1)))
	report := time.NewTicker(15 * time.Second)
	var rush <-chan time.Time
	if rushEvery > 0 {
		t := time.NewTicker(time.Duration(rushEvery) * time.Minute)
		rush = t.C
		go s.rushAndLog(ctx, rushUsers) // one rush at start, visible right away
	}
	for {
		select {
		case <-ctx.Done():
			log.Info("simulator stopped")
			return
		case <-tick.C:
			go s.step(ctx)
		case <-rush:
			go s.rushAndLog(ctx, rushUsers)
		case <-report.C:
			out := []any{}
			for k, v := range s.stats {
				out = append(out, k, v.Load())
			}
			log.Info("simulator stats", out...)
		}
	}
}

func (s *sim) login(ctx context.Context, n int) error {
	for i := 1; i <= n; i++ {
		t, err := s.c.Login(ctx, fmt.Sprintf("yuk%03d@sahnebilet.test", i), password)
		if err != nil {
			return err
		}
		s.users = append(s.users, t)
		var o struct {
			Variant struct {
				Name       string `json:"name"`
				CouponCode string `json:"coupon_code"`
			} `json:"variant"`
		}
		r, err := s.c.Do(ctx, http.MethodGet, "/api/offers/me", t, nil, "")
		if err == nil && r.Status == 200 && r.Decode(&o) == nil {
			s.offers = append(s.offers, offer{arm: o.Variant.Name, coupon: o.Variant.CouponCode})
		} else {
			s.offers = append(s.offers, offer{})
		}
	}
	var err error
	if s.box, err = s.c.Login(ctx, "gise@sahnebilet.test", password); err != nil {
		return err
	}
	if s.mgr, err = s.c.Login(ctx, "amir@sahnebilet.test", password); err != nil {
		return err
	}
	s.gate, err = s.c.Login(ctx, "kapi@sahnebilet.test", password)
	return err
}

// pick draws from the seeded rng under a lock (steps run concurrently).
func (s *sim) pick(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.IntN(n)
}

func (s *sim) upcoming(ctx context.Context, token string, within time.Duration) ([]client.Showtime, error) {
	from := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	to := time.Now().Add(within).UTC().Format(time.RFC3339)
	r, err := s.c.Do(ctx, http.MethodGet, "/api/showtimes?limit=100&from="+from+"&to="+to, token, nil, "")
	if err != nil || r.Status != 200 {
		return nil, fmt.Errorf("showtimes: %v %s", err, r)
	}
	var p struct {
		Items []client.Showtime `json:"items"`
	}
	return p.Items, r.Decode(&p)
}

func (s *sim) step(ctx context.Context) {
	switch x := s.pick(10); {
	case x < 6:
		i := s.pick(len(s.users))
		s.customer(ctx, s.users[i], "", s.offers[i])
	case x < 8:
		s.customer(ctx, s.box, "box", offer{})
	default:
		s.scan(ctx)
	}
}

// customer: browse, ask for a suggestion, hold, then buy or abandon.
func (s *sim) customer(ctx context.Context, token, kind string, of offer) {
	shows, err := s.upcoming(ctx, token, 8*time.Hour)
	if err != nil || len(shows) == 0 {
		s.stats["errors"].Add(1)
		return
	}
	st := shows[s.pick(len(shows))]
	n := 1 + s.pick(4)
	r, err := s.c.Do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/suggest?n=%d", st.ID, n), token, nil, "")
	if err != nil || r.Status != 200 {
		s.stats["errors"].Add(1)
		return
	}
	var sg struct {
		Items []client.Suggestion `json:"items"`
	}
	if r.Decode(&sg) != nil || len(sg.Items) == 0 {
		return
	}
	choice := sg.Items[s.pick(len(sg.Items))]
	hold, ok := s.hold(ctx, token, st.ID, choice.SeatIDs)
	if !ok {
		return
	}
	rate := 30
	if r, ok := abandonRate[of.arm]; ok {
		rate = r
	}
	if kind != "box" && s.pick(100) < rate {
		return // abandoned cart: the sweeper frees it after the TTL
	}
	if s.order(ctx, token, hold.ID, of.coupon) {
		if kind == "box" {
			s.stats["box_office"].Add(1)
		}
	}
}

func (s *sim) hold(ctx context.Context, token string, showtime int64, seats []int64) (client.Hold, bool) {
	r, err := s.c.Do(ctx, http.MethodPost, "/api/holds", token, map[string]any{"showtime_id": showtime, "seat_ids": seats}, randKey())
	if err != nil {
		s.stats["errors"].Add(1)
		return client.Hold{}, false
	}
	if r.Status != http.StatusCreated {
		s.stats["hold_conflict"].Add(1)
		return client.Hold{}, false
	}
	s.stats["hold_ok"].Add(1)
	var h client.Hold
	return h, r.Decode(&h) == nil
}

func (s *sim) order(ctx context.Context, token, holdID, coupon string) bool {
	body := map[string]any{"hold_id": holdID, "combos": s.pick(3)}
	if coupon != "" && s.pick(10) < 7 {
		body["coupon_code"] = coupon
	}
	r, err := s.c.Do(ctx, http.MethodPost, "/api/orders", token, body, randKey())
	if err == nil && r.Status == http.StatusUnprocessableEntity && body["coupon_code"] != nil {
		delete(body, "coupon_code") // e.g. below the coupon's minimum basket: pay without it
		r, err = s.c.Do(ctx, http.MethodPost, "/api/orders", token, body, randKey())
	}
	if err != nil || r.Status != http.StatusCreated {
		s.stats["order_fail"].Add(1)
		return false
	}
	s.stats["order_ok"].Add(1)
	var o client.Order
	if r.Decode(&o) != nil {
		return true
	}
	for _, l := range o.Lines {
		if l.Kind != "ticket" {
			continue
		}
		q, err := s.c.Do(ctx, http.MethodGet, "/api/tickets/"+l.TicketID+"/qr", token, nil, "")
		if err == nil && q.Status == 200 {
			var v struct {
				QR string `json:"qr"`
			}
			if q.Decode(&v) == nil {
				s.mu.Lock()
				s.tickets = append(s.tickets, v.QR)
				if len(s.tickets) > 2000 {
					s.tickets = s.tickets[len(s.tickets)-2000:]
				}
				s.mu.Unlock()
			}
		}
	}
	return true
}

func (s *sim) scan(ctx context.Context) {
	s.mu.Lock()
	if len(s.tickets) == 0 {
		s.mu.Unlock()
		return
	}
	i := s.rng.IntN(len(s.tickets))
	code := s.tickets[i]
	s.mu.Unlock()
	gate := fmt.Sprintf("KAPI-%d", 1+s.pick(4))
	if _, err := s.c.Do(ctx, http.MethodPost, "/api/gate/scan", s.gate, map[string]string{"gate_id": gate, "code": code}, ""); err != nil {
		s.stats["errors"].Add(1)
		return
	}
	s.stats["scan"].Add(1)
}

// RushResult summarises one premiere rush.
type RushResult struct {
	ShowtimeID int64
	Movie      string
	Customers  int
	Won        int64
	Taken      int64 // 409: seat taken, busy, too many carts
	Rule       int64 // 422: single-seat rule
	Other      int64 // any other HTTP status (must be 0)
	Network    int64 // request never got an HTTP answer (client/OS limits)
	Orders     int64
	SeatsSold  int64
	Took       time.Duration
	Integrity  struct {
		SoldSeats    int  `json:"sold_seats"`
		ValidTickets int  `json:"valid_tickets"`
		Duplicates   int  `json:"duplicate_seats"`
		OK           bool `json:"ok"`
	}
}

func (s *sim) rushAndLog(ctx context.Context, n int) {
	res, err := s.rush(ctx, n)
	if err != nil {
		s.log.Error("rush failed", "error", err.Error())
		return
	}
	s.log.Info("rush finished", "showtime_id", res.ShowtimeID, "customers", res.Customers, "won", res.Won, "taken", res.Taken,
		"rule", res.Rule, "other", res.Other, "network", res.Network, "seats_sold", res.SeatsSold, "took_ms", res.Took.Milliseconds(), "integrity_ok", res.Integrity.OK)
}

// rush is the premiere morning: n customers open the same show at the same
// instant and each grabs 1-4 adjacent seats in the middle rows, where
// everybody wants to sit. Winners pay immediately. Afterwards the hall
// manager's integrity endpoint must report no double sale.
func (s *sim) rush(ctx context.Context, n int) (RushResult, error) {
	var res RushResult
	shows, err := s.upcoming(ctx, s.users[0], 10*24*time.Hour)
	if err != nil {
		return res, err
	}
	var st client.Showtime
	for _, x := range shows {
		if x.Taken == 0 && x.StartsAt.After(time.Now().Add(24*time.Hour)) && x.Capacity > st.Capacity {
			st = x // the biggest untouched show at least a day ahead
		}
	}
	if st.ID == 0 {
		if len(shows) == 0 {
			return res, fmt.Errorf("no upcoming showtime")
		}
		st = shows[s.pick(len(shows))]
	}
	r, err := s.c.Do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/seats", st.ID), s.users[0], nil, "")
	if err != nil || r.Status != 200 {
		return res, fmt.Errorf("seat map: %v %s", err, r)
	}
	var m client.SeatMap
	if err := r.Decode(&m); err != nil {
		return res, err
	}
	// candidate blocks: 1-4 adjacent sellable seats in the middle 40% of rows
	byRow := map[int][]client.Seat{}
	maxRow := 0
	for _, x := range m.Seats {
		if x.Kind == "standard" || x.Kind == "vip" {
			byRow[x.Row] = append(byRow[x.Row], x)
		}
		maxRow = max(maxRow, x.Row)
	}
	var blocks [][]int64
	for row := maxRow * 4 / 10; row <= maxRow*8/10; row++ {
		seats := byRow[row]
		sort.Slice(seats, func(i, j int) bool { return seats[i].Col < seats[j].Col })
		for size := 1; size <= 4; size++ {
			for i := 0; i+size <= len(seats); i++ {
				if seats[i+size-1].Col-seats[i].Col != size-1 {
					continue // the block would cross the aisle
				}
				ids := make([]int64, size)
				for k := range ids {
					ids[k] = seats[i+k].ID
				}
				blocks = append(blocks, ids)
			}
		}
	}
	if len(blocks) == 0 {
		return res, fmt.Errorf("no seats to fight for")
	}
	choice := make([]int, n)
	for i := range choice {
		choice[i] = s.pick(len(blocks)) // seeded: the same SEED replays the same rush
	}
	res.ShowtimeID, res.Movie, res.Customers = st.ID, st.MovieTitle, n
	s.log.Info("rush started", "showtime_id", st.ID, "movie", st.MovieTitle, "customers", n, "candidate_blocks", len(blocks))

	var wg sync.WaitGroup
	gate := make(chan struct{})
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := s.users[i%len(s.users)]
			seats := blocks[choice[i]]
			<-gate
			r, err := s.c.Do(ctx, http.MethodPost, "/api/holds", tok, map[string]any{"showtime_id": st.ID, "seat_ids": seats}, randKey())
			switch {
			case err != nil:
				atomic.AddInt64(&res.Network, 1)
			case r.Status == http.StatusCreated:
				atomic.AddInt64(&res.Won, 1)
				s.stats["rush_won"].Add(1)
				var h client.Hold
				if r.Decode(&h) == nil && s.order(ctx, tok, h.ID, s.offers[i%len(s.offers)].coupon) {
					atomic.AddInt64(&res.Orders, 1)
					atomic.AddInt64(&res.SeatsSold, int64(len(seats)))
				}
			case r.Status == http.StatusConflict:
				atomic.AddInt64(&res.Taken, 1)
				s.stats["rush_lost"].Add(1)
			case r.Status == http.StatusUnprocessableEntity:
				atomic.AddInt64(&res.Rule, 1)
				s.stats["rush_lost"].Add(1)
			default:
				atomic.AddInt64(&res.Other, 1)
			}
		}(i)
	}
	close(gate)
	wg.Wait()
	res.Took = time.Since(start)
	r, err = s.c.Do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/integrity", st.ID), s.mgr, nil, "")
	if err != nil || r.Status != 200 {
		return res, fmt.Errorf("integrity: %v %s", err, r)
	}
	return res, r.Decode(&res.Integrity)
}

func randKey() string {
	return fmt.Sprintf("sim-%d-%d", time.Now().UnixNano(), rand.Int64())
}
