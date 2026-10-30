// Package httpapi exposes the service over HTTP/JSON.
package httpapi

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"sahnebilet/internal/service"
)

//go:embed openapi.yaml
var openapiSpec []byte

type Server struct {
	svc  *service.Service
	log  *slog.Logger
	stop <-chan struct{} // closed on shutdown: ends SSE streams
}

const (
	cust  = service.RoleCustomer
	box   = service.RoleBoxOffice
	mgr   = service.RoleHallManager
	prog  = service.RoleProgrammer
	acct  = service.RoleAccounting
	gateR = service.RoleGate
)

// New builds the HTTP handler with all routes. Every /api route except
// login and refresh requires a token; the role lists below are the RBAC
// matrix (documented in README).
func New(svc *service.Service, log *slog.Logger, stop <-chan struct{}) http.Handler {
	s := &Server{svc: svc, log: log, stop: stop}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})

	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/refresh", s.refresh)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.Handle("GET /api/me", s.auth(s.me))

	mux.Handle("GET /api/halls", s.auth(s.listHalls))
	mux.Handle("GET /api/halls/{id}/seats", s.auth(s.hallSeats))
	mux.Handle("PATCH /api/seats/{id}", s.auth(s.updateSeat, mgr))

	mux.Handle("GET /api/movies", s.auth(s.listMovies))
	mux.Handle("POST /api/movies", s.auth(s.createMovie, prog))

	mux.Handle("GET /api/showtimes", s.auth(s.listShowtimes))
	mux.Handle("GET /api/showtimes/{id}", s.auth(s.getShowtime))
	mux.Handle("GET /api/showtimes/{id}/seats", s.auth(s.showtimeSeats))
	mux.Handle("GET /api/showtimes/{id}/events", s.auth(s.seatEvents))
	mux.Handle("GET /api/showtimes/{id}/suggest", s.auth(s.suggest, cust, box, mgr))
	mux.Handle("GET /api/showtimes/{id}/integrity", s.auth(s.integrity, mgr, acct))

	mux.Handle("POST /api/holds", s.auth(s.idem(s.createHold), cust, box, mgr))
	mux.Handle("GET /api/holds/{id}", s.auth(s.getHold, cust, box, mgr))
	mux.Handle("POST /api/holds/{id}/extend", s.auth(s.extendHold, cust, box, mgr))
	mux.Handle("DELETE /api/holds/{id}", s.auth(s.releaseHold, cust, box, mgr))

	mux.Handle("POST /api/quote", s.auth(s.quote, cust, box, mgr))
	mux.Handle("POST /api/orders", s.auth(s.idem(s.createOrder), cust, box, mgr))
	mux.Handle("GET /api/orders", s.auth(s.listOrders, cust, box, mgr, acct))
	mux.Handle("GET /api/orders/{id}", s.auth(s.getOrder, cust, box, mgr, acct))
	mux.Handle("POST /api/orders/{id}/refund", s.auth(s.idem(s.refund), cust, box, mgr))
	mux.Handle("GET /api/tickets/{id}/qr", s.auth(s.ticketQR, cust, box))

	mux.Handle("GET /api/gate/state", s.auth(s.gateState, gateR, mgr))
	mux.Handle("POST /api/gate/scan", s.auth(s.gateScan, gateR))
	mux.Handle("POST /api/gate/sync", s.auth(s.gateSync, gateR))
	mux.Handle("GET /api/gate/conflicts", s.auth(s.gateConflicts, gateR, mgr))

	mux.Handle("GET /api/reports/revenue", s.auth(s.revenue, acct))
	mux.Handle("GET /api/reports/distributor", s.auth(s.distributor, acct))
	mux.Handle("GET /api/reports/ab", s.auth(s.abReport, acct))
	mux.Handle("GET /api/offers/me", s.auth(s.offer, cust))

	mux.Handle("POST /api/schedule/generate", s.auth(s.generateSchedule, prog))
	mux.Handle("GET /api/schedule/validate", s.auth(s.validateSchedule, prog, mgr))

	mux.Handle("GET /api/coupons", s.auth(s.listCoupons, acct, prog))
	mux.Handle("POST /api/coupons", s.auth(s.createCoupon, acct))
	mux.Handle("GET /api/gift-cards/{code}", s.auth(s.getGiftCard, cust, box))
	mux.Handle("POST /api/gift-cards", s.auth(s.idem(s.issueGiftCard), box))

	mux.Handle("GET /api/audit", s.auth(s.auditLog, acct, mgr))
	mux.Handle("GET /api/stats/live", s.auth(s.live))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errBody("NOT_FOUND", "no route for "+r.Method+" "+r.URL.Path))
	})
	return s.observe(withActorSlot(mux))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.svc.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "database": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, &service.Error{Status: http.StatusBadRequest, Code: "BAD_ID", Message: "id must be a positive integer"}
	}
	return id, nil
}

func queryInt(r *http.Request, k string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return v
	}
	return def
}

// --- auth

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.svc.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, a service.Actor) {
	writeJSON(w, http.StatusOK, a)
}

// --- catalog

func (s *Server) listHalls(w http.ResponseWriter, r *http.Request, a service.Actor) {
	h, err := s.svc.ListHalls(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": h})
}

func (s *Server) hallSeats(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	seats, err := s.svc.HallSeats(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": seats})
}

func (s *Server) updateSeat(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	seat, err := s.svc.UpdateSeatKind(r.Context(), a, id, req.Kind)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seat)
}

func (s *Server) listMovies(w http.ResponseWriter, r *http.Request, a service.Actor) {
	m, err := s.svc.ListMovies(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": m})
}

func (s *Server) createMovie(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var m service.Movie
	if err := decode(r, &m); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.svc.CreateMovie(r.Context(), a, m)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listShowtimes(w http.ResponseWriter, r *http.Request, a service.Actor) {
	q := r.URL.Query()
	f := service.ShowtimeFilter{Cursor: q.Get("cursor"), Limit: queryInt(r, "limit", 50)}
	f.From = time.Now().Add(-3 * time.Hour)
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, r, &service.Error{Status: 400, Code: "BAD_QUERY", Message: "from must be RFC3339"})
			return
		}
		f.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, r, &service.Error{Status: 400, Code: "BAD_QUERY", Message: "to must be RFC3339"})
			return
		}
		f.To = t
	}
	f.MovieID = int64(queryInt(r, "movie_id", 0))
	f.HallID = int64(queryInt(r, "hall_id", 0))
	p, err := s.svc.ListShowtimes(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) getShowtime(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.svc.GetShowtime(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) showtimeSeats(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	m, err := s.svc.ShowtimeSeats(r.Context(), a, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) suggest(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sg, err := s.svc.Suggest(r.Context(), a, id, queryInt(r, "n", 2), r.URL.Query().Get("accessible") == "true")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": sg})
}

func (s *Server) integrity(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in, err := s.svc.CheckIntegrity(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// --- holds & orders

func (s *Server) createHold(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req service.HoldRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	h, err := s.svc.CreateHold(r.Context(), a, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) getHold(w http.ResponseWriter, r *http.Request, a service.Actor) {
	h, err := s.svc.GetHold(r.Context(), a, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) extendHold(w http.ResponseWriter, r *http.Request, a service.Actor) {
	h, err := s.svc.ExtendHold(r.Context(), a, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) releaseHold(w http.ResponseWriter, r *http.Request, a service.Actor) {
	if err := s.svc.ReleaseHold(r.Context(), a, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) quote(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req service.OrderRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	q, err := s.svc.Quote(r.Context(), a, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req service.OrderRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	o, err := s.svc.CreateOrder(r.Context(), a, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request, a service.Actor) {
	p, err := s.svc.ListOrders(r.Context(), a, r.URL.Query().Get("cursor"), queryInt(r, "limit", 20))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request, a service.Actor) {
	o, err := s.svc.GetOrder(r.Context(), a, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) refund(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req struct {
		TicketIDs []string `json:"ticket_ids"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.svc.Refund(r.Context(), a, r.PathValue("id"), req.TicketIDs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) ticketQR(w http.ResponseWriter, r *http.Request, a service.Actor) {
	pass, err := s.svc.TicketQR(r.Context(), a, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pass)
}

// --- gate

func (s *Server) gateState(w http.ResponseWriter, r *http.Request, a service.Actor) {
	st, err := s.svc.State(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) gateScan(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req struct {
		GateID string `json:"gate_id"`
		Code   string `json:"code"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.svc.Scan(r.Context(), a, req.GateID, req.Code)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) gateSync(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req service.SyncRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.svc.Sync(r.Context(), a, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) gateConflicts(w http.ResponseWriter, r *http.Request, a service.Actor) {
	c, err := s.svc.ListConflicts(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": c})
}

// --- reports & admin

func (s *Server) rangeQuery(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	loc := s.svc.Cfg.TimeZone
	from, err := time.ParseInLocation("2006-01-02", q.Get("from"), loc)
	if err != nil {
		return from, from, &service.Error{Status: 400, Code: "BAD_QUERY", Message: "from must be YYYY-MM-DD"}
	}
	to, err := time.ParseInLocation("2006-01-02", q.Get("to"), loc)
	if err != nil {
		return from, to, &service.Error{Status: 400, Code: "BAD_QUERY", Message: "to must be YYYY-MM-DD"}
	}
	return from, to.AddDate(0, 0, 1), nil // inclusive end date
}

func (s *Server) revenue(w http.ResponseWriter, r *http.Request, a service.Actor) {
	from, to, err := s.rangeQuery(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.Revenue(r.Context(), from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) distributor(w http.ResponseWriter, r *http.Request, a service.Actor) {
	from, to, err := s.rangeQuery(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.svc.Distributor(r.Context(), from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (s *Server) generateSchedule(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req struct {
		From string `json:"from"`
		Days int    `json:"days"`
		Save bool   `json:"save"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.svc.GenerateSchedule(r.Context(), a, req.From, req.Days, req.Save)
	if err != nil {
		if se, ok := err.(*service.Error); ok && se.Code == "SCHEDULE_INVALID" {
			se.Details = res.Problems
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) validateSchedule(w http.ResponseWriter, r *http.Request, a service.Actor) {
	res, err := s.svc.ValidateSchedule(r.Context(), r.URL.Query().Get("from"), queryInt(r, "days", 7))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) listCoupons(w http.ResponseWriter, r *http.Request, a service.Actor) {
	c, err := s.svc.ListCoupons(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": c})
}

func (s *Server) createCoupon(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var c service.Coupon
	if err := decode(r, &c); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.svc.CreateCoupon(r.Context(), a, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getGiftCard(w http.ResponseWriter, r *http.Request, a service.Actor) {
	g, err := s.svc.GetGiftCard(r.Context(), r.PathValue("code"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) issueGiftCard(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var req struct {
		Amount int64 `json:"amount_kurus"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	g, err := s.svc.IssueGiftCard(r.Context(), a, req.Amount)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request, a service.Actor) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	items, next, err := s.svc.Audit(r.Context(), before, queryInt(r, "limit", 50))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"items": items}
	if next > 0 {
		out["next_before"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) live(w http.ResponseWriter, r *http.Request, a service.Actor) {
	l, err := s.svc.Live(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// seatEvents streams "seats" events (Server-Sent Events) whenever the seat
// map of the showtime changes; the client then refetches the map. The
// browser uses fetch() streaming so the token stays in the Authorization
// header instead of the URL.
func (s *Server) seatEvents(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.svc.GetShowtime(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, cancel := s.svc.Subscribe(id)
	defer cancel()
	send := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: 3000\nevent: ready\ndata: {\"showtime_id\":%d}\n\n", id) {
		return
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	maxAge := time.NewTimer(30 * time.Minute) // clients reconnect; keeps connections from living forever
	defer maxAge.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		case <-maxAge.C:
			return
		case <-ch:
			if !send("event: seats\ndata: {\"showtime_id\":%d}\n\n", id) {
				return
			}
		case <-ping.C:
			if !send(": ping\n\n") {
				return
			}
		}
	}
}

func (s *Server) offer(w http.ResponseWriter, r *http.Request, a service.Actor) {
	writeJSON(w, http.StatusOK, s.svc.OfferFor(a))
}

func (s *Server) abReport(w http.ResponseWriter, r *http.Request, a service.Actor) {
	from, to, err := s.rangeQuery(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.svc.ABReport(r.Context(), from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
