// Command verify exercises every feature (F-1..F-7) and hard task (T-1..T-5)
// end to end against a running, seeded system and prints a PASS/FAIL table.
// It exits non-zero if any check fails.
//
// Environment: API_URL (default http://api:8080), WEB_URL (default
// http://web:8080), DATABASE_URL (used only to fast-forward hold expiry and
// to prove database-level constraints).
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"sahnebilet/internal/client"
	"sahnebilet/internal/domain/experiment"
	"sahnebilet/internal/domain/gate"
)

const pw = "Demo1234!"

type result struct {
	id, name string
	ok       bool
	detail   string
	took     time.Duration
}

type V struct {
	c       *client.Client
	web     string
	db      *pgxpool.Pool
	tok     map[string]string // role -> token
	load    []string          // customer tokens yuk101..yuk200
	results []result
	used    map[int64]bool // showtimes already used by a check
}

func main() {
	api := env("API_URL", "http://api:8080")
	v := &V{c: client.New(api), web: env("WEB_URL", "http://web:8080"), tok: map[string]string{}, used: map[int64]bool{}}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	fmt.Printf("SahneBilet verify against %s\n\n", api)
	if err := v.c.WaitHealthy(ctx, 3*time.Minute); err != nil {
		fmt.Println("FATAL:", err)
		os.Exit(2)
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			v.db = pool
			defer pool.Close()
		}
	}
	for role, email := range map[string]string{
		"customer": "musteri@sahnebilet.test", "box": "gise@sahnebilet.test", "mgr": "amir@sahnebilet.test",
		"prog": "programci@sahnebilet.test", "acct": "muhasebe@sahnebilet.test", "gate": "kapi@sahnebilet.test",
	} {
		t, err := v.c.Login(ctx, email, pw)
		if err != nil {
			fmt.Println("FATAL: demo login failed:", err)
			os.Exit(2)
		}
		v.tok[role] = t
	}
	// Repeated runs: earlier runs leave carts open on purpose (a hold is the
	// state some checks assert). Release them so the 3-carts-per-customer
	// limit does not leak from one run into the next.
	if v.db != nil {
		if _, err := v.db.Exec(ctx, `UPDATE show_seats SET status = 'free', hold_id = NULL, held_until = NULL
			WHERE hold_id IN (SELECT h.id FROM holds h JOIN users u ON u.id = h.user_id
				WHERE h.status = 'active' AND (u.email LIKE 'yuk1%' OR u.email = 'yuk200@sahnebilet.test'))`); err != nil {
			fmt.Println("FATAL: cleanup:", err)
			os.Exit(2)
		}
		if _, err := v.db.Exec(ctx, `UPDATE holds SET status = 'released' WHERE status = 'active' AND user_id IN
			(SELECT id FROM users WHERE email LIKE 'yuk1%' OR email = 'yuk200@sahnebilet.test')`); err != nil {
			fmt.Println("FATAL: cleanup:", err)
			os.Exit(2)
		}
	}
	for i := 101; i <= 200; i++ {
		t, err := v.c.Login(ctx, fmt.Sprintf("yuk%03d@sahnebilet.test", i), pw)
		if err != nil {
			fmt.Println("FATAL: load user login failed:", err)
			os.Exit(2)
		}
		v.load = append(v.load, t)
	}

	checks := []struct {
		id, name string
		fn       func(context.Context) error
	}{
		{"HEALTH", "GET /health reports database ok", v.health},
		{"AUTH-1", "wrong password is rejected with 401", v.authWrongPassword},
		{"AUTH-2", "missing / invalid token is rejected with 401", v.authNoToken},
		{"AUTH-3", "role violations are rejected with 403 on the server", v.authRoles},
		{"AUTH-4", "refresh token rotates and cannot be reused", v.authRefresh},
		{"F-1", "hall and seat map; hall manager blocks a seat", v.f1},
		{"F-2", "movies, programmer adds a film, keyset-paged showtimes", v.f2},
		{"F-3", "online sale and box office sale", v.f3},
		{"F-4", "hold + cart: get, extend (max 2), release", v.f4},
		{"F-5", "price, coupon, gift card in a quote and an order", v.f5},
		{"F-6", "signed QR, gate admits once, rejects reuse and forgery", v.f6},
		{"F-7", "refund and revenue report", v.f7},
		{"T-1", "200 concurrent holds on the same seats: exactly one wins; expiry; no double sale", v.t1},
		{"T-2", "single-seat rule with manager override; group and accessible suggestions", v.t2},
		{"T-3", "10 halls x 2 weeks schedule generated and validated; overlaps impossible", v.t3},
		{"T-4", "dynamic price, coupon limits, partial gift card, refund tax split, distributor share", v.t4},
		{"T-5", "offline scanners: duplicate entry and refunded ticket reconciled on sync", v.t5},
		{"IDEM", "Idempotency-Key replays the same response, rejects a different body", v.idem},
		{"AUDIT", "audit log records critical actions and is append-only", v.audit},
		{"OPS", "metrics, OpenAPI, uniform error body", v.ops},
		{"LIVE", "seat map changes are pushed over SSE (bonus)", v.live},
		{"AB", "campaign A/B: stable assignment, offer in cart, z-test report (bonus)", v.ab},
		{"WEB", "frontend is served and proxies the API", v.webCheck},
	}
	for _, ch := range checks {
		start := time.Now()
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			return ch.fn(ctx)
		}()
		r := result{id: ch.id, name: ch.name, ok: err == nil, took: time.Since(start)}
		if err != nil {
			r.detail = err.Error()
		}
		v.results = append(v.results, r)
		mark := "PASS"
		if !r.ok {
			mark = "FAIL"
		}
		fmt.Printf("  %-6s %-4s %s\n", ch.id, mark, ch.name)
	}
	os.Exit(v.report())
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (v *V) report() int {
	fmt.Println()
	fmt.Println("+--------+------+----------+----------------------------------------------------------------------------+")
	fmt.Println("| ID     | RES  | TIME     | CHECK                                                                      |")
	fmt.Println("+--------+------+----------+----------------------------------------------------------------------------+")
	failed := 0
	for _, r := range v.results {
		res := "PASS"
		if !r.ok {
			res = "FAIL"
			failed++
		}
		name := r.name
		if len(name) > 74 {
			name = name[:74]
		}
		fmt.Printf("| %-6s | %-4s | %7.2fs | %-74s |\n", r.id, res, r.took.Seconds(), name)
		if !r.ok {
			d := r.detail
			if len(d) > 74 {
				d = d[:74]
			}
			fmt.Printf("| %-6s | %-4s | %8s | %-74s |\n", "", "", "", "-> "+d)
		}
	}
	fmt.Println("+--------+------+----------+----------------------------------------------------------------------------+")
	fmt.Printf("%d checks, %d passed, %d failed\n", len(v.results), len(v.results)-failed, failed)
	for _, r := range v.results {
		if !r.ok {
			fmt.Printf("\n%s: %s\n", r.id, r.detail)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// --- helpers

func (v *V) do(ctx context.Context, method, path, tok string, body any) (client.Resp, error) {
	return v.c.Do(ctx, method, path, tok, body, "")
}

var keySeq atomic.Int64

// newKey returns a unique Idempotency-Key. A timestamp alone is not enough:
// on coarse clocks two goroutines get the same value, and the API then
// (correctly) replays the first response for the second request.
func newKey() string {
	return fmt.Sprintf("verify-%d-%d", time.Now().UnixNano(), keySeq.Add(1))
}

func (v *V) doIdem(ctx context.Context, method, path, tok string, body any) (client.Resp, error) {
	return v.c.Do(ctx, method, path, tok, body, newKey())
}

func expect(r client.Resp, err error, status int, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if r.Status != status {
		return fmt.Errorf("%s: want %d, got %s", what, status, r)
	}
	return nil
}

// showtime returns an upcoming showtime starting in [minAhead, maxAhead],
// not used by another check of this run, with no seats taken. Checks that do
// not count seats can accept a partly sold show (busy), e.g. when the
// simulator has already sold into every show of the next day.
func (v *V) showtime(ctx context.Context, minAhead, maxAhead time.Duration) (client.Showtime, error) {
	return v.pickShowtime(ctx, minAhead, maxAhead, false)
}

func (v *V) busyShowtime(ctx context.Context, minAhead, maxAhead time.Duration) (client.Showtime, error) {
	return v.pickShowtime(ctx, minAhead, maxAhead, true)
}

func (v *V) pickShowtime(ctx context.Context, minAhead, maxAhead time.Duration, busy bool) (client.Showtime, error) {
	from := time.Now().Add(minAhead).UTC().Format(time.RFC3339)
	to := time.Now().Add(maxAhead).UTC().Format(time.RFC3339)
	cursor := ""
	for page := 0; page < 20; page++ {
		r, err := v.do(ctx, http.MethodGet, "/api/showtimes?limit=200&from="+from+"&to="+to+"&cursor="+cursor, v.tok["customer"], nil)
		if err := expect(r, err, 200, "list showtimes"); err != nil {
			return client.Showtime{}, err
		}
		var p struct {
			Items      []client.Showtime `json:"items"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := r.Decode(&p); err != nil {
			return client.Showtime{}, err
		}
		for _, st := range p.Items {
			if !v.used[st.ID] && st.Capacity >= 80 && (st.Taken == 0 || (busy && st.Taken*2 < st.Capacity)) {
				v.used[st.ID] = true
				return st, nil
			}
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	return client.Showtime{}, fmt.Errorf("no free showtime between +%s and +%s", minAhead, maxAhead)
}

func (v *V) seats(ctx context.Context, tok string, id int64) (client.SeatMap, error) {
	r, err := v.do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/seats", id), tok, nil)
	if err := expect(r, err, 200, "seat map"); err != nil {
		return client.SeatMap{}, err
	}
	var m client.SeatMap
	return m, r.Decode(&m)
}

// rowStart finds a row whose first k seats (from column 1) are free and
// standard/vip; returns their ids left to right.
func rowStart(m client.SeatMap, k int, skipRows int) ([]int64, error) {
	byRow := map[int][]client.Seat{}
	for _, s := range m.Seats {
		byRow[s.Row] = append(byRow[s.Row], s)
	}
	rows := []int{}
	for r := range byRow {
		rows = append(rows, r)
	}
	sort.Ints(rows)
	for _, r := range rows {
		if skipRows > 0 {
			skipRows--
			continue
		}
		row := byRow[r]
		sort.Slice(row, func(i, j int) bool { return row[i].Col < row[j].Col })
		if len(row) < k+2 {
			continue
		}
		ok := true
		for _, s := range row[:k+2] {
			if s.Status != "free" || (s.Kind != "standard" && s.Kind != "vip") {
				ok = false
			}
		}
		if ok {
			ids := make([]int64, k)
			for i := range ids {
				ids[i] = row[i].ID
			}
			return ids, nil
		}
	}
	return nil, errors.New("no suitable row")
}

func (v *V) hold(ctx context.Context, tok string, st int64, seats []int64) (client.Hold, client.Resp, error) {
	r, err := v.doIdem(ctx, http.MethodPost, "/api/holds", tok, map[string]any{"showtime_id": st, "seat_ids": seats})
	var h client.Hold
	if err == nil && r.Status == 201 {
		err = r.Decode(&h)
	}
	return h, r, err
}

func (v *V) buy(ctx context.Context, tok string, st int64, seats []int64, extra map[string]any) (client.Order, error) {
	h, r, err := v.hold(ctx, tok, st, seats)
	if err := expect(r, err, 201, "hold"); err != nil {
		return client.Order{}, err
	}
	body := map[string]any{"hold_id": h.ID}
	for k, x := range extra {
		body[k] = x
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders", tok, body)
	if err := expect(r, err, 201, "order"); err != nil {
		return client.Order{}, err
	}
	var o client.Order
	return o, r.Decode(&o)
}

func (v *V) qr(ctx context.Context, tok, ticketID string) (string, error) {
	r, err := v.do(ctx, http.MethodGet, "/api/tickets/"+ticketID+"/qr", tok, nil)
	if err := expect(r, err, 200, "ticket qr"); err != nil {
		return "", err
	}
	var q struct {
		QR string `json:"qr"`
	}
	return q.QR, r.Decode(&q)
}

func (v *V) scan(ctx context.Context, gateID, code string) (string, error) {
	r, err := v.do(ctx, http.MethodPost, "/api/gate/scan", v.tok["gate"], map[string]string{"gate_id": gateID, "code": code})
	if err := expect(r, err, 200, "scan"); err != nil {
		return "", err
	}
	var s struct {
		Result string `json:"result"`
	}
	return s.Result, r.Decode(&s)
}

func tickets(o client.Order) []client.OrderLine {
	var out []client.OrderLine
	for _, l := range o.Lines {
		if l.Kind == "ticket" {
			out = append(out, l)
		}
	}
	return out
}

// --- checks

func (v *V) health(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodGet, "/health", "", nil)
	if err := expect(r, err, 200, "health"); err != nil {
		return err
	}
	if !strings.Contains(string(r.Body), `"database":"ok"`) {
		return fmt.Errorf("unexpected body %s", r.Body)
	}
	return nil
}

func (v *V) authWrongPassword(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodPost, "/api/auth/login", "", map[string]string{"email": "musteri@sahnebilet.test", "password": "wrong"})
	if err := expect(r, err, 401, "wrong password"); err != nil {
		return err
	}
	if r.ErrCode() != "INVALID_CREDENTIALS" {
		return fmt.Errorf("code %s", r.ErrCode())
	}
	r, err = v.do(ctx, http.MethodPost, "/api/auth/login", "", map[string]string{"email": "yok@sahnebilet.test", "password": "x"})
	return expect(r, err, 401, "unknown user")
}

func (v *V) authNoToken(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodGet, "/api/me", "", nil)
	if err := expect(r, err, 401, "no token"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodGet, "/api/me", "not-a-jwt", nil)
	if err := expect(r, err, 401, "bad token"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodGet, "/api/me", v.tok["customer"], nil)
	return expect(r, err, 200, "valid token")
}

func (v *V) authRoles(ctx context.Context) error {
	cases := []struct {
		role, method, path string
		body               any
	}{
		{"customer", "GET", "/api/reports/revenue?from=2026-01-01&to=2026-12-31", nil},
		{"customer", "POST", "/api/gate/scan", map[string]string{"gate_id": "X", "code": "x"}},
		{"customer", "PATCH", "/api/seats/1", map[string]string{"kind": "blocked"}},
		{"gate", "POST", "/api/movies", map[string]any{"title": "x"}},
		{"gate", "POST", "/api/holds", map[string]any{}},
		{"box", "POST", "/api/schedule/generate", map[string]any{}},
		{"acct", "POST", "/api/gate/sync", map[string]any{}},
		{"prog", "GET", "/api/audit", nil},
	}
	for _, c := range cases {
		r, err := v.c.Do(ctx, c.method, c.path, v.tok[c.role], c.body, "k")
		if err := expect(r, err, 403, c.role+" "+c.method+" "+c.path); err != nil {
			return err
		}
	}
	return nil
}

func (v *V) authRefresh(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodPost, "/api/auth/login", "", map[string]string{"email": "musteri@sahnebilet.test", "password": pw})
	if err := expect(r, err, 200, "login"); err != nil {
		return err
	}
	var t struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = r.Decode(&t)
	r, err = v.do(ctx, http.MethodPost, "/api/auth/refresh", "", map[string]string{"refresh_token": t.RefreshToken})
	if err := expect(r, err, 200, "refresh"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodPost, "/api/auth/refresh", "", map[string]string{"refresh_token": t.RefreshToken})
	return expect(r, err, 401, "reused refresh token")
}

func (v *V) f1(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodGet, "/api/halls", v.tok["customer"], nil)
	if err := expect(r, err, 200, "halls"); err != nil {
		return err
	}
	var h struct {
		Items []struct {
			ID       int64 `json:"id"`
			Capacity int   `json:"capacity"`
		} `json:"items"`
	}
	_ = r.Decode(&h)
	if len(h.Items) < 10 {
		return fmt.Errorf("want 10 halls, got %d", len(h.Items))
	}
	r, err = v.do(ctx, http.MethodGet, fmt.Sprintf("/api/halls/%d/seats", h.Items[0].ID), v.tok["customer"], nil)
	if err := expect(r, err, 200, "hall seats"); err != nil {
		return err
	}
	var s struct {
		Items []client.Seat `json:"items"`
	}
	_ = r.Decode(&s)
	kinds := map[string]int{}
	for _, x := range s.Items {
		kinds[x.Kind]++
	}
	if kinds["accessible"] == 0 || kinds["vip"] == 0 || kinds["blocked"] == 0 {
		return fmt.Errorf("seat map lacks kinds: %v", kinds)
	}
	// Block a broken seat and put it back. A seat with a ticket or hold in an
	// upcoming show must be refused with 409 SEAT_IN_USE, so walk the halls
	// (last first, front row first) until a seat that is free everywhere.
	refused := 0
	for hi := len(h.Items) - 1; hi >= 0; hi-- {
		r, err = v.do(ctx, http.MethodGet, fmt.Sprintf("/api/halls/%d/seats", h.Items[hi].ID), v.tok["mgr"], nil)
		if err := expect(r, err, 200, "hall seats"); err != nil {
			return err
		}
		_ = r.Decode(&s)
		for _, x := range s.Items {
			if x.Kind != "standard" {
				continue
			}
			r, err = v.do(ctx, http.MethodPatch, fmt.Sprintf("/api/seats/%d", x.ID), v.tok["mgr"], map[string]string{"kind": "blocked"})
			if err == nil && r.Status == 409 && r.ErrCode() == "SEAT_IN_USE" {
				refused++
				continue
			}
			if err := expect(r, err, 200, "block seat"); err != nil {
				return err
			}
			r, err = v.do(ctx, http.MethodPatch, fmt.Sprintf("/api/seats/%d", x.ID), v.tok["mgr"], map[string]string{"kind": "standard"})
			return expect(r, err, 200, "unblock seat")
		}
	}
	return fmt.Errorf("every standard seat has an upcoming ticket (%d refused)", refused)
}

func (v *V) f2(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodGet, "/api/movies", v.tok["customer"], nil)
	if err := expect(r, err, 200, "movies"); err != nil {
		return err
	}
	var m struct {
		Items []any `json:"items"`
	}
	_ = r.Decode(&m)
	if len(m.Items) < 8 {
		return fmt.Errorf("want >= 8 movies, got %d", len(m.Items))
	}
	r, err = v.do(ctx, http.MethodPost, "/api/movies", v.tok["prog"], map[string]any{
		"title": fmt.Sprintf("Doğrulama Filmi %d", time.Now().Unix()), "kind": "film", "duration_min": 100, "distributor": "Test Dağıtım",
		"release_date": time.Now().Format("2006-01-02"), "min_weekly_shows": 0, "min_prime_bp": 0,
		"share_week1_bp": 5000, "share_week2_bp": 4500, "share_later_bp": 4000})
	if err := expect(r, err, 201, "create movie"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodPost, "/api/movies", v.tok["prog"], map[string]any{"title": "", "duration_min": 5})
	if err := expect(r, err, 422, "invalid movie"); err != nil {
		return err
	}
	// keyset pagination: two pages, no overlap, ordered
	r, err = v.do(ctx, http.MethodGet, "/api/showtimes?limit=5", v.tok["customer"], nil)
	if err := expect(r, err, 200, "page 1"); err != nil {
		return err
	}
	var p1, p2 struct {
		Items      []client.Showtime `json:"items"`
		NextCursor string            `json:"next_cursor"`
	}
	_ = r.Decode(&p1)
	if len(p1.Items) != 5 || p1.NextCursor == "" {
		return fmt.Errorf("page 1: %d items, cursor %q", len(p1.Items), p1.NextCursor)
	}
	r, err = v.do(ctx, http.MethodGet, "/api/showtimes?limit=5&cursor="+p1.NextCursor, v.tok["customer"], nil)
	if err := expect(r, err, 200, "page 2"); err != nil {
		return err
	}
	_ = r.Decode(&p2)
	if len(p2.Items) == 0 {
		return errors.New("page 2 empty")
	}
	last := p1.Items[4]
	first := p2.Items[0]
	if first.StartsAt.Before(last.StartsAt) || first.ID == last.ID {
		return errors.New("page 2 does not continue page 1")
	}
	return nil
}

func (v *V) f3(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 2)
	if err != nil {
		return err
	}
	o, err := v.buy(ctx, v.tok["customer"], st.ID, ids, nil)
	if err != nil {
		return err
	}
	if o.Channel != "online" || len(tickets(o)) != 2 || o.Status != "paid" {
		return fmt.Errorf("online order: %+v", o)
	}
	ids, err = rowStart(m, 2, 3)
	if err != nil {
		return err
	}
	o, err = v.buy(ctx, v.tok["box"], st.ID, ids, map[string]any{"tickets": []map[string]any{{"seat_id": ids[0], "category": "student"}}})
	if err != nil {
		return err
	}
	if o.Channel != "box_office" {
		return fmt.Errorf("box office order channel %s", o.Channel)
	}
	m2, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	sold := 0
	for _, s := range m2.Seats {
		if s.Status == "sold" {
			sold++
		}
	}
	if sold != 4 {
		return fmt.Errorf("seat map shows %d sold seats, want 4", sold)
	}
	return nil
}

func (v *V) f4(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 1)
	if err != nil {
		return err
	}
	tok := v.load[0]
	h, r, err := v.hold(ctx, tok, st.ID, ids)
	if err := expect(r, err, 201, "hold"); err != nil {
		return err
	}
	if time.Until(h.ExpiresAt) < 9*time.Minute || time.Until(h.ExpiresAt) > 11*time.Minute {
		return fmt.Errorf("hold ttl %s, want ~10m", time.Until(h.ExpiresAt))
	}
	r, err = v.do(ctx, http.MethodGet, "/api/holds/"+h.ID, tok, nil)
	if err := expect(r, err, 200, "get hold"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodGet, "/api/holds/"+h.ID, v.load[1], nil)
	if err := expect(r, err, 404, "other user's hold"); err != nil {
		return err
	}
	for i := 1; i <= 2; i++ {
		r, err = v.do(ctx, http.MethodPost, "/api/holds/"+h.ID+"/extend", tok, nil)
		if err := expect(r, err, 200, fmt.Sprintf("extend %d", i)); err != nil {
			return err
		}
	}
	r, err = v.do(ctx, http.MethodPost, "/api/holds/"+h.ID+"/extend", tok, nil)
	if err := expect(r, err, 409, "third extend"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodDelete, "/api/holds/"+h.ID, tok, nil)
	if err := expect(r, err, 204, "release"); err != nil {
		return err
	}
	m, err = v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	for _, s := range m.Seats {
		if (s.ID == ids[0] || s.ID == ids[1]) && s.Status != "free" {
			return fmt.Errorf("seat %d still %s after release", s.ID, s.Status)
		}
	}
	return nil
}

func (v *V) f5(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 2)
	if err != nil {
		return err
	}
	tok := v.load[2]
	h, r, err := v.hold(ctx, tok, st.ID, ids)
	if err := expect(r, err, 201, "hold"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodPost, "/api/quote", tok, map[string]any{"hold_id": h.ID, "combos": 1, "coupon_code": "HOSGELDIN10"})
	if err := expect(r, err, 200, "quote"); err != nil {
		return err
	}
	var q struct {
		Subtotal, Discount, Total, Payable, GiftApplied int64
	}
	_ = r.Decode(&q)
	ticketsTotal := 2 * m.Prices["full"]
	if q.Discount != (ticketsTotal+5)/10 || q.Subtotal != ticketsTotal+m.Prices["combo"] {
		return fmt.Errorf("quote %+v, tickets %d", q, ticketsTotal)
	}
	var gc struct {
		Balance int64 `json:"balance_kurus"`
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/gift-cards", v.tok["box"], map[string]any{"amount_kurus": 10000})
	if err := expect(r, err, 201, "issue gift card"); err != nil {
		return err
	}
	var card struct {
		Code string `json:"code"`
	}
	_ = r.Decode(&card)
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders", tok, map[string]any{"hold_id": h.ID, "combos": 1, "coupon_code": "HOSGELDIN10", "gift_card_code": card.Code})
	if err := expect(r, err, 201, "order"); err != nil {
		return err
	}
	var o client.Order
	_ = r.Decode(&o)
	if o.GiftPaid != 10000 || o.Total != q.Total {
		return fmt.Errorf("order total %d gift %d, quote total %d", o.Total, o.GiftPaid, q.Total)
	}
	r, err = v.do(ctx, http.MethodGet, "/api/gift-cards/"+card.Code, tok, nil)
	if err := expect(r, err, 200, "gift card balance"); err != nil {
		return err
	}
	_ = r.Decode(&gc)
	if gc.Balance != 0 {
		return fmt.Errorf("gift card balance %d, want 0", gc.Balance)
	}
	return nil
}

func (v *V) f6(ctx context.Context) error {
	st, err := v.busyShowtime(ctx, 90*time.Minute, 23*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 1)
	if err != nil {
		return err
	}
	o, err := v.buy(ctx, v.load[3], st.ID, ids, nil)
	if err != nil {
		return err
	}
	code, err := v.qr(ctx, v.load[3], tickets(o)[0].TicketID)
	if err != nil {
		return err
	}
	if res, err := v.scan(ctx, "KAPI-1", code); err != nil || res != gate.Accepted {
		return fmt.Errorf("first scan %s %v", res, err)
	}
	if res, err := v.scan(ctx, "KAPI-2", code); err != nil || res != gate.RejectUsed {
		return fmt.Errorf("second scan %s %v", res, err)
	}
	parts := strings.Split(code, ".")
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	if res, err := v.scan(ctx, "KAPI-1", forged); err != nil || res != gate.RejectSignature {
		return fmt.Errorf("forged scan %s %v", res, err)
	}
	r, err := v.do(ctx, http.MethodGet, "/api/tickets/"+tickets(o)[0].TicketID+"/qr", v.load[4], nil)
	return expect(r, err, 404, "someone else's ticket QR")
}

func (v *V) f7(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 1)
	if err != nil {
		return err
	}
	tok := v.load[5]
	o, err := v.buy(ctx, tok, st.ID, ids, map[string]any{"combos": 1})
	if err != nil {
		return err
	}
	t := tickets(o)
	r, err := v.doIdem(ctx, http.MethodPost, "/api/orders/"+o.ID+"/refund", tok, map[string]any{"ticket_ids": []string{t[0].TicketID}})
	if err := expect(r, err, 200, "partial refund"); err != nil {
		return err
	}
	var rf struct {
		Status string `json:"status"`
		Refund struct {
			Amount int64 `json:"amount"`
			Tax    struct{ Net, EntTax, VAT int64 } `json:"tax"`
		} `json:"refund"`
	}
	_ = r.Decode(&rf)
	if rf.Status != "partially_refunded" || rf.Refund.Amount != t[0].Paid {
		return fmt.Errorf("refund %+v, ticket paid %d", rf, t[0].Paid)
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders/"+o.ID+"/refund", v.load[6], map[string]any{})
	if err := expect(r, err, 404, "refund someone else's order"); err != nil {
		return err
	}
	day := st.StartsAt.In(time.FixedZone("TRT", 3*3600)).Format("2006-01-02")
	r, err = v.do(ctx, http.MethodGet, "/api/reports/revenue?from="+day+"&to="+day, v.tok["acct"], nil)
	if err := expect(r, err, 200, "revenue report"); err != nil {
		return err
	}
	var rep struct {
		Totals struct {
			Tickets       int   `json:"tickets"`
			Paid          int64 `json:"paid"`
			Net           int64 `json:"net"`
			EntTax        int64 `json:"ent_tax"`
			VAT           int64 `json:"vat"`
			RefundedCount int   `json:"refunded_count"`
		} `json:"totals"`
	}
	_ = r.Decode(&rep)
	tt := rep.Totals
	if tt.RefundedCount < 1 || tt.Tickets < 1 || tt.Net+tt.EntTax+tt.VAT != tt.Paid {
		return fmt.Errorf("revenue totals %+v", tt)
	}
	return nil
}

func (v *V) t1(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	target, err := rowStart(m, 2, 4)
	if err != nil {
		return err
	}
	// 1) 200 concurrent requests for the same two seats
	const n = 200
	var wins, conflicts, other atomic.Int64
	var winner atomic.Value
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tok := v.load[i%len(v.load)]
			h, r, err := v.hold(ctx, tok, st.ID, target)
			switch {
			case err == nil && r.Status == 201:
				wins.Add(1)
				winner.Store([2]string{tok, h.ID})
			case err == nil && r.Status == 409:
				conflicts.Add(1)
			default:
				other.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || other.Load() != 0 {
		return fmt.Errorf("race: %d winners, %d conflicts, %d other (want 1 / %d / 0)", wins.Load(), conflicts.Load(), other.Load(), n-1)
	}
	// 2) a cart that overlaps a held seat is rejected as a whole
	m, _ = v.seats(ctx, v.tok["customer"], st.ID)
	next, err := rowStart(m, 3, 5)
	if err != nil {
		return err
	}
	_, r, err := v.hold(ctx, v.load[50], st.ID, []int64{target[1], next[0]})
	if err := expect(r, err, 409, "overlapping cart"); err != nil {
		return err
	}
	m, _ = v.seats(ctx, v.tok["customer"], st.ID)
	for _, s := range m.Seats {
		if s.ID == next[0] && s.Status != "free" {
			return errors.New("partial hold leaked: non-overlapping seat got held")
		}
	}
	// 3) expiry: fast-forward the winner's hold, the seats must come back
	w := winner.Load().([2]string)
	if v.db == nil {
		return errors.New("DATABASE_URL not set: cannot fast-forward hold expiry")
	}
	if _, err := v.db.Exec(ctx, `UPDATE holds SET expires_at = now() - interval '1 second' WHERE id::text = $1`, w[1]); err != nil {
		return err
	}
	if _, err := v.db.Exec(ctx, `UPDATE show_seats SET held_until = now() - interval '1 second' WHERE hold_id::text = $1`, w[1]); err != nil {
		return err
	}
	m, _ = v.seats(ctx, v.tok["customer"], st.ID)
	for _, s := range m.Seats {
		if s.ID == target[0] && s.Status != "free" {
			return fmt.Errorf("expired hold still blocks seat (%s)", s.Status)
		}
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders", w[0], map[string]any{"hold_id": w[1]})
	if err := expect(r, err, 409, "order on expired hold"); err != nil {
		return err
	}
	// 4) mini load: 300 buyers on random adjacent pairs, then the invariant
	pairs := [][]int64{}
	byRow := map[int][]client.Seat{}
	for _, s := range m.Seats {
		if s.Kind != "blocked" && s.Kind != "accessible" {
			byRow[s.Row] = append(byRow[s.Row], s)
		}
	}
	for _, row := range byRow {
		sort.Slice(row, func(i, j int) bool { return row[i].Col < row[j].Col })
		for i := 0; i+1 < len(row); i += 2 {
			pairs = append(pairs, []int64{row[i].ID, row[i+1].ID})
		}
	}
	var sold atomic.Int64
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := v.load[i%len(v.load)]
			if _, err := v.buy(ctx, tok, st.ID, pairs[(i*7)%len(pairs)], nil); err == nil {
				sold.Add(1)
			}
		}(i)
	}
	wg.Wait()
	r, err = v.do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/integrity", st.ID), v.tok["mgr"], nil)
	if err := expect(r, err, 200, "integrity"); err != nil {
		return err
	}
	var in struct {
		OK             bool `json:"ok"`
		DuplicateSeats int  `json:"duplicate_seats"`
		SoldSeats      int  `json:"sold_seats"`
		ValidTickets   int  `json:"valid_tickets"`
	}
	_ = r.Decode(&in)
	if !in.OK || in.DuplicateSeats != 0 || in.SoldSeats != int(sold.Load())*2 {
		return fmt.Errorf("integrity %+v, orders %d", in, sold.Load())
	}
	// 5) the database itself refuses a second ticket for a sold seat
	if v.db != nil {
		_, err := v.db.Exec(ctx, `INSERT INTO order_lines (order_id, kind, ticket_id, showtime_id, seat_id, category, gross_kurus, discount_kurus, paid_kurus, gift_kurus, net_kurus, ent_tax_kurus, vat_kurus)
			SELECT order_id, kind, gen_random_uuid(), showtime_id, seat_id, category, gross_kurus, discount_kurus, paid_kurus, gift_kurus, net_kurus, ent_tax_kurus, vat_kurus
			FROM order_lines WHERE showtime_id = $1 AND kind = 'ticket' AND status = 'valid' LIMIT 1`, st.ID)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23505" {
			return fmt.Errorf("duplicate ticket insert was not refused by the unique index: %v", err)
		}
	}
	return nil
}

func (v *V) t2(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	row, err := rowStart(m, 3, 1)
	if err != nil {
		return err
	}
	// seats 2+3 leave seat 1 alone at the wall
	_, r, err := v.hold(ctx, v.load[10], st.ID, row[1:3])
	if err := expect(r, err, 422, "orphan-creating hold"); err != nil {
		return err
	}
	if r.ErrCode() != "SINGLE_SEAT_RULE" {
		return fmt.Errorf("code %s", r.ErrCode())
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/holds", v.load[10], map[string]any{"showtime_id": st.ID, "seat_ids": row[1:3], "override_single_seat": true})
	if err := expect(r, err, 403, "customer override"); err != nil {
		return err
	}
	_, r, err = v.hold(ctx, v.load[10], st.ID, row[0:2])
	if err := expect(r, err, 201, "rule-conforming hold"); err != nil {
		return err
	}
	row2, err := rowStart(m, 3, 3)
	if err != nil {
		return err
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/holds", v.tok["mgr"], map[string]any{"showtime_id": st.ID, "seat_ids": row2[1:3], "override_single_seat": true})
	if err := expect(r, err, 201, "manager override"); err != nil {
		return err
	}
	acc := map[int64]bool{}
	for _, s := range m.Seats {
		if s.Kind == "accessible" {
			acc[s.ID] = true
		}
	}
	r, err = v.do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/suggest?n=4", st.ID), v.tok["customer"], nil)
	if err := expect(r, err, 200, "suggest"); err != nil {
		return err
	}
	var sg struct {
		Items []client.Suggestion `json:"items"`
	}
	_ = r.Decode(&sg)
	if len(sg.Items) == 0 {
		return errors.New("no group suggestion")
	}
	for _, s := range sg.Items {
		if len(s.SeatIDs) != 4 {
			return fmt.Errorf("suggestion size %d", len(s.SeatIDs))
		}
		for _, id := range s.SeatIDs {
			if acc[id] {
				return errors.New("accessible seat offered to a normal group")
			}
		}
	}
	// the best suggestion must be sellable without override
	_, r, err = v.hold(ctx, v.load[11], st.ID, sg.Items[0].SeatIDs)
	if err := expect(r, err, 201, "hold best suggestion"); err != nil {
		return err
	}
	r, err = v.do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/suggest?n=2&accessible=true", st.ID), v.tok["customer"], nil)
	if err := expect(r, err, 200, "accessible suggest"); err != nil {
		return err
	}
	_ = r.Decode(&sg)
	if len(sg.Items) == 0 {
		return errors.New("no accessible suggestion")
	}
	for _, s := range sg.Items {
		has := false
		for _, id := range s.SeatIDs {
			has = has || acc[id]
		}
		if !has {
			return errors.New("accessible suggestion without accessible seat")
		}
	}
	return nil
}

func (v *V) t3(ctx context.Context) error {
	trt := time.FixedZone("TRT", 3*3600)
	// the seeded period starts on the day of the first showtime
	r, err := v.do(ctx, http.MethodGet, "/api/showtimes?limit=1&from=2000-01-01T00:00:00Z&to=2100-01-01T00:00:00Z", v.tok["prog"], nil)
	if err := expect(r, err, 200, "first showtime"); err != nil {
		return err
	}
	var first struct {
		Items []client.Showtime `json:"items"`
	}
	if err := r.Decode(&first); err != nil || len(first.Items) == 0 {
		return errors.New("no showtimes")
	}
	today := first.Items[0].StartsAt.In(trt).Format("2006-01-02")
	r, err = v.do(ctx, http.MethodGet, "/api/schedule/validate?from="+today+"&days=14", v.tok["prog"], nil)
	if err := expect(r, err, 200, "validate seeded schedule"); err != nil {
		return err
	}
	var res struct {
		Shows       int `json:"shows"`
		Improvement *struct {
			Before int64 `json:"objective_before"`
			After  int64 `json:"objective_after"`
		} `json:"improvement"`
		Problems []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"problems"`
	}
	_ = r.Decode(&res)
	// a manually added movie (F-2) has no contract minimum, so it cannot break the check
	if len(res.Problems) > 0 || res.Shows < 10*14*4 {
		return fmt.Errorf("seeded schedule: %d shows, problems %+v", res.Shows, res.Problems)
	}
	future := time.Now().In(trt).AddDate(0, 0, 40).Format("2006-01-02")
	r, err = v.do(ctx, http.MethodPost, "/api/schedule/generate", v.tok["prog"], map[string]any{"from": future, "days": 14, "save": false})
	if err := expect(r, err, 200, "generate"); err != nil {
		return err
	}
	_ = r.Decode(&res)
	if len(res.Problems) > 0 || res.Shows < 10*14*4 {
		return fmt.Errorf("generated: %d shows, problems %+v", res.Shows, res.Problems)
	}
	if res.Improvement == nil || res.Improvement.After < res.Improvement.Before {
		return fmt.Errorf("improvement round missing or made the plan worse: %+v", res.Improvement)
	}
	r, err = v.do(ctx, http.MethodPost, "/api/schedule/generate", v.tok["prog"], map[string]any{"from": today, "days": 7, "save": true})
	if err := expect(r, err, 409, "save over existing period"); err != nil {
		return err
	}
	if v.db != nil {
		_, err := v.db.Exec(ctx, `INSERT INTO showtimes (movie_id, hall_id, starts_at, ends_at, busy_until, base_price_kurus, capacity)
			SELECT movie_id, hall_id, starts_at + interval '10 minutes', ends_at + interval '10 minutes', busy_until + interval '10 minutes', base_price_kurus, capacity
			FROM showtimes ORDER BY id LIMIT 1`)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23P01" {
			return fmt.Errorf("overlapping showtime was not refused by the exclusion constraint: %v", err)
		}
	}
	return nil
}

func (v *V) t4(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	if m.Prices["student"] >= m.Prices["full"] || m.Prices["full"] <= 0 {
		return fmt.Errorf("prices %v", m.Prices)
	}
	// single-use coupon: first order ok, second refused; full refund restores it
	ids, _ := rowStart(m, 2, 1)
	tok := v.load[20]
	var coupon string
	r, err := v.do(ctx, http.MethodPost, "/api/coupons", v.tok["acct"], map[string]any{
		"code": fmt.Sprintf("TEK%d", time.Now().UnixNano()%1e9), "percent": true, "value": 2000, "min_total": 0, "max_uses": 1,
		"valid_from": time.Now().Add(-time.Hour), "valid_to": time.Now().Add(24 * time.Hour)})
	if err := expect(r, err, 201, "create coupon"); err != nil {
		return err
	}
	var cp struct {
		Code string `json:"code"`
	}
	_ = r.Decode(&cp)
	coupon = cp.Code
	o1, err := v.buy(ctx, tok, st.ID, ids, map[string]any{"coupon_code": coupon, "combos": 1})
	if err != nil {
		return err
	}
	// coupon only discounts tickets
	for _, l := range o1.Lines {
		if l.Kind == "combo" && l.Paid != 12000 {
			return fmt.Errorf("combo was discounted: %+v", l)
		}
		if l.Tax.Net+l.Tax.EntTax+l.Tax.VAT != l.Paid {
			return fmt.Errorf("tax split does not sum: %+v", l)
		}
		if l.Kind == "combo" && l.Tax.EntTax != 0 {
			return errors.New("combo carries entertainment tax")
		}
	}
	ids2, _ := rowStart(m, 2, 3)
	h, r, err := v.hold(ctx, v.load[21], st.ID, ids2)
	if err := expect(r, err, 201, "hold 2"); err != nil {
		return err
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders", v.load[21], map[string]any{"hold_id": h.ID, "coupon_code": coupon})
	if err := expect(r, err, 409, "exhausted coupon"); err != nil {
		return err
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders/"+o1.ID+"/refund", tok, map[string]any{})
	if err := expect(r, err, 200, "full refund"); err != nil {
		return err
	}
	var rf struct {
		Status string `json:"status"`
		Refund struct {
			Amount        int64 `json:"amount"`
			RestoreCoupon bool  `json:"restore_coupon"`
			Tax           struct {
				Net    int64 `json:"net"`
				EntTax int64 `json:"ent_tax"`
				VAT    int64 `json:"vat"`
			} `json:"tax"`
		} `json:"refund"`
	}
	_ = r.Decode(&rf)
	if rf.Status != "refunded" || rf.Refund.Amount != o1.Total || !rf.Refund.RestoreCoupon ||
		rf.Refund.Tax.Net+rf.Refund.Tax.EntTax+rf.Refund.Tax.VAT != rf.Refund.Amount {
		return fmt.Errorf("full refund %+v, order total %d", rf, o1.Total)
	}
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders", v.load[21], map[string]any{"hold_id": h.ID, "coupon_code": coupon})
	if err := expect(r, err, 201, "coupon usable again after full refund"); err != nil {
		return err
	}
	// gift card used partially, refund goes back to the card
	r, err = v.doIdem(ctx, http.MethodPost, "/api/gift-cards", v.tok["box"], map[string]any{"amount_kurus": 50000})
	if err := expect(r, err, 201, "gift card"); err != nil {
		return err
	}
	var card struct {
		Code    string `json:"code"`
		Balance int64  `json:"balance_kurus"`
	}
	_ = r.Decode(&card)
	ids3, _ := rowStart(m, 2, 5)
	o3, err := v.buy(ctx, v.load[22], st.ID, ids3, map[string]any{"gift_card_code": card.Code})
	if err != nil {
		return err
	}
	if o3.GiftPaid != o3.Total || o3.Total > 50000 {
		return fmt.Errorf("gift paid %d of %d", o3.GiftPaid, o3.Total)
	}
	r, _ = v.do(ctx, http.MethodGet, "/api/gift-cards/"+card.Code, v.load[22], nil)
	_ = r.Decode(&card)
	if card.Balance != 50000-o3.Total {
		return fmt.Errorf("balance %d after spending %d", card.Balance, o3.Total)
	}
	t := tickets(o3)
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders/"+o3.ID+"/refund", v.load[22], map[string]any{"ticket_ids": []string{t[0].TicketID}})
	if err := expect(r, err, 200, "partial refund to gift card"); err != nil {
		return err
	}
	r, _ = v.do(ctx, http.MethodGet, "/api/gift-cards/"+card.Code, v.load[22], nil)
	_ = r.Decode(&card)
	if card.Balance != 50000-o3.Total+t[0].GiftPaid {
		return fmt.Errorf("balance %d after refund of %d", card.Balance, t[0].GiftPaid)
	}
	// distributor share report
	day := st.StartsAt.In(time.FixedZone("TRT", 3*3600)).Format("2006-01-02")
	r, err = v.do(ctx, http.MethodGet, "/api/reports/distributor?from="+day+"&to="+day, v.tok["acct"], nil)
	if err := expect(r, err, 200, "distributor report"); err != nil {
		return err
	}
	var dr struct {
		Items []struct {
			NetRevenue  int64 `json:"net_revenue"`
			ShareBP     int64 `json:"share_bp"`
			ShareAmount int64 `json:"share_amount"`
		} `json:"items"`
	}
	_ = r.Decode(&dr)
	if len(dr.Items) == 0 {
		return errors.New("distributor report empty")
	}
	for _, x := range dr.Items {
		want := (x.NetRevenue*x.ShareBP + 5000) / 10000
		if x.ShareAmount != want || x.ShareBP == 0 {
			return fmt.Errorf("share %+v, want amount %d", x, want)
		}
	}
	return nil
}

func (v *V) t5(ctx context.Context) error {
	st, err := v.busyShowtime(ctx, 90*time.Minute, 23*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 2)
	if err != nil {
		return err
	}
	tok := v.load[30]
	o, err := v.buy(ctx, tok, st.ID, ids, nil)
	if err != nil {
		return err
	}
	t := tickets(o)
	codeA, err := v.qr(ctx, tok, t[0].TicketID)
	if err != nil {
		return err
	}
	codeB, err := v.qr(ctx, tok, t[1].TicketID)
	if err != nil {
		return err
	}
	// scanners download the public key + state, then the network goes down
	r, err := v.do(ctx, http.MethodGet, "/api/gate/state", v.tok["gate"], nil)
	if err := expect(r, err, 200, "gate state"); err != nil {
		return err
	}
	var gs struct {
		PublicKey string `json:"public_key"`
	}
	_ = r.Decode(&gs)
	pk, err := base64.StdEncoding.DecodeString(gs.PublicKey)
	if err != nil || len(pk) != ed25519.PublicKeySize {
		return errors.New("bad public key")
	}
	g1 := gate.NewScanner("KAPI-A", ed25519.PublicKey(pk))
	g2 := gate.NewScanner("KAPI-B", ed25519.PublicKey(pk))
	now := time.Now()
	if res, _ := g1.Scan(codeA, now); res != gate.Accepted {
		return fmt.Errorf("offline G1 scan A: %s", res)
	}
	if res, _ := g1.Scan(codeA, now.Add(time.Second)); res != gate.RejectUsed {
		return fmt.Errorf("offline G1 rescan A: %s", res)
	}
	if res, _ := g2.Scan(codeA, now.Add(30*time.Second)); res != gate.Accepted {
		return fmt.Errorf("offline G2 scan A (it cannot know): %s", res)
	}
	// ticket B is refunded at the box office while the gates are offline
	r, err = v.doIdem(ctx, http.MethodPost, "/api/orders/"+o.ID+"/refund", v.tok["box"], map[string]any{"ticket_ids": []string{t[1].TicketID}})
	if err := expect(r, err, 200, "refund B"); err != nil {
		return err
	}
	if res, _ := g2.Scan(codeB, now.Add(40*time.Second)); res != gate.Accepted {
		return fmt.Errorf("offline G2 scan B: %s", res)
	}
	if res, _ := g1.Scan("SB1.e30."+base64.RawURLEncoding.EncodeToString(make([]byte, 64)), now); res != gate.RejectSignature {
		return fmt.Errorf("forged offline: %s", res)
	}
	sync := func(sc *gate.Scanner) (int, []string, error) {
		r, err := v.do(ctx, http.MethodPost, "/api/gate/sync", v.tok["gate"], map[string]any{"gate_id": sc.GateID, "scans": sc.Pending()})
		if err := expect(r, err, 200, "sync "+sc.GateID); err != nil {
			return 0, nil, err
		}
		var st struct {
			gate.SyncState
			Conflicts []gate.Conflict `json:"conflicts"`
		}
		if err := r.Decode(&st); err != nil {
			return 0, nil, err
		}
		pending := len(sc.Pending())
		sc.ApplySync(st.SyncState)
		var kinds []string
		for _, c := range st.Conflicts {
			kinds = append(kinds, c.Kind)
		}
		return pending, kinds, nil
	}
	if _, k, err := sync(g1); err != nil || len(k) != 0 {
		return fmt.Errorf("G1 sync conflicts %v %v", k, err)
	}
	g2events := g2.Pending()
	_, k, err := sync(g2)
	if err != nil {
		return err
	}
	sort.Strings(k)
	if strings.Join(k, ",") != gate.ConflictRefundedEntry+","+gate.ConflictDuplicate {
		return fmt.Errorf("G2 sync conflicts %v, want duplicate + refunded", k)
	}
	// replaying the same upload must not create new conflicts
	r, err = v.do(ctx, http.MethodPost, "/api/gate/sync", v.tok["gate"], map[string]any{"gate_id": "KAPI-B", "scans": g2events})
	if err := expect(r, err, 200, "re-sync"); err != nil {
		return err
	}
	var again struct {
		Conflicts []gate.Conflict `json:"conflicts"`
	}
	_ = r.Decode(&again)
	dups := 0
	for _, c := range again.Conflicts {
		if c.Kind == gate.ConflictDuplicate && c.GateID == "KAPI-B" {
			dups++
		}
	}
	if dups != 0 {
		return fmt.Errorf("re-sync produced %d new duplicate conflicts", dups)
	}
	// after sync both gates reject A, and B is reported as refunded online
	if res, _ := g2.Scan(codeA, now.Add(time.Minute)); res != gate.RejectUsed {
		return fmt.Errorf("G2 after sync scan A: %s", res)
	}
	if res, _ := g1.Scan(codeB, now.Add(time.Minute)); res != gate.RejectRefunded {
		return fmt.Errorf("G1 after sync scan B: %s", res)
	}
	if res, err := v.scan(ctx, "KAPI-C", codeB); err != nil || res != gate.RejectRefunded {
		return fmt.Errorf("online scan of refunded B: %s %v", res, err)
	}
	r, err = v.do(ctx, http.MethodGet, "/api/gate/conflicts", v.tok["mgr"], nil)
	return expect(r, err, 200, "conflict list for hall manager")
}

func (v *V) idem(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 1)
	if err != nil {
		return err
	}
	tok := v.load[40]
	key := newKey()
	body := map[string]any{"showtime_id": st.ID, "seat_ids": ids}
	r1, err := v.c.Do(ctx, http.MethodPost, "/api/holds", tok, body, key)
	if err := expect(r1, err, 201, "first"); err != nil {
		return err
	}
	r2, err := v.c.Do(ctx, http.MethodPost, "/api/holds", tok, body, key)
	if err := expect(r2, err, 201, "replay"); err != nil {
		return err
	}
	if string(r1.Body) != string(r2.Body) || r2.Header.Get("Idempotent-Replayed") != "true" {
		return errors.New("replay returned a different response")
	}
	r3, err := v.c.Do(ctx, http.MethodPost, "/api/holds", tok, map[string]any{"showtime_id": st.ID, "seat_ids": ids[:1]}, key)
	if err := expect(r3, err, 422, "same key, different body"); err != nil {
		return err
	}
	r4, err := v.do(ctx, http.MethodPost, "/api/holds", tok, body)
	return expect(r4, err, 400, "missing key")
}

func (v *V) audit(ctx context.Context) error {
	want := []string{"order.create", "order.refund", "seat.kind", "hold.single_seat_override", "gate.sync_conflicts"}
	seen := map[string]bool{}
	before := int64(0)
	for page := 0; page < 100; page++ {
		path := "/api/audit?limit=200"
		if before > 0 {
			path += fmt.Sprintf("&before=%d", before)
		}
		r, err := v.do(ctx, http.MethodGet, path, v.tok["acct"], nil)
		if err := expect(r, err, 200, "audit"); err != nil {
			return err
		}
		var a struct {
			Items []struct {
				Action string `json:"action"`
			} `json:"items"`
			NextBefore int64 `json:"next_before"`
		}
		_ = r.Decode(&a)
		for _, x := range a.Items {
			seen[x.Action] = true
		}
		all := true
		for _, w := range want {
			all = all && seen[w]
		}
		if all || a.NextBefore == 0 {
			break
		}
		before = a.NextBefore
	}
	for _, w := range want {
		if !seen[w] {
			return fmt.Errorf("audit log has no %s", w)
		}
	}
	if v.db != nil {
		_, err := v.db.Exec(ctx, `UPDATE audit_log SET action = 'tampered' WHERE id = (SELECT max(id) FROM audit_log)`)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			return fmt.Errorf("audit log update was not refused: %v", err)
		}
	}
	return nil
}

func (v *V) ops(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodGet, "/metrics", "", nil)
	if err := expect(r, err, 200, "metrics"); err != nil {
		return err
	}
	for _, m := range []string{"http_requests_total", "sahnebilet_holds_created_total", "sahnebilet_tickets_sold_total"} {
		if !strings.Contains(string(r.Body), m) {
			return fmt.Errorf("metric %s missing", m)
		}
	}
	r, err = v.do(ctx, http.MethodGet, "/api/openapi.yaml", "", nil)
	if err := expect(r, err, 200, "openapi"); err != nil {
		return err
	}
	if !strings.Contains(string(r.Body), "/api/holds") {
		return errors.New("openapi spec does not document /api/holds")
	}
	r, err = v.do(ctx, http.MethodGet, "/api/nope", v.tok["customer"], nil)
	if err := expect(r, err, 404, "unknown route"); err != nil {
		return err
	}
	if r.ErrCode() == "" {
		return errors.New("error body has no error.code")
	}
	r, err = v.do(ctx, http.MethodGet, "/api/showtimes/abc/seats", v.tok["customer"], nil)
	if err := expect(r, err, 400, "bad id"); err != nil {
		return err
	}
	return nil
}

func (v *V) webCheck(ctx context.Context) error {
	wc := client.New(v.web)
	r, err := wc.Do(ctx, http.MethodGet, "/", "", nil, "")
	if err := expect(r, err, 200, "frontend index"); err != nil {
		return err
	}
	if !strings.Contains(string(r.Body), "SahneBilet") {
		return errors.New("index does not look like the SahneBilet app")
	}
	r, err = wc.Do(ctx, http.MethodGet, "/api/me", v.tok["customer"], nil, "")
	return expect(r, err, 200, "frontend proxies /api")
}

// live opens the SSE stream of a showtime, holds seats in it and expects a
// "seats" event within a few seconds.
func (v *V) live(ctx context.Context) error {
	st, err := v.showtime(ctx, 3*time.Hour, 14*24*time.Hour)
	if err != nil {
		return err
	}
	m, err := v.seats(ctx, v.tok["customer"], st.ID)
	if err != nil {
		return err
	}
	ids, err := rowStart(m, 2, 1)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, fmt.Sprintf("%s/api/showtimes/%d/events", v.c.Base, st.ID), nil)
	req.Header.Set("Authorization", "Bearer "+v.tok["customer"])
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	events := make(chan string, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			if n > 0 {
				events <- string(buf[:n])
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()
	waitFor := func(substr string) error {
		for {
			select {
			case chunk, ok := <-events:
				if !ok {
					return errors.New("stream closed before " + substr)
				}
				if strings.Contains(chunk, substr) {
					return nil
				}
			case <-sctx.Done():
				return errors.New("timeout waiting for " + substr)
			}
		}
	}
	if err := waitFor("event: ready"); err != nil {
		return err
	}
	start := time.Now()
	if _, r, err := v.hold(ctx, v.load[41], st.ID, ids); err != nil || r.Status != 201 {
		return fmt.Errorf("hold: %v %s", err, r)
	}
	if err := waitFor("event: seats"); err != nil {
		return err
	}
	if d := time.Since(start); d > 3*time.Second {
		return fmt.Errorf("event took %s", d)
	}
	r, err := v.do(ctx, http.MethodGet, fmt.Sprintf("/api/showtimes/%d/events", st.ID), "", nil)
	return expect(r, err, 401, "stream without token")
}

// ab checks the campaign experiment: every customer gets the arm the pure
// assignment function predicts, the report has both arms with consistent
// numbers, and only accounting can read it.
func (v *V) ab(ctx context.Context) error {
	r, err := v.do(ctx, http.MethodGet, "/api/me", v.load[0], nil)
	if err := expect(r, err, 200, "me"); err != nil {
		return err
	}
	var me struct {
		UserID int64 `json:"user_id"`
	}
	_ = r.Decode(&me)
	r, err = v.do(ctx, http.MethodGet, "/api/offers/me", v.load[0], nil)
	if err := expect(r, err, 200, "offer"); err != nil {
		return err
	}
	var of struct {
		Experiment string `json:"experiment"`
		Variant    struct {
			Name       string `json:"name"`
			CouponCode string `json:"coupon_code"`
		} `json:"variant"`
	}
	_ = r.Decode(&of)
	want := []string{"A", "B"}[experiment.Assign(of.Experiment, me.UserID, 2)]
	if of.Variant.Name != want || of.Variant.CouponCode == "" {
		return fmt.Errorf("offer %+v, expected arm %s", of, want)
	}
	day := time.Now().In(time.FixedZone("TRT", 3*3600)).Format("2006-01-02")
	r, err = v.do(ctx, http.MethodGet, "/api/reports/ab?from="+day+"&to="+day, v.tok["acct"], nil)
	if err := expect(r, err, 200, "ab report"); err != nil {
		return err
	}
	var rep struct {
		Arms []struct {
			Exposed        int     `json:"exposed"`
			Converted      int     `json:"converted"`
			Carts          int     `json:"carts"`
			PaidCarts      int     `json:"paid_carts"`
			Orders         int     `json:"orders"`
			ConversionRate float64 `json:"conversion_rate"`
		} `json:"arms"`
		Comparison struct {
			PValue float64 `json:"p_value"`
		} `json:"comparison"`
	}
	_ = r.Decode(&rep)
	if len(rep.Arms) != 2 || rep.Arms[0].Exposed+rep.Arms[1].Exposed == 0 {
		return fmt.Errorf("report arms %+v", rep.Arms)
	}
	for _, a := range rep.Arms {
		if a.Converted > a.Exposed || a.Converted > a.Orders || a.PaidCarts > a.Carts || a.Carts < a.Exposed || a.ConversionRate < 0 || a.ConversionRate > 1 {
			return fmt.Errorf("inconsistent arm %+v", a)
		}
	}
	if rep.Comparison.PValue < 0 || rep.Comparison.PValue > 1 {
		return fmt.Errorf("p-value %v", rep.Comparison.PValue)
	}
	r, err = v.do(ctx, http.MethodGet, "/api/reports/ab?from="+day+"&to="+day, v.tok["customer"], nil)
	return expect(r, err, 403, "customer reads the A/B report")
}
