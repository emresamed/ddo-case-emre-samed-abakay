package service_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"sahnebilet/internal/config"
	"sahnebilet/internal/db"
	"sahnebilet/internal/domain/gate"
	"sahnebilet/internal/domain/ticket"
	"sahnebilet/internal/seed"
	"sahnebilet/internal/service"
)

// These tests need a real PostgreSQL (TEST_DATABASE_URL). The database is
// wiped and seeded once per run. CI provides it as a service container.

var (
	svc  *service.Service
	pool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Println("TEST_DATABASE_URL not set: skipping service integration tests")
		os.Exit(0)
	}
	ctx := context.Background()
	var err error
	pool, err = db.Connect(ctx, url)
	if err != nil {
		panic(err)
	}
	loc, _ := time.LoadLocation("Europe/Istanbul")
	cfg := config.Config{
		JWTSecret: []byte("test-secret-test-secret-test-secret!"), AccessTTL: time.Minute, RefreshTTL: time.Hour,
		HoldTTL: 10 * time.Minute, TicketSeed: "c2FobmViaWxldC1kZW1vLXRpY2tldC1zZWVkLTMyYiE=", TimeZone: loc,
		BcryptCost: 4, ScheduleDays: 14, SeedValue: 42,
	}
	svc, err = service.New(pool, cfg)
	if err != nil {
		panic(err)
	}
	if err := db.Reset(ctx, pool); err != nil {
		panic(err)
	}
	if err := seed.Run(ctx, pool, svc, 42, 14, seed.Today(loc)); err != nil {
		panic(err)
	}
	lctx, stopListener := context.WithCancel(ctx)
	go svc.RunSeatListener(lctx, func(err error) { fmt.Println("listener:", err) })
	time.Sleep(200 * time.Millisecond) // let LISTEN register
	code := m.Run()
	stopListener()
	pool.Close()
	os.Exit(code)
}

var usedShows sync.Map

// freshShow returns a showtime starting tomorrow or later, unused by other tests.
func freshShow(t *testing.T) int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id FROM showtimes WHERE starts_at > now() + interval '1 day' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, loaded := usedShows.LoadOrStore(id, true); !loaded {
			return id
		}
	}
	t.Fatal("no showtime left")
	return 0
}

// rowSeats returns the seat ids of row r (columns 1..n) of a showtime's hall.
func rowSeats(t *testing.T, show int64, r, n int) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT s.id FROM seats s JOIN showtimes st ON st.hall_id = s.hall_id
		WHERE st.id = $1 AND s.row_no = $2 AND s.col_no <= $3 ORDER BY s.col_no`, show, r, n)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	if len(out) != n {
		t.Fatalf("row %d has %d seats", r, len(out))
	}
	return out
}

func customer(t *testing.T, i int) service.Actor {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, fmt.Sprintf("yuk%03d@sahnebilet.test", i)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return service.Actor{UserID: id, Role: service.RoleCustomer}
}

func staff(t *testing.T, email, role string) service.Actor {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return service.Actor{UserID: id, Role: role}
}

func code(err error) string {
	var e *service.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestConcurrentHolds_ExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	seats := rowSeats(t, show, 5, 2)
	const n = 200
	var wins, taken atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	actors := make([]service.Actor, n)
	for i := range actors {
		actors[i] = customer(t, 1+i%200)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := actors[i]
			<-start
			_, err := svc.CreateHold(ctx, a, service.HoldRequest{ShowtimeID: show, SeatIDs: seats})
			switch code(err) {
			case "":
				wins.Add(1)
			case "SEAT_TAKEN", "BUSY", "TOO_MANY_HOLDS":
				taken.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("want exactly 1 winner, got %d (rejected %d)", wins.Load(), taken.Load())
	}
}

func TestHold_MultiSeatIsAtomic(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	seats := rowSeats(t, show, 3, 4)
	if _, err := svc.CreateHold(ctx, customer(t, 1), service.HoldRequest{ShowtimeID: show, SeatIDs: seats[2:4]}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateHold(ctx, customer(t, 2), service.HoldRequest{ShowtimeID: show, SeatIDs: seats[0:3]})
	if code(err) != "SEAT_TAKEN" {
		t.Fatalf("want SEAT_TAKEN, got %v", err)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM show_seats WHERE showtime_id = $1 AND seat_id = $2`, show, seats[0]).Scan(&status)
	if status != "free" {
		t.Fatalf("seat 1 leaked into a failed cart: %s", status)
	}
}

func TestHold_ExpiryFreesSeatsAndBlocksCheckout(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	seats := rowSeats(t, show, 4, 2)
	a := customer(t, 3)
	h, err := svc.CreateHold(ctx, a, service.HoldRequest{ShowtimeID: show, SeatIDs: seats})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pool.Exec(ctx, `UPDATE holds SET expires_at = now() - interval '1 second' WHERE id::text = $1`, h.ID)
	_, _ = pool.Exec(ctx, `UPDATE show_seats SET held_until = now() - interval '1 second' WHERE hold_id::text = $1`, h.ID)
	if _, err := svc.ExpireHolds(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM show_seats WHERE showtime_id = $1 AND seat_id = $2`, show, seats[0]).Scan(&status)
	if status != "free" {
		t.Fatalf("sweeper did not free the seat: %s", status)
	}
	if _, err := svc.CreateOrder(ctx, a, service.OrderRequest{HoldID: h.ID}); code(err) != "HOLD_EXPIRED" {
		t.Fatalf("want HOLD_EXPIRED, got %v", err)
	}
	if _, err := svc.CreateHold(ctx, customer(t, 4), service.HoldRequest{ShowtimeID: show, SeatIDs: seats}); err != nil {
		t.Fatalf("expired seats must be holdable again: %v", err)
	}
}

func TestHold_ExtendLimit(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	a := customer(t, 5)
	h, err := svc.CreateHold(ctx, a, service.HoldRequest{ShowtimeID: show, SeatIDs: rowSeats(t, show, 2, 2)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.ExtendHold(ctx, a, h.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.ExtendHold(ctx, a, h.ID); code(err) != "EXTENSION_LIMIT" {
		t.Fatalf("want EXTENSION_LIMIT, got %v", err)
	}
	if _, err := svc.ExtendHold(ctx, customer(t, 6), h.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("other user must not see the hold: %v", err)
	}
}

func TestSingleSeatRule_ConcurrentCartsCannotCreateOrphan(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 5; round++ {
		show := freshShow(t)
		s := rowSeats(t, show, 6, 5)
		// A takes 1-2, B takes 4-5: together they would orphan seat 3
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		actors := []service.Actor{customer(t, 100+2*round), customer(t, 101+2*round)}
		for i, pair := range [][]int64{s[0:2], s[3:5]} {
			wg.Add(1)
			go func(i int, pair []int64) {
				defer wg.Done()
				<-start
				_, errs[i] = svc.CreateHold(ctx, actors[i], service.HoldRequest{ShowtimeID: show, SeatIDs: pair})
			}(i, pair)
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, e := range errs {
			if e == nil {
				ok++
			} else if code(e) != "SINGLE_SEAT_RULE" {
				t.Fatalf("unexpected %v", e)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d carts succeeded, want 1 (%v)", round, ok, errs)
		}
	}
}

func TestSingleSeatRule_ManagerOverrideIsAudited(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	s := rowSeats(t, show, 7, 3)
	if _, err := svc.CreateHold(ctx, customer(t, 12), service.HoldRequest{ShowtimeID: show, SeatIDs: s[1:3], OverrideSingleSeat: true}); code(err) != "OVERRIDE_NOT_ALLOWED" {
		t.Fatalf("customer override: %v", err)
	}
	mgr := staff(t, "amir@sahnebilet.test", service.RoleHallManager)
	h, err := svc.CreateHold(ctx, mgr, service.HoldRequest{ShowtimeID: show, SeatIDs: s[1:3], OverrideSingleSeat: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'hold.single_seat_override' AND entity_id = $1`, h.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("override not audited")
	}
}

func buy(t *testing.T, a service.Actor, show int64, seats []int64, req service.OrderRequest) (service.Order, error) {
	t.Helper()
	h, err := svc.CreateHold(context.Background(), a, service.HoldRequest{ShowtimeID: show, SeatIDs: seats})
	if err != nil {
		return service.Order{}, err
	}
	req.HoldID = h.ID
	return svc.CreateOrder(context.Background(), a, req)
}

func TestCoupon_SingleUseUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < 8; i++ {
		seats := rowSeats(t, show, 1+i, 2)
		a := customer(t, 20+i)
		h, err := svc.CreateHold(ctx, a, service.HoldRequest{ShowtimeID: show, SeatIDs: seats})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.CreateOrder(ctx, a, service.OrderRequest{HoldID: h.ID, CouponCode: "TEKKULLANIM"})
			if err == nil {
				ok.Add(1)
			} else if c := code(err); c != "COUPON_EXHAUSTED" && c != "BUSY" {
				t.Errorf("unexpected %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("single-use coupon used %d times", ok.Load())
	}
}

func TestGiftCard_NeverOverdrawnUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	box := staff(t, "gise@sahnebilet.test", service.RoleBoxOffice)
	card, err := svc.IssueGiftCard(ctx, box, 30000)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var spent atomic.Int64
	for i := 0; i < 6; i++ {
		seats := rowSeats(t, show, 2+i, 2)
		a := customer(t, 40+i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := buy(t, a, show, seats, service.OrderRequest{GiftCardCode: card.Code})
			if err == nil {
				spent.Add(o.GiftPaid)
			}
		}()
	}
	wg.Wait()
	g, err := svc.GetGiftCard(ctx, card.Code)
	if err != nil {
		t.Fatal(err)
	}
	if g.Balance < 0 || g.Balance+spent.Load() != 30000 {
		t.Fatalf("balance %d + spent %d != 30000", g.Balance, spent.Load())
	}
}

func TestOrderRefund_TaxAndGiftCardRestored(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	a := customer(t, 60)
	o, err := buy(t, a, show, rowSeats(t, show, 3, 2), service.OrderRequest{Combos: 1, GiftCardCode: "HC-DEMO-100", CouponCode: "HOSGELDIN10"})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range o.Lines {
		if l.Tax.Net+l.Tax.EntTax+l.Tax.VAT != l.Paid {
			t.Fatalf("line tax does not sum: %+v", l)
		}
	}
	before, _ := svc.GetGiftCard(ctx, "HC-DEMO-100")
	res, err := svc.Refund(ctx, a, o.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "refunded" || res.Refund.Amount != o.Total || !res.Refund.RestoreCoupon {
		t.Fatalf("refund %+v vs total %d", res, o.Total)
	}
	after, _ := svc.GetGiftCard(ctx, "HC-DEMO-100")
	if after.Balance-before.Balance != o.GiftPaid {
		t.Fatalf("gift card got %d back, paid %d", after.Balance-before.Balance, o.GiftPaid)
	}
	if _, err := svc.Refund(ctx, a, o.ID, nil); code(err) != "NOTHING_TO_REFUND" {
		t.Fatalf("double refund: %v", err)
	}
	// seats are sellable again and the unique index allows a new ticket
	if _, err := buy(t, customer(t, 61), show, rowSeats(t, show, 3, 2), service.OrderRequest{}); err != nil {
		t.Fatalf("rebuy after refund: %v", err)
	}
}

func TestRefund_UsedTicketAndForeignOrder(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	a := customer(t, 62)
	o, err := buy(t, a, show, rowSeats(t, show, 4, 2), service.OrderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refund(ctx, customer(t, 63), o.ID, nil); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("foreign refund: %v", err)
	}
	_, _ = pool.Exec(ctx, `UPDATE order_lines SET status = 'used' WHERE order_id::text = $1 AND kind = 'ticket'`, o.ID)
	if _, err := svc.Refund(ctx, a, o.ID, nil); code(err) != "TICKET_USED" {
		t.Fatalf("used ticket refund: %v", err)
	}
}

func TestGate_OnlineScanOnceAndOfflineSyncConflicts(t *testing.T) {
	ctx := context.Background()
	var show int64
	if err := pool.QueryRow(ctx, `SELECT id FROM showtimes WHERE starts_at > now() + interval '2 hours' AND starts_at < now() + interval '20 hours' ORDER BY starts_at DESC LIMIT 1`).Scan(&show); err != nil {
		t.Skip("no showtime in the QR validity window right now")
	}
	usedShows.Store(show, true)
	a := customer(t, 70)
	o, err := buy(t, a, show, rowSeats(t, show, 8, 2), service.OrderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	gateUser := staff(t, "kapi@sahnebilet.test", service.RoleGate)
	t1, t2 := o.Lines[0].TicketID, o.Lines[1].TicketID
	pass1, err := svc.TicketQR(ctx, a, t1)
	if err != nil {
		t.Fatal(err)
	}
	qr1 := pass1.QR
	if pass1.Seat == "" || pass1.MovieTitle == "" || !pass1.ValidFrom.Before(pass1.StartsAt) {
		t.Fatalf("pass details missing: %+v", pass1)
	}
	if _, err := ticket.Verify(svc.PublicKey(), qr1, time.Now()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svc.Scan(ctx, gateUser, fmt.Sprintf("G%d", i), qr1)
			if err == nil && r.Result == gate.Accepted {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("ticket admitted %d times online", accepted.Load())
	}
	// offline: G-X admitted t1 earlier than the online scan, and t2 after it was refunded
	if _, err := svc.Refund(ctx, staff(t, "gise@sahnebilet.test", service.RoleBoxOffice), o.ID, []string{t2}); err != nil {
		t.Fatal(err)
	}
	ev := []gate.Event{
		{TicketID: t1, ScannedAt: time.Now().Add(-time.Hour)},
		{TicketID: t2, ScannedAt: time.Now()},
	}
	st, err := svc.Sync(ctx, gateUser, service.SyncRequest{GateID: "G-X", Scans: ev})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, c := range st.Conflicts {
		kinds[c.Kind]++
	}
	if kinds[gate.ConflictDuplicate] != 1 || kinds[gate.ConflictRefundedEntry] != 1 {
		t.Fatalf("conflicts %+v", st.Conflicts)
	}
	var usedBy string
	_ = pool.QueryRow(ctx, `SELECT used_gate FROM order_lines WHERE ticket_id::text = $1`, t1).Scan(&usedBy)
	if usedBy != "G-X" {
		t.Fatalf("earliest scan must win, used_gate=%s", usedBy)
	}
	st, err = svc.Sync(ctx, gateUser, service.SyncRequest{GateID: "G-X", Scans: ev})
	if err != nil || len(st.Conflicts) != 0 {
		t.Fatalf("replayed sync must be a no-op: %v %+v", err, st.Conflicts)
	}
}

func TestDatabaseConstraints(t *testing.T) {
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO showtimes (movie_id, hall_id, starts_at, ends_at, busy_until, base_price_kurus, capacity)
		SELECT movie_id, hall_id, starts_at + interval '5 minutes', ends_at, busy_until, base_price_kurus, capacity FROM showtimes ORDER BY id LIMIT 1`)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23P01" {
		t.Fatalf("overlapping showtime accepted: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE audit_log SET action = 'x' WHERE id = (SELECT min(id) FROM audit_log)`)
	if err == nil {
		t.Fatal("audit log is not append-only")
	}
	_, err = pool.Exec(ctx, `UPDATE gift_cards SET balance_kurus = -1 WHERE code = 'HC-DEMO-500'`)
	if err == nil {
		t.Fatal("negative gift card balance accepted")
	}
}

func TestSchedule_GeneratePreviewAndValidateSeeded(t *testing.T) {
	ctx := context.Background()
	prog := staff(t, "programci@sahnebilet.test", service.RoleProgrammer)
	today := seed.Today(svc.Cfg.TimeZone).Format("2006-01-02")
	v, err := svc.ValidateSchedule(ctx, today, 14)
	if err != nil || len(v.Problems) != 0 {
		t.Fatalf("seeded schedule: %v %+v", err, v.Problems)
	}
	future := seed.Today(svc.Cfg.TimeZone).AddDate(0, 0, 60).Format("2006-01-02")
	g, err := svc.GenerateSchedule(ctx, prog, future, 14, false)
	if err != nil || len(g.Problems) != 0 || g.Shows < 560 {
		t.Fatalf("generate: %v shows=%d %+v", err, g.Shows, g.Problems)
	}
	if _, err := svc.GenerateSchedule(ctx, prog, today, 7, true); code(err) != "PERIOD_NOT_EMPTY" {
		t.Fatalf("save over existing: %v", err)
	}
}

func TestAuth_LoginRefreshRotation(t *testing.T) {
	ctx := context.Background()
	if _, err := svc.Login(ctx, "musteri@sahnebilet.test", "yanlis"); code(err) != "INVALID_CREDENTIALS" {
		t.Fatalf("wrong password: %v", err)
	}
	tk, err := svc.Login(ctx, "MUSTERI@sahnebilet.test", seed.DemoPassword)
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.ParseAccess(tk.AccessToken)
	if err != nil || a.Role != service.RoleCustomer {
		t.Fatalf("%v %+v", err, a)
	}
	if _, err := svc.Refresh(ctx, tk.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, tk.RefreshToken); code(err) != "INVALID_REFRESH_TOKEN" {
		t.Fatalf("reused refresh token: %v", err)
	}
	var e *service.Error
	if errors.As(err, &e) && e.Status != http.StatusUnauthorized {
		t.Fatal("refresh error must be 401")
	}
}

// Regression: refunding one ticket of a two-ticket order used to refund both.
func TestRefund_PartialKeepsOtherTickets(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	a := customer(t, 64)
	o, err := buy(t, a, show, rowSeats(t, show, 5, 2), service.OrderRequest{Combos: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := o.Lines[0]
	res, err := svc.Refund(ctx, a, o.ID, []string{first.TicketID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "partially_refunded" || res.Refund.Amount != first.Paid || len(res.LineIDs) != 1 || res.Refund.RestoreCoupon {
		t.Fatalf("partial refund %+v, ticket paid %d", res, first.Paid)
	}
	after, _ := svc.GetOrder(ctx, a, o.ID)
	valid := 0
	for _, l := range after.Lines {
		if l.Status == "valid" {
			valid++
		}
	}
	if valid != 2 { // the other ticket and the combo
		t.Fatalf("want 2 valid lines after partial refund, got %d", valid)
	}
	if _, err := svc.Refund(ctx, a, o.ID, []string{"not-a-ticket"}); code(err) != "VALIDATION_ERROR" {
		t.Fatalf("unknown ticket id: %v", err)
	}
}

func TestLiveSeatEvents_NotifyOnHoldAndRelease(t *testing.T) {
	ctx := context.Background()
	show := freshShow(t)
	ch, cancel := svc.Subscribe(show)
	defer cancel()
	other, cancelOther := svc.Subscribe(show + 100000)
	defer cancelOther()
	a := customer(t, 65)
	h, err := svc.CreateHold(ctx, a, service.HoldRequest{ShowtimeID: show, SeatIDs: rowSeats(t, show, 6, 2)})
	if err != nil {
		t.Fatal(err)
	}
	wait := func(what string) {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("no seat event after %s", what)
		}
	}
	wait("hold")
	if err := svc.ReleaseHold(ctx, a, h.ID); err != nil {
		t.Fatal(err)
	}
	wait("release")
	select {
	case <-other:
		t.Fatal("subscriber of another showtime was notified")
	default:
	}
}
